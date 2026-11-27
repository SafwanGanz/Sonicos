package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseDriver      string
	DatabaseURL         string
	RedisURL            string
	RedisConfigured     bool
	Prefix              string
	Workers             int
	QueueSize           int
	LogLevel            string
	DashboardPassword   string
	Port                string
	PortExplicit        bool
	CORSAllowedOrigins  []string
	CloudPlatform       string
	HasPersistentVolume bool
	CloudEphemeral      bool
	AutoApproveRequests bool
	BlockedNumbers      []string
	OwnerJIDs           []string
	AIAPIKey            string
	AIBaseURL           string
	AIModel             string
	AISTTModel          string

	GeminiWeb2APIEnabled    bool
	GeminiWeb2APIBaseURL    string
	GeminiWeb2APIKey        string
	GeminiWeb2APIModel      string
	GeminiWeb2APIFallback   string
	GeminiWeb2APITimeout    time.Duration
	GeminiWeb2APIMaxRetries int
	GeminiAPIKey            string
	GeminiSTTModel          string
	GeminiSTTFallbackModel  string
	EdgeTTSVoice            string
}

func Load() (Config, error) {
	rawPort := strings.TrimSpace(os.Getenv("PORT"))
	port := rawPort
	if port == "" {
		port = "8080"
	} else {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return Config{}, fmt.Errorf("PORT must be an integer between 1 and 65535")
		}
	}

	platform, hasVolume := detectCloudPlatform()
	dbURL := env("DATABASE_URL", "file:./data/sonicos.db?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000")
	dbDriver := normalizeDriver(env("DATABASE_DRIVER", "sqlite3"), dbURL)
	if dbDriver == "pgx" {
		normalized, err := normalizePostgresURL(dbURL)
		if err != nil {
			return Config{}, fmt.Errorf("invalid Postgres DATABASE_URL: %w", err)
		}
		dbURL = normalized
	}

	rawRedis := strings.TrimSpace(os.Getenv("REDIS_URL"))
	redisURL := rawRedis
	redisConfigured := rawRedis != ""
	if redisURL == "" && platform == "" {

		redisURL = "redis://localhost:6379/0"
		redisConfigured = true
	}

	cfg := Config{
		DatabaseDriver:      dbDriver,
		DatabaseURL:         dbURL,
		RedisURL:            redisURL,
		RedisConfigured:     redisConfigured,
		Prefix:              env("BOT_PREFIX", "!"),
		LogLevel:            strings.ToLower(env("LOG_LEVEL", "info")),
		DashboardPassword:   strings.TrimSpace(os.Getenv("DASHBOARD_PASSWORD")),
		Port:                port,
		PortExplicit:        rawPort != "",
		CORSAllowedOrigins:  splitCSV(os.Getenv("CORS_ALLOWED_ORIGINS")),
		CloudPlatform:       platform,
		HasPersistentVolume: hasVolume,
		CloudEphemeral:      platform != "" && !hasVolume && dbDriver == "sqlite3",
		AutoApproveRequests: envBool("AUTO_APPROVE_REQUESTS", false),
		BlockedNumbers:      splitCSV(os.Getenv("BLOCKED_NUMBERS")),
		OwnerJIDs:           mergeCSV(splitCSV(os.Getenv("BOT_OWNER_JIDS")), splitCSV(os.Getenv("BOT_OWNER"))),
		AIAPIKey:            strings.TrimSpace(os.Getenv("AI_API_KEY")),
		AIBaseURL:           env("AI_BASE_URL", "https://api.openai.com/v1"),
		AIModel:             env("AI_MODEL", "gpt-4o-mini"),
		AISTTModel:          env("AI_STT_MODEL", "whisper-1"),

		GeminiWeb2APIEnabled:   envBool("GEMINI_WEB2API_ENABLED", false),
		GeminiWeb2APIBaseURL:   env("GEMINI_WEB2API_BASE_URL", "http://127.0.0.1:8081/v1"),
		GeminiWeb2APIKey:       strings.TrimSpace(os.Getenv("GEMINI_WEB2API_API_KEY")),
		GeminiWeb2APIModel:     env("GEMINI_WEB2API_MODEL", ""),
		GeminiWeb2APIFallback:  env("GEMINI_WEB2API_FALLBACK_MODEL", "gemini-3.5-flash"),
		GeminiAPIKey:           strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
		GeminiSTTModel:         env("GEMINI_STT_MODEL", "gemini-2.5-flash"),
		GeminiSTTFallbackModel: env("GEMINI_STT_FALLBACK_MODEL", "gemini-flash-lite"),
		EdgeTTSVoice:           env("EDGE_TTS_VOICE", "en-US-AriaNeural"),
	}

	var err error
	cfg.GeminiWeb2APITimeout, err = envDuration("GEMINI_WEB2API_TIMEOUT", 180*time.Second)
	if err != nil {
		return Config{}, err
	}
	cfg.GeminiWeb2APIMaxRetries, err = envInt("GEMINI_WEB2API_MAX_RETRIES", 2)
	if err != nil {
		return Config{}, err
	}
	if cfg.GeminiWeb2APIMaxRetries < 0 || cfg.GeminiWeb2APIMaxRetries > 10 {
		return Config{}, fmt.Errorf("GEMINI_WEB2API_MAX_RETRIES must be between 0 and 10")
	}
	cfg.Workers, err = envInt("WORKERS", 8)
	if err != nil {
		return Config{}, err
	}
	cfg.QueueSize, err = envInt("QUEUE_SIZE", 512)
	if err != nil {
		return Config{}, err
	}
	if cfg.Workers < 1 || cfg.Workers > 256 {
		return Config{}, fmt.Errorf("WORKERS must be between 1 and 256")
	}
	if cfg.QueueSize < cfg.Workers {
		return Config{}, fmt.Errorf("QUEUE_SIZE must be at least WORKERS")
	}
	if cfg.Prefix == "" {
		return Config{}, fmt.Errorf("BOT_PREFIX cannot be empty")
	}
	if cfg.DatabaseDriver != "sqlite3" && cfg.DatabaseDriver != "pgx" {
		return Config{}, fmt.Errorf("DATABASE_DRIVER must be sqlite3 or pgx")
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL cannot be empty")
	}
	return cfg, nil
}

func detectCloudPlatform() (platform string, hasVolume bool) {
	if envSet("RAILWAY_ENVIRONMENT") || envSet("RAILWAY_PROJECT_ID") || envSet("RAILWAY_SERVICE_ID") {
		return "railway", envSet("RAILWAY_VOLUME_MOUNT_PATH")
	}
	if envSet("DYNO") || envSet("HEROKU_APP_ID") || envSet("HEROKU_APP_NAME") {
		return "heroku", false
	}
	return "", true
}

func envSet(key string) bool {
	return strings.TrimSpace(os.Getenv(key)) != ""
}

func normalizeDriver(driver, dbURL string) string {
	lowerURL := strings.ToLower(strings.TrimSpace(dbURL))
	if strings.HasPrefix(lowerURL, "postgres://") || strings.HasPrefix(lowerURL, "postgresql://") {
		return "pgx"
	}
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "postgres", "postgresql", "pgx":
		return "pgx"
	default:
		return strings.ToLower(strings.TrimSpace(driver))
	}
}

func normalizePostgresURL(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || (u.Host == "" && strings.Contains(trimmed, "@")) || strings.Contains(u.Fragment, "@") {
		if repaired, ok := repairUnencodedPasswordURI(trimmed); ok {
			if u2, err2 := url.Parse(repaired); err2 == nil {
				u, err = u2, nil
			}
		}
	}
	if err != nil {
		return "", err
	}
	if u.Scheme == "" {
		return trimmed, nil
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	q := u.Query()

	if host != "" && !isLocalHost(host) && !q.Has("sslmode") {
		q.Set("sslmode", "require")
	}

	if (strings.Contains(host, "pooler.supabase.com") || port == "6543") && !q.Has("default_query_exec_mode") {
		q.Set("default_query_exec_mode", "simple_protocol")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func repairUnencodedPasswordURI(raw string) (string, bool) {
	schemeIdx := strings.Index(raw, "://")
	if schemeIdx <= 0 {
		return "", false
	}
	scheme := raw[:schemeIdx]
	rest := raw[schemeIdx+3:]
	atIdx := strings.LastIndex(rest, "@")
	if atIdx <= 0 || atIdx == len(rest)-1 {
		return "", false
	}
	userinfo := rest[:atIdx]
	hostAndPath := rest[atIdx+1:]
	user, pass, hasPass := strings.Cut(userinfo, ":")
	if !hasPass {
		return "", false
	}
	return scheme + "://" + url.UserPassword(user, pass).String() + "@" + hostAndPath, true
}

func isLocalHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "postgres" || host == "db"
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return parsed, nil
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return strings.ToLower(value) == "true" || value == "1" || strings.ToLower(value) == "yes"
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration like 180s or 3m: %w", key, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return parsed, nil
}

func mergeCSV(lists ...[]string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, list := range lists {
		for _, v := range list {
			key := strings.ToLower(strings.TrimSpace(v))
			if key == "" {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, strings.TrimRight(trimmed, "/"))
		}
	}
	return out
}
