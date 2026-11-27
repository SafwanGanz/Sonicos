package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/audit"
	"github.com/SafwanGanz/Sonicos/internal/command"
	"github.com/SafwanGanz/Sonicos/internal/whatsapp"
	"go.mau.fi/whatsmeow/types"
)

type fakeBot struct {
	state          string
	deviceID       *types.JID
	connectedSince time.Time
	qrSnap         whatsapp.QRSnapshot
	qrErr          error
	refreshErr     error
	pairCode       string
	pairExpiry     time.Time
	pairErr        error
	lastCountry    string
	lastPhone      string
}

func (f *fakeBot) ClientState() string                  { return f.state }
func (f *fakeBot) DeviceID() *types.JID                 { return f.deviceID }
func (f *fakeBot) ConnectedSince() time.Time            { return f.connectedSince }
func (f *fakeBot) QRCode() (whatsapp.QRSnapshot, error) { return f.qrSnap, f.qrErr }
func (f *fakeBot) RefreshQR() error                     { return f.refreshErr }
func (f *fakeBot) RequestPairing(_ context.Context, cc, phone string) (string, time.Time, error) {
	f.lastCountry = cc
	f.lastPhone = phone
	return f.pairCode, f.pairExpiry, f.pairErr
}
func (f *fakeBot) Reconnect() error                           { return nil }
func (f *fakeBot) Disconnect() error                          { f.state = "disconnected"; return nil }
func (f *fakeBot) Unlink() error                              { f.state = "disconnected"; f.deviceID = nil; return nil }
func (f *fakeBot) ListGroups() []whatsapp.GroupInfo           { return nil }
func (f *fakeBot) GetGroup(string) *whatsapp.GroupInfo        { return nil }
func (f *fakeBot) ListUsers() []whatsapp.GroupParticipantInfo { return nil }

func newTestServer(bot BotClient, password string, origins []string) (*Server, *audit.Logger) {
	reg := command.NewRegistry()
	aud := audit.New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), 100)
	srv := NewServer(bot, reg, nil, aud, password, origins, slog.Default())
	return srv, aud
}

func loginTestSession(t *testing.T, handler http.Handler, password string, https bool) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Password: password})
	req := httptest.NewRequest(http.MethodPost, "/auth/login", bytes.NewReader(body))
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Set("Content-Type", "application/json")
	if https {
		req.Header.Set("X-Forwarded-Proto", "https")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed with status %d: %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("missing session cookie on login response")
	return nil
}

func TestIndexHealthAndAuthEnforcement(t *testing.T) {
	bot := &fakeBot{state: "qr"}
	srv, _ := newTestServer(bot, "Secr3tPass", nil)
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "SONICOS") {
		t.Fatal("GET / did not serve embedded dashboard HTML")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("missing security headers on /: %v", rec.Header())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d", rec.Code)
	}

	for _, path := range []string{"/auth/me", "/status", "/qr", "/logs", "/events"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s unauthenticated status = %d, want 401", path, rec.Code)
		}
	}
}

func TestLoginOpaqueCookieRateLimitAndLogout(t *testing.T) {
	bot := &fakeBot{state: "qr"}
	srv, aud := newTestServer(bot, "StrongPass99", nil)
	h := srv.Handler()

	for i := range rateLimitMaxFails {
		req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"password":"wrong"}`))
		req.RemoteAddr = "203.0.113.77:9000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"password":"StrongPass99"}`))
	req.RemoteAddr = "203.0.113.77:9000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt status = %d, want 429", rec.Code)
	}

	cookie := loginTestSession(t, h, "StrongPass99", true)
	if cookie.Value == "" || cookie.Value == "StrongPass99" {
		t.Fatalf("session cookie must be an opaque random token, got %q", cookie.Value)
	}
	if !cookie.HttpOnly || !cookie.Secure {
		t.Fatalf("expected HttpOnly=true Secure=true cookie, got %+v", cookie)
	}

	meReq := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	meReq.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, meReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/me with cookie = %d, want 200", rec.Code)
	}

	outReq := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	outReq.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, outReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /auth/logout = %d, want 200", rec.Code)
	}

	meReq2 := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	meReq2.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, meReq2)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /auth/me after logout = %d, want 401", rec.Code)
	}

	entries, _ := aud.List("", "", "authentication", 1, 20)
	if len(entries) < 7 {
		t.Fatalf("expected authentication audit entries, got %d", len(entries))
	}
}

func TestStrictCORSAndCSRF(t *testing.T) {
	bot := &fakeBot{state: "qr"}
	srv, _ := newTestServer(bot, "Secret123", []string{"https://panel.example.com"})
	h := srv.Handler()
	cookie := loginTestSession(t, h, "Secret123", false)

	evilReq := httptest.NewRequest(http.MethodPost, "/connection/qr/refresh", nil)
	evilReq.Host = "bot.example.com"
	evilReq.Header.Set("Origin", "https://evil.example")
	evilReq.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, evilReq)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("untrusted Origin POST status = %d, want 403", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("untrusted origin must not receive ACAO header")
	}

	okReq := httptest.NewRequest(http.MethodGet, "/status", nil)
	okReq.Host = "bot.example.com"
	okReq.Header.Set("Origin", "https://panel.example.com")
	okReq.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, okReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed origin status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://panel.example.com" {
		t.Fatalf("missing ACAO for allowed origin: %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestQRPNGAndPhonePairing(t *testing.T) {
	exp := time.Now().UTC().Add(20 * time.Second)
	bot := &fakeBot{
		state: "qr",
		qrSnap: whatsapp.QRSnapshot{
			Code:      "2@sample-whatsapp-qr-payload",
			State:     "qr",
			ExpiresAt: exp,
		},
		pairCode:   "ABCD1234",
		pairExpiry: time.Now().UTC().Add(160 * time.Second),
	}
	srv, aud := newTestServer(bot, "Secret123", nil)
	h := srv.Handler()
	cookie := loginTestSession(t, h, "Secret123", false)

	qrReq := httptest.NewRequest(http.MethodGet, "/qr", nil)
	qrReq.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, qrReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /qr = %d: %s", rec.Code, rec.Body.String())
	}
	var qrResp qrResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &qrResp); err != nil {
		t.Fatalf("decode qr response: %v", err)
	}
	if !strings.HasPrefix(qrResp.Image, "data:image/png;base64,iVBORw0KGgo") {
		t.Fatalf("qr_image is not a valid PNG data URI: %q", qrResp.Image[:min(len(qrResp.Image), 40)])
	}
	if qrResp.QR != "2@sample-whatsapp-qr-payload" || qrResp.ExpiresAt == "" {
		t.Fatalf("unexpected qr response: %+v", qrResp)
	}

	bot.qrErr = whatsapp.ErrAlreadyConnected
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, qrReq)
	if rec.Code != http.StatusConflict {
		t.Fatalf("GET /qr when connected = %d, want 409", rec.Code)
	}

	pairReq := httptest.NewRequest(http.MethodPost, "/pair", strings.NewReader(`{"country_code":"91","phone_number":"9876543210"}`))
	pairReq.Header.Set("Content-Type", "application/json")
	pairReq.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, pairReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /pair = %d: %s", rec.Code, rec.Body.String())
	}
	var pResp pairResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pResp); err != nil {
		t.Fatalf("decode pair response: %v", err)
	}
	if pResp.PairingCode != "ABCD1234" || pResp.ExpiresAt == "" {
		t.Fatalf("unexpected pair response: %+v", pResp)
	}

	entries, _ := aud.List("", "", "onboarding", 1, 10)
	if len(entries) == 0 || !strings.Contains(entries[0].Message, "+****3210") {
		t.Fatalf("expected masked phone in onboarding audit entry, got %+v", entries)
	}
	if strings.Contains(entries[0].Message, "9876543210") {
		t.Fatal("raw phone number leaked in audit log")
	}
}

func TestDisabledHandler(t *testing.T) {
	aud := audit.New(nil, 10)
	h := NewDisabledHandler(aud)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d, want 200", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "DASHBOARD_PASSWORD") {
		t.Fatalf("GET / disabled status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLoginGlobalRateLimitMitigatesXFFSpoofing(t *testing.T) {
	bot := &fakeBot{state: "qr"}
	srv, _ := newTestServer(bot, "StrongPass99", nil)
	h := srv.Handler()

	for i := range globalRateLimitMaxFails {
		req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"password":"wrong"}`))
		req.RemoteAddr = "198.51.100.1:4321"
		req.Header.Set("X-Forwarded-For", "198.51.100."+string(rune('1'+i%9))+", 10.0.0.1")
		if i >= 9 {
			req.Header.Set("X-Forwarded-For", "198.51.101."+string(rune('1'+i%9))+", 10.0.0.1")
		}
		if i >= 18 {
			req.Header.Set("X-Forwarded-For", "198.51.102."+string(rune('1'+i%9))+", 10.0.0.1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"password":"StrongPass99"}`))
	req.RemoteAddr = "198.51.100.1:4321"
	req.Header.Set("X-Forwarded-For", "203.0.113.250")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed XFF attempt after global limit = %d, want 429", rec.Code)
	}
}
