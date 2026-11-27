package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/SafwanGanz/Sonicos/internal/ai"
	"github.com/SafwanGanz/Sonicos/internal/aimode"
	"github.com/SafwanGanz/Sonicos/internal/api"
	"github.com/SafwanGanz/Sonicos/internal/application"
	"github.com/SafwanGanz/Sonicos/internal/audit"
	"github.com/SafwanGanz/Sonicos/internal/command"
	"github.com/SafwanGanz/Sonicos/internal/config"
	"github.com/SafwanGanz/Sonicos/internal/gemini"
	"github.com/SafwanGanz/Sonicos/internal/idempotency"
	"github.com/SafwanGanz/Sonicos/internal/moderation"
	"github.com/SafwanGanz/Sonicos/internal/owner"
	_ "github.com/SafwanGanz/Sonicos/internal/plugins"
	"github.com/SafwanGanz/Sonicos/internal/speech"
	"github.com/SafwanGanz/Sonicos/internal/transcript"
	"github.com/SafwanGanz/Sonicos/internal/whatsapp"
)

func main() {
	if err := run(); err != nil {
		slog.Error("bot stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))
	slog.SetDefault(logger)

	owner.Set(cfg.OwnerJIDs)

	auditLog := audit.New(logger, 1000)

	registry, err := command.Default()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sessionStore, db, err := whatsapp.OpenStoreWithDB(ctx, cfg.DatabaseDriver, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer sessionStore.Close()

	var deduper application.Idempotency
	if cfg.RedisURL != "" {
		redisCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		deduper, err = idempotency.New(redisCtx, cfg.RedisURL)
		cancel()
		if err != nil {
			logger.Warn("redis unavailable; falling back to SQL feature store and in-memory idempotency", "error", err)
			deduper = idempotency.NewMemory()
		}
	} else {
		logger.Info("cloud environment without REDIS_URL; using SQL feature store and in-memory idempotency", "platform", cfg.CloudPlatform)
		deduper = idempotency.NewMemory()
	}
	defer deduper.Close()

	processor, err := application.NewProcessor(registry, deduper, cfg.Prefix, 30*time.Second, logger)
	if err != nil {
		return err
	}

	bot, err := whatsapp.NewBot(sessionStore, processor, cfg.Workers, cfg.QueueSize, logger, cfg.AutoApproveRequests, cfg.BlockedNumbers)
	if err != nil {
		return err
	}
	if cfg.DatabaseDriver == "pgx" {
		bot.EnableFastStore(ctx, db)
	}

	var modStore command.ModerationStore
	modBackend := "redis"
	if redisStore, ok := deduper.(*idempotency.Store); ok {
		modStore = moderation.NewRedisStore(redisStore.Client())
	} else {
		sqlModStore, err := moderation.NewSQLStore(ctx, db, cfg.DatabaseDriver)
		if err != nil {
			return err
		}
		modStore = sqlModStore
		modBackend = cfg.DatabaseDriver
	}
	bot.SetModerationStore(modStore)

	var aiModeStore command.AIModeStore
	if redisStore, ok := deduper.(*idempotency.Store); ok {
		aiModeStore = aimode.NewRedisStore(redisStore.Client())
	} else {
		aiSQLStore, err := aimode.NewSQLStore(ctx, db, cfg.DatabaseDriver)
		if err != nil {
			return err
		}
		aiModeStore = aiSQLStore
	}
	var textProvider ai.TextProvider
	var web2apiClient *ai.Client
	if cfg.GeminiWeb2APIEnabled {

		web2apiClient = ai.NewClient(ai.Config{
			APIKey:         cfg.GeminiWeb2APIKey,
			BaseURL:        cfg.GeminiWeb2APIBaseURL,
			Model:          cfg.GeminiWeb2APIModel,
			Timeout:        cfg.GeminiWeb2APITimeout,
			MaxRetries:     cfg.GeminiWeb2APIMaxRetries,
			AllowAnonymous: true,
		})
		if web2apiClient != nil {
			textProvider = web2apiClient
		}
	}
	if textProvider == nil && cfg.AIAPIKey != "" {
		textProvider = ai.NewClient(ai.Config{
			APIKey:     cfg.AIAPIKey,
			BaseURL:    cfg.AIBaseURL,
			Model:      cfg.AIModel,
			Timeout:    90 * time.Second,
			MaxRetries: 2,
		})
	}

	geminiSTT := gemini.NewProvider(gemini.Config{
		APIKey:        cfg.GeminiAPIKey,
		Model:         cfg.GeminiSTTModel,
		FallbackModel: cfg.GeminiSTTFallbackModel,
		MaxRetries:    2,
	})
	var sttProviders []transcript.Provider
	if geminiSTT != nil {
		sttProviders = append(sttProviders, geminiSTT)
	}
	if cfg.AIAPIKey != "" {
		sttClient := ai.NewClient(ai.Config{APIKey: cfg.AIAPIKey, BaseURL: cfg.AIBaseURL, Timeout: 90 * time.Second, MaxRetries: 1})
		if openAISTT := ai.NewOpenAITranscription(sttClient, cfg.AISTTModel); openAISTT != nil {
			sttProviders = append(sttProviders, openAISTT)
		}
	}
	sttEngine := transcript.NewEngine(logger, sttProviders...)
	ttsProvider := speech.NewEdgeTTS(cfg.EdgeTTSVoice)

	bot.SetAIMode(aiModeStore, textProvider, sttEngine, ttsProvider, registry, logger)
	logger.Info("ai driven mode initialized (default off until owner enables)",
		"text_backend", providerName(textProvider),
		"stt_providers", sttEngine.Providers(),
		"tts_provider", ttsName(ttsProvider),
		"owners_configured", len(cfg.OwnerJIDs),
	)
	go runAIDiagnostics(ctx, web2apiClient, cfg, geminiSTT, sttEngine, ttsProvider, logger)

	persistent := !cfg.CloudEphemeral
	if cfg.CloudEphemeral {
		logger.Warn("running in volume-less ephemeral cloud mode; WhatsApp session and SQL feature store will reset on redeploy unless a PostgreSQL/Supabase DATABASE_URL is configured", "platform", cfg.CloudPlatform, "database_driver", cfg.DatabaseDriver, "feature_store", modBackend)
	}
	auditLog.Record("system", "info", "system", "storage", "storage_initialized", "success", "", "storage backends initialized", map[string]any{
		"platform":        cfg.CloudPlatform,
		"database_driver": cfg.DatabaseDriver,
		"feature_store":   modBackend,
		"persistent":      persistent,
	})

	var srv *http.Server
	switch {
	case cfg.DashboardPassword != "":
		apiServer := api.NewServer(bot, registry, modStore, auditLog, cfg.DashboardPassword, cfg.CORSAllowedOrigins, logger)
		apiServer.SetStorageInfo(cfg.DatabaseDriver, modBackend, persistent, cfg.CloudPlatform)
		bot.SetEventListener(apiServer.HandleBotEvent)
		srv = &http.Server{
			Addr:              ":" + cfg.Port,
			Handler:           apiServer.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		auditLog.Record("system", "info", "system", "web-dashboard", "dashboard_started", "success", "", "web onboarding dashboard enabled", map[string]any{"port": cfg.Port})
	case cfg.PortExplicit:
		logger.Warn("web dashboard disabled (DASHBOARD_PASSWORD not set); binding PORT with locked health server for cloud runtime", "port", cfg.Port)
		auditLog.Record("system", "warning", "system", "web-dashboard", "dashboard_disabled_cloud_port", "success", "", "DASHBOARD_PASSWORD empty; locked health endpoint bound on PORT", map[string]any{"port": cfg.Port})
		srv = &http.Server{
			Addr:              ":" + cfg.Port,
			Handler:           api.NewDisabledHandler(auditLog),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
	default:
		logger.Info("web dashboard disabled (DASHBOARD_PASSWORD not set); terminal pairing active")
		auditLog.Record("system", "info", "system", "web-dashboard", "dashboard_disabled", "success", "", "web dashboard disabled; terminal pairing mode active", nil)
	}

	if srv != nil {
		go func() {
			logger.Info("HTTP server starting", "port", cfg.Port)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("HTTP server failed", "error", err)
			}
		}()

		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
	}

	logger.Info("starting WhatsApp bot", "workers", cfg.Workers, "queue_size", cfg.QueueSize, "prefix", cfg.Prefix)
	return bot.Run(ctx)
}

func runAIDiagnostics(ctx context.Context, web2api *ai.Client, cfg config.Config, geminiSTT *gemini.Provider, stt *transcript.Engine, tts speech.Synthesizer, logger *slog.Logger) {
	diagCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()

	if web2api != nil {
		model, err := web2api.ResolveModel(diagCtx, cfg.GeminiWeb2APIModel, cfg.GeminiWeb2APIFallback)
		if err != nil {
			logger.Warn("gemini-web2api model validation failed", "base_url", cfg.GeminiWeb2APIBaseURL, "error", err)
		} else {
			logger.Info("gemini-web2api model ready", "base_url", cfg.GeminiWeb2APIBaseURL, "model", model)
			probeCtx, probeCancel := context.WithTimeout(diagCtx, 30*time.Second)
			defer probeCancel()
			res, err := web2api.Chat(probeCtx, ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: "Reply with the single word: pong"}}})
			if err != nil {
				logger.Warn("gemini-web2api chat probe failed", "error", err)
			} else {
				logger.Info("gemini-web2api chat probe succeeded", "reply", strings.TrimSpace(res.Text))
			}
		}
	} else {
		logger.Info("gemini-web2api disabled (set GEMINI_WEB2API_ENABLED=true to use the local server)")
	}

	if geminiSTT != nil {
		status, err := geminiSTT.HealthCheck(diagCtx)
		if err != nil {
			logger.Warn("gemini speech-to-text unavailable", "status", status, "error", err)
		} else {
			logger.Info("gemini speech-to-text ready", "status", status)
		}
	} else {
		logger.Info("gemini speech-to-text not configured (set GEMINI_API_KEY for free-tier voice transcription)")
	}
	if len(stt.Providers()) == 0 {
		logger.Warn("no transcription provider available; voice messages cannot be transcribed")
	}
	if ttsName(tts) == "" {
		logger.Info("no speech synthesis provider available; AI replies will be text only")
	}
}

func providerName(p ai.TextProvider) string {
	if p == nil || !p.Available() {
		return "none"
	}
	return p.Name()
}

func ttsName(s speech.Synthesizer) string {
	if s == nil || !s.Available() {
		return ""
	}
	return s.Name()
}

func logLevel(value string) slog.Level {
	switch strings.ToLower(value) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
