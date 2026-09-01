package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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

func TestStateFileSchemaIsAnExactSecretFreeAllowlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	data := NewData()
	data.UpdatedAt = now
	data.LastSuccessfulSend = now
	data.LastNotificationErr = "previous notification delivery failed"
	data.Accounts["codex:one"] = &Account{
		AccountKey:                      "codex:one",
		Provider:                        "codex",
		AuthIndex:                       "one",
		Label:                           "safe-account",
		Identity:                        "safe-identity",
		Health:                          health.Suspect,
		PreviousHealth:                  health.Healthy,
		FirstDetectedAt:                 now,
		LastChangedAt:                   now,
		LastAlertAt:                     now,
		LastAlertAttemptAt:              now,
		LastAlertAttemptGeneration:      7,
		LastRecoveryAt:                  now,
		LastRecoveryAttemptAt:           now,
		LastReasonCode:                  health.ReasonHTTP503,
		AlertSent:                       true,
		IncidentGeneration:              8,
		RecoveryPendingFrom:             health.CredentialDown,
		SuspectSince:                    now,
		SuspectClass:                    health.ConfirmationTransient,
		SuspectTarget:                   health.CredentialDown,
		SuspectReason:                   health.ReasonHTTP503,
		RemovedAt:                       now,
		LastSuccessfulHealthObservation: now,
		LastObservedAt:                  now,
		CPAStatus:                       "error",
		CPAUnavailable:                  true,
		QuotaLimited:                    true,
	}
	if err := (Store{Path: path}).Save(data); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, document, []string{
		"accounts", "last_notification_error", "last_successful_send", "updated_at", "version",
	})

	var accounts map[string]map[string]json.RawMessage
	if err := json.Unmarshal(document["accounts"], &accounts); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, accounts["codex:one"], []string{
		"account_key", "alert_sent", "auth_index", "cpa_status", "cpa_unavailable",
		"first_detected_at", "health", "identity", "incident_generation", "label",
		"last_alert_at", "last_alert_attempt_at", "last_alert_attempt_generation",
		"last_changed_at", "last_observed_at", "last_reason_code", "last_recovery_at",
		"last_recovery_attempt_at", "last_successful_health_observation", "previous_health",
		"provider", "quota_limited", "recovery_pending_from", "removed_at", "suspect_class",
		"suspect_reason", "suspect_since", "suspect_target",
	})

	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{
		"access_token", "refresh_token", "authorization", "pushover_app_token", "pushover_user_key",
	} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("state contains forbidden secret field %q: %s", forbidden, raw)
		}
	}
}

func assertExactJSONKeys[T any](t *testing.T, values map[string]T, want []string) {
	t.Helper()
	got := make([]string, 0, len(values))
	for key := range values {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("JSON keys = %v, want %v", got, want)
	}
}

func TestLoadNormalizesLegacyReasonsWithoutBreakingDedupe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	legacy := `{
		"version": 1,
		"accounts": {
			"claude:one": {
				"account_key": "claude:one",
				"provider": "claude",
				"auth_index": "one",
				"health": "credential_down",
				"last_reason_code": "provider prose that must not escape",
				"last_alert_at": "` + now.Format(time.RFC3339) + `",
				"alert_sent": true,
				"incident_generation": 3
			}
		}
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := (Store{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	account := loaded.Accounts["claude:one"]
	if account == nil {
		t.Fatal("legacy account missing after load")
	}
	if account.LastReasonCode != health.ReasonUnclassifiedCredentialError {
		t.Fatalf("normalized reason = %q", account.LastReasonCode)
	}
	if !account.AlertSent || !account.LastAlertAt.Equal(now) || account.IncidentGeneration != 3 {
		t.Fatalf("dedupe state changed during normalization: %+v", account)
	}
}

func TestLoadRestartsLegacyUntypedSuspicion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{
		"version": 1,
		"accounts": {
			"codex:one": {
				"account_key": "codex:one",
				"health": "suspect",
				"last_reason_code": "http_503",
				"suspect_since": "2026-09-01T12:00:00Z"
			}
		}
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := (Store{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	account := loaded.Accounts["codex:one"]
	if account == nil {
		t.Fatal("legacy account missing after load")
	}
	if !account.SuspectSince.IsZero() || account.SuspectClass != health.ConfirmationNone || account.SuspectTarget != health.Unknown || account.SuspectReason != health.ReasonNone {
		t.Fatalf("legacy suspicion confirmation was not reset: %+v", account)
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
