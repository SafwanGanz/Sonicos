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

	"github.com/SafwanGanz/Sonicos/internal/api"
	"github.com/SafwanGanz/Sonicos/internal/application"
	"github.com/SafwanGanz/Sonicos/internal/audit"
	"github.com/SafwanGanz/Sonicos/internal/command"
	"github.com/SafwanGanz/Sonicos/internal/config"
	"github.com/SafwanGanz/Sonicos/internal/idempotency"
	"github.com/SafwanGanz/Sonicos/internal/moderation"
	_ "github.com/SafwanGanz/Sonicos/internal/plugins"
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
