package whatsapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
)

func openFastStoreTestDB(t *testing.T) (*sql.DB, *sqlstore.Container) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "faststore.db")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	container := sqlstore.NewWithDB(db, "sqlite3", nil)
	if err := container.Upgrade(context.Background()); err != nil {
		t.Fatalf("container.Upgrade: %v", err)
	}
	return db, container
}

func makeTestDevice(container *sqlstore.Container, user string) *store.Device {
	dev := container.NewDevice()
	jid := types.NewJID(user, types.DefaultUserServer)
	jid.Device = 1
	dev.ID = &jid
	dev.LID = types.NewJID("999000111222", types.HiddenUserServer)
	dev.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte("details"),
		AccountSignature:    bytes.Repeat([]byte{0x01}, 64),
		AccountSignatureKey: bytes.Repeat([]byte{0x02}, 32),
		DeviceSignature:     bytes.Repeat([]byte{0x03}, 64),
	}
	return dev
}

func TestFastStore_BatchedPreKeysSessionsAndPreload(t *testing.T) {
	ctx := context.Background()
	db, container := openFastStoreTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	dev := makeTestDevice(container, "15551234567")

	fdc := attachFastStore(ctx, dev, db, logger)
	t.Cleanup(func() { fdc.Close(context.Background()) })

	if err := dev.Save(ctx); err != nil {
		t.Fatalf("dev.Save: %v", err)
	}
	if _, ok := dev.Sessions.(*fastSessionStore); !ok {
		t.Fatalf("expected dev.Sessions to be *fastSessionStore after Save, got %T", dev.Sessions)
	}
	if dev.Container != fdc {
		t.Fatal("expected dev.Container to remain *fastDeviceContainer after Save")
	}

	preKeys, err := dev.PreKeys.GetOrGenPreKeys(ctx, 812)
	if err != nil {
		t.Fatalf("GetOrGenPreKeys(812): %v", err)
	}
	if len(preKeys) != 812 {
		t.Fatalf("expected 812 prekeys, got %d", len(preKeys))
	}
	var pkCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM whatsmeow_pre_keys WHERE jid=?`, dev.ID.String()).Scan(&pkCount); err != nil {
		t.Fatalf("count prekeys in SQL: %v", err)
	}
	if pkCount != 812 {
		t.Fatalf("expected 812 prekeys persisted in SQL, got %d", pkCount)
	}
	if gotPK, err := dev.PreKeys.GetPreKey(ctx, preKeys[100].KeyID); err != nil || gotPK == nil || gotPK.KeyID != preKeys[100].KeyID {
		t.Fatalf("GetPreKey cache lookup failed: got=%v err=%v", gotPK, err)
	}

	sessBatch := make(map[string][]byte, 50)
	addrs := make([]string, 0, 50)
	for i := range 50 {
		addr := fmt.Sprintf("1555000%04d:0", i)
		sessBatch[addr] = []byte(fmt.Sprintf("session-payload-%d", i))
		addrs = append(addrs, addr)
	}
	if err := dev.Sessions.PutManySessions(ctx, sessBatch); err != nil {
		t.Fatalf("PutManySessions: %v", err)
	}

	gotMany, err := dev.Sessions.GetManySessions(ctx, addrs)
	if err != nil {
		t.Fatalf("GetManySessions: %v", err)
	}
	if len(gotMany) != 50 || !bytes.Equal(gotMany["15550000007:0"], []byte("session-payload-7")) {
		t.Fatalf("unexpected GetManySessions result: len=%d sample=%q", len(gotMany), gotMany["15550000007:0"])
	}
	if has, err := dev.Sessions.HasSession(ctx, "15550000007:0"); err != nil || !has {
		t.Fatalf("HasSession = %v, %v", has, err)
	}

	var idKey [32]byte
	copy(idKey[:], bytes.Repeat([]byte{0xAB}, 32))
	if err := dev.Identities.PutIdentity(ctx, "15550000007:0", idKey); err != nil {
		t.Fatalf("PutIdentity: %v", err)
	}
	if trusted, err := dev.Identities.IsTrustedIdentity(ctx, "15550000007:0", idKey); err != nil || !trusted {
		t.Fatalf("IsTrustedIdentity = %v, %v", trusted, err)
	}
	var wrongKey [32]byte
	wrongKey[0] = 0xCD
	if trusted, err := dev.Identities.IsTrustedIdentity(ctx, "15550000007:0", wrongKey); err != nil || trusted {
		t.Fatalf("expected mismatching key to be untrusted, got trusted=%v err=%v", trusted, err)
	}

	if err := dev.SenderKeys.PutSenderKey(ctx, "1203630001@g.us", "15550000007:0", []byte("sk-bytes")); err != nil {
		t.Fatalf("PutSenderKey: %v", err)
	}
	if sk, err := dev.SenderKeys.GetSenderKey(ctx, "1203630001@g.us", "15550000007:0"); err != nil || !bytes.Equal(sk, []byte("sk-bytes")) {
		t.Fatalf("GetSenderKey = %q, %v", sk, err)
	}

	chatJID := types.NewJID("1203630001", types.GroupServer)
	senderJID := types.NewJID("15550000007", types.DefaultUserServer)
	if err := dev.MsgSecrets.PutMessageSecret(ctx, chatJID, senderJID, "MSG-1", []byte("secret-1")); err != nil {
		t.Fatalf("PutMessageSecret: %v", err)
	}
	if sec, realSender, err := dev.MsgSecrets.GetMessageSecret(ctx, chatJID, senderJID, "MSG-1"); err != nil || !bytes.Equal(sec, []byte("secret-1")) || realSender != senderJID {
		t.Fatalf("GetMessageSecret = %q, %v, %v", sec, realSender, err)
	}

	evHash := sha256.Sum256([]byte("ciphertext-1"))
	err = dev.EventBuffer.DoDecryptionTxn(ctx, func(txCtx context.Context) error {
		return dev.EventBuffer.PutBufferedEvent(txCtx, evHash, []byte("plaintext-1"), time.Unix(1700000000, 0))
	})
	if err != nil {
		t.Fatalf("DoDecryptionTxn: %v", err)
	}
	if ev, err := dev.EventBuffer.GetBufferedEvent(ctx, evHash); err != nil || ev == nil || !bytes.Equal(ev.Plaintext, []byte("plaintext-1")) {
		t.Fatalf("GetBufferedEvent before clear = %+v, err=%v", ev, err)
	}
	if err := dev.EventBuffer.ClearBufferedEventPlaintext(ctx, evHash); err != nil {
		t.Fatalf("ClearBufferedEventPlaintext: %v", err)
	}
	if ev, err := dev.EventBuffer.GetBufferedEvent(ctx, evHash); err != nil || ev == nil || ev.Plaintext != nil {
		t.Fatalf("expected cleared plaintext in RAM, got %+v, err=%v", ev, err)
	}

	if err := dev.EventBuffer.AddOutgoingEvent(ctx, chatJID, "OUT-1", "raw", []byte("outgoing-payload")); err != nil {
		t.Fatalf("AddOutgoingEvent: %v", err)
	}
	if fmtName, pt, err := dev.EventBuffer.GetOutgoingEvent(ctx, chatJID, types.EmptyJID, "OUT-1"); err != nil || fmtName != "raw" || !bytes.Equal(pt, []byte("outgoing-payload")) {
		t.Fatalf("GetOutgoingEvent = %q, %q, %v", fmtName, pt, err)
	}

	if err := fdc.Flush(ctx); err != nil {
		t.Fatalf("fdc.Flush: %v", err)
	}

	var sessCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM whatsmeow_sessions WHERE our_jid=?`, dev.ID.String()).Scan(&sessCount); err != nil || sessCount != 50 {
		t.Fatalf("persisted session count = %d, err=%v", sessCount, err)
	}
	var outFmt string
	var outPt []byte
	if err := db.QueryRowContext(ctx, `SELECT format, plaintext FROM whatsmeow_retry_buffer WHERE our_jid=? AND chat_jid=? AND message_id=?`, dev.ID.String(), chatJID.String(), "OUT-1").Scan(&outFmt, &outPt); err != nil || outFmt != "raw" || !bytes.Equal(outPt, []byte("outgoing-payload")) {
		t.Fatalf("persisted retry_buffer = %q, %q, err=%v", outFmt, outPt, err)
	}
	var msgSec []byte
	if err := db.QueryRowContext(ctx, `SELECT key FROM whatsmeow_message_secrets WHERE our_jid=? AND chat_jid=? AND sender_jid=? AND message_id=?`, dev.ID.String(), chatJID.String(), senderJID.String(), "MSG-1").Scan(&msgSec); err != nil || !bytes.Equal(msgSec, []byte("secret-1")) {
		t.Fatalf("persisted message_secrets = %q, err=%v", msgSec, err)
	}
	var ptCheck []byte
	if err := db.QueryRowContext(ctx, `SELECT plaintext FROM whatsmeow_event_buffer WHERE our_jid=? AND ciphertext_hash=?`, dev.ID.String(), evHash[:]).Scan(&ptCheck); err != nil || ptCheck != nil {
		t.Fatalf("persisted event_buffer plaintext after clear = %v, err=%v", ptCheck, err)
	}

	dev2, err := container.GetFirstDevice(ctx)
	if err != nil || dev2 == nil {
		t.Fatalf("GetFirstDevice: %v", err)
	}
	fdc2 := attachFastStore(ctx, dev2, db, logger)
	t.Cleanup(func() { fdc2.Close(context.Background()) })

	if sess, err := dev2.Sessions.GetSession(ctx, "15550000042:0"); err != nil || !bytes.Equal(sess, []byte("session-payload-42")) {
		t.Fatalf("preloaded session = %q, err=%v", sess, err)
	}
	if trusted, err := dev2.Identities.IsTrustedIdentity(ctx, "15550000007:0", idKey); err != nil || !trusted {
		t.Fatalf("preloaded identity = %v, err=%v", trusted, err)
	}
	if sk, err := dev2.SenderKeys.GetSenderKey(ctx, "1203630001@g.us", "15550000007:0"); err != nil || !bytes.Equal(sk, []byte("sk-bytes")) {
		t.Fatalf("preloaded sender key = %q, err=%v", sk, err)
	}
	if ev, err := dev2.EventBuffer.GetBufferedEvent(ctx, evHash); err != nil || ev == nil || ev.Plaintext != nil {
		t.Fatalf("preloaded event buffer = %+v, err=%v", ev, err)
	}
}

func TestFastStore_ConcurrentRaceFree(t *testing.T) {
	ctx := context.Background()
	db, container := openFastStoreTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	dev := makeTestDevice(container, "15559990000")
	if err := dev.Save(ctx); err != nil {
		t.Fatalf("dev.Save: %v", err)
	}
	fdc := attachFastStore(ctx, dev, db, logger)
	t.Cleanup(func() { fdc.Close(context.Background()) })

	var idKey [32]byte
	idKey[0] = 0x42
	chatJID := types.NewJID("1203639999", types.GroupServer)
	senderJID := types.NewJID("15551112222", types.DefaultUserServer)

	var wg sync.WaitGroup
	for w := range 6 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 20 {
				addr := fmt.Sprintf("1555%02d%02d:0", worker, i)
				_ = dev.Sessions.PutManySessions(ctx, map[string][]byte{addr: []byte("sess")})
				_, _ = dev.Sessions.GetSession(ctx, addr)
				_ = dev.Identities.PutIdentity(ctx, addr, idKey)
				_, _ = dev.Identities.IsTrustedIdentity(ctx, addr, idKey)
				_ = dev.SenderKeys.PutSenderKey(ctx, chatJID.String(), addr, []byte("sk"))
				_, _ = dev.SenderKeys.GetSenderKey(ctx, chatJID.String(), addr)
				_ = dev.MsgSecrets.PutMessageSecret(ctx, chatJID, senderJID, types.MessageID(addr), []byte("sec"))
				_, _, _ = dev.MsgSecrets.GetMessageSecret(ctx, chatJID, senderJID, types.MessageID(addr))
				h := sha256.Sum256([]byte(addr))
				_ = dev.EventBuffer.PutBufferedEvent(ctx, h, []byte("pt"), time.Now())
				_, _ = dev.EventBuffer.GetBufferedEvent(ctx, h)
				_ = dev.EventBuffer.ClearBufferedEventPlaintext(ctx, h)
			}
		}(w)
	}
	wg.Wait()

	if err := fdc.Flush(ctx); err != nil {
		t.Fatalf("final concurrent Flush: %v", err)
	}
}
