package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

func TestStoreAtomicRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	store := Store{Path: path}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	data := NewData()
	data.Accounts["claude:one"] = &Account{
		AccountKey:      "claude:one",
		Provider:        "claude",
		AuthIndex:       "one",
		Label:           "a@example.com",
		Health:          health.ReauthRequired,
		FirstDetectedAt: now,
		LastAlertAt:     now.Add(time.Second),
		AlertSent:       true,
	}
	if err := store.Save(data); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".state-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	account := loaded.Accounts["claude:one"]
	if account == nil || !account.AlertSent || !account.LastAlertAt.Equal(now.Add(time.Second)) {
		t.Fatalf("round-trip lost dedupe state: %+v", account)
	}
}

func TestStoreCorruptStateDoesNotReturnPartialData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"accounts":`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := (Store{Path: path}).Load()
	if err == nil {
		t.Fatal("expected corrupt-state error")
	}
	if len(loaded.Accounts) != 0 {
		t.Fatalf("corrupt state returned accounts: %+v", loaded)
	}
}

func TestStateFileContainsNoSecretFieldsOrValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	data := NewData()
	data.Accounts["codex:one"] = &Account{
		AccountKey: "codex:one",
		Provider:   "codex",
		AuthIndex:  "one",
		Label:      "safe@example.com",
		Health:     health.Healthy,
	}
	if err := (Store{Path: path}).Save(data); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{"access_token", "refresh_token", "authorization", "pushover_app_token", "pushover_user_key"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("state contains forbidden secret field %q: %s", forbidden, raw)
		}
	}
}

func TestPruneRemovedRetention(t *testing.T) {
	now := time.Now().UTC()
	data := NewData()
	data.Accounts["old"] = &Account{Health: health.Removed, RemovedAt: now.Add(-8 * 24 * time.Hour)}
	data.Accounts["recent"] = &Account{Health: health.Removed, RemovedAt: now.Add(-6 * 24 * time.Hour)}
	data.Accounts["healthy"] = &Account{Health: health.Healthy}
	if got := PruneRemoved(&data, now, 7*24*time.Hour); got != 1 {
		t.Fatalf("pruned = %d, want 1", got)
	}
	if data.Accounts["old"] != nil || data.Accounts["recent"] == nil || data.Accounts["healthy"] == nil {
		t.Fatalf("unexpected accounts after prune: %+v", data.Accounts)
	}
}

func TestResolvePathUsesAuthDirectoryAndOverride(t *testing.T) {
	roster := []protocol.HostAuthFileEntry{{Path: "/root/.cli-proxy-api/claude.json"}}
	got := ResolvePath("", roster)
	want := "/root/.cli-proxy-api/.plugin-state/account-health-pushover/state.json"
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	override := filepath.Join(t.TempDir(), "custom.json")
	if got := ResolvePath(override, roster); got != override {
		t.Fatalf("override = %q", got)
	}
}
