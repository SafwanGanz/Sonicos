package moderation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/command"
)

const (
	sqlCacheTTL        = 60 * time.Second
	sqlCacheMaxEntries = 2000
)

type rateBucket struct {
	count     int
	expiresAt time.Time
}

type cachedShields struct {
	shields   command.GroupShields
	expiresAt time.Time
}

type cachedBanned struct {
	byFile    map[string]command.BannedSticker
	byAudio   map[string]command.BannedSticker
	expiresAt time.Time
}

type cachedSeen struct {
	info      command.StickerInfo
	expiresAt time.Time
}

type SQLStore struct {
	db           *sql.DB
	isPostgres   bool
	rateMu       sync.Mutex
	rates        map[string]rateBucket
	cacheMu      sync.RWMutex
	shieldsCache map[string]cachedShields
	bannedCache  map[string]cachedBanned
	seenCache    map[string]cachedSeen
}

func NewSQLStore(ctx context.Context, db *sql.DB, driver string) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("sql database handle is required")
	}
	lower := strings.ToLower(strings.TrimSpace(driver))
	store := &SQLStore{
		db:           db,
		isPostgres:   lower == "pgx" || strings.HasPrefix(lower, "postgres"),
		rates:        make(map[string]rateBucket),
		shieldsCache: make(map[string]cachedShields),
		bannedCache:  make(map[string]cachedBanned),
		seenCache:    make(map[string]cachedSeen),
	}
	if err := store.initSchema(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *SQLStore) initSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sonicos_banned_stickers (
			group_jid TEXT NOT NULL,
			file_sha256 TEXT NOT NULL DEFAULT '',
			audio_sha256 TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			added_by TEXT NOT NULL DEFAULT '',
			added_at BIGINT NOT NULL,
			PRIMARY KEY (group_jid, file_sha256, audio_sha256)
		)`,
		`CREATE TABLE IF NOT EXISTS sonicos_strikes (
			group_jid TEXT NOT NULL,
			user_id TEXT NOT NULL,
			strikes INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (group_jid, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS sonicos_seen_stickers (
			group_jid TEXT NOT NULL,
			message_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			expires_at BIGINT NOT NULL,
			PRIMARY KEY (group_jid, message_id)
		)`,
		`CREATE TABLE IF NOT EXISTS sonicos_group_shields (
			group_jid TEXT NOT NULL,
			field TEXT NOT NULL,
			value TEXT NOT NULL,
			PRIMARY KEY (group_jid, field)
		)`,
		`CREATE TABLE IF NOT EXISTS sonicos_staged_media (
			scope_key TEXT PRIMARY KEY,
			payload TEXT NOT NULL,
			expires_at BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sonicos_reaction_stickers (
			owner_id TEXT NOT NULL,
			emoji_key TEXT NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (owner_id, emoji_key)
		)`,
		`CREATE TABLE IF NOT EXISTS sonicos_welcome_settings (
			group_jid TEXT PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 0,
			custom_text TEXT NOT NULL DEFAULT '',
			updated_at BIGINT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create moderation schema: %w", err)
		}
	}
	return nil
}

func (s *SQLStore) rebind(query string) string {
	if !s.isPostgres || !strings.Contains(query, "?") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 1
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			n++
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

func pruneCacheMap[V any](m map[string]V, expiresFn func(V) time.Time, now time.Time) {
	if len(m) < sqlCacheMaxEntries {
		return
	}
	for k, v := range m {
		if !now.Before(expiresFn(v)) {
			delete(m, k)
		}
	}
	if len(m) >= sqlCacheMaxEntries {
		clear(m)
	}
}

func (s *SQLStore) invalidateBanned(groupJID string) {
	s.cacheMu.Lock()
	delete(s.bannedCache, groupJID)
	s.cacheMu.Unlock()
}

func (s *SQLStore) loadBannedGroup(ctx context.Context, groupJID string) (cachedBanned, error) {
	now := time.Now()
	s.cacheMu.RLock()
	if s.bannedCache != nil {
		if entry, ok := s.bannedCache[groupJID]; ok && now.Before(entry.expiresAt) {
			s.cacheMu.RUnlock()
			return entry, nil
		}
	}
	s.cacheMu.RUnlock()

	entries, err := s.ListBannedStickers(ctx, groupJID)
	if err != nil {
		return cachedBanned{}, err
	}

	byFile := make(map[string]command.BannedSticker, len(entries))
	byAudio := make(map[string]command.BannedSticker, len(entries))
	for _, entry := range entries {
		if fh := normHash(entry.FileSHA256); fh != "" {
			if _, exists := byFile[fh]; !exists {
				byFile[fh] = entry
			}
		}
		if ah := normHash(entry.AudioSHA256); ah != "" {
			if _, exists := byAudio[ah]; !exists {
				byAudio[ah] = entry
			}
		}
	}

	cached := cachedBanned{
		byFile:    byFile,
		byAudio:   byAudio,
		expiresAt: time.Now().Add(sqlCacheTTL),
	}
	s.cacheMu.Lock()
	if s.bannedCache == nil {
		s.bannedCache = make(map[string]cachedBanned)
	}
	pruneCacheMap(s.bannedCache, func(v cachedBanned) time.Time { return v.expiresAt }, time.Now())
	s.bannedCache[groupJID] = cached
	s.cacheMu.Unlock()
	return cached, nil
}

func (s *SQLStore) BanSticker(ctx context.Context, groupJID string, entry command.BannedSticker) error {
	entry.FileSHA256 = normHash(entry.FileSHA256)
	entry.AudioSHA256 = normHash(entry.AudioSHA256)
	if entry.FileSHA256 == "" && entry.AudioSHA256 == "" {
		return errors.New("at least one of file or audio SHA-256 hash is required")
	}
	if entry.AddedAt == 0 {
		entry.AddedAt = time.Now().Unix()
	}
	query := s.rebind(`
		INSERT INTO sonicos_banned_stickers (group_jid, file_sha256, audio_sha256, reason, added_by, added_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (group_jid, file_sha256, audio_sha256)
		DO UPDATE SET reason = excluded.reason, added_by = excluded.added_by, added_at = excluded.added_at
	`)
	if _, err := s.db.ExecContext(ctx, query, groupJID, entry.FileSHA256, entry.AudioSHA256, entry.Reason, entry.AddedBy, entry.AddedAt); err != nil {
		return fmt.Errorf("store banned sticker: %w", err)
	}
	s.invalidateBanned(groupJID)
	return nil
}

func (s *SQLStore) UnbanSticker(ctx context.Context, groupJID, fileSHA256, audioSHA256 string) (bool, error) {
	fileHash := normHash(fileSHA256)
	audioHash := normHash(audioSHA256)
	if fileHash == "" && audioHash == "" {
		return false, nil
	}
	query := s.rebind(`
		DELETE FROM sonicos_banned_stickers
		WHERE group_jid = ?
		  AND ((? <> '' AND file_sha256 = ?) OR (? <> '' AND audio_sha256 = ?))
	`)
	res, err := s.db.ExecContext(ctx, query, groupJID, fileHash, fileHash, audioHash, audioHash)
	if err != nil {
		return false, fmt.Errorf("remove banned sticker: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check deleted banned sticker rows: %w", err)
	}
	if n > 0 {
		s.invalidateBanned(groupJID)
	}
	return n > 0, nil
}

func (s *SQLStore) UnbanStickerByPrefix(ctx context.Context, groupJID, hashPrefix string) (bool, error) {
	prefix := normHash(hashPrefix)
	if len(prefix) < 6 {
		return false, errors.New("hash prefix must be at least 6 characters")
	}
	entries, err := s.ListBannedStickers(ctx, groupJID)
	if err != nil {
		return false, err
	}
	var matches []command.BannedSticker
	for _, entry := range entries {
		if (entry.FileSHA256 != "" && strings.HasPrefix(entry.FileSHA256, prefix)) ||
			(entry.AudioSHA256 != "" && strings.HasPrefix(entry.AudioSHA256, prefix)) {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return false, nil
	}
	if len(matches) > 1 {
		return false, fmt.Errorf("hash prefix %q matches %d entries; provide more characters", prefix, len(matches))
	}
	return s.UnbanSticker(ctx, groupJID, matches[0].FileSHA256, matches[0].AudioSHA256)
}

func (s *SQLStore) IsStickerBanned(ctx context.Context, groupJID, fileSHA256, audioSHA256 string) (command.BannedSticker, bool, error) {
	fh := normHash(fileSHA256)
	ah := normHash(audioSHA256)
	if fh == "" && ah == "" {
		return command.BannedSticker{}, false, nil
	}
	cached, err := s.loadBannedGroup(ctx, groupJID)
	if err != nil {
		return command.BannedSticker{}, false, err
	}
	if fh != "" {
		if entry, ok := cached.byFile[fh]; ok {
			return entry, true, nil
		}
	}
	if ah != "" {
		if entry, ok := cached.byAudio[ah]; ok {
			return entry, true, nil
		}
	}
	return command.BannedSticker{}, false, nil
}

func (s *SQLStore) ListBannedStickers(ctx context.Context, groupJID string) ([]command.BannedSticker, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`
		SELECT file_sha256, audio_sha256, reason, added_by, added_at
		FROM sonicos_banned_stickers
		WHERE group_jid = ?
		ORDER BY added_at ASC, file_sha256 ASC, audio_sha256 ASC
	`), groupJID)
	if err != nil {
		return nil, fmt.Errorf("list banned stickers: %w", err)
	}
	defer rows.Close()

	var out []command.BannedSticker
	for rows.Next() {
		var entry command.BannedSticker
		if err := rows.Scan(&entry.FileSHA256, &entry.AudioSHA256, &entry.Reason, &entry.AddedBy, &entry.AddedAt); err != nil {
			return nil, fmt.Errorf("scan banned sticker: %w", err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate banned stickers: %w", err)
	}
	return out, nil
}

func (s *SQLStore) AddStrike(ctx context.Context, groupJID, userID string) (int, error) {
	var strikes int
	err := s.db.QueryRowContext(ctx, s.rebind(`
		INSERT INTO sonicos_strikes (group_jid, user_id, strikes)
		VALUES (?, ?, 1)
		ON CONFLICT (group_jid, user_id)
		DO UPDATE SET strikes = sonicos_strikes.strikes + 1
		RETURNING strikes
	`), groupJID, userID).Scan(&strikes)
	if err != nil {
		return 0, fmt.Errorf("increment sticker strike: %w", err)
	}
	return strikes, nil
}

func (s *SQLStore) GetStrikes(ctx context.Context, groupJID, userID string) (int, error) {
	var strikes int
	err := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT strikes FROM sonicos_strikes WHERE group_jid = ? AND user_id = ?
	`), groupJID, userID).Scan(&strikes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get sticker strikes: %w", err)
	}
	return strikes, nil
}

func (s *SQLStore) ResetStrikes(ctx context.Context, groupJID, userID string) error {
	if _, err := s.db.ExecContext(ctx, s.rebind(`
		DELETE FROM sonicos_strikes WHERE group_jid = ? AND user_id = ?
	`), groupJID, userID); err != nil {
		return fmt.Errorf("reset sticker strikes: %w", err)
	}
	return nil
}

func seenCacheKey(groupJID, messageID string) string {
	return groupJID + "\x00" + messageID
}

func (s *SQLStore) RememberSticker(ctx context.Context, groupJID, messageID string, info command.StickerInfo) error {
	if strings.TrimSpace(groupJID) == "" || strings.TrimSpace(messageID) == "" {
		return nil
	}
	info.FileSHA256 = normHash(info.FileSHA256)
	info.AudioSHA256 = normHash(info.AudioSHA256)
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	expiresAt := time.Now().Add(seenStickerTTL).Unix()
	if _, err := s.db.ExecContext(ctx, s.rebind(`
		INSERT INTO sonicos_seen_stickers (group_jid, message_id, payload, expires_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (group_jid, message_id)
		DO UPDATE SET payload = excluded.payload, expires_at = excluded.expires_at
	`), groupJID, messageID, string(raw), expiresAt); err != nil {
		return fmt.Errorf("remember sticker: %w", err)
	}
	key := seenCacheKey(groupJID, messageID)
	now := time.Now()
	s.cacheMu.Lock()
	if s.seenCache == nil {
		s.seenCache = make(map[string]cachedSeen)
	}
	pruneCacheMap(s.seenCache, func(v cachedSeen) time.Time { return v.expiresAt }, now)
	s.seenCache[key] = cachedSeen{
		info:      info,
		expiresAt: now.Add(sqlCacheTTL),
	}
	s.cacheMu.Unlock()
	return nil
}

func (s *SQLStore) LookupSticker(ctx context.Context, groupJID, messageID string) (command.StickerInfo, bool, error) {
	if strings.TrimSpace(groupJID) == "" || strings.TrimSpace(messageID) == "" {
		return command.StickerInfo{}, false, nil
	}
	key := seenCacheKey(groupJID, messageID)
	now := time.Now()
	s.cacheMu.RLock()
	if s.seenCache != nil {
		if entry, ok := s.seenCache[key]; ok && now.Before(entry.expiresAt) {
			s.cacheMu.RUnlock()
			return entry.info, true, nil
		}
	}
	s.cacheMu.RUnlock()

	var payload string
	var expiresAt int64
	err := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT payload, expires_at FROM sonicos_seen_stickers WHERE group_jid = ? AND message_id = ?
	`), groupJID, messageID).Scan(&payload, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return command.StickerInfo{}, false, nil
	}
	if err != nil {
		return command.StickerInfo{}, false, fmt.Errorf("lookup seen sticker: %w", err)
	}
	if expiresAt > 0 && time.Now().Unix() > expiresAt {
		_, _ = s.db.ExecContext(ctx, s.rebind(`DELETE FROM sonicos_seen_stickers WHERE group_jid = ? AND message_id = ?`), groupJID, messageID)
		return command.StickerInfo{}, false, nil
	}
	var info command.StickerInfo
	if err := json.Unmarshal([]byte(payload), &info); err != nil {
		return command.StickerInfo{}, false, nil
	}
	now = time.Now()
	s.cacheMu.Lock()
	if s.seenCache == nil {
		s.seenCache = make(map[string]cachedSeen)
	}
	pruneCacheMap(s.seenCache, func(v cachedSeen) time.Time { return v.expiresAt }, now)
	s.seenCache[key] = cachedSeen{
		info:      info,
		expiresAt: now.Add(sqlCacheTTL),
	}
	s.cacheMu.Unlock()
	return info, true, nil
}

func (s *SQLStore) GetShields(ctx context.Context, groupJID string) (command.GroupShields, error) {
	now := time.Now()
	s.cacheMu.RLock()
	if s.shieldsCache != nil {
		if entry, ok := s.shieldsCache[groupJID]; ok && now.Before(entry.expiresAt) {
			s.cacheMu.RUnlock()
			return entry.shields, nil
		}
	}
	s.cacheMu.RUnlock()

	shields := command.GroupShields{AntiLoudDB: defaultAntiLoudDB}
	rows, err := s.db.QueryContext(ctx, s.rebind(`
		SELECT field, value FROM sonicos_group_shields WHERE group_jid = ?
	`), groupJID)
	if err != nil {
		return shields, fmt.Errorf("get group shields: %w", err)
	}
	defer rows.Close()

	vals := make(map[string]string)
	for rows.Next() {
		var field, value string
		if err := rows.Scan(&field, &value); err != nil {
			return shields, fmt.Errorf("scan group shield: %w", err)
		}
		vals[field] = value
	}
	if err := rows.Err(); err != nil {
		return shields, fmt.Errorf("iterate group shields: %w", err)
	}

	shields.AntiLoud = vals["anti_loud"] == "1" || strings.EqualFold(vals["anti_loud"], "true")
	shields.AntiLink = vals["anti_link"] == "1" || strings.EqualFold(vals["anti_link"], "true")
	shields.AntiSpam = vals["anti_spam"] == "1" || strings.EqualFold(vals["anti_spam"], "true")
	shields.AntiStickerFlood = vals["anti_sticker_flood"] == "1" || strings.EqualFold(vals["anti_sticker_flood"], "true")
	if rawDB := strings.TrimSpace(vals["anti_loud_db"]); rawDB != "" {
		if parsed, err := strconv.ParseFloat(rawDB, 64); err == nil {
			shields.AntiLoudDB = parsed
		}
	}

	now = time.Now()
	s.cacheMu.Lock()
	if s.shieldsCache == nil {
		s.shieldsCache = make(map[string]cachedShields)
	}
	pruneCacheMap(s.shieldsCache, func(v cachedShields) time.Time { return v.expiresAt }, now)
	s.shieldsCache[groupJID] = cachedShields{
		shields:   shields,
		expiresAt: now.Add(sqlCacheTTL),
	}
	s.cacheMu.Unlock()
	return shields, nil
}

func (s *SQLStore) SetShield(ctx context.Context, groupJID, field, value string) error {
	if _, err := s.db.ExecContext(ctx, s.rebind(`
		INSERT INTO sonicos_group_shields (group_jid, field, value)
		VALUES (?, ?, ?)
		ON CONFLICT (group_jid, field)
		DO UPDATE SET value = excluded.value
	`), groupJID, field, value); err != nil {
		return fmt.Errorf("set group shield %q: %w", field, err)
	}
	s.cacheMu.Lock()
	delete(s.shieldsCache, groupJID)
	s.cacheMu.Unlock()
	return nil
}

func (s *SQLStore) IncrementRate(_ context.Context, groupJID, bucket, userID string, windowSec int) (int, error) {
	if windowSec <= 0 {
		windowSec = 10
	}
	key := rateKey(groupJID, bucket, userID)
	now := time.Now()

	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	for k, entry := range s.rates {
		if now.After(entry.expiresAt) {
			delete(s.rates, k)
		}
	}

	entry, ok := s.rates[key]
	if !ok || now.After(entry.expiresAt) {
		entry = rateBucket{
			count:     1,
			expiresAt: now.Add(time.Duration(windowSec) * time.Second),
		}
	} else {
		entry.count++
	}
	s.rates[key] = entry
	return entry.count, nil
}

func (s *SQLStore) SaveStage(ctx context.Context, scopeKey string, media command.StagedMedia, ttlSec int) error {
	if len(media.Data) > maxStagedMediaSize {
		return fmt.Errorf("staged media exceeds %d MiB limit", maxStagedMediaSize>>20)
	}
	if ttlSec <= 0 {
		ttlSec = 600
	}
	raw, err := json.Marshal(media)
	if err != nil {
		return fmt.Errorf("marshal staged media: %w", err)
	}
	expiresAt := time.Now().Add(time.Duration(ttlSec) * time.Second).Unix()
	if _, err := s.db.ExecContext(ctx, s.rebind(`
		INSERT INTO sonicos_staged_media (scope_key, payload, expires_at)
		VALUES (?, ?, ?)
		ON CONFLICT (scope_key)
		DO UPDATE SET payload = excluded.payload, expires_at = excluded.expires_at
	`), scopeKey, string(raw), expiresAt); err != nil {
		return fmt.Errorf("save staged media: %w", err)
	}
	return nil
}

func (s *SQLStore) LoadStage(ctx context.Context, scopeKey string) (command.StagedMedia, bool, error) {
	var payload string
	var expiresAt int64
	err := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT payload, expires_at FROM sonicos_staged_media WHERE scope_key = ?
	`), scopeKey).Scan(&payload, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return command.StagedMedia{}, false, nil
	}
	if err != nil {
		return command.StagedMedia{}, false, fmt.Errorf("load staged media: %w", err)
	}
	if expiresAt > 0 && time.Now().Unix() > expiresAt {
		_, _ = s.db.ExecContext(ctx, s.rebind(`DELETE FROM sonicos_staged_media WHERE scope_key = ?`), scopeKey)
		return command.StagedMedia{}, false, nil
	}
	var media command.StagedMedia
	if err := json.Unmarshal([]byte(payload), &media); err != nil {
		return command.StagedMedia{}, false, nil
	}
	return media, true, nil
}

func (s *SQLStore) ClearStage(ctx context.Context, scopeKey string) error {
	if _, err := s.db.ExecContext(ctx, s.rebind(`
		DELETE FROM sonicos_staged_media WHERE scope_key = ?
	`), scopeKey); err != nil {
		return fmt.Errorf("clear staged media: %w", err)
	}
	return nil
}

func (s *SQLStore) SaveReactionSticker(ctx context.Context, ownerID, emoji string, sticker command.SavedReactionSticker) error {
	ownerID = strings.TrimSpace(ownerID)
	key := normReactionEmojiKey(emoji)
	if ownerID == "" || key == "" {
		return errors.New("owner and emoji key are required")
	}
	if len(sticker.Data) == 0 {
		return errors.New("reaction sticker data is empty")
	}
	if len(sticker.Data) > maxReactionStickerBytes {
		return fmt.Errorf("reaction sticker exceeds %d KiB limit", maxReactionStickerBytes>>10)
	}
	if strings.TrimSpace(sticker.Emoji) == "" {
		sticker.Emoji = strings.TrimSpace(emoji)
	}
	if sticker.SavedAt == 0 {
		sticker.SavedAt = time.Now().Unix()
	}

	var exists int
	err := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT 1 FROM sonicos_reaction_stickers WHERE owner_id = ? AND emoji_key = ?
	`), ownerID, key).Scan(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check reaction sticker library: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if err := s.db.QueryRowContext(ctx, s.rebind(`
			SELECT COUNT(*) FROM sonicos_reaction_stickers WHERE owner_id = ?
		`), ownerID).Scan(&count); err != nil {
			return fmt.Errorf("count reaction sticker library: %w", err)
		}
		if count >= maxReactionLibraryEntries {
			return fmt.Errorf("reaction sticker library limit reached (%d max)", maxReactionLibraryEntries)
		}
	}

	payload, err := json.Marshal(sticker)
	if err != nil {
		return fmt.Errorf("marshal reaction sticker: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, s.rebind(`
		INSERT INTO sonicos_reaction_stickers (owner_id, emoji_key, payload)
		VALUES (?, ?, ?)
		ON CONFLICT (owner_id, emoji_key)
		DO UPDATE SET payload = excluded.payload
	`), ownerID, key, string(payload)); err != nil {
		return fmt.Errorf("save reaction sticker: %w", err)
	}
	return nil
}

func (s *SQLStore) GetReactionSticker(ctx context.Context, ownerID, emoji string) (command.SavedReactionSticker, bool, error) {
	ownerID = strings.TrimSpace(ownerID)
	key := normReactionEmojiKey(emoji)
	if ownerID == "" || key == "" {
		return command.SavedReactionSticker{}, false, nil
	}
	var payload string
	err := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT payload FROM sonicos_reaction_stickers WHERE owner_id = ? AND emoji_key = ?
	`), ownerID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return command.SavedReactionSticker{}, false, nil
	}
	if err != nil {
		return command.SavedReactionSticker{}, false, fmt.Errorf("get reaction sticker: %w", err)
	}
	var st command.SavedReactionSticker
	if err := json.Unmarshal([]byte(payload), &st); err != nil {
		return command.SavedReactionSticker{}, false, nil
	}
	return st, true, nil
}

func (s *SQLStore) DeleteReactionSticker(ctx context.Context, ownerID, emoji string) (bool, error) {
	ownerID = strings.TrimSpace(ownerID)
	key := normReactionEmojiKey(emoji)
	if ownerID == "" || key == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, s.rebind(`
		DELETE FROM sonicos_reaction_stickers WHERE owner_id = ? AND emoji_key = ?
	`), ownerID, key)
	if err != nil {
		return false, fmt.Errorf("delete reaction sticker: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check deleted reaction sticker rows: %w", err)
	}
	return n > 0, nil
}

func (s *SQLStore) ListReactionStickers(ctx context.Context, ownerID string) ([]command.SavedReactionSticker, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`
		SELECT emoji_key, payload FROM sonicos_reaction_stickers WHERE owner_id = ?
	`), ownerID)
	if err != nil {
		return nil, fmt.Errorf("list reaction stickers: %w", err)
	}
	defer rows.Close()

	var out []command.SavedReactionSticker
	for rows.Next() {
		var key, payload string
		if err := rows.Scan(&key, &payload); err != nil {
			return nil, fmt.Errorf("scan reaction sticker: %w", err)
		}
		var st command.SavedReactionSticker
		if err := json.Unmarshal([]byte(payload), &st); err != nil {
			continue
		}
		if strings.TrimSpace(st.Emoji) == "" {
			st.Emoji = key
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reaction stickers: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SavedAt != out[j].SavedAt {
			return out[i].SavedAt < out[j].SavedAt
		}
		return out[i].Emoji < out[j].Emoji
	})
	return out, nil
}

type WelcomeSettings struct {
	Enabled    bool
	CustomText string
}

func (s *SQLStore) GetWelcomeSettings(ctx context.Context, groupJID string) (WelcomeSettings, error) {
	var enabled int
	var customText string
	err := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT enabled, custom_text FROM sonicos_welcome_settings WHERE group_jid = ?
	`), groupJID).Scan(&enabled, &customText)
	if errors.Is(err, sql.ErrNoRows) {
		return WelcomeSettings{Enabled: false, CustomText: ""}, nil
	}
	if err != nil {
		return WelcomeSettings{}, fmt.Errorf("get welcome settings: %w", err)
	}
	return WelcomeSettings{Enabled: enabled == 1, CustomText: customText}, nil
}

func (s *SQLStore) SetWelcomeEnabled(ctx context.Context, groupJID string, enabled bool) error {
	now := time.Now().Unix()
	val := 0
	if enabled {
		val = 1
	}
	if _, err := s.db.ExecContext(ctx, s.rebind(`
		INSERT INTO sonicos_welcome_settings (group_jid, enabled, custom_text, updated_at)
		VALUES (?, ?, '', ?)
		ON CONFLICT (group_jid) DO UPDATE SET enabled = excluded.enabled, updated_at = excluded.updated_at
	`), groupJID, val, now); err != nil {
		return fmt.Errorf("set welcome enabled: %w", err)
	}
	return nil
}

func (s *SQLStore) SetWelcomeCustomText(ctx context.Context, groupJID, text string) error {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, s.rebind(`
		INSERT INTO sonicos_welcome_settings (group_jid, enabled, custom_text, updated_at)
		VALUES (?, 0, ?, ?)
		ON CONFLICT (group_jid) DO UPDATE SET custom_text = excluded.custom_text, updated_at = excluded.updated_at
	`), groupJID, text, now); err != nil {
		return fmt.Errorf("set welcome custom text: %w", err)
	}
	return nil
}
