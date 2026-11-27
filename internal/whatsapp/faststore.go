package whatsapp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/keys"
)

const (
	fastStoreFlushInterval   = 300 * time.Millisecond
	fastStoreImmediateThresh = 32
	fastStoreMaxBatchRows    = 200
	fastStorePreKeyBatchRows = 200
	fastStoreMsgSecretMax    = 4000
	fastStoreEventBufMax     = 5000
	fastStoreOutgoingBufMax  = 2000
)

type senderKeyID struct {
	group string
	user  string
}

type msgSecretKey struct {
	chat   string
	sender string
	id     string
}

type msgSecretEntry struct {
	secret []byte
	sender types.JID
}

type outgoingBufKey struct {
	chat string
	id   string
}

type outgoingBufEntry struct {
	format    string
	plaintext []byte
}

type fastDeviceContainer struct {
	inner  store.DeviceContainer
	db     *sql.DB
	logger *slog.Logger

	mu      sync.Mutex
	session *fastSessionStore
}

func attachFastStore(ctx context.Context, device *store.Device, db *sql.DB, logger *slog.Logger) *fastDeviceContainer {
	if device == nil || db == nil {
		return nil
	}
	if existing, ok := device.Container.(*fastDeviceContainer); ok {
		return existing
	}
	fdc := &fastDeviceContainer{
		inner:  device.Container,
		db:     db,
		logger: logger,
	}
	device.Container = fdc
	if device.ID != nil && device.Initialized {
		fs := newFastSessionStore(ctx, device, db, logger)
		fdc.session = fs
		device.SetAllStores(fs)
	}
	return fdc
}

func (c *fastDeviceContainer) PutDevice(ctx context.Context, device *store.Device) error {
	if err := c.inner.PutDevice(ctx, device); err != nil {
		return err
	}
	device.Container = c
	if device.ID != nil {
		c.mu.Lock()
		if _, ok := device.Sessions.(*fastSessionStore); !ok {
			if c.session == nil || c.session.ourJID != device.ID.String() {
				if c.session != nil {
					c.session.Close(ctx)
				}
				c.session = newFastSessionStore(ctx, device, c.db, c.logger)
			}
			device.SetAllStores(c.session)
		}
		c.mu.Unlock()
	}
	return nil
}

func (c *fastDeviceContainer) DeleteDevice(ctx context.Context, device *store.Device) error {
	c.mu.Lock()
	s := c.session
	c.session = nil
	c.mu.Unlock()
	if s != nil {
		s.Close(ctx)
	}
	return c.inner.DeleteDevice(ctx, device)
}

func (c *fastDeviceContainer) Flush(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	s := c.session
	c.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Flush(ctx)
}

func (c *fastDeviceContainer) Close(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	s := c.session
	c.mu.Unlock()
	if s != nil {
		s.Close(ctx)
	}
}

type fastSessionStore struct {
	store.AllSessionSpecificStores
	db     *sql.DB
	ourJID string
	logger *slog.Logger

	mu               sync.RWMutex
	sessionsLoaded   bool
	sessions         map[string][]byte
	dirtySessions    map[string][]byte
	identitiesLoaded bool
	identities       map[string][32]byte
	dirtyIdentities  map[string][32]byte
	senderKeysLoaded bool
	senderKeys       map[senderKeyID][]byte
	dirtySenderKeys  map[senderKeyID][]byte

	preKeyMu    sync.Mutex
	preKeyCache map[uint32]*keys.PreKey

	msgSecrets      map[msgSecretKey]msgSecretEntry
	dirtyMsgSecrets map[msgSecretKey][]byte

	eventBufLoaded  bool
	eventBufEvicted bool
	eventBuf        map[[32]byte]*store.BufferedEvent
	dirtyEventBuf   map[[32]byte]store.BufferedEvent
	dirtyClearedBuf map[[32]byte]struct{}

	outgoingBuf   map[outgoingBufKey]outgoingBufEntry
	dirtyOutgoing map[outgoingBufKey]outgoingBufEntry

	flushMu     sync.Mutex
	flushSignal chan struct{}
	stopOnce    sync.Once
	stopCh      chan struct{}
	doneCh      chan struct{}
}

func newFastSessionStore(ctx context.Context, device *store.Device, db *sql.DB, logger *slog.Logger) *fastSessionStore {
	inner := underlyingAllStores(device)
	s := &fastSessionStore{
		AllSessionSpecificStores: inner,
		db:                       db,
		ourJID:                   device.ID.String(),
		logger:                   logger,
		sessions:                 make(map[string][]byte),
		dirtySessions:            make(map[string][]byte),
		identities:               make(map[string][32]byte),
		dirtyIdentities:          make(map[string][32]byte),
		senderKeys:               make(map[senderKeyID][]byte),
		dirtySenderKeys:          make(map[senderKeyID][]byte),
		preKeyCache:              make(map[uint32]*keys.PreKey),
		msgSecrets:               make(map[msgSecretKey]msgSecretEntry),
		dirtyMsgSecrets:          make(map[msgSecretKey][]byte),
		eventBuf:                 make(map[[32]byte]*store.BufferedEvent),
		dirtyEventBuf:            make(map[[32]byte]store.BufferedEvent),
		dirtyClearedBuf:          make(map[[32]byte]struct{}),
		outgoingBuf:              make(map[outgoingBufKey]outgoingBufEntry),
		dirtyOutgoing:            make(map[outgoingBufKey]outgoingBufEntry),
		flushSignal:              make(chan struct{}, 1),
		stopCh:                   make(chan struct{}),
		doneCh:                   make(chan struct{}),
	}
	s.preload(ctx)
	go s.runFlusher()
	return s
}

func underlyingAllStores(device *store.Device) store.AllSessionSpecificStores {
	if existing, ok := device.Sessions.(*fastSessionStore); ok {
		return existing.AllSessionSpecificStores
	}
	return device.Sessions.(store.AllSessionSpecificStores)
}

func (s *fastSessionStore) preload(ctx context.Context) {
	if s.db == nil || s.ourJID == "" {
		return
	}
	loadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		rows, err := s.db.QueryContext(loadCtx, `SELECT their_id, session FROM whatsmeow_sessions WHERE our_jid=$1`, s.ourJID)
		if err != nil {
			s.warn("preload sessions failed", err)
			return
		}
		defer rows.Close()
		loaded := make(map[string][]byte)
		for rows.Next() {
			var addr string
			var sess []byte
			if scanErr := rows.Scan(&addr, &sess); scanErr != nil {
				s.warn("scan preloaded session failed", scanErr)
				return
			}
			loaded[addr] = append([]byte(nil), sess...)
		}
		if rows.Err() != nil {
			s.warn("iterate preloaded sessions failed", rows.Err())
			return
		}
		s.mu.Lock()
		for k, v := range loaded {
			if _, dirty := s.dirtySessions[k]; !dirty {
				s.sessions[k] = v
			}
		}
		s.sessionsLoaded = true
		s.mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		rows, err := s.db.QueryContext(loadCtx, `SELECT their_id, identity FROM whatsmeow_identity_keys WHERE our_jid=$1`, s.ourJID)
		if err != nil {
			s.warn("preload identities failed", err)
			return
		}
		defer rows.Close()
		loaded := make(map[string][32]byte)
		for rows.Next() {
			var addr string
			var raw []byte
			if scanErr := rows.Scan(&addr, &raw); scanErr != nil {
				s.warn("scan preloaded identity failed", scanErr)
				return
			}
			if len(raw) == 32 {
				var key [32]byte
				copy(key[:], raw)
				loaded[addr] = key
			}
		}
		if rows.Err() != nil {
			s.warn("iterate preloaded identities failed", rows.Err())
			return
		}
		s.mu.Lock()
		for k, v := range loaded {
			if _, dirty := s.dirtyIdentities[k]; !dirty {
				s.identities[k] = v
			}
		}
		s.identitiesLoaded = true
		s.mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		rows, err := s.db.QueryContext(loadCtx, `SELECT chat_id, sender_id, sender_key FROM whatsmeow_sender_keys WHERE our_jid=$1`, s.ourJID)
		if err != nil {
			s.warn("preload sender keys failed", err)
			return
		}
		defer rows.Close()
		loaded := make(map[senderKeyID][]byte)
		for rows.Next() {
			var chatID, senderID string
			var key []byte
			if scanErr := rows.Scan(&chatID, &senderID, &key); scanErr != nil {
				s.warn("scan preloaded sender key failed", scanErr)
				return
			}
			loaded[senderKeyID{group: chatID, user: senderID}] = append([]byte(nil), key...)
		}
		if rows.Err() != nil {
			s.warn("iterate preloaded sender keys failed", rows.Err())
			return
		}
		s.mu.Lock()
		for k, v := range loaded {
			if _, dirty := s.dirtySenderKeys[k]; !dirty {
				s.senderKeys[k] = v
			}
		}
		s.senderKeysLoaded = true
		s.mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		rows, err := s.db.QueryContext(loadCtx, `SELECT ciphertext_hash, plaintext, server_timestamp, insert_timestamp FROM whatsmeow_event_buffer WHERE our_jid=$1`, s.ourJID)
		if err != nil {
			s.warn("preload event buffer failed", err)
			return
		}
		defer rows.Close()
		loaded := make(map[[32]byte]*store.BufferedEvent)
		for rows.Next() {
			var hashRaw, pt []byte
			var serverTS, insertTS int64
			if scanErr := rows.Scan(&hashRaw, &pt, &serverTS, &insertTS); scanErr != nil {
				s.warn("scan preloaded event buffer failed", scanErr)
				return
			}
			if len(hashRaw) != sha256.Size {
				continue
			}
			var h [32]byte
			copy(h[:], hashRaw)
			var ptCopy []byte
			if pt != nil {
				ptCopy = append([]byte(nil), pt...)
			}
			loaded[h] = &store.BufferedEvent{
				Plaintext:  ptCopy,
				ServerTime: time.Unix(serverTS, 0),
				InsertTime: time.UnixMilli(insertTS),
			}
		}
		if rows.Err() != nil {
			s.warn("iterate preloaded event buffer failed", rows.Err())
			return
		}
		s.mu.Lock()
		evicted := false
		for h, ev := range loaded {
			if len(s.eventBuf) >= fastStoreEventBufMax {
				evicted = true
				break
			}
			if _, dirty := s.dirtyEventBuf[h]; !dirty {
				if _, cleared := s.dirtyClearedBuf[h]; !cleared {
					s.eventBuf[h] = ev
				}
			}
		}
		s.eventBufLoaded = true
		if evicted {
			s.eventBufEvicted = true
		}
		s.mu.Unlock()
	}()

	wg.Wait()
}

func (s *fastSessionStore) warn(msg string, err error) {
	if s.logger != nil && err != nil {
		s.logger.Warn(msg, "error", err)
	}
}

func (s *fastSessionStore) runFlusher() {
	defer close(s.doneCh)
	ticker := time.NewTicker(fastStoreFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := s.Flush(ctx); err != nil {
				s.warn("final faststore flush failed", err)
			}
			cancel()
			return
		case <-ticker.C:
			s.flushBackground()
		case <-s.flushSignal:
			s.flushBackground()
		}
	}
}

func (s *fastSessionStore) flushBackground() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.Flush(ctx); err != nil {
		s.warn("background faststore flush failed", err)
	}
}

func (s *fastSessionStore) Close(ctx context.Context) {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	select {
	case <-s.doneCh:
	case <-ctx.Done():
	}
}

func (s *fastSessionStore) maybeSignalFlushLocked() {
	total := len(s.dirtySessions) + len(s.dirtyIdentities) + len(s.dirtySenderKeys) +
		len(s.dirtyMsgSecrets) + len(s.dirtyEventBuf) + len(s.dirtyClearedBuf) + len(s.dirtyOutgoing)
	if total >= fastStoreImmediateThresh {
		select {
		case s.flushSignal <- struct{}{}:
		default:
		}
	}
}

func (s *fastSessionStore) Flush(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	sessions := s.dirtySessions
	s.dirtySessions = make(map[string][]byte)
	identities := s.dirtyIdentities
	s.dirtyIdentities = make(map[string][32]byte)
	senderKeys := s.dirtySenderKeys
	s.dirtySenderKeys = make(map[senderKeyID][]byte)
	msgSecrets := s.dirtyMsgSecrets
	s.dirtyMsgSecrets = make(map[msgSecretKey][]byte)
	eventBuf := s.dirtyEventBuf
	s.dirtyEventBuf = make(map[[32]byte]store.BufferedEvent)
	clearedBuf := s.dirtyClearedBuf
	s.dirtyClearedBuf = make(map[[32]byte]struct{})
	outgoing := s.dirtyOutgoing
	s.dirtyOutgoing = make(map[outgoingBufKey]outgoingBufEntry)
	s.mu.Unlock()

	var errs []error
	if err := s.flushSessions(ctx, sessions); err != nil {
		errs = append(errs, err)
	}
	if err := s.flushIdentities(ctx, identities); err != nil {
		errs = append(errs, err)
	}
	if err := s.flushSenderKeys(ctx, senderKeys); err != nil {
		errs = append(errs, err)
	}
	if err := s.flushMsgSecrets(ctx, msgSecrets); err != nil {
		errs = append(errs, err)
	}
	if err := s.flushEventBuffer(ctx, eventBuf, clearedBuf); err != nil {
		errs = append(errs, err)
	}
	if err := s.flushOutgoingEvents(ctx, outgoing); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *fastSessionStore) flushSessions(ctx context.Context, batch map[string][]byte) error {
	if len(batch) == 0 {
		return nil
	}
	type item struct {
		addr string
		sess []byte
	}
	items := make([]item, 0, len(batch))
	for addr, sess := range batch {
		items = append(items, item{addr: addr, sess: sess})
	}
	for start := 0; start < len(items); start += fastStoreMaxBatchRows {
		end := min(start+fastStoreMaxBatchRows, len(items))
		chunk := items[start:end]
		args := make([]any, 0, len(chunk)*3)
		var b strings.Builder
		b.WriteString(`INSERT INTO whatsmeow_sessions (our_jid, their_id, session) VALUES `)
		for i, it := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			p := len(args) + 1
			fmt.Fprintf(&b, "($%d,$%d,$%d)", p, p+1, p+2)
			args = append(args, s.ourJID, it.addr, it.sess)
		}
		b.WriteString(` ON CONFLICT (our_jid, their_id) DO UPDATE SET session=excluded.session`)
		if _, err := s.db.ExecContext(ctx, b.String(), args...); err != nil {
			s.mu.Lock()
			for _, rem := range items[start:] {
				if _, exists := s.dirtySessions[rem.addr]; !exists {
					s.dirtySessions[rem.addr] = rem.sess
				}
			}
			s.mu.Unlock()
			return fmt.Errorf("flush sessions batch: %w", err)
		}
	}
	return nil
}

func (s *fastSessionStore) flushIdentities(ctx context.Context, batch map[string][32]byte) error {
	if len(batch) == 0 {
		return nil
	}
	type item struct {
		addr string
		key  [32]byte
	}
	items := make([]item, 0, len(batch))
	for addr, key := range batch {
		items = append(items, item{addr: addr, key: key})
	}
	for start := 0; start < len(items); start += fastStoreMaxBatchRows {
		end := min(start+fastStoreMaxBatchRows, len(items))
		chunk := items[start:end]
		args := make([]any, 0, len(chunk)*3)
		var b strings.Builder
		b.WriteString(`INSERT INTO whatsmeow_identity_keys (our_jid, their_id, identity) VALUES `)
		for i, it := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			p := len(args) + 1
			fmt.Fprintf(&b, "($%d,$%d,$%d)", p, p+1, p+2)
			k := it.key
			args = append(args, s.ourJID, it.addr, k[:])
		}
		b.WriteString(` ON CONFLICT (our_jid, their_id) DO UPDATE SET identity=excluded.identity`)
		if _, err := s.db.ExecContext(ctx, b.String(), args...); err != nil {
			s.mu.Lock()
			for _, rem := range items[start:] {
				if _, exists := s.dirtyIdentities[rem.addr]; !exists {
					s.dirtyIdentities[rem.addr] = rem.key
				}
			}
			s.mu.Unlock()
			return fmt.Errorf("flush identities batch: %w", err)
		}
	}
	return nil
}

func (s *fastSessionStore) flushSenderKeys(ctx context.Context, batch map[senderKeyID][]byte) error {
	if len(batch) == 0 {
		return nil
	}
	type item struct {
		id  senderKeyID
		key []byte
	}
	items := make([]item, 0, len(batch))
	for id, key := range batch {
		items = append(items, item{id: id, key: key})
	}
	for start := 0; start < len(items); start += fastStoreMaxBatchRows {
		end := min(start+fastStoreMaxBatchRows, len(items))
		chunk := items[start:end]
		args := make([]any, 0, len(chunk)*4)
		var b strings.Builder
		b.WriteString(`INSERT INTO whatsmeow_sender_keys (our_jid, chat_id, sender_id, sender_key) VALUES `)
		for i, it := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			p := len(args) + 1
			fmt.Fprintf(&b, "($%d,$%d,$%d,$%d)", p, p+1, p+2, p+3)
			args = append(args, s.ourJID, it.id.group, it.id.user, it.key)
		}
		b.WriteString(` ON CONFLICT (our_jid, chat_id, sender_id) DO UPDATE SET sender_key=excluded.sender_key`)
		if _, err := s.db.ExecContext(ctx, b.String(), args...); err != nil {
			s.mu.Lock()
			for _, rem := range items[start:] {
				if _, exists := s.dirtySenderKeys[rem.id]; !exists {
					s.dirtySenderKeys[rem.id] = rem.key
				}
			}
			s.mu.Unlock()
			return fmt.Errorf("flush sender keys batch: %w", err)
		}
	}
	return nil
}

func (s *fastSessionStore) flushMsgSecrets(ctx context.Context, batch map[msgSecretKey][]byte) error {
	if len(batch) == 0 {
		return nil
	}
	type item struct {
		k      msgSecretKey
		secret []byte
	}
	items := make([]item, 0, len(batch))
	for k, secret := range batch {
		items = append(items, item{k: k, secret: secret})
	}
	for start := 0; start < len(items); start += fastStoreMaxBatchRows {
		end := min(start+fastStoreMaxBatchRows, len(items))
		chunk := items[start:end]
		args := make([]any, 0, len(chunk)*5)
		var b strings.Builder
		b.WriteString(`INSERT INTO whatsmeow_message_secrets (our_jid, chat_jid, sender_jid, message_id, key) VALUES `)
		for i, it := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			p := len(args) + 1
			fmt.Fprintf(&b, "($%d,$%d,$%d,$%d,$%d)", p, p+1, p+2, p+3, p+4)
			args = append(args, s.ourJID, it.k.chat, it.k.sender, it.k.id, it.secret)
		}
		b.WriteString(` ON CONFLICT (our_jid, chat_jid, sender_jid, message_id) DO NOTHING`)
		if _, err := s.db.ExecContext(ctx, b.String(), args...); err != nil {
			s.mu.Lock()
			for _, rem := range items[start:] {
				if _, exists := s.dirtyMsgSecrets[rem.k]; !exists {
					s.dirtyMsgSecrets[rem.k] = rem.secret
				}
			}
			s.mu.Unlock()
			return fmt.Errorf("flush message secrets batch: %w", err)
		}
	}
	return nil
}

func (s *fastSessionStore) flushEventBuffer(ctx context.Context, inserts map[[32]byte]store.BufferedEvent, cleared map[[32]byte]struct{}) error {
	if len(inserts) > 0 {
		type item struct {
			h  [32]byte
			ev store.BufferedEvent
		}
		items := make([]item, 0, len(inserts))
		for h, ev := range inserts {
			items = append(items, item{h: h, ev: ev})
		}
		for start := 0; start < len(items); start += fastStoreMaxBatchRows {
			end := min(start+fastStoreMaxBatchRows, len(items))
			chunk := items[start:end]
			args := make([]any, 0, len(chunk)*5)
			var b strings.Builder
			b.WriteString(`INSERT INTO whatsmeow_event_buffer (our_jid, ciphertext_hash, plaintext, server_timestamp, insert_timestamp) VALUES `)
			for i, it := range chunk {
				if i > 0 {
					b.WriteByte(',')
				}
				p := len(args) + 1
				fmt.Fprintf(&b, "($%d,$%d,$%d,$%d,$%d)", p, p+1, p+2, p+3, p+4)
				h := it.h
				args = append(args, s.ourJID, h[:], it.ev.Plaintext, it.ev.ServerTime.Unix(), it.ev.InsertTime.UnixMilli())
			}
			b.WriteString(` ON CONFLICT (our_jid, ciphertext_hash) DO UPDATE SET plaintext=excluded.plaintext`)
			if _, err := s.db.ExecContext(ctx, b.String(), args...); err != nil {
				s.mu.Lock()
				for _, rem := range items[start:] {
					if _, exists := s.dirtyEventBuf[rem.h]; !exists {
						s.dirtyEventBuf[rem.h] = rem.ev
					}
				}
				for h := range cleared {
					s.dirtyClearedBuf[h] = struct{}{}
				}
				s.mu.Unlock()
				return fmt.Errorf("flush event buffer batch: %w", err)
			}
		}
	}

	if len(cleared) > 0 {
		hashes := make([][32]byte, 0, len(cleared))
		for h := range cleared {
			hashes = append(hashes, h)
		}
		for start := 0; start < len(hashes); start += fastStoreMaxBatchRows {
			end := min(start+fastStoreMaxBatchRows, len(hashes))
			chunk := hashes[start:end]
			args := make([]any, 0, 1+len(chunk))
			args = append(args, s.ourJID)
			var b strings.Builder
			b.WriteString(`UPDATE whatsmeow_event_buffer SET plaintext=NULL WHERE our_jid=$1 AND ciphertext_hash IN (`)
			for i, h := range chunk {
				if i > 0 {
					b.WriteByte(',')
				}
				p := len(args) + 1
				fmt.Fprintf(&b, "$%d", p)
				hCopy := h
				args = append(args, hCopy[:])
			}
			b.WriteByte(')')
			if _, err := s.db.ExecContext(ctx, b.String(), args...); err != nil {
				s.mu.Lock()
				for _, h := range hashes[start:] {
					s.dirtyClearedBuf[h] = struct{}{}
				}
				s.mu.Unlock()
				return fmt.Errorf("clear event buffer plaintext batch: %w", err)
			}
		}
	}
	return nil
}

func (s *fastSessionStore) flushOutgoingEvents(ctx context.Context, batch map[outgoingBufKey]outgoingBufEntry) error {
	if len(batch) == 0 {
		return nil
	}
	type item struct {
		k   outgoingBufKey
		val outgoingBufEntry
	}
	items := make([]item, 0, len(batch))
	for k, val := range batch {
		items = append(items, item{k: k, val: val})
	}
	nowMS := time.Now().UnixMilli()
	for start := 0; start < len(items); start += fastStoreMaxBatchRows {
		end := min(start+fastStoreMaxBatchRows, len(items))
		chunk := items[start:end]
		args := make([]any, 0, len(chunk)*6)
		var b strings.Builder
		b.WriteString(`INSERT INTO whatsmeow_retry_buffer (our_jid, chat_jid, message_id, format, plaintext, timestamp) VALUES `)
		for i, it := range chunk {
			if i > 0 {
				b.WriteByte(',')
			}
			p := len(args) + 1
			fmt.Fprintf(&b, "($%d,$%d,$%d,$%d,$%d,$%d)", p, p+1, p+2, p+3, p+4, p+5)
			args = append(args, s.ourJID, it.k.chat, it.k.id, it.val.format, it.val.plaintext, nowMS)
		}
		b.WriteString(` ON CONFLICT (our_jid, chat_jid, message_id) DO UPDATE SET format=excluded.format, plaintext=excluded.plaintext, timestamp=excluded.timestamp`)
		if _, err := s.db.ExecContext(ctx, b.String(), args...); err != nil {
			s.mu.Lock()
			for _, rem := range items[start:] {
				if _, exists := s.dirtyOutgoing[rem.k]; !exists {
					s.dirtyOutgoing[rem.k] = rem.val
				}
			}
			s.mu.Unlock()
			return fmt.Errorf("flush outgoing retry buffer batch: %w", err)
		}
	}
	return nil
}

func (s *fastSessionStore) GetSession(ctx context.Context, address string) ([]byte, error) {
	s.mu.RLock()
	sess, ok := s.sessions[address]
	loaded := s.sessionsLoaded
	s.mu.RUnlock()
	if ok || loaded {
		if len(sess) == 0 {
			return nil, nil
		}
		return append([]byte(nil), sess...), nil
	}

	sess, err := s.AllSessionSpecificStores.GetSession(ctx, address)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if _, exists := s.sessions[address]; !exists {
		if len(sess) > 0 {
			s.sessions[address] = append([]byte(nil), sess...)
		} else {
			s.sessions[address] = nil
		}
	}
	s.mu.Unlock()
	return sess, nil
}

func (s *fastSessionStore) HasSession(ctx context.Context, address string) (bool, error) {
	s.mu.RLock()
	sess, ok := s.sessions[address]
	loaded := s.sessionsLoaded
	s.mu.RUnlock()
	if ok || loaded {
		return len(sess) > 0, nil
	}
	return s.AllSessionSpecificStores.HasSession(ctx, address)
}

func (s *fastSessionStore) GetManySessions(ctx context.Context, addresses []string) (map[string][]byte, error) {
	if len(addresses) == 0 {
		return nil, nil
	}
	result := make(map[string][]byte, len(addresses))
	var missing []string

	s.mu.RLock()
	loaded := s.sessionsLoaded
	for _, addr := range addresses {
		sess, ok := s.sessions[addr]
		if ok || loaded {
			if len(sess) > 0 {
				result[addr] = append([]byte(nil), sess...)
			} else {
				result[addr] = nil
			}
		} else {
			missing = append(missing, addr)
		}
	}
	s.mu.RUnlock()

	if len(missing) == 0 {
		return result, nil
	}

	fetched, err := s.AllSessionSpecificStores.GetManySessions(ctx, missing)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, addr := range missing {
		sess := fetched[addr]
		result[addr] = sess
		if _, exists := s.sessions[addr]; !exists {
			if len(sess) > 0 {
				s.sessions[addr] = append([]byte(nil), sess...)
			} else {
				s.sessions[addr] = nil
			}
		}
	}
	s.mu.Unlock()
	return result, nil
}

func (s *fastSessionStore) PutSession(_ context.Context, address string, session []byte) error {
	cp := append([]byte(nil), session...)
	s.mu.Lock()
	s.sessions[address] = cp
	s.dirtySessions[address] = cp
	s.maybeSignalFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) PutManySessions(_ context.Context, sessions map[string][]byte) error {
	if len(sessions) == 0 {
		return nil
	}
	s.mu.Lock()
	for addr, sess := range sessions {
		cp := append([]byte(nil), sess...)
		s.sessions[addr] = cp
		s.dirtySessions[addr] = cp
	}
	s.maybeSignalFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) DeleteSession(ctx context.Context, address string) error {
	s.mu.Lock()
	delete(s.dirtySessions, address)
	s.sessions[address] = nil
	s.mu.Unlock()
	return s.AllSessionSpecificStores.DeleteSession(ctx, address)
}

func (s *fastSessionStore) DeleteAllSessions(ctx context.Context, phone string) error {
	if err := s.Flush(ctx); err != nil {
		s.warn("flush before DeleteAllSessions failed", err)
	}
	if err := s.AllSessionSpecificStores.DeleteAllSessions(ctx, phone); err != nil {
		return err
	}
	prefix := phone + ":"
	s.mu.Lock()
	for addr := range s.sessions {
		if strings.HasPrefix(addr, prefix) {
			delete(s.sessions, addr)
		}
	}
	for addr := range s.dirtySessions {
		if strings.HasPrefix(addr, prefix) {
			delete(s.dirtySessions, addr)
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) MigratePNToLID(ctx context.Context, pn, lid types.JID) error {
	if err := s.Flush(ctx); err != nil {
		s.warn("flush before MigratePNToLID failed", err)
	}
	if err := s.AllSessionSpecificStores.MigratePNToLID(ctx, pn, lid); err != nil {
		return err
	}
	pnPrefix := pn.SignalAddressUser() + ":"
	s.mu.Lock()
	for addr := range s.sessions {
		if strings.HasPrefix(addr, pnPrefix) {
			delete(s.sessions, addr)
		}
	}
	for addr := range s.identities {
		if strings.HasPrefix(addr, pnPrefix) {
			delete(s.identities, addr)
		}
	}
	for sk := range s.senderKeys {
		if strings.HasPrefix(sk.user, pnPrefix) {
			delete(s.senderKeys, sk)
		}
	}
	s.sessionsLoaded = false
	s.identitiesLoaded = false
	s.senderKeysLoaded = false
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) IsTrustedIdentity(ctx context.Context, address string, key [32]byte) (bool, error) {
	s.mu.RLock()
	existing, ok := s.identities[address]
	loaded := s.identitiesLoaded
	s.mu.RUnlock()
	if ok {
		return existing == key, nil
	}
	if loaded {
		return true, nil
	}
	return s.AllSessionSpecificStores.IsTrustedIdentity(ctx, address, key)
}

func (s *fastSessionStore) PutIdentity(_ context.Context, address string, key [32]byte) error {
	s.mu.Lock()
	s.identities[address] = key
	s.dirtyIdentities[address] = key
	s.maybeSignalFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) DeleteIdentity(ctx context.Context, address string) error {
	s.mu.Lock()
	delete(s.dirtyIdentities, address)
	delete(s.identities, address)
	s.mu.Unlock()
	return s.AllSessionSpecificStores.DeleteIdentity(ctx, address)
}

func (s *fastSessionStore) DeleteAllIdentities(ctx context.Context, phone string) error {
	if err := s.Flush(ctx); err != nil {
		s.warn("flush before DeleteAllIdentities failed", err)
	}
	if err := s.AllSessionSpecificStores.DeleteAllIdentities(ctx, phone); err != nil {
		return err
	}
	prefix := phone + ":"
	s.mu.Lock()
	for addr := range s.identities {
		if strings.HasPrefix(addr, prefix) {
			delete(s.identities, addr)
		}
	}
	for addr := range s.dirtyIdentities {
		if strings.HasPrefix(addr, prefix) {
			delete(s.dirtyIdentities, addr)
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) GetSenderKey(ctx context.Context, group, user string) ([]byte, error) {
	id := senderKeyID{group: group, user: user}
	s.mu.RLock()
	key, ok := s.senderKeys[id]
	loaded := s.senderKeysLoaded
	s.mu.RUnlock()
	if ok || loaded {
		if len(key) == 0 {
			return nil, nil
		}
		return append([]byte(nil), key...), nil
	}

	fetched, err := s.AllSessionSpecificStores.GetSenderKey(ctx, group, user)
	if err != nil {
		return nil, err
	}
	if len(fetched) > 0 {
		s.mu.Lock()
		if _, exists := s.senderKeys[id]; !exists {
			s.senderKeys[id] = append([]byte(nil), fetched...)
		}
		s.mu.Unlock()
	}
	return fetched, nil
}

func (s *fastSessionStore) PutSenderKey(_ context.Context, group, user string, session []byte) error {
	id := senderKeyID{group: group, user: user}
	cp := append([]byte(nil), session...)
	s.mu.Lock()
	s.senderKeys[id] = cp
	s.dirtySenderKeys[id] = cp
	s.maybeSignalFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) GetOrGenPreKeys(ctx context.Context, count uint32) ([]*keys.PreKey, error) {
	if s.db == nil {
		return s.AllSessionSpecificStores.GetOrGenPreKeys(ctx, count)
	}
	s.preKeyMu.Lock()
	defer s.preKeyMu.Unlock()

	rows, err := s.db.QueryContext(ctx, `SELECT key_id, key FROM whatsmeow_pre_keys WHERE jid=$1 AND uploaded=false ORDER BY key_id LIMIT $2`, s.ourJID, count)
	if err != nil {
		return nil, fmt.Errorf("failed to query existing prekeys: %w", err)
	}
	var newKeys []*keys.PreKey
	for rows.Next() {
		var id uint32
		var priv []byte
		if scanErr := rows.Scan(&id, &priv); scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		if len(priv) != 32 {
			_ = rows.Close()
			return nil, fmt.Errorf("invalid prekey %d length %d", id, len(priv))
		}
		var privArr [32]byte
		copy(privArr[:], priv)
		k := &keys.PreKey{
			KeyPair: *keys.NewKeyPairFromPrivateKey(privArr),
			KeyID:   id,
		}
		newKeys = append(newKeys, k)
		s.preKeyCache[k.KeyID] = k
	}
	if rows.Err() != nil {
		_ = rows.Close()
		return nil, rows.Err()
	}
	_ = rows.Close()

	already := uint32(len(newKeys))
	if count > already {
		var lastKeyID sql.NullInt32
		if err := s.db.QueryRowContext(ctx, `SELECT MAX(key_id) FROM whatsmeow_pre_keys WHERE jid=$1`, s.ourJID).Scan(&lastKeyID); err != nil {
			return nil, fmt.Errorf("failed to query next prekey ID: %w", err)
		}
		nextKeyID := uint32(lastKeyID.Int32) + 1
		needed := int(count - already)
		generated := make([]*keys.PreKey, needed)
		for i := range needed {
			generated[i] = keys.NewPreKey(nextKeyID)
			nextKeyID++
		}
		for start := 0; start < len(generated); start += fastStorePreKeyBatchRows {
			end := min(start+fastStorePreKeyBatchRows, len(generated))
			chunk := generated[start:end]
			args := make([]any, 0, len(chunk)*4)
			var b strings.Builder
			b.WriteString(`INSERT INTO whatsmeow_pre_keys (jid, key_id, key, uploaded) VALUES `)
			for i, k := range chunk {
				if i > 0 {
					b.WriteByte(',')
				}
				p := len(args) + 1
				fmt.Fprintf(&b, "($%d,$%d,$%d,$%d)", p, p+1, p+2, p+3)
				priv := k.Priv
				args = append(args, s.ourJID, k.KeyID, priv[:], false)
			}
			b.WriteString(` ON CONFLICT (jid, key_id) DO NOTHING`)
			if _, err := s.db.ExecContext(ctx, b.String(), args...); err != nil {
				return nil, fmt.Errorf("failed to batch insert prekeys: %w", err)
			}
		}
		for _, k := range generated {
			s.preKeyCache[k.KeyID] = k
		}
		newKeys = slices.Grow(newKeys, len(generated))
		newKeys = append(newKeys, generated...)
	}
	return newKeys, nil
}

func (s *fastSessionStore) GetPreKey(ctx context.Context, id uint32) (*keys.PreKey, error) {
	s.preKeyMu.Lock()
	cached := s.preKeyCache[id]
	s.preKeyMu.Unlock()
	if cached != nil {
		return cached, nil
	}
	k, err := s.AllSessionSpecificStores.GetPreKey(ctx, id)
	if err == nil && k != nil {
		s.preKeyMu.Lock()
		s.preKeyCache[id] = k
		s.preKeyMu.Unlock()
	}
	return k, err
}

func (s *fastSessionStore) RemovePreKey(ctx context.Context, id uint32) error {
	s.preKeyMu.Lock()
	delete(s.preKeyCache, id)
	s.preKeyMu.Unlock()
	return s.AllSessionSpecificStores.RemovePreKey(ctx, id)
}

func (s *fastSessionStore) PutMessageSecret(_ context.Context, chat, sender types.JID, id types.MessageID, secret []byte) error {
	if chat.IsEmpty() || sender.IsEmpty() || id == "" || len(secret) == 0 {
		return nil
	}
	chatNA := chat.ToNonAD()
	senderNA := sender.ToNonAD()
	k := msgSecretKey{chat: chatNA.String(), sender: senderNA.String(), id: string(id)}
	cp := append([]byte(nil), secret...)

	s.mu.Lock()
	if len(s.msgSecrets) >= fastStoreMsgSecretMax {
		clear(s.msgSecrets)
	}
	s.msgSecrets[k] = msgSecretEntry{secret: cp, sender: senderNA}
	s.dirtyMsgSecrets[k] = cp
	s.maybeSignalFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) PutMessageSecrets(_ context.Context, inserts []store.MessageSecretInsert) error {
	if len(inserts) == 0 {
		return nil
	}
	s.mu.Lock()
	for _, insert := range inserts {
		if insert.Chat.IsEmpty() || insert.Sender.IsEmpty() || insert.ID == "" || len(insert.Secret) == 0 {
			continue
		}
		chatNA := insert.Chat.ToNonAD()
		senderNA := insert.Sender.ToNonAD()
		k := msgSecretKey{chat: chatNA.String(), sender: senderNA.String(), id: string(insert.ID)}
		if len(s.msgSecrets) >= fastStoreMsgSecretMax {
			clear(s.msgSecrets)
		}
		cp := append([]byte(nil), insert.Secret...)
		s.msgSecrets[k] = msgSecretEntry{secret: cp, sender: senderNA}
		s.dirtyMsgSecrets[k] = cp
	}
	s.maybeSignalFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) GetMessageSecret(ctx context.Context, chat, sender types.JID, id types.MessageID) ([]byte, types.JID, error) {
	k := msgSecretKey{chat: chat.ToNonAD().String(), sender: sender.ToNonAD().String(), id: string(id)}
	s.mu.RLock()
	entry, ok := s.msgSecrets[k]
	s.mu.RUnlock()
	if ok {
		return append([]byte(nil), entry.secret...), entry.sender, nil
	}
	secret, realSender, err := s.AllSessionSpecificStores.GetMessageSecret(ctx, chat, sender, id)
	if err == nil && len(secret) > 0 {
		s.mu.Lock()
		if len(s.msgSecrets) >= fastStoreMsgSecretMax {
			clear(s.msgSecrets)
		}
		s.msgSecrets[k] = msgSecretEntry{secret: append([]byte(nil), secret...), sender: realSender}
		s.mu.Unlock()
	}
	return secret, realSender, err
}

func (s *fastSessionStore) DoDecryptionTxn(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

func (s *fastSessionStore) GetBufferedEvent(ctx context.Context, ciphertextHash [32]byte) (*store.BufferedEvent, error) {
	s.mu.RLock()
	ev, ok := s.eventBuf[ciphertextHash]
	loaded := s.eventBufLoaded && !s.eventBufEvicted
	s.mu.RUnlock()
	if ok {
		return cloneBufferedEvent(ev), nil
	}
	if loaded {
		return nil, nil
	}
	return s.AllSessionSpecificStores.GetBufferedEvent(ctx, ciphertextHash)
}

func (s *fastSessionStore) PutBufferedEvent(_ context.Context, ciphertextHash [32]byte, plaintext []byte, serverTimestamp time.Time) error {
	var ptCopy []byte
	if plaintext != nil {
		ptCopy = append([]byte(nil), plaintext...)
	}
	ev := store.BufferedEvent{
		Plaintext:  ptCopy,
		ServerTime: serverTimestamp,
		InsertTime: time.Now(),
	}
	s.mu.Lock()
	if len(s.eventBuf) >= fastStoreEventBufMax {
		clear(s.eventBuf)
		s.eventBufEvicted = true
	}
	s.eventBuf[ciphertextHash] = cloneBufferedEvent(&ev)
	s.dirtyEventBuf[ciphertextHash] = ev
	delete(s.dirtyClearedBuf, ciphertextHash)
	s.maybeSignalFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) ClearBufferedEventPlaintext(_ context.Context, ciphertextHash [32]byte) error {
	s.mu.Lock()
	if ev, ok := s.eventBuf[ciphertextHash]; ok && ev != nil {
		ev.Plaintext = nil
	}
	if dirty, ok := s.dirtyEventBuf[ciphertextHash]; ok {
		dirty.Plaintext = nil
		s.dirtyEventBuf[ciphertextHash] = dirty
	} else {
		s.dirtyClearedBuf[ciphertextHash] = struct{}{}
	}
	s.maybeSignalFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) DeleteOldBufferedHashes(ctx context.Context) error {
	if err := s.Flush(ctx); err != nil {
		s.warn("flush before DeleteOldBufferedHashes failed", err)
	}
	if err := s.AllSessionSpecificStores.DeleteOldBufferedHashes(ctx); err != nil {
		return err
	}
	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	s.mu.Lock()
	for h, ev := range s.eventBuf {
		if ev != nil && ev.InsertTime.Before(cutoff) {
			delete(s.eventBuf, h)
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) AddOutgoingEvent(_ context.Context, chatJID types.JID, id types.MessageID, format string, plaintext []byte) error {
	k := outgoingBufKey{chat: chatJID.String(), id: string(id)}
	val := outgoingBufEntry{
		format:    format,
		plaintext: append([]byte(nil), plaintext...),
	}
	s.mu.Lock()
	if len(s.outgoingBuf) >= fastStoreOutgoingBufMax {
		clear(s.outgoingBuf)
	}
	s.outgoingBuf[k] = val
	s.dirtyOutgoing[k] = val
	s.maybeSignalFlushLocked()
	s.mu.Unlock()
	return nil
}

func (s *fastSessionStore) GetOutgoingEvent(ctx context.Context, chatJID, altChatJID types.JID, id types.MessageID) (string, []byte, error) {
	s.mu.RLock()
	entry, ok := s.outgoingBuf[outgoingBufKey{chat: chatJID.String(), id: string(id)}]
	if !ok && !altChatJID.IsEmpty() {
		entry, ok = s.outgoingBuf[outgoingBufKey{chat: altChatJID.String(), id: string(id)}]
	}
	s.mu.RUnlock()
	if ok {
		return entry.format, append([]byte(nil), entry.plaintext...), nil
	}
	return s.AllSessionSpecificStores.GetOutgoingEvent(ctx, chatJID, altChatJID, id)
}

func cloneBufferedEvent(ev *store.BufferedEvent) *store.BufferedEvent {
	if ev == nil {
		return nil
	}
	var pt []byte
	if ev.Plaintext != nil {
		pt = append([]byte(nil), ev.Plaintext...)
	}
	return &store.BufferedEvent{
		Plaintext:  pt,
		ServerTime: ev.ServerTime,
		InsertTime: ev.InsertTime,
	}
}
