package aimode

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func openTestStore(t *testing.T) *SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?_foreign_keys=on&cache=shared")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewSQLStore(context.Background(), db, "sqlite3")
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	return store
}

func TestFreshInstallDefaultsOff(t *testing.T) {
	store := openTestStore(t)
	state, err := store.GetState(context.Background())
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state.Enabled {
		t.Fatal("fresh installation must default to OFF")
	}
}

func TestSetStatePersists(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.SetState(ctx, true, "123456789"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	state, err := store.GetState(ctx)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if !state.Enabled || state.Owner != "123456789" {
		t.Fatalf("state = %+v, want enabled with owner", state)
	}
	if err := store.SetState(ctx, false, "123456789"); err != nil {
		t.Fatalf("SetState off: %v", err)
	}
	state, err = store.GetState(ctx)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state.Enabled {
		t.Fatal("disabled state must persist")
	}
}

func TestGroupScopeIndependent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.SetGroupEnabled(ctx, "group@g.us", true); err != nil {
		t.Fatalf("SetGroupEnabled: %v", err)
	}
	enabled, err := store.GetGroupEnabled(ctx, "group@g.us")
	if err != nil || !enabled {
		t.Fatalf("group enabled = %v, err = %v", enabled, err)
	}
	state, err := store.GetState(ctx)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state.Enabled {
		t.Fatal("group ON must not enable the global mode")
	}
}

func TestGroupInheritsGlobalByDefault(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	enabled, err := store.GetGroupEnabled(ctx, "fresh@g.us")
	if err != nil {
		t.Fatalf("GetGroupEnabled: %v", err)
	}
	if !enabled {
		t.Fatal("groups without an explicit setting must inherit the global mode")
	}
	if err := store.SetGroupEnabled(ctx, "fresh@g.us", false); err != nil {
		t.Fatalf("SetGroupEnabled: %v", err)
	}
	enabled, err = store.GetGroupEnabled(ctx, "fresh@g.us")
	if err != nil {
		t.Fatalf("GetGroupEnabled: %v", err)
	}
	if enabled {
		t.Fatal("explicit group OFF must persist")
	}
}

func TestIsOwner(t *testing.T) {
	owners := []string{"1234567890@s.whatsapp.net", " 0987654321 "}
	cases := []struct {
		sender string
		want   bool
	}{
		{"1234567890:12@s.whatsapp.net", true},
		{"1234567890@s.whatsapp.net", true},
		{"0987654321", true},
		{"1111111111@s.whatsapp.net", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsOwner(tc.sender, owners); got != tc.want {
			t.Fatalf("IsOwner(%q) = %v, want %v", tc.sender, got, tc.want)
		}
	}
}

func TestParseOwnerList(t *testing.T) {
	got := ParseOwnerList(" 123 , 456 ,, ")
	if len(got) != 2 || got[0] != "123" || got[1] != "456" {
		t.Fatalf("ParseOwnerList = %v", got)
	}
	if ParseOwnerList("   ") != nil {
		t.Fatal("blank list must be nil")
	}
}
