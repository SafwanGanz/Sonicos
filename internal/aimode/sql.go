package aimode

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type SQLStore struct {
	db         *sql.DB
	isPostgres bool
}

func NewSQLStore(ctx context.Context, db *sql.DB, driver string) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("sql database handle is required")
	}
	lower := strings.ToLower(strings.TrimSpace(driver))
	store := &SQLStore{db: db, isPostgres: lower == "pgx" || strings.HasPrefix(lower, "postgres")}
	_, err := db.ExecContext(ctx, store.rebind(`
		CREATE TABLE IF NOT EXISTS sonicos_ai_mode (
			id INTEGER PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 0,
			owner TEXT NOT NULL DEFAULT '',
			updated_at BIGINT NOT NULL DEFAULT 0
		)
	`))
	if err != nil {
		return nil, fmt.Errorf("create sonicos_ai_mode: %w", err)
	}
	_, err = db.ExecContext(ctx, store.rebind(`
		CREATE TABLE IF NOT EXISTS sonicos_ai_group_mode (
			group_jid TEXT PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 0,
			updated_at BIGINT NOT NULL DEFAULT 0
		)
	`))
	if err != nil {
		return nil, fmt.Errorf("create sonicos_ai_group_mode: %w", err)
	}
	return store, nil
}

func (s *SQLStore) GetState(ctx context.Context) (State, error) {
	var enabled int
	var owner string
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT enabled, owner, updated_at FROM sonicos_ai_mode WHERE id = 1
	`)).Scan(&enabled, &owner, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {

		return State{Enabled: false}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("get ai mode: %w", err)
	}
	return State{Enabled: enabled == 1, Owner: owner, UpdatedAt: updatedAt}, nil
}

func (s *SQLStore) SetState(ctx context.Context, enabled bool, owner string) error {
	val := 0
	if enabled {
		val = 1
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, s.rebind(`
		INSERT INTO sonicos_ai_mode (id, enabled, owner, updated_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET enabled = excluded.enabled, owner = excluded.owner, updated_at = excluded.updated_at
	`), val, owner, now)
	if err != nil {
		return fmt.Errorf("set ai mode: %w", err)
	}
	return nil
}

func (s *SQLStore) GetGroupEnabled(ctx context.Context, groupJID string) (bool, error) {
	var enabled int
	err := s.db.QueryRowContext(ctx, s.rebind(`
		SELECT enabled FROM sonicos_ai_group_mode WHERE group_jid = ?
	`), groupJID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {

		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get ai group mode: %w", err)
	}
	return enabled == 1, nil
}

func (s *SQLStore) SetGroupEnabled(ctx context.Context, groupJID string, enabled bool) error {
	val := 0
	if enabled {
		val = 1
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, s.rebind(`
		INSERT INTO sonicos_ai_group_mode (group_jid, enabled, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (group_jid) DO UPDATE SET enabled = excluded.enabled, updated_at = excluded.updated_at
	`), groupJID, val, now)
	if err != nil {
		return fmt.Errorf("set ai group mode: %w", err)
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
