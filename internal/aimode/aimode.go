package aimode

import (
	"context"
	"strings"
)

type State struct {
	Enabled   bool
	Owner     string
	UpdatedAt int64
}

type Store interface {
	GetState(ctx context.Context) (State, error)
	SetState(ctx context.Context, enabled bool, owner string) error
	GetGroupEnabled(ctx context.Context, groupJID string) (bool, error)
	SetGroupEnabled(ctx context.Context, groupJID string, enabled bool) error
}

func NormalizeJID(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return s
}

func IsOwner(sender string, owners []string) bool {
	n := NormalizeJID(sender)
	if n == "" {
		return false
	}
	for _, o := range owners {
		if NormalizeJID(o) == n {
			return true
		}
	}
	return false
}

func ParseOwnerList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
