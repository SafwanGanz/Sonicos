package whatsapp

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/store/sqlstore"
)

func OpenStore(ctx context.Context, driver, address string) (*sqlstore.Container, error) {
	container, _, err := OpenStoreWithDB(ctx, driver, address)
	return container, err
}

func OpenStoreWithDB(ctx context.Context, driver, address string) (*sqlstore.Container, *sql.DB, error) {
	if driver == "sqlite3" && strings.HasPrefix(address, "file:") {
		path := strings.TrimPrefix(strings.SplitN(address, "?", 2)[0], "file:")
		if path != ":memory:" && path != "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return nil, nil, fmt.Errorf("create database directory: %w", err)
			}
		}
	}

	db, err := sql.Open(driver, address)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s database: %w", driver, err)
	}

	switch driver {
	case "sqlite3":

		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	case "pgx":
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(30 * time.Minute)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("connect to %s database: %w", driver, err)
	}

	if driver == "sqlite3" {
		pragmaCtx, pragmaCancel := context.WithTimeout(ctx, 5*time.Second)
		_, _ = db.ExecContext(pragmaCtx, "PRAGMA foreign_keys = ON; PRAGMA journal_mode = WAL; PRAGMA synchronous = NORMAL; PRAGMA busy_timeout = 5000;")
		pragmaCancel()
	}

	container := sqlstore.NewWithDB(db, driver, nil)
	if err := container.Upgrade(ctx); err != nil {
		_ = container.Close()
		return nil, nil, fmt.Errorf("upgrade whatsmeow store: %w", err)
	}
	if driver == "pgx" && container.LIDMap != nil {
		lidCtx, lidCancel := context.WithTimeout(ctx, 10*time.Second)
		_ = container.LIDMap.FillCache(lidCtx)
		lidCancel()
	}
	return container, db, nil
}
