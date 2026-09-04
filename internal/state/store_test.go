package state

import (
	"context"
	"encoding/json"
	"errors"
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

func TestStoreRejectsSupersededWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	oldWriter := Store{Path: path}
	if err := oldWriter.Claim(); err != nil {
		t.Fatal(err)
	}
	defer oldWriter.Release()
	oldData := NewData()
	oldData.Accounts["old"] = &Account{Health: health.Healthy}
	if err := oldWriter.Save(oldData); err != nil {
		t.Fatal(err)
	}

	newWriter := Store{Path: path}
	if err := newWriter.Claim(); err != nil {
		t.Fatal(err)
	}
	defer newWriter.Release()
	oldWriter.Release()
	newData := NewData()
	newData.Accounts["new"] = &Account{Health: health.Healthy}
	if err := newWriter.Save(newData); err != nil {
		t.Fatal(err)
	}
	if err := oldWriter.Save(oldData); !errors.Is(err, ErrWriterSuperseded) {
		t.Fatalf("superseded save error=%v, want ErrWriterSuperseded", err)
	}
	loaded, err := newWriter.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Accounts["new"] == nil || loaded.Accounts["old"] != nil {
		t.Fatalf("superseded writer replaced newer state: %+v", loaded.Accounts)
	}
}

func TestWriterClaimSerializesWithInFlightCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	oldWriter := Store{Path: path}
	if err := oldWriter.Claim(); err != nil {
		t.Fatal(err)
	}
	commitValidated := make(chan struct{})
	releaseCommit := make(chan struct{})
	oldWriter.beforeCommit = func() {
		close(commitValidated)
		<-releaseCommit
	}
	oldData := NewData()
	oldData.Accounts["old"] = &Account{Health: health.Healthy}
	saveDone := make(chan error, 1)
	go func() { saveDone <- oldWriter.Save(oldData) }()
	select {
	case <-commitValidated:
	case <-time.After(5 * time.Second):
		t.Fatal("old writer did not reach its validated commit")
	}

	newWriter := Store{Path: path}
	claimStarted := make(chan struct{})
	newWriter.beforeClaim = func() { close(claimStarted) }
	claimDone := make(chan error, 1)
	go func() { claimDone <- newWriter.Claim() }()
	select {
	case <-claimStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement claim did not reach the path lock")
	}
	select {
	case err := <-claimDone:
		t.Fatalf("replacement claim passed an in-flight commit: %v", err)
	default:
	}

	close(releaseCommit)
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	if err := <-claimDone; err != nil {
		t.Fatal(err)
	}
	defer oldWriter.Release()
	defer newWriter.Release()
	newData := NewData()
	newData.Accounts["new"] = &Account{Health: health.Healthy}
	if err := newWriter.Save(newData); err != nil {
		t.Fatal(err)
	}
	if err := oldWriter.Save(oldData); !errors.Is(err, ErrWriterSuperseded) {
		t.Fatalf("old writer remained active after serialized claim: %v", err)
	}
}

func TestWriterClaimContextExpiresBehindInFlightCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	oldWriter := Store{Path: path}
	if err := oldWriter.Claim(); err != nil {
		t.Fatal(err)
	}
	commitValidated := make(chan struct{})
	releaseCommit := make(chan struct{})
	oldWriter.beforeCommit = func() {
		close(commitValidated)
		<-releaseCommit
	}
	saveDone := make(chan error, 1)
	go func() { saveDone <- oldWriter.Save(NewData()) }()
	select {
	case <-commitValidated:
	case <-time.After(5 * time.Second):
		t.Fatal("old writer did not enter its commit")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	newWriter := Store{Path: path}
	started := time.Now()
	err := newWriter.ClaimContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("claim error=%v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("context-bounded claim returned too late: %s", elapsed)
	}
	close(releaseCommit)
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	oldWriter.Release()
}

func TestWriterLeaseCanonicalizesSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	realPath := filepath.Join(realDir, "state.json")
	aliasPath := filepath.Join(aliasDir, "state.json")

	oldWriter := Store{Path: aliasPath}
	if err := oldWriter.Claim(); err != nil {
		t.Fatal(err)
	}
	commitValidated := make(chan struct{})
	releaseCommit := make(chan struct{})
	oldWriter.beforeCommit = func() {
		close(commitValidated)
		<-releaseCommit
	}
	saveDone := make(chan error, 1)
	go func() { saveDone <- oldWriter.Save(NewData()) }()
	select {
	case <-commitValidated:
	case <-time.After(5 * time.Second):
		t.Fatal("aliased writer did not enter its commit")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	newWriter := Store{Path: realPath}
	if err := newWriter.ClaimContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("real-path claim bypassed aliased commit: %v", err)
	}
	close(releaseCommit)
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	oldWriter.Release()
}

func TestWriterLeaseRemainsStableWhenStateFileIsSymlink(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.json")
	if err := (Store{Path: targetPath}).Save(NewData()); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "state.json")
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	store := Store{Path: linkPath}
	if err := store.Claim(); err != nil {
		t.Fatal(err)
	}
	defer store.Release()
	first := NewData()
	first.Accounts["first"] = &Account{Health: health.Healthy}
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	second := NewData()
	second.Accounts["second"] = &Account{Health: health.Healthy}
	if err := store.Save(second); err != nil {
		t.Fatalf("second save changed lease authority after replacing leaf symlink: %v", err)
	}
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("atomic save unexpectedly retained the state-file symlink")
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Accounts["second"] == nil || loaded.Accounts["first"] != nil {
		t.Fatalf("second save was not persisted through stable authority: %+v", loaded.Accounts)
	}
}

func TestSuppressionRollbackCanonicalizesSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	realPath := filepath.Join(realDir, "state.json")
	aliasPath := filepath.Join(aliasDir, "state.json")
	attemptAt := time.Now().UTC()
	data := NewData()
	data.Accounts["claude:one"] = &Account{
		AccountKey:                 "claude:one",
		Identity:                   "claude|email|same@example.com",
		Health:                     health.ReauthRequired,
		IncidentGeneration:         4,
		LastAlertAttemptAt:         attemptAt,
		LastAlertAttemptGeneration: 4,
	}
	RegisterSuppressionRollback(aliasPath, SuppressionRollback{
		AccountKey: "claude:one",
		Identity:   "claude|email|same@example.com",
		Kind:       AlertSuppression,
		Generation: 4,
		AttemptAt:  attemptAt,
	})
	if err := (Store{Path: realPath}).Save(data); err != nil {
		t.Fatal(err)
	}
	loaded, err := (Store{Path: realPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	account := loaded.Accounts["claude:one"]
	if account == nil || !account.LastAlertAttemptAt.IsZero() || account.LastAlertAttemptGeneration != 0 {
		t.Fatalf("rollback registered through symlink alias was not applied: %+v", account)
	}
}

func TestSuppressionRollbackFollowsUniqueIdentityWithoutClearingReusedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	attemptAt := time.Now().UTC()
	data := NewData()
	data.Accounts["claude:old-key"] = &Account{
		AccountKey:                 "claude:old-key",
		Identity:                   "claude|email|unrelated@example.com",
		Health:                     health.ReauthRequired,
		IncidentGeneration:         4,
		LastAlertAttemptAt:         attemptAt,
		LastAlertAttemptGeneration: 4,
	}
	data.Accounts["claude:new-key"] = &Account{
		AccountKey:                 "claude:new-key",
		Identity:                   "claude|email|same@example.com",
		Health:                     health.ReauthRequired,
		IncidentGeneration:         4,
		LastAlertAttemptAt:         attemptAt,
		LastAlertAttemptGeneration: 4,
	}
	RegisterSuppressionRollback(path, SuppressionRollback{
		AccountKey: "claude:old-key",
		Identity:   "claude|email|same@example.com",
		Kind:       AlertSuppression,
		Generation: 4,
		AttemptAt:  attemptAt,
	})
	if err := (Store{Path: path}).Save(data); err != nil {
		t.Fatal(err)
	}
	loaded, err := (Store{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if reused := loaded.Accounts["claude:old-key"]; reused == nil || reused.LastAlertAttemptAt.IsZero() || reused.LastAlertAttemptGeneration != 4 {
		t.Fatalf("rollback cleared an unrelated account that reused the old key: %+v", reused)
	}
	if moved := loaded.Accounts["claude:new-key"]; moved == nil || !moved.LastAlertAttemptAt.IsZero() || moved.LastAlertAttemptGeneration != 0 {
		t.Fatalf("rollback did not follow the unique logical identity: %+v", moved)
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
		Quota: QuotaState{
			Percent:            96.5,
			ResetAt:            now,
			ObservedAt:         now,
			LastPollAt:         now,
			LastError:          "usage endpoint rate limited the poll (HTTP 429)",
			WindowKey:          "2026-09-01T12:00:00Z",
			WarningSentAt:      now,
			ExhaustedSentAt:    now,
			WarningAttemptAt:   now,
			ExhaustedAttemptAt: now,
		},
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
		"provider", "quota", "quota_limited", "recovery_pending_from", "removed_at", "suspect_class",
		"suspect_reason", "suspect_since", "suspect_target",
	})
	var quotaDoc map[string]json.RawMessage
	if err := json.Unmarshal(accounts["codex:one"]["quota"], &quotaDoc); err != nil {
		t.Fatal(err)
	}
	assertExactJSONKeys(t, quotaDoc, []string{
		"percent", "reset_at", "observed_at", "last_poll_at", "last_error", "window_key",
		"warning_sent_at", "exhausted_sent_at", "warning_attempt_at", "exhausted_attempt_at",
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
	want := "/root/.cli-proxy-api/.plugin-state/account-health-pushover/state.ahp"
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	override := filepath.Join(t.TempDir(), "custom.json")
	if got := ResolvePath(override, roster); got != override {
		t.Fatalf("override = %q", got)
	}
}

func TestDefaultStateFileIsNotAJSONCredentialCandidate(t *testing.T) {
	// Current CPA lists every *.json beneath the auth directory as an auth
	// file, so the default state name must never end in .json.
	if strings.HasSuffix(strings.ToLower(FileName), ".json") {
		t.Fatalf("default state file %q would be listed by CPA as a credential", FileName)
	}
	if ListedAsCredential(DefaultPath("/root/.cli-proxy-api"), "/root/.cli-proxy-api") {
		t.Fatal("default path reported as credential-listed")
	}
	if !ListedAsCredential("/root/.cli-proxy-api/.plugin-state/x/state.json", "/root/.cli-proxy-api") {
		t.Fatal("nested .json beneath auth dir was not reported")
	}
	if ListedAsCredential("/CLIProxyAPI/plugins/ahp/state.json", "/root/.cli-proxy-api") {
		t.Fatal(".json outside auth dir was reported")
	}
	if ListedAsCredential("/root/.cli-proxy-api-other/state.json", "/root/.cli-proxy-api") {
		t.Fatal("sibling directory with shared prefix was reported")
	}
}

func TestResolvePathIgnoresPluginStateRosterEntries(t *testing.T) {
	// Regression: CPA sorts the roster by name and ".plugin-state/..." sorts
	// before every real credential, so the old resolver latched onto its own
	// state file and nested a new directory on every restart.
	nested := "/root/.cli-proxy-api/.plugin-state/account-health-pushover/state.json"
	roster := []protocol.HostAuthFileEntry{
		{Name: ".plugin-state/account-health-pushover/state.json", Path: nested},
		{Name: "claude-a.json", Path: "/root/.cli-proxy-api/claude-a.json"},
	}
	if got := AuthDirectoryFromRoster(roster); got != "/root/.cli-proxy-api" {
		t.Fatalf("auth dir = %q", got)
	}
	if got := ResolvePath("", roster); got != DefaultPath("/root/.cli-proxy-api") {
		t.Fatalf("path = %q", got)
	}
	onlyState := roster[:1]
	if got := AuthDirectoryFromRoster(onlyState); got != "" {
		t.Fatalf("plugin-state-only roster produced auth dir %q", got)
	}
	for _, path := range []string{nested, `C:\auth\.plugin-state\x\state.json`, ".plugin-state/x"} {
		if !IsPluginStatePath(path) {
			t.Fatalf("%q not recognised as plugin state", path)
		}
	}
	if IsPluginStatePath("/root/.cli-proxy-api/claude.json") || IsPluginStatePath("") {
		t.Fatal("credential path recognised as plugin state")
	}
}

func TestMigrateLegacyAdoptsNewestNestedStateAndRemovesTree(t *testing.T) {
	authDir := t.TempDir()
	store := Store{Path: DefaultPath(authDir)}
	base := filepath.Dir(store.Path)
	writeLegacy := func(dir string, generation uint64, mod time.Time) string {
		t.Helper()
		data := NewData()
		data.Accounts["claude:one"] = &Account{AccountKey: "claude:one", Provider: "claude", AuthIndex: "one", Health: health.ReauthRequired, AlertSent: true, IncidentGeneration: generation}
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, LegacyFileName)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
		return path
	}
	now := time.Now()
	writeLegacy(base, 1, now.Add(-3*time.Hour))
	level2 := filepath.Join(base, stateDirName, pluginDirName)
	writeLegacy(level2, 2, now.Add(-2*time.Hour))
	level3 := filepath.Join(level2, stateDirName, pluginDirName)
	writeLegacy(level3, 3, now.Add(-time.Hour))

	if err := store.Claim(); err != nil {
		t.Fatal(err)
	}
	defer store.Release()
	migrated, err := store.MigrateLegacy()
	if err != nil || !migrated {
		t.Fatalf("migrated=%v err=%v", migrated, err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if account := loaded.Accounts["claude:one"]; account == nil || account.IncidentGeneration != 3 || !account.AlertSent {
		t.Fatalf("newest nested state was not adopted: %+v", account)
	}
	if _, err := os.Stat(filepath.Join(base, LegacyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy state.json remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, stateDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nested plugin-state tree remains: %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		t.Fatalf("unexpected state directory contents: %v", entries)
	}

	again, err := store.MigrateLegacy()
	if err != nil || again {
		t.Fatalf("second migration migrated=%v err=%v", again, err)
	}
}

func TestMigrateLegacyIsNoopWithoutLegacyFileOrWhenCurrentExists(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "s", FileName)}
	migrated, err := store.MigrateLegacy()
	if err != nil || migrated {
		t.Fatalf("empty dir: migrated=%v err=%v", migrated, err)
	}
	if err := store.Claim(); err != nil {
		t.Fatal(err)
	}
	defer store.Release()
	if err := store.Save(NewData()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(store.Path), LegacyFileName), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, err = store.MigrateLegacy()
	if err != nil || migrated {
		t.Fatalf("current present: migrated=%v err=%v", migrated, err)
	}
}

func TestMigrateLegacyRejectsCorruptLegacyFileWithoutTouchingIt(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "s", FileName)}
	dir := filepath.Dir(store.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, LegacyFileName)
	if err := os.WriteFile(legacy, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, err := store.MigrateLegacy()
	if err == nil || migrated {
		t.Fatalf("corrupt legacy: migrated=%v err=%v", migrated, err)
	}
	if _, statErr := os.Stat(legacy); statErr != nil {
		t.Fatalf("corrupt legacy file was removed: %v", statErr)
	}
	if _, statErr := os.Stat(store.Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("state file was created from corrupt input: %v", statErr)
	}
}
