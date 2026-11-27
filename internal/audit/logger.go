package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Entry struct {
	ID        string         `json:"id"`
	TS        string         `json:"ts"`
	Service   string         `json:"service"`
	Level     string         `json:"level"`
	EventType string         `json:"type"`
	Actor     string         `json:"actor"`
	Resource  string         `json:"resource"`
	Action    string         `json:"action"`
	Result    string         `json:"result"`
	SourceIP  string         `json:"source_ip,omitempty"`
	Message   string         `json:"message"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	PrevHash  string         `json:"prev_hash"`
	Hash      string         `json:"hash"`
}

type Logger struct {
	mu         sync.Mutex
	slogger    *slog.Logger
	service    string
	prevHash   string
	entries    []Entry
	maxEntries int
	seq        atomic.Uint64
}

func New(slogger *slog.Logger, maxEntries int) *Logger {
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return &Logger{
		slogger:    slogger,
		service:    "sonicos",
		entries:    make([]Entry, 0, min(maxEntries, 256)),
		maxEntries: maxEntries,
	}
}

func (l *Logger) Record(eventType, level, actor, resource, action, result, sourceIP, message string, metadata map[string]any) Entry {
	cleanMeta := sanitizeMetadata(metadata)
	seq := l.seq.Add(1)
	now := time.Now().UTC()

	entry := Entry{
		ID:        fmt.Sprintf("aud-%d-%d", now.UnixNano(), seq),
		TS:        now.Format(time.RFC3339Nano),
		Service:   l.service,
		Level:     normalizeLevel(level),
		EventType: sanitizeString(eventType),
		Actor:     sanitizeString(actor),
		Resource:  sanitizeString(resource),
		Action:    sanitizeString(action),
		Result:    sanitizeString(result),
		SourceIP:  sanitizeString(sourceIP),
		Message:   sanitizeString(message),
		Metadata:  cleanMeta,
	}

	l.mu.Lock()
	entry.PrevHash = l.prevHash
	entry.Hash = computeEntryHash(entry.PrevHash, entry)
	l.prevHash = entry.Hash
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.maxEntries {
		l.entries = l.entries[len(l.entries)-l.maxEntries:]
	}
	l.mu.Unlock()

	if l.slogger != nil {
		attrs := []any{
			"audit", true,
			"audit_id", entry.ID,
			"service", entry.Service,
			"event_type", entry.EventType,
			"actor", entry.Actor,
			"resource", entry.Resource,
			"action", entry.Action,
			"result", entry.Result,
			"prev_hash", entry.PrevHash,
			"hash", entry.Hash,
		}
		if entry.SourceIP != "" {
			attrs = append(attrs, "source_ip", entry.SourceIP)
		}
		if len(entry.Metadata) > 0 {
			attrs = append(attrs, "metadata", entry.Metadata)
		}
		switch entry.Level {
		case "error":
			l.slogger.Error(entry.Message, attrs...)
		case "warn", "warning":
			l.slogger.Warn(entry.Message, attrs...)
		default:
			l.slogger.Info(entry.Message, attrs...)
		}
	}

	return entry
}

func (l *Logger) List(q, level, eventType string, page, pageSize int) ([]Entry, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}

	qLower := strings.ToLower(strings.TrimSpace(q))
	levelLower := normalizeLevelFilter(level)
	typeLower := strings.ToLower(strings.TrimSpace(eventType))

	l.mu.Lock()
	snapshot := make([]Entry, len(l.entries))
	copy(snapshot, l.entries)
	l.mu.Unlock()

	filtered := make([]Entry, 0, len(snapshot))
	for i := len(snapshot) - 1; i >= 0; i-- {
		e := snapshot[i]
		if levelLower != "" && normalizeLevelFilter(e.Level) != levelLower {
			continue
		}
		if typeLower != "" && strings.ToLower(e.EventType) != typeLower {
			continue
		}
		if qLower != "" {
			haystack := strings.ToLower(e.Message + " " + e.Action + " " + e.Resource + " " + e.Actor + " " + e.Result + " " + e.SourceIP)
			if !strings.Contains(haystack, qLower) {
				continue
			}
		}
		filtered = append(filtered, e)
	}

	total := len(filtered)
	start := (page - 1) * pageSize
	if start >= total {
		return []Entry{}, total
	}
	end := min(start+pageSize, total)
	return filtered[start:end], total
}

func VerifyChain(entries []Entry) bool {
	if len(entries) == 0 {
		return true
	}
	prev := entries[0].PrevHash
	for _, entry := range entries {
		if entry.PrevHash != prev {
			return false
		}
		expected := computeEntryHash(prev, entry)
		if entry.Hash != expected {
			return false
		}
		prev = entry.Hash
	}
	return true
}

func ClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if clean := sanitizeIP(first); clean != "" {
			return clean
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if clean := sanitizeIP(xri); clean != "" {
			return clean
		}
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return sanitizeIP(host)
	}
	return sanitizeIP(r.RemoteAddr)
}

func MaskPhone(phone string) string {
	var digits strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	if len(d) < 4 {
		return "****"
	}
	return "+****" + d[len(d)-4:]
}

func computeEntryHash(prevHash string, entry Entry) string {
	canonical := struct {
		ID        string         `json:"id"`
		TS        string         `json:"ts"`
		Service   string         `json:"service"`
		Level     string         `json:"level"`
		EventType string         `json:"type"`
		Actor     string         `json:"actor"`
		Resource  string         `json:"resource"`
		Action    string         `json:"action"`
		Result    string         `json:"result"`
		SourceIP  string         `json:"source_ip,omitempty"`
		Message   string         `json:"message"`
		Metadata  map[string]any `json:"metadata,omitempty"`
	}{
		ID:        entry.ID,
		TS:        entry.TS,
		Service:   entry.Service,
		Level:     entry.Level,
		EventType: entry.EventType,
		Actor:     entry.Actor,
		Resource:  entry.Resource,
		Action:    entry.Action,
		Result:    entry.Result,
		SourceIP:  entry.SourceIP,
		Message:   entry.Message,
		Metadata:  entry.Metadata,
	}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256([]byte(prevHash + ":" + string(raw)))
	return hex.EncodeToString(sum[:])
}

func sanitizeString(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}

func sanitizeIP(s string) string {
	clean := sanitizeString(s)
	if clean == "" {
		return ""
	}
	if ip := net.ParseIP(clean); ip != nil {
		return ip.String()
	}

	clean = strings.ReplaceAll(clean, " ", "")
	return clean
}

func sanitizeMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		cleanKey := sanitizeString(k)
		switch val := v.(type) {
		case string:
			out[cleanKey] = sanitizeString(val)
		default:
			out[cleanKey] = val
		}
	}
	return out
}

func normalizeLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "error", "err", "critical":
		return "error"
	case "warn", "warning":
		return "warning"
	default:
		return "info"
	}
}

func normalizeLevelFilter(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "":
		return ""
	case "error", "err", "critical":
		return "error"
	case "warn", "warning":
		return "warning"
	default:
		return "info"
	}
}
