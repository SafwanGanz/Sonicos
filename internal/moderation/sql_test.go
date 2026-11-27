package moderation

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SafwanGanz/Sonicos/internal/command"
	_ "github.com/mattn/go-sqlite3"
)

func openTestSQLStore(t *testing.T) *SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?_foreign_keys=on&cache=shared")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	store, err := NewSQLStore(context.Background(), db, "sqlite3")
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	return store
}

func TestSQLStoreRebindPostgres(t *testing.T) {
	pg := &SQLStore{isPostgres: true}
	got := pg.rebind("SELECT * FROM tbl WHERE a = ? AND b = ? AND c = ?")
	want := "SELECT * FROM tbl WHERE a = $1 AND b = $2 AND c = $3"
	if got != want {
		t.Fatalf("rebind = %q, want %q", got, want)
	}
}

func TestSQLStoreBannedStickersAndStrikes(t *testing.T) {
	s := openTestSQLStore(t)
	ctx := context.Background()
	group := "1203630001@g.us"

	err := s.BanSticker(ctx, group, command.BannedSticker{
		FileSHA256:  "AABBCC112233",
		AudioSHA256: "DDEEFF445566",
		Reason:      "loud",
		AddedBy:     "admin@s.whatsapp.net",
		AddedAt:     100,
	})
	if err != nil {
		t.Fatalf("BanSticker 1: %v", err)
	}
	err = s.BanSticker(ctx, group, command.BannedSticker{
		FileSHA256: "AABBDD998877",
		Reason:     "spam",
		AddedBy:    "admin@s.whatsapp.net",
		AddedAt:    200,
	})
	if err != nil {
		t.Fatalf("BanSticker 2: %v", err)
	}

	got, ok, err := s.IsStickerBanned(ctx, group, "aabbcc112233", "")
	if err != nil || !ok || got.Reason != "loud" {
		t.Fatalf("IsStickerBanned by file: ok=%v err=%v got=%+v", ok, err, got)
	}
	got, ok, err = s.IsStickerBanned(ctx, group, "", "ddeeff445566")
	if err != nil || !ok || got.Reason != "loud" {
		t.Fatalf("IsStickerBanned by audio: ok=%v err=%v got=%+v", ok, err, got)
	}

	list, err := s.ListBannedStickers(ctx, group)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListBannedStickers len=%d err=%v", len(list), err)
	}

	if _, err := s.UnbanStickerByPrefix(ctx, group, "aabb"); err == nil {
		t.Fatal("expected error for <6 char prefix")
	}
	if removed, err := s.UnbanStickerByPrefix(ctx, group, "aabbcc"); err != nil || !removed {
		t.Fatalf("UnbanStickerByPrefix removed=%v err=%v", removed, err)
	}
	if _, ok, err := s.IsStickerBanned(ctx, group, "", "ddeeff445566"); err != nil || ok {
		t.Fatalf("expected audio hash to also be removed with entry, ok=%v err=%v", ok, err)
	}

	for want := 1; want <= 3; want++ {
		n, err := s.AddStrike(ctx, group, "user1")
		if err != nil || n != want {
			t.Fatalf("AddStrike = %d, %v (want %d)", n, err, want)
		}
	}
	if n, err := s.GetStrikes(ctx, group, "user1"); err != nil || n != 3 {
		t.Fatalf("GetStrikes = %d, %v", n, err)
	}
	if err := s.ResetStrikes(ctx, group, "user1"); err != nil {
		t.Fatalf("ResetStrikes: %v", err)
	}
	if n, err := s.GetStrikes(ctx, group, "user1"); err != nil || n != 0 {
		t.Fatalf("GetStrikes after reset = %d, %v", n, err)
	}
}

func TestSQLStoreShieldsSeenStagingRateAndReactionLibrary(t *testing.T) {
	s := openTestSQLStore(t)
	ctx := context.Background()
	group := "1203630002@g.us"

	def, err := s.GetShields(ctx, group)
	if err != nil || def.AntiLoud || def.AntiLoudDB != defaultAntiLoudDB {
		t.Fatalf("default shields = %+v, err=%v", def, err)
	}
	if err := s.SetShield(ctx, group, "anti_loud", "1"); err != nil {
		t.Fatalf("SetShield anti_loud: %v", err)
	}
	if err := s.SetShield(ctx, group, "anti_loud_db", "-8.5"); err != nil {
		t.Fatalf("SetShield anti_loud_db: %v", err)
	}
	if err := s.SetShield(ctx, group, "anti_link", "true"); err != nil {
		t.Fatalf("SetShield anti_link: %v", err)
	}
	sh, err := s.GetShields(ctx, group)
	if err != nil || !sh.AntiLoud || sh.AntiLoudDB != -8.5 || !sh.AntiLink {
		t.Fatalf("updated shields = %+v, err=%v", sh, err)
	}

	if err := s.RememberSticker(ctx, group, "msg-1", command.StickerInfo{FileSHA256: "ABC", AudioSHA256: "DEF", IsAudioSticker: true}); err != nil {
		t.Fatalf("RememberSticker: %v", err)
	}
	seen, ok, err := s.LookupSticker(ctx, group, "msg-1")
	if err != nil || !ok || seen.FileSHA256 != "abc" || !seen.IsAudioSticker {
		t.Fatalf("LookupSticker = %+v ok=%v err=%v", seen, ok, err)
	}

	for i := 1; i <= 3; i++ {
		n, err := s.IncrementRate(ctx, group, "spam", "user1", 60)
		if err != nil || n != i {
			t.Fatalf("IncrementRate = %d, %v (want %d)", n, err, i)
		}
	}

	if err := s.SaveStage(ctx, "scope-1", command.StagedMedia{Kind: command.MediaImage, MIMEType: "image/png", Data: []byte("pngdata")}, 60); err != nil {
		t.Fatalf("SaveStage: %v", err)
	}
	st, ok, err := s.LoadStage(ctx, "scope-1")
	if err != nil || !ok || string(st.Data) != "pngdata" {
		t.Fatalf("LoadStage = %+v ok=%v err=%v", st, ok, err)
	}
	if err := s.ClearStage(ctx, "scope-1"); err != nil {
		t.Fatalf("ClearStage: %v", err)
	}
	if _, ok, _ := s.LoadStage(ctx, "scope-1"); ok {
		t.Fatal("expected cleared stage to be absent")
	}

	err = s.SaveReactionSticker(ctx, "global", "❤️", command.SavedReactionSticker{
		Emoji: "❤️",
		Data:  []byte("webpbytes"),
	})
	if err != nil {
		t.Fatalf("SaveReactionSticker: %v", err)
	}
	rec, ok, err := s.GetReactionSticker(ctx, "global", "❤")
	if err != nil || !ok || string(rec.Data) != "webpbytes" {
		t.Fatalf("GetReactionSticker normalized = %+v ok=%v err=%v", rec, ok, err)
	}
	items, err := s.ListReactionStickers(ctx, "global")
	if err != nil || len(items) != 1 {
		t.Fatalf("ListReactionStickers = %d err=%v", len(items), err)
	}
	if removed, err := s.DeleteReactionSticker(ctx, "global", "❤"); err != nil || !removed {
		t.Fatalf("DeleteReactionSticker removed=%v err=%v", removed, err)
	}
	if err := s.SaveReactionSticker(ctx, "global", "🔥", command.SavedReactionSticker{Data: make([]byte, maxReactionStickerBytes+1)}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected oversized reaction sticker error, got %v", err)
	}
}

func TestSQLStoreCachingInvalidationAndConcurrency(t *testing.T) {
	s := openTestSQLStore(t)
	ctx := context.Background()
	group := "1203630009@g.us"

	if err := s.SetShield(ctx, group, "anti_link", "1"); err != nil {
		t.Fatalf("SetShield: %v", err)
	}
	sh, err := s.GetShields(ctx, group)
	if err != nil || !sh.AntiLink {
		t.Fatalf("GetShields = %+v, err=%v", sh, err)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE sonicos_group_shields SET value = '0' WHERE group_jid = ? AND field = 'anti_link'`, group); err != nil {
		t.Fatalf("direct SQL update: %v", err)
	}
	shCached, err := s.GetShields(ctx, group)
	if err != nil || !shCached.AntiLink {
		t.Fatalf("expected cached AntiLink=true before invalidation, got %+v, err=%v", shCached, err)
	}

	if err := s.SetShield(ctx, group, "anti_spam", "1"); err != nil {
		t.Fatalf("SetShield anti_spam: %v", err)
	}
	shRefreshed, err := s.GetShields(ctx, group)
	if err != nil || shRefreshed.AntiLink || !shRefreshed.AntiSpam {
		t.Fatalf("expected invalidated shields (AntiLink=false, AntiSpam=true), got %+v, err=%v", shRefreshed, err)
	}

	if err := s.BanSticker(ctx, group, command.BannedSticker{FileSHA256: "feedbeef01", Reason: "test"}); err != nil {
		t.Fatalf("BanSticker: %v", err)
	}
	if _, ok, err := s.IsStickerBanned(ctx, group, "feedbeef01", ""); err != nil || !ok {
		t.Fatalf("IsStickerBanned = ok:%v err:%v", ok, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sonicos_banned_stickers WHERE group_jid = ?`, group); err != nil {
		t.Fatalf("direct delete banned: %v", err)
	}
	if _, ok, err := s.IsStickerBanned(ctx, group, "feedbeef01", ""); err != nil || !ok {
		t.Fatalf("expected cached ban match after direct DB delete, got ok:%v err:%v", ok, err)
	}
	if err := s.BanSticker(ctx, group, command.BannedSticker{FileSHA256: "feedbeef02", Reason: "new"}); err != nil {
		t.Fatalf("BanSticker 2: %v", err)
	}
	if _, ok, err := s.IsStickerBanned(ctx, group, "feedbeef01", ""); err != nil || ok {
		t.Fatalf("expected cache invalidation to drop deleted feedbeef01, got ok:%v err:%v", ok, err)
	}

	if err := s.RememberSticker(ctx, group, "seen-1", command.StickerInfo{FileSHA256: "CAFEBABE"}); err != nil {
		t.Fatalf("RememberSticker: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sonicos_seen_stickers WHERE group_jid = ?`, group); err != nil {
		t.Fatalf("direct delete seen: %v", err)
	}
	st, ok, err := s.LookupSticker(ctx, group, "seen-1")
	if err != nil || !ok || st.FileSHA256 != "cafebabe" {
		t.Fatalf("expected cached seen sticker, got %+v ok=%v err=%v", st, ok, err)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range 10 {
				_, _ = s.GetShields(ctx, group)
				_, _, _ = s.IsStickerBanned(ctx, group, "feedbeef02", "")
				_ = s.RememberSticker(ctx, group, fmt.Sprintf("m-%d-%d", id, j), command.StickerInfo{FileSHA256: "deadbeef"})
				_, _, _ = s.LookupSticker(ctx, group, fmt.Sprintf("m-%d-%d", id, j))
			}
		}(i)
	}
	wg.Wait()
}
