package audit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoggerHashChainAndStructuredJSON(t *testing.T) {
	var buf bytes.Buffer
	slogger := slog.New(slog.NewJSONHandler(&buf, nil))
	l := New(slogger, 10)

	e1 := l.Record("authentication", "info", "admin", "dashboard-auth", "login", "success", "203.0.113.10\r\n", "user\nauthenticated", map[string]any{"method": "password\n"})
	e2 := l.Record("onboarding", "warning", "admin", "whatsapp-pairing", "phone_pair_request", "failure", "203.0.113.10", "pairing failed", map[string]any{"phone": MaskPhone("+15551234567")})

	if e1.PrevHash != "" || e1.Hash == "" {
		t.Fatalf("invalid first hash: prev=%q hash=%q", e1.PrevHash, e1.Hash)
	}
	if e2.PrevHash != e1.Hash || e2.Hash == "" {
		t.Fatalf("hash chain broken: e1=%q e2.prev=%q", e1.Hash, e2.PrevHash)
	}
	if strings.Contains(e1.Message, "\n") || strings.Contains(e1.SourceIP, "\n") {
		t.Fatalf("newline not sanitized: msg=%q ip=%q", e1.Message, e1.SourceIP)
	}

	if !VerifyChain([]Entry{e1, e2}) {
		t.Fatal("VerifyChain failed on valid chain")
	}

	tampered := e2
	tampered.Message = "forged"
	if VerifyChain([]Entry{e1, tampered}) {
		t.Fatal("VerifyChain did not detect tampering")
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON log lines, got %d", len(lines))
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &parsed); err != nil {
		t.Fatalf("invalid JSON log line: %v", err)
	}
	if parsed["audit"] != true || parsed["event_type"] != "authentication" {
		t.Fatalf("unexpected JSON audit fields: %v", parsed)
	}
}

func TestLoggerListFilteringAndPagination(t *testing.T) {
	l := New(nil, 5)
	l.Record("authentication", "info", "admin", "auth", "login", "success", "1.1.1.1", "ok 1", nil)
	l.Record("authentication", "warning", "anon", "auth", "login", "failure", "1.1.1.2", "bad 2", nil)
	l.Record("onboarding", "info", "admin", "qr", "qr_refresh", "success", "1.1.1.1", "qr refreshed", nil)

	items, total := l.List("", "warning", "", 1, 10)
	if total != 1 || len(items) != 1 || items[0].Message != "bad 2" {
		t.Fatalf("unexpected warning filter result: total=%d items=%v", total, items)
	}

	items, total = l.List("qr", "", "onboarding", 1, 10)
	if total != 1 || len(items) != 1 || items[0].Action != "qr_refresh" {
		t.Fatalf("unexpected type/query filter result: total=%d items=%v", total, items)
	}

	all, total := l.List("", "", "", 1, 2)
	if total != 3 || len(all) != 2 || all[0].Action != "qr_refresh" {
		t.Fatalf("unexpected newest-first pagination: total=%d all=%v", total, all)
	}
}

func TestMaskPhoneAndClientIP(t *testing.T) {
	if got := MaskPhone("+1 (555) 123-4567"); got != "+****4567" {
		t.Fatalf("MaskPhone = %q, want +****4567", got)
	}
	if got := MaskPhone("12"); got != "****" {
		t.Fatalf("MaskPhone short = %q, want ****", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.42, 10.0.0.1")
	req.RemoteAddr = "127.0.0.1:12345"
	if got := ClientIP(req); got != "198.51.100.42" {
		t.Fatalf("ClientIP XFF = %q, want 198.51.100.42", got)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.RemoteAddr = "192.0.2.9:5555"
	if got := ClientIP(req2); got != "192.0.2.9" {
		t.Fatalf("ClientIP RemoteAddr = %q, want 192.0.2.9", got)
	}
}
