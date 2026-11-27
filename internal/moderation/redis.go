package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/SafwanGanz/Sonicos/internal/command"
)

const (
	seenStickerTTL            = 24 * time.Hour
	defaultAntiLoudDB         = -6.0
	maxStagedMediaSize        = 10 << 20
	maxReactionStickerBytes   = 1 << 20
	maxReactionLibraryEntries = 100
)

// RedisStore persists per-group sticker blacklists, user strike counters,
// and a short-lived message-ID -> sticker-hash cache in Redis.
type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(client *redis.Client) *RedisStore {
	if client == nil {
		return nil
	}
	return &RedisStore{client: client}
}

func banFileKey(groupJID string) string  { return "sonicos:mod:ban:file:" + groupJID }
func banAudioKey(groupJID string) string { return "sonicos:mod:ban:audio:" + groupJID }
func strikesKey(groupJID string) string  { return "sonicos:mod:strikes:" + groupJID }
func seenKey(groupJID, messageID string) string {
	return "sonicos:mod:seen:" + groupJID + ":" + messageID
}

func normHash(h string) string {
	return strings.ToLower(strings.TrimSpace(h))
}

func (s *RedisStore) BanSticker(ctx context.Context, groupJID string, entry command.BannedSticker) error {
	entry.FileSHA256 = normHash(entry.FileSHA256)
	entry.AudioSHA256 = normHash(entry.AudioSHA256)
	if entry.FileSHA256 == "" && entry.AudioSHA256 == "" {
		return errors.New("at least one of file or audio SHA-256 hash is required")
	}
	if entry.AddedAt == 0 {
		entry.AddedAt = time.Now().Unix()
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal banned sticker: %w", err)
	}
	if entry.FileSHA256 != "" {
		if err := s.client.HSet(ctx, banFileKey(groupJID), entry.FileSHA256, payload).Err(); err != nil {
			return fmt.Errorf("store banned file hash: %w", err)
		}
	}
	if entry.AudioSHA256 != "" {
		if err := s.client.HSet(ctx, banAudioKey(groupJID), entry.AudioSHA256, payload).Err(); err != nil {
			return fmt.Errorf("store banned audio hash: %w", err)
		}
	}
	return nil
}

func (s *RedisStore) UnbanSticker(ctx context.Context, groupJID, fileSHA256, audioSHA256 string) (bool, error) {
	fileHash := normHash(fileSHA256)
	audioHash := normHash(audioSHA256)
	if fileHash == "" && audioHash == "" {
		return false, nil
	}

	// Look up stored entry so paired file+audio hashes are removed together cleanly.
	if stored, ok, err := s.IsStickerBanned(ctx, groupJID, fileHash, audioHash); err != nil {
		return false, err
	} else if ok {
		if stored.FileSHA256 != "" {
			fileHash = normHash(stored.FileSHA256)
		}
		if stored.AudioSHA256 != "" {
			audioHash = normHash(stored.AudioSHA256)
		}
	}

	var removed int64
	if fileHash != "" {
		n, err := s.client.HDel(ctx, banFileKey(groupJID), fileHash).Result()
		if err != nil {
			return false, fmt.Errorf("remove banned file hash: %w", err)
		}
		removed += n
	}
	if audioHash != "" {
		n, err := s.client.HDel(ctx, banAudioKey(groupJID), audioHash).Result()
		if err != nil {
			return false, fmt.Errorf("remove banned audio hash: %w", err)
		}
		removed += n
	}
	return removed > 0, nil
}

func (s *RedisStore) UnbanStickerByPrefix(ctx context.Context, groupJID, hashPrefix string) (bool, error) {
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

func (s *RedisStore) IsStickerBanned(ctx context.Context, groupJID, fileSHA256, audioSHA256 string) (command.BannedSticker, bool, error) {
	if fh := normHash(fileSHA256); fh != "" {
		raw, err := s.client.HGet(ctx, banFileKey(groupJID), fh).Bytes()
		if err != nil && !errors.Is(err, redis.Nil) {
			return command.BannedSticker{}, false, fmt.Errorf("lookup banned file hash: %w", err)
		}
		if err == nil {
			var entry command.BannedSticker
			if uerr := json.Unmarshal(raw, &entry); uerr != nil {
				return command.BannedSticker{FileSHA256: fh}, true, nil
			}
			return entry, true, nil
		}
	}
	if ah := normHash(audioSHA256); ah != "" {
		raw, err := s.client.HGet(ctx, banAudioKey(groupJID), ah).Bytes()
		if err != nil && !errors.Is(err, redis.Nil) {
			return command.BannedSticker{}, false, fmt.Errorf("lookup banned audio hash: %w", err)
		}
		if err == nil {
			var entry command.BannedSticker
			if uerr := json.Unmarshal(raw, &entry); uerr != nil {
				return command.BannedSticker{AudioSHA256: ah}, true, nil
			}
			return entry, true, nil
		}
	}
	return command.BannedSticker{}, false, nil
}

func (s *RedisStore) ListBannedStickers(ctx context.Context, groupJID string) ([]command.BannedSticker, error) {
	seen := make(map[string]command.BannedSticker)
	for _, key := range []string{banFileKey(groupJID), banAudioKey(groupJID)} {
		vals, err := s.client.HGetAll(ctx, key).Result()
		if err != nil {
			return nil, fmt.Errorf("list banned stickers: %w", err)
		}
		for hash, raw := range vals {
			var entry command.BannedSticker
			if err := json.Unmarshal([]byte(raw), &entry); err != nil {
				entry = command.BannedSticker{FileSHA256: hash}
			}
			entry.FileSHA256 = normHash(entry.FileSHA256)
			entry.AudioSHA256 = normHash(entry.AudioSHA256)
			dedupKey := entry.FileSHA256 + "|" + entry.AudioSHA256
			seen[dedupKey] = entry
		}
	}
	out := make([]command.BannedSticker, 0, len(seen))
	for _, entry := range seen {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AddedAt != out[j].AddedAt {
			return out[i].AddedAt < out[j].AddedAt
		}
		if out[i].FileSHA256 != out[j].FileSHA256 {
			return out[i].FileSHA256 < out[j].FileSHA256
		}
		return out[i].AudioSHA256 < out[j].AudioSHA256
	})
	return out, nil
}

func (s *RedisStore) AddStrike(ctx context.Context, groupJID, userID string) (int, error) {
	n, err := s.client.HIncrBy(ctx, strikesKey(groupJID), userID, 1).Result()
	if err != nil {
		return 0, fmt.Errorf("increment sticker strike: %w", err)
	}
	return int(n), nil
}

func (s *RedisStore) GetStrikes(ctx context.Context, groupJID, userID string) (int, error) {
	raw, err := s.client.HGet(ctx, strikesKey(groupJID), userID).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get sticker strikes: %w", err)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func (s *RedisStore) ResetStrikes(ctx context.Context, groupJID, userID string) error {
	if err := s.client.HDel(ctx, strikesKey(groupJID), userID).Err(); err != nil {
		return fmt.Errorf("reset sticker strikes: %w", err)
	}
	return nil
}

func (s *RedisStore) RememberSticker(ctx context.Context, groupJID, messageID string, info command.StickerInfo) error {
	if strings.TrimSpace(groupJID) == "" || strings.TrimSpace(messageID) == "" {
		return nil
	}
	info.FileSHA256 = normHash(info.FileSHA256)
	info.AudioSHA256 = normHash(info.AudioSHA256)
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, seenKey(groupJID, messageID), raw, seenStickerTTL).Err()
}

func (s *RedisStore) LookupSticker(ctx context.Context, groupJID, messageID string) (command.StickerInfo, bool, error) {
	if strings.TrimSpace(groupJID) == "" || strings.TrimSpace(messageID) == "" {
		return command.StickerInfo{}, false, nil
	}
	raw, err := s.client.Get(ctx, seenKey(groupJID, messageID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return command.StickerInfo{}, false, nil
	}
	if err != nil {
		return command.StickerInfo{}, false, fmt.Errorf("lookup seen sticker: %w", err)
	}
	var info command.StickerInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return command.StickerInfo{}, false, nil
	}
	return info, true, nil
}

func shieldsKey(groupJID string) string { return "sonicos:mod:shields:" + groupJID }
func rateKey(groupJID, bucket, userID string) string {
	return "sonicos:mod:rate:" + groupJID + ":" + bucket + ":" + userID
}
func stageKey(scopeKey string) string { return "sonicos:mod:stage:" + scopeKey }

func (s *RedisStore) GetShields(ctx context.Context, groupJID string) (command.GroupShields, error) {
	shields := command.GroupShields{AntiLoudDB: defaultAntiLoudDB}
	vals, err := s.client.HGetAll(ctx, shieldsKey(groupJID)).Result()
	if err != nil {
		return shields, fmt.Errorf("get group shields: %w", err)
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
	return shields, nil
}

func (s *RedisStore) SetShield(ctx context.Context, groupJID, field, value string) error {
	if err := s.client.HSet(ctx, shieldsKey(groupJID), field, value).Err(); err != nil {
		return fmt.Errorf("set group shield %q: %w", field, err)
	}
	return nil
}

func (s *RedisStore) IncrementRate(ctx context.Context, groupJID, bucket, userID string, windowSec int) (int, error) {
	if windowSec <= 0 {
		windowSec = 10
	}
	key := rateKey(groupJID, bucket, userID)
	count, err := s.client.Incr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("increment rate bucket %q: %w", bucket, err)
	}
	if count == 1 {
		if err := s.client.Expire(ctx, key, time.Duration(windowSec)*time.Second).Err(); err != nil {
			return int(count), fmt.Errorf("expire rate bucket %q: %w", bucket, err)
		}
	}
	return int(count), nil
}

func (s *RedisStore) SaveStage(ctx context.Context, scopeKey string, media command.StagedMedia, ttlSec int) error {
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
	if err := s.client.Set(ctx, stageKey(scopeKey), raw, time.Duration(ttlSec)*time.Second).Err(); err != nil {
		return fmt.Errorf("save staged media: %w", err)
	}
	return nil
}

func (s *RedisStore) LoadStage(ctx context.Context, scopeKey string) (command.StagedMedia, bool, error) {
	raw, err := s.client.Get(ctx, stageKey(scopeKey)).Bytes()
	if errors.Is(err, redis.Nil) {
		return command.StagedMedia{}, false, nil
	}
	if err != nil {
		return command.StagedMedia{}, false, fmt.Errorf("load staged media: %w", err)
	}
	var media command.StagedMedia
	if err := json.Unmarshal(raw, &media); err != nil {
		return command.StagedMedia{}, false, nil
	}
	return media, true, nil
}

func (s *RedisStore) ClearStage(ctx context.Context, scopeKey string) error {
	if err := s.client.Del(ctx, stageKey(scopeKey)).Err(); err != nil {
		return fmt.Errorf("clear staged media: %w", err)
	}
	return nil
}

func reactionLibraryKey(ownerID string) string {
	return "sonicos:sreact:lib:" + strings.TrimSpace(ownerID)
}

func normReactionEmojiKey(raw string) string {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.ReplaceAll(cleaned, "\uFE0F", "")
	cleaned = strings.ReplaceAll(cleaned, "\uFE0E", "")
	return strings.ToLower(cleaned)
}

func (s *RedisStore) SaveReactionSticker(ctx context.Context, ownerID, emoji string, sticker command.SavedReactionSticker) error {
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

	libKey := reactionLibraryKey(ownerID)
	exists, err := s.client.HExists(ctx, libKey, key).Result()
	if err != nil {
		return fmt.Errorf("check reaction sticker library: %w", err)
	}
	if !exists {
		count, err := s.client.HLen(ctx, libKey).Result()
		if err != nil {
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
	if err := s.client.HSet(ctx, libKey, key, payload).Err(); err != nil {
		return fmt.Errorf("save reaction sticker: %w", err)
	}
	return nil
}

func (s *RedisStore) GetReactionSticker(ctx context.Context, ownerID, emoji string) (command.SavedReactionSticker, bool, error) {
	ownerID = strings.TrimSpace(ownerID)
	key := normReactionEmojiKey(emoji)
	if ownerID == "" || key == "" {
		return command.SavedReactionSticker{}, false, nil
	}
	raw, err := s.client.HGet(ctx, reactionLibraryKey(ownerID), key).Bytes()
	if errors.Is(err, redis.Nil) {
		return command.SavedReactionSticker{}, false, nil
	}
	if err != nil {
		return command.SavedReactionSticker{}, false, fmt.Errorf("get reaction sticker: %w", err)
	}
	var st command.SavedReactionSticker
	if err := json.Unmarshal(raw, &st); err != nil {
		return command.SavedReactionSticker{}, false, nil
	}
	return st, true, nil
}

func (s *RedisStore) DeleteReactionSticker(ctx context.Context, ownerID, emoji string) (bool, error) {
	ownerID = strings.TrimSpace(ownerID)
	key := normReactionEmojiKey(emoji)
	if ownerID == "" || key == "" {
		return false, nil
	}
	n, err := s.client.HDel(ctx, reactionLibraryKey(ownerID), key).Result()
	if err != nil {
		return false, fmt.Errorf("delete reaction sticker: %w", err)
	}
	return n > 0, nil
}

func (s *RedisStore) ListReactionStickers(ctx context.Context, ownerID string) ([]command.SavedReactionSticker, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return nil, nil
	}
	vals, err := s.client.HGetAll(ctx, reactionLibraryKey(ownerID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list reaction stickers: %w", err)
	}
	out := make([]command.SavedReactionSticker, 0, len(vals))
	for key, raw := range vals {
		var st command.SavedReactionSticker
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			continue
		}
		if strings.TrimSpace(st.Emoji) == "" {
			st.Emoji = key
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SavedAt != out[j].SavedAt {
			return out[i].SavedAt < out[j].SavedAt
		}
		return out[i].Emoji < out[j].Emoji
	})
	return out, nil
}

func (s *RedisStore) SetWelcomeEnabled(ctx context.Context, groupJID string, enabled bool) error {
	val := "0"
	if enabled {
		val = "1"
	}
	key := "sonicos:mod:welcome:" + groupJID
	return s.client.Set(ctx, key, val, 0).Err()
}

func (s *RedisStore) GetWelcomeSettings(ctx context.Context, groupJID string) (WelcomeSettings, error) {
	enabled, err := s.client.Get(ctx, "sonicos:mod:welcome:"+groupJID).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return WelcomeSettings{}, fmt.Errorf("get welcome settings: %w", err)
	}
	customText, err := s.client.Get(ctx, "sonicos:mod:welcome:text:"+groupJID).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return WelcomeSettings{}, fmt.Errorf("get welcome custom text: %w", err)
	}
	return WelcomeSettings{Enabled: enabled == "1", CustomText: customText}, nil
}

func (s *RedisStore) SetWelcomeCustomText(ctx context.Context, groupJID, text string) error {
	return s.client.Set(ctx, "sonicos:mod:welcome:text:"+groupJID, text, 0).Err()
}
