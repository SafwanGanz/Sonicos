package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SafwanGanz/Sonicos/cmd/static"
	"github.com/SafwanGanz/Sonicos/internal/audit"
	"github.com/SafwanGanz/Sonicos/internal/command"
	"github.com/SafwanGanz/Sonicos/internal/whatsapp"
	"go.mau.fi/whatsmeow/types"
	"rsc.io/qr"
)

const (
	sessionCookieName       = "sonicos_session"
	sessionTTL              = 7 * 24 * time.Hour
	maxSessions             = 64
	rateLimitMaxFails       = 5
	globalRateLimitMaxFails = 25
	rateLimitWindow         = 5 * time.Minute
	sseHeartbeat            = 25 * time.Second
)

type BotClient interface {
	ClientState() string
	DeviceID() *types.JID
	ConnectedSince() time.Time
	QRCode() (whatsapp.QRSnapshot, error)
	RefreshQR() error
	RequestPairing(ctx context.Context, countryCode, phoneNumber string) (string, time.Time, error)
	Reconnect() error
	Disconnect() error
	Unlink() error
	ListGroups() []whatsapp.GroupInfo
	GetGroup(jid string) *whatsapp.GroupInfo
	ListUsers() []whatsapp.GroupParticipantInfo
}

type Server struct {
	bot            BotClient
	registry       *command.Registry
	modStore       command.ModerationStore
	audit          *audit.Logger
	logger         *slog.Logger
	authPassword   string
	allowedOrigins map[string]struct{}

	mu       sync.RWMutex
	settings map[string]any

	sessionMu sync.RWMutex
	sessions  map[string]time.Time

	rateMu              sync.Mutex
	loginAttempts       map[string][]time.Time
	globalLoginAttempts []time.Time

	clientsMu sync.Mutex
	clients   map[chan []byte]struct{}

	startedAt time.Time
	version   string

	dbDriver          string
	modBackend        string
	storagePersistent bool
	cloudPlatform     string

	modMu        sync.Mutex
	modEvents    []modHistoryItem
	maxModEvents int
}

type statusResponse struct {
	Connected        bool          `json:"connected"`
	State            string        `json:"state"`
	AccountID        string        `json:"account_id"`
	Phone            string        `json:"phone"`
	PushName         string        `json:"push_name"`
	UptimeSeconds    int64         `json:"uptime_seconds"`
	ConnectedSince   string        `json:"connected_since,omitempty"`
	TotalMessages    int64         `json:"total_messages"`
	ActiveGroups     int           `json:"active_groups"`
	TotalUsers       int           `json:"total_users"`
	ModerationEvents int           `json:"moderation_events"`
	TotalWarnings    int           `json:"total_warnings"`
	Maintenance      bool          `json:"maintenance"`
	Version          string        `json:"version"`
	Health           []healthCheck `json:"health"`
}

type healthCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type loginRequest struct {
	Password string `json:"password"`
}

type loginResponse struct {
	Authenticated bool `json:"authenticated"`
}

type qrResponse struct {
	State     string `json:"state"`
	QR        string `json:"qr,omitempty"`
	Image     string `json:"qr_image,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

type pairRequest struct {
	CountryCode string `json:"country_code"`
	PhoneNumber string `json:"phone_number"`
}

type pairResponse struct {
	PairingCode string `json:"pairing_code"`
	ExpiresAt   string `json:"expires_at"`
}

type settingsRequest struct {
	BotName     string `json:"bot_name"`
	Description string `json:"description"`
	Language    string `json:"language"`
	Timezone    string `json:"timezone"`
	AutoStart   bool   `json:"auto_start"`
	Maintenance bool   `json:"maintenance"`
}

type settingsResponse struct {
	BotName     string `json:"bot_name"`
	Prefix      string `json:"prefix"`
	Description string `json:"description"`
	Language    string `json:"language"`
	Timezone    string `json:"timezone"`
	AutoStart   bool   `json:"auto_start"`
	Maintenance bool   `json:"maintenance"`
}

type prefixRequest struct {
	Prefix string `json:"prefix"`
}

type groupResponse struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	Members           int          `json:"members"`
	BotEnabled        bool         `json:"bot_enabled"`
	WelcomeEnabled    bool         `json:"welcome_enabled"`
	ModerationEnabled bool         `json:"moderation_enabled"`
	AdminOnlyCommands bool         `json:"admin_only_commands"`
	WelcomeMessage    string       `json:"welcome_message"`
	DisabledCommands  []string     `json:"disabled_commands"`
	Admins            []groupAdmin `json:"admins"`
}

type groupAdmin struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

type groupPatchRequest struct {
	BotEnabled        *bool    `json:"bot_enabled,omitempty"`
	WelcomeEnabled    *bool    `json:"welcome_enabled,omitempty"`
	ModerationEnabled *bool    `json:"moderation_enabled,omitempty"`
	AdminOnlyCommands *bool    `json:"admin_only_commands,omitempty"`
	WelcomeMessage    string   `json:"welcome_message,omitempty"`
	DisabledCommands  []string `json:"disabled_commands,omitempty"`
}

type moderationSettings struct {
	Enabled            bool    `json:"enabled"`
	DryRun             bool    `json:"dry_run"`
	AutoDelete         bool    `json:"auto_delete"`
	RemovalEnabled     bool    `json:"removal_enabled"`
	WarningThreshold   int     `json:"warning_threshold"`
	RemovalViolation   int     `json:"removal_violation"`
	HighConfidence     float64 `json:"high_confidence"`
	PossibleConfidence float64 `json:"possible_confidence"`
}

type enrolledItem struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Type           string `json:"type"`
	EnrolledAt     string `json:"enrolled_at"`
	EnrolledBy     string `json:"enrolled_by"`
	EnrolledByName string `json:"enrolled_by_name,omitempty"`
}

type matchItem struct {
	ID         string  `json:"id"`
	Tier       string  `json:"tier"`
	TS         string  `json:"ts"`
	User       string  `json:"user"`
	UserName   string  `json:"user_name"`
	Group      string  `json:"group"`
	GroupName  string  `json:"group_name"`
	Type       string  `json:"type"`
	Confidence float64 `json:"confidence"`
	Status     string  `json:"status"`
}

type matchReviewRequest struct {
	Decision string `json:"decision"`
}

type modHistoryItem struct {
	TS        string `json:"ts"`
	User      string `json:"user"`
	UserName  string `json:"user_name"`
	Group     string `json:"group"`
	GroupName string `json:"group_name"`
	Action    string `json:"action"`
	DryRun    bool   `json:"dry_run"`
	Detail    string `json:"detail"`
}

type userItem struct {
	JID       string `json:"jid"`
	Name      string `json:"name"`
	Display   string `json:"display"`
	Group     string `json:"group"`
	GroupName string `json:"group_name"`
	IsAdmin   bool   `json:"is_admin"`
	Warnings  int    `json:"warnings"`
}

type userHistoryItem struct {
	TS      string `json:"ts"`
	Action  string `json:"action"`
	Type    string `json:"type"`
	Detail  string `json:"detail"`
	Message string `json:"message"`
}

type commandItem struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	Enabled     bool     `json:"enabled"`
	Permission  string   `json:"permission"`
	AdminOnly   bool     `json:"admin_only"`
}

type pagedResponse struct {
	Items []any `json:"items"`
	Total int   `json:"total"`
}

func NewServer(
	bot BotClient,
	registry *command.Registry,
	modStore command.ModerationStore,
	auditLog *audit.Logger,
	authPassword string,
	allowedOrigins []string,
	logger *slog.Logger,
) *Server {
	if auditLog == nil {
		auditLog = audit.New(logger, 1000)
	}
	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		clean := strings.TrimRight(strings.TrimSpace(o), "/")
		if clean != "" {
			originSet[strings.ToLower(clean)] = struct{}{}
		}
	}

	s := &Server{
		bot:               bot,
		registry:          registry,
		modStore:          modStore,
		audit:             auditLog,
		logger:            logger,
		authPassword:      authPassword,
		allowedOrigins:    originSet,
		settings:          make(map[string]any),
		sessions:          make(map[string]time.Time),
		loginAttempts:     make(map[string][]time.Time),
		clients:           make(map[chan []byte]struct{}),
		startedAt:         time.Now(),
		version:           "v0.1.0",
		dbDriver:          "sqlite3",
		modBackend:        "memory",
		storagePersistent: true,
		modEvents:         make([]modHistoryItem, 0, 500),
		maxModEvents:      500,
	}

	prefix := strings.TrimSpace(os.Getenv("BOT_PREFIX"))
	if prefix == "" {
		prefix = "!"
	}
	s.settings["bot_name"] = "Sonicos"
	s.settings["prefix"] = prefix
	s.settings["description"] = "A helpful WhatsApp assistant for community groups."
	s.settings["language"] = "en"
	s.settings["timezone"] = "UTC"
	s.settings["auto_start"] = true
	s.settings["maintenance"] = false

	return s
}

func NewDisabledHandler(auditLog *audit.Logger) http.Handler {
	const disabledMsg = "Dashboard disabled: set DASHBOARD_PASSWORD in your environment variables and redeploy."
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setCommonSecurityHeaders(w)
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok","dashboard":"disabled"}`))
			return
		}
		if auditLog != nil {
			auditLog.Record("system", "warning", "anonymous", r.URL.Path, "disabled_dashboard_access", "denied", audit.ClientIP(r), "request rejected because DASHBOARD_PASSWORD is not configured", nil)
		}
		w.Header().Set("Cache-Control", "no-store")
		if strings.Contains(r.Header.Get("Accept"), "text/html") && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sonicos — Dashboard Disabled</title><style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0e0e0e;color:#f1efea;font-family:system-ui,-apple-system,sans-serif;padding:24px}.box{max-width:520px;border:1px solid #333;background:#141414;padding:28px}h1{margin:0 0 12px;font-weight:400}p{margin:0;color:#8b8b86;line-height:1.6}code{color:#ff4a1c}</style></head><body><div class="box"><h1>Dashboard disabled</h1><p>Set <code>DASHBOARD_PASSWORD</code> in your environment variables and restart the bot to enable the protected WhatsApp onboarding dashboard. Terminal QR pairing remains active in logs.</p></div></body></html>`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": disabledMsg})
	})
}

func setCommonSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (s *Server) isOriginAllowed(origin string, r *http.Request) bool {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	normalized := strings.ToLower(u.Scheme + "://" + u.Host)
	if _, ok := s.allowedOrigins[normalized]; ok {
		return true
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if xfh := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); xfh != "" {
		firstHost, _, _ := strings.Cut(xfh, ",")
		if strings.EqualFold(u.Host, strings.TrimSpace(firstHost)) {
			return true
		}
	}
	return false
}

func (s *Server) applyCORSAndCSRF(w http.ResponseWriter, r *http.Request) bool {
	setCommonSecurityHeaders(w)
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" {
		if !s.isOriginAllowed(origin, r) {
			if r.Method == http.MethodOptions || isMutatingMethod(r.Method) {
				s.writeError(w, http.StatusForbidden, "origin not allowed")
				return false
			}
			return true
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Requested-With")
	}

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

func isMutatingMethod(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.applyCORSAndCSRF(w, r) {
			return
		}
		next(w, r)
	}
}

func (s *Server) corsAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.applyCORSAndCSRF(w, r) {
			return
		}
		if _, ok := s.validateSession(r); !ok {
			s.audit.Record("authentication", "warning", "anonymous", r.URL.Path, "unauthorized_access", "failure", audit.ClientIP(r), "unauthenticated API request rejected", nil)
			s.writeError(w, http.StatusUnauthorized, "Not signed in")
			return
		}
		next(w, r)
	}
}

func (s *Server) newSessionToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	now := time.Now().UTC()
	exp := now.Add(sessionTTL)

	s.sessionMu.Lock()
	for k, v := range s.sessions {
		if now.After(v) {
			delete(s.sessions, k)
		}
	}
	for len(s.sessions) >= maxSessions {
		var oldestKey string
		var oldestExp time.Time
		for k, v := range s.sessions {
			if oldestKey == "" || v.Before(oldestExp) {
				oldestKey = k
				oldestExp = v
			}
		}
		if oldestKey != "" {
			delete(s.sessions, oldestKey)
		} else {
			break
		}
	}
	s.sessions[token] = exp
	s.sessionMu.Unlock()
	return token, nil
}

func (s *Server) validateSession(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	now := time.Now().UTC()
	s.sessionMu.RLock()
	exp, ok := s.sessions[cookie.Value]
	s.sessionMu.RUnlock()
	if !ok {
		return "", false
	}
	if now.After(exp) {
		s.sessionMu.Lock()
		delete(s.sessions, cookie.Value)
		s.sessionMu.Unlock()
		return "", false
	}
	return cookie.Value, true
}

func (s *Server) revokeSession(token string) {
	if token == "" {
		return
	}
	s.sessionMu.Lock()
	delete(s.sessions, token)
	s.sessionMu.Unlock()
}

func pruneAttempts(attempts []time.Time, cutoff time.Time) []time.Time {
	pruned := attempts[:0]
	for _, ts := range attempts {
		if ts.After(cutoff) {
			pruned = append(pruned, ts)
		}
	}
	return pruned
}

func retryAfterFrom(attempts []time.Time, now time.Time) int {
	retryAfter := int(attempts[0].Add(rateLimitWindow).Sub(now).Seconds()) + 1
	if retryAfter < 1 {
		return 1
	}
	return retryAfter
}

func (s *Server) checkLoginRateLimit(ip string) (bool, int) {
	if ip == "" {
		ip = "unknown"
	}
	now := time.Now().UTC()
	cutoff := now.Add(-rateLimitWindow)

	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	s.globalLoginAttempts = pruneAttempts(s.globalLoginAttempts, cutoff)
	if len(s.globalLoginAttempts) >= globalRateLimitMaxFails {
		return true, retryAfterFrom(s.globalLoginAttempts, now)
	}

	pruned := pruneAttempts(s.loginAttempts[ip], cutoff)
	if len(pruned) == 0 {
		delete(s.loginAttempts, ip)
		return false, 0
	}
	s.loginAttempts[ip] = pruned
	if len(pruned) >= rateLimitMaxFails {
		return true, retryAfterFrom(pruned, now)
	}
	return false, 0
}

func (s *Server) recordLoginFailure(ip string) {
	if ip == "" {
		ip = "unknown"
	}
	now := time.Now().UTC()
	cutoff := now.Add(-rateLimitWindow)

	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	s.globalLoginAttempts = append(pruneAttempts(s.globalLoginAttempts, cutoff), now)
	s.loginAttempts[ip] = append(pruneAttempts(s.loginAttempts[ip], cutoff), now)
}

func (s *Server) clearLoginFailures(ip string) {
	if ip == "" {
		return
	}
	s.rateMu.Lock()
	delete(s.loginAttempts, ip)
	s.rateMu.Unlock()
}

func (s *Server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if r.Body == nil {
		return errors.New("missing request body")
	}
	limited := http.MaxBytesReader(w, r.Body, 1<<20)
	defer limited.Close()
	return json.NewDecoder(limited).Decode(v)
}

func (s *Server) writeError(w http.ResponseWriter, code int, msg string) {
	s.json(w, code, map[string]string{"error": msg})
}

func (s *Server) broadcast(event string, data any) {
	payload, _ := json.Marshal(data)
	msg := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(payload))
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- []byte(msg):
		default:

		}
	}
}

func (s *Server) addClient(ch chan []byte) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	s.clients[ch] = struct{}{}
}

func (s *Server) removeClient(ch chan []byte) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	delete(s.clients, ch)
	close(ch)
}

func (s *Server) SetStorageInfo(dbDriver, modBackend string, persistent bool, platform string) {
	s.mu.Lock()
	if dbDriver != "" {
		s.dbDriver = dbDriver
	}
	if modBackend != "" {
		s.modBackend = modBackend
	}
	s.storagePersistent = persistent
	s.cloudPlatform = platform
	s.mu.Unlock()
}

func (s *Server) HandleBotEvent(event string, data any) {
	switch event {
	case "qr":
		if snap, ok := data.(whatsapp.QRSnapshot); ok {
			s.broadcast("qr", buildQRResponse(snap))
		}
	case "status":
		if meta, ok := data.(map[string]any); ok {
			if evName, _ := meta["event"].(string); evName != "" {
				level := "info"
				result := "success"
				if evName == "disconnected" || evName == "logged_out" {
					level = "warning"
				}
				s.audit.Record("onboarding", level, "whatsapp", "whatsapp-session", "whatsapp_"+evName, result, "", "WhatsApp session event: "+evName, meta)
			}
		}
		s.broadcast("status", s.buildStatus())
	case "pairing":
		s.broadcast("pairing", data)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	setCommonSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	s.json(w, http.StatusOK, map[string]string{
		"status":   "ok",
		"whatsapp": s.bot.ClientState(),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ip := audit.ClientIP(r)
	if limited, retryAfter := s.checkLoginRateLimit(ip); limited {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		s.audit.Record("authentication", "warning", "anonymous", "dashboard-auth", "login_rate_limited", "failure", ip, "login blocked due to repeated failed attempts", map[string]any{"retry_after_seconds": retryAfter})
		s.writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
		return
	}

	var req loginRequest
	if err := s.readJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if s.authPassword == "" || subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.authPassword)) != 1 {
		s.recordLoginFailure(ip)
		s.audit.Record("authentication", "warning", "anonymous", "dashboard-auth", "login", "failure", ip, "dashboard login failed: incorrect password", nil)
		s.writeError(w, http.StatusUnauthorized, "incorrect password")
		return
	}

	token, err := s.newSessionToken()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to create session")
		return
	}
	s.clearLoginFailures(ip)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	s.audit.Record("authentication", "info", "admin", "dashboard-auth", "login", "success", ip, "user authenticated via dashboard", nil)
	s.broadcast("activity", map[string]string{"type": "authentication"})
	s.json(w, http.StatusOK, loginResponse{Authenticated: true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.revokeSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	s.audit.Record("authentication", "info", "admin", "dashboard-auth", "logout", "success", audit.ClientIP(r), "user logged out from dashboard", nil)
	s.json(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.validateSession(r); !ok {
		s.json(w, http.StatusUnauthorized, map[string]any{"authenticated": false, "error": "Not signed in"})
		return
	}
	s.json(w, http.StatusOK, map[string]any{"authenticated": true})
}

func (s *Server) buildStatus() statusResponse {
	st := s.bot.ClientState()
	connected := st == "connected"

	accountID := ""
	phone := ""
	pushName := ""
	if dev := s.bot.DeviceID(); dev != nil {
		accountID = dev.String()
		phone = dev.User
	}

	sinceStr := ""
	if connected {
		if since := s.bot.ConnectedSince(); !since.IsZero() {
			sinceStr = since.UTC().Format(time.RFC3339)
		}
	}

	s.mu.RLock()
	dbDriver := s.dbDriver
	modBackend := s.modBackend
	persistent := s.storagePersistent
	platform := s.cloudPlatform
	s.mu.RUnlock()

	dbStatus := "ok"
	dbDetail := fmt.Sprintf("%s (persistent)", dbDriver)
	if dbDriver == "pgx" {
		dbDetail = "PostgreSQL (persistent)"
	} else if !persistent {
		dbStatus = "degraded"
		dbDetail = fmt.Sprintf("SQLite ephemeral on %s — add PostgreSQL DATABASE_URL for persistence", platform)
	}
	featureDetail := fmt.Sprintf("Active (%s)", modBackend)
	if modBackend == "pgx" {
		featureDetail = "PostgreSQL (persistent)"
	}

	return statusResponse{
		Connected:        connected,
		State:            st,
		AccountID:        accountID,
		Phone:            phone,
		PushName:         pushName,
		UptimeSeconds:    int64(time.Since(s.startedAt).Seconds()),
		ConnectedSince:   sinceStr,
		TotalMessages:    s.getStat("messages", 0),
		ActiveGroups:     int(s.getStat("groups", 0)),
		TotalUsers:       int(s.getStat("users", 0)),
		ModerationEvents: int(s.getStat("moderation", 0)),
		TotalWarnings:    int(s.getStat("warnings", 0)),
		Maintenance:      s.getBool("maintenance", false),
		Version:          s.version,
		Health: []healthCheck{
			{Name: "WhatsApp socket", Status: healthStatus(connected), Detail: healthDetail(connected)},
			{Name: "Database", Status: dbStatus, Detail: dbDetail},
			{Name: "Feature store", Status: "ok", Detail: featureDetail},
			{Name: "Media scanner", Status: "ok", Detail: "Ready"},
		},
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.json(w, http.StatusOK, s.buildStatus())
}

func healthStatus(connected bool) string {
	if connected {
		return "ok"
	}
	return "down"
}

func healthDetail(connected bool) string {
	if connected {
		return "Connected"
	}
	return "Not connected"
}

func encodeQRPNGDataURL(payload string) string {
	if payload == "" {
		return ""
	}
	code, err := qr.Encode(payload, qr.M)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())
}

func buildQRResponse(snap whatsapp.QRSnapshot) qrResponse {
	resp := qrResponse{
		State: snap.State,
		QR:    snap.Code,
		Error: snap.Error,
	}
	if snap.Code != "" {
		resp.Image = encodeQRPNGDataURL(snap.Code)
	}
	if !snap.ExpiresAt.IsZero() {
		resp.ExpiresAt = snap.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return resp
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	snap, err := s.bot.QRCode()
	if err != nil {
		if errors.Is(err, whatsapp.ErrAlreadyConnected) {
			s.writeError(w, http.StatusConflict, "Already connected")
			return
		}
		if errors.Is(err, whatsapp.ErrNoQR) {
			s.writeError(w, http.StatusNotFound, "No QR available")
			return
		}
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.json(w, http.StatusOK, buildQRResponse(snap))
}

func (s *Server) handleQRRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ip := audit.ClientIP(r)
	if err := s.bot.RefreshQR(); err != nil {
		if errors.Is(err, whatsapp.ErrAlreadyConnected) {
			s.writeError(w, http.StatusConflict, "Already connected")
			return
		}
		s.audit.Record("onboarding", "error", "admin", "whatsapp-qr", "qr_refresh", "failure", ip, "QR refresh failed: "+err.Error(), nil)
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit.Record("onboarding", "info", "admin", "whatsapp-qr", "qr_refresh", "success", ip, "WhatsApp QR pairing session refreshed", nil)
	s.json(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req pairRequest
	if err := s.readJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if strings.TrimSpace(req.CountryCode) == "" || strings.TrimSpace(req.PhoneNumber) == "" {
		s.writeError(w, http.StatusBadRequest, "country_code and phone_number are required")
		return
	}

	ip := audit.ClientIP(r)
	maskedPhone := audit.MaskPhone(req.CountryCode + req.PhoneNumber)

	code, expiresAt, err := s.bot.RequestPairing(r.Context(), req.CountryCode, req.PhoneNumber)
	if err != nil {
		s.audit.Record("onboarding", "warning", "admin", "whatsapp-pairing", "phone_pair_request", "failure", ip, "phone pairing failed for "+maskedPhone, map[string]any{"phone": maskedPhone})
		if errors.Is(err, whatsapp.ErrAlreadyConnected) {
			s.writeError(w, http.StatusConflict, "Already connected. Unlink the account first.")
			return
		}
		if strings.Contains(err.Error(), "digits") {
			s.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.audit.Record("onboarding", "info", "admin", "whatsapp-pairing", "phone_pair_request", "success", ip, "pairing code generated for "+maskedPhone, map[string]any{"phone": maskedPhone})
	s.json(w, http.StatusOK, pairResponse{
		PairingCode: code,
		ExpiresAt:   expiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ip := audit.ClientIP(r)
	if err := s.bot.Reconnect(); err != nil {
		s.audit.Record("onboarding", "error", "admin", "whatsapp-session", "reconnect", "failure", ip, "WhatsApp reconnect failed", nil)
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit.Record("onboarding", "info", "admin", "whatsapp-session", "reconnect", "success", ip, "WhatsApp reconnect requested", nil)
	s.json(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ip := audit.ClientIP(r)
	if err := s.bot.Disconnect(); err != nil {
		s.audit.Record("onboarding", "error", "admin", "whatsapp-session", "disconnect", "failure", ip, "WhatsApp disconnect failed", nil)
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit.Record("onboarding", "warning", "admin", "whatsapp-session", "disconnect", "success", ip, "WhatsApp disconnected from dashboard", nil)
	s.json(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handleUnlink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ip := audit.ClientIP(r)
	if err := s.bot.Unlink(); err != nil {
		s.audit.Record("onboarding", "error", "admin", "whatsapp-session", "unlink", "failure", ip, "WhatsApp unlink failed", nil)
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit.Record("onboarding", "warning", "admin", "whatsapp-session", "unlink", "success", ip, "WhatsApp account unlinked from dashboard", nil)
	s.json(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp := settingsResponse{
			BotName:     s.getStr("bot_name", "Sonicos"),
			Prefix:      s.getStr("prefix", "!"),
			Description: s.getStr("description", ""),
			Language:    s.getStr("language", "en"),
			Timezone:    s.getStr("timezone", "UTC"),
			AutoStart:   s.getBool("auto_start", true),
			Maintenance: s.getBool("maintenance", false),
		}
		s.json(w, http.StatusOK, resp)
	case http.MethodPut:
		var req settingsRequest
		if err := s.readJSON(w, r, &req); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		s.mu.Lock()
		if req.BotName != "" {
			s.settings["bot_name"] = req.BotName
		}
		if req.Description != "" {
			s.settings["description"] = req.Description
		}
		if req.Language != "" {
			s.settings["language"] = req.Language
		}
		if req.Timezone != "" {
			s.settings["timezone"] = req.Timezone
		}
		s.settings["auto_start"] = req.AutoStart
		s.settings["maintenance"] = req.Maintenance
		s.mu.Unlock()
		s.broadcast("config", map[string]string{"action": "settings_updated"})
		s.audit.Record("administrative", "info", "admin", "bot-settings", "settings_updated", "success", audit.ClientIP(r), "dashboard settings updated", map[string]any{"maintenance": req.Maintenance})
		s.broadcast("activity", map[string]string{"type": "administrative"})
		s.json(w, http.StatusOK, map[string]string{"ok": "true"})
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleChangePrefix(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req prefixRequest
	if err := s.readJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Prefix == "" || len(req.Prefix) > 3 || strings.Contains(req.Prefix, " ") {
		s.writeError(w, http.StatusBadRequest, "prefix must be 1-3 characters with no spaces")
		return
	}
	s.mu.Lock()
	s.settings["prefix"] = req.Prefix
	s.mu.Unlock()
	s.broadcast("config", map[string]string{"action": "prefix_changed", "prefix": req.Prefix})
	s.audit.Record("administrative", "info", "admin", "bot-settings", "prefix_changed", "success", audit.ClientIP(r), "bot prefix changed to "+req.Prefix, map[string]any{"prefix": req.Prefix})
	s.json(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.registry == nil {
		s.json(w, http.StatusOK, []commandItem{})
		return
	}
	cmds := s.registry.Commands()
	out := make([]commandItem, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, commandItem{
			Name:        c.Name,
			Aliases:     c.Aliases,
			Category:    c.Category,
			Description: c.Description,
			Enabled:     true,
			Permission:  "everyone",
		})
	}
	s.json(w, http.StatusOK, out)
}

func (s *Server) handleCommandPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/commands/")
	if name == "" {
		s.writeError(w, http.StatusBadRequest, "command name required")
		return
	}
	s.audit.Record("administrative", "info", "admin", "command:"+name, "command_updated", "success", audit.ClientIP(r), "command configuration updated", nil)
	s.json(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	groups := s.bot.ListGroups()
	out := make([]groupResponse, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupResponse{
			ID:                g.JID,
			Name:              g.Name,
			Members:           len(g.Participants),
			BotEnabled:        true,
			WelcomeEnabled:    false,
			ModerationEnabled: false,
			AdminOnlyCommands: false,
		})
	}
	s.json(w, http.StatusOK, out)
}

func (s *Server) handleGroup(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/groups/")
	if id == "" || id == "/" {
		s.writeError(w, http.StatusBadRequest, "group id required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		g := s.bot.GetGroup(id)
		if g == nil {
			s.writeError(w, http.StatusNotFound, "group not found")
			return
		}
		resp := groupResponse{
			ID:                g.JID,
			Name:              g.Name,
			Members:           len(g.Participants),
			BotEnabled:        true,
			WelcomeEnabled:    false,
			ModerationEnabled: false,
			AdminOnlyCommands: false,
		}
		s.json(w, http.StatusOK, resp)
	case http.MethodPatch:
		var req groupPatchRequest
		if err := s.readJSON(w, r, &req); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		s.broadcast("group", map[string]string{"id": id, "action": "updated"})
		s.addModEvent("dashboard", "group_updated", "group "+id)
		s.audit.Record("administrative", "info", "admin", "group:"+id, "group_updated", "success", audit.ClientIP(r), "group settings updated", nil)
		s.json(w, http.StatusOK, map[string]string{"ok": "true"})
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleModSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		resp := moderationSettings{
			Enabled:            s.getBool("mod_enabled", true),
			DryRun:             s.getBool("mod_dry_run", true),
			AutoDelete:         s.getBool("mod_auto_delete", false),
			RemovalEnabled:     s.getBool("mod_removal_enabled", true),
			WarningThreshold:   s.getInt("mod_warning_threshold", 3),
			RemovalViolation:   s.getInt("mod_removal_violation", 4),
			HighConfidence:     s.getFloat("mod_high_confidence", 0.9),
			PossibleConfidence: s.getFloat("mod_possible_confidence", 0.6),
		}
		s.json(w, http.StatusOK, resp)
	case http.MethodPut:
		var req moderationSettings
		if err := s.readJSON(w, r, &req); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		s.mu.Lock()
		s.settings["mod_enabled"] = req.Enabled
		s.settings["mod_dry_run"] = req.DryRun
		s.settings["mod_auto_delete"] = req.AutoDelete
		s.settings["mod_removal_enabled"] = req.RemovalEnabled
		s.settings["mod_warning_threshold"] = req.WarningThreshold
		s.settings["mod_removal_violation"] = req.RemovalViolation
		s.settings["mod_high_confidence"] = req.HighConfidence
		s.settings["mod_possible_confidence"] = req.PossibleConfidence
		s.mu.Unlock()
		s.broadcast("moderation", map[string]string{"action": "settings_updated"})
		s.audit.Record("administrative", "info", "admin", "moderation-settings", "moderation_settings_updated", "success", audit.ClientIP(r), "moderation settings updated", map[string]any{"enabled": req.Enabled, "dry_run": req.DryRun})
		s.json(w, http.StatusOK, map[string]string{"ok": "true"})
	default:
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleModEnrolled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.modStore == nil {
		s.json(w, http.StatusOK, []enrolledItem{})
		return
	}
	groups := s.bot.ListGroups()
	ctx := r.Context()
	seen := make(map[string]struct{})
	var items []enrolledItem
	for _, g := range groups {
		entries, err := s.modStore.ListBannedStickers(ctx, g.JID)
		if err != nil {
			continue
		}
		for _, e := range entries {
			key := e.FileSHA256 + "|" + e.AudioSHA256
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			items = append(items, enrolledItem{
				ID:         key,
				Label:      e.Reason,
				Type:       "sticker",
				EnrolledAt: time.Unix(e.AddedAt, 0).UTC().Format(time.RFC3339),
				EnrolledBy: e.AddedBy,
			})
		}
	}
	s.json(w, http.StatusOK, items)
}

func (s *Server) handleModEnrolledItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/moderation/enrolled/")
	if id == "" {
		s.writeError(w, http.StatusBadRequest, "id required")
		return
	}
	s.json(w, http.StatusOK, nil)
}

func (s *Server) handleModMatches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.modMu.Lock()
	items := make([]matchItem, 0, len(s.modEvents))
	for _, e := range s.modEvents {
		items = append(items, matchItem{
			ID:         e.TS,
			Tier:       "high",
			TS:         e.TS,
			User:       e.User,
			Type:       e.Action,
			Confidence: 1.0,
			Status:     "reviewed",
		})
	}
	s.modMu.Unlock()
	s.json(w, http.StatusOK, items)
}

func (s *Server) handleModReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/moderation/matches/")
	id = strings.TrimSuffix(id, "/review")
	if id == "" {
		s.writeError(w, http.StatusBadRequest, "id required")
		return
	}
	var req matchReviewRequest
	if err := s.readJSON(w, r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	s.broadcast("moderation", map[string]string{"id": id, "action": "review", "decision": req.Decision})
	s.addModEvent("dashboard", "review_"+req.Decision, "match "+id)
	s.json(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handleModHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.modMu.Lock()
	rows := make([]modHistoryItem, len(s.modEvents))
	copy(rows, s.modEvents)
	s.modMu.Unlock()
	anyRows := make([]any, len(rows))
	for i := range rows {
		anyRows[i] = rows[i]
	}
	s.json(w, http.StatusOK, pagedResponse{Items: anyRows, Total: len(rows)})
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	users := s.bot.ListUsers()
	ctx := r.Context()
	items := make([]userItem, 0, len(users))
	for _, u := range users {
		warnings := 0
		if s.modStore != nil {
			for _, g := range u.Groups {
				n, err := s.modStore.GetStrikes(ctx, g, u.JID)
				if err == nil && n > warnings {
					warnings = n
				}
			}
		}
		items = append(items, userItem{
			JID:       u.JID,
			Name:      u.Name,
			Display:   u.Name,
			Group:     strings.Join(u.Groups, ","),
			GroupName: strings.Join(u.GroupNames, ","),
			IsAdmin:   u.IsAdmin,
			Warnings:  warnings,
		})
	}
	anyItems := make([]any, len(items))
	for i := range items {
		anyItems[i] = items[i]
	}
	s.json(w, http.StatusOK, pagedResponse{Items: anyItems, Total: len(items)})
}

func (s *Server) handleUserRoutes(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/users/")
	parts := strings.Split(path, "/")
	if len(parts) < 1 || parts[0] == "" {
		s.writeError(w, http.StatusNotFound, "not found")
		return
	}
	jid := parts[0]
	if len(parts) >= 2 && parts[1] == "history" && r.Method == http.MethodGet {
		s.handleUserHistoryWithJID(w, r, jid)
		return
	}
	if len(parts) >= 3 && parts[1] == "warnings" && parts[2] == "reset" && r.Method == http.MethodPost {
		s.handleUserResetWithJID(w, r, jid)
		return
	}
	s.writeError(w, http.StatusNotFound, "not found")
}

func (s *Server) handleUserHistoryWithJID(w http.ResponseWriter, r *http.Request, jid string) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.json(w, http.StatusOK, []userHistoryItem{})
}

func (s *Server) handleUserResetWithJID(w http.ResponseWriter, r *http.Request, jid string) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.audit.Record("administrative", "info", "admin", "user:"+jid, "warnings_reset", "success", audit.ClientIP(r), "warnings reset via dashboard", nil)
	s.json(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))

	items, total := s.audit.List(q.Get("q"), q.Get("level"), q.Get("type"), page, pageSize)
	anyItems := make([]any, len(items))
	for i := range items {
		anyItems[i] = items[i]
	}
	s.json(w, http.StatusOK, pagedResponse{Items: anyItems, Total: total})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan []byte, 16)
	s.addClient(ch)
	defer s.removeClient(ch)

	statusPayload, _ := json.Marshal(s.buildStatus())
	_, _ = fmt.Fprintf(w, "event: status\ndata: %s\n\n", string(statusPayload))

	if snap, err := s.bot.QRCode(); err == nil && snap.Code != "" {
		qrPayload, _ := json.Marshal(buildQRResponse(snap))
		_, _ = fmt.Fprintf(w, "event: qr\ndata: %s\n\n", string(qrPayload))
	}
	flusher.Flush()

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			_, _ = w.Write(msg)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
		}
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data: blob:; connect-src 'self' https: http:")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(static.IndexHTML)
	}
}

func (s *Server) getStr(key, def string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.settings[key]; ok {
		if str, ok := v.(string); ok {
			return str
		}
	}
	return def
}

func (s *Server) getBool(key string, def bool) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.settings[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func (s *Server) getInt(key string, def int) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.settings[key]; ok {
		if i, ok := v.(int); ok {
			return i
		}
	}
	return def
}

func (s *Server) getFloat(key string, def float64) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.settings[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return def
}

func (s *Server) getStat(key string, def int64) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.settings[key]; ok {
		if i, ok := v.(int64); ok {
			return i
		}
	}
	return def
}

func (s *Server) addModEvent(user, action, detail string) {
	s.modMu.Lock()
	event := modHistoryItem{
		TS:     time.Now().UTC().Format(time.RFC3339),
		User:   user,
		Action: action,
		Detail: detail,
	}
	s.modEvents = append(s.modEvents, event)
	if len(s.modEvents) > s.maxModEvents {
		s.modEvents = s.modEvents[len(s.modEvents)-s.maxModEvents:]
	}
	s.modMu.Unlock()
	s.audit.Record("administrative", "info", user, "moderation", action, "success", "", "moderation: "+action+" - "+detail, nil)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", s.handleHealth)

	mux.HandleFunc("/status", s.corsAuth(s.handleStatus))
	mux.HandleFunc("/events", s.corsAuth(s.handleEvents))
	mux.HandleFunc("/qr", s.corsAuth(s.handleQR))
	mux.HandleFunc("/connection/qr/refresh", s.corsAuth(s.handleQRRefresh))
	mux.HandleFunc("/pair", s.corsAuth(s.handlePair))
	mux.HandleFunc("/connection/reconnect", s.corsAuth(s.handleReconnect))
	mux.HandleFunc("/connection/disconnect", s.corsAuth(s.handleDisconnect))
	mux.HandleFunc("/connection/unlink", s.corsAuth(s.handleUnlink))
	mux.HandleFunc("/settings", s.corsAuth(s.handleSettings))
	mux.HandleFunc("/commands/prefix", s.corsAuth(s.handleChangePrefix))
	mux.HandleFunc("/commands", s.corsAuth(s.handleCommands))
	mux.HandleFunc("/commands/", s.corsAuth(s.handleCommandPatch))
	mux.HandleFunc("/groups", s.corsAuth(s.handleGroups))
	mux.HandleFunc("/groups/", s.corsAuth(s.handleGroup))
	mux.HandleFunc("/moderation/settings", s.corsAuth(s.handleModSettings))
	mux.HandleFunc("/moderation/enrolled", s.corsAuth(s.handleModEnrolled))
	mux.HandleFunc("/moderation/enrolled/", s.corsAuth(s.handleModEnrolledItem))
	mux.HandleFunc("/moderation/matches", s.corsAuth(s.handleModMatches))
	mux.HandleFunc("/moderation/matches/", s.corsAuth(s.handleModReview))
	mux.HandleFunc("/moderation/history", s.corsAuth(s.handleModHistory))
	mux.HandleFunc("/users", s.corsAuth(s.handleUsers))
	mux.HandleFunc("/users/", s.corsAuth(s.handleUserRoutes))
	mux.HandleFunc("/logs", s.corsAuth(s.handleLogs))

	mux.HandleFunc("/auth/login", s.cors(s.handleLogin))
	mux.HandleFunc("/auth/logout", s.cors(s.handleLogout))
	mux.HandleFunc("/auth/me", s.cors(s.handleMe))

	mux.HandleFunc("/", s.cors(s.handleIndex))

	return mux
}
