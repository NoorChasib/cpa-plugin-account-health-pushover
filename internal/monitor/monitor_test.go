package monitor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/notifier"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/state"
)

type fakeHost struct {
	mu         sync.Mutex
	roster     []protocol.HostAuthFileEntry
	runtime    map[string]protocol.HostAuthFileEntry
	listErr    error
	runtimeErr map[string]error
	listCalls  int
	logs       []string
}

func (h *fakeHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listCalls++
	return append([]protocol.HostAuthFileEntry(nil), h.roster...), h.listErr
}

func (h *fakeHost) GetRuntime(_ context.Context, authIndex string) (protocol.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.runtimeErr[authIndex]; err != nil {
		return protocol.HostAuthFileEntry{}, err
	}
	return h.runtime[authIndex], nil
}

func (h *fakeHost) Log(_ context.Context, _ string, message string, _ map[string]any) {
	h.mu.Lock()
	h.logs = append(h.logs, message)
	h.mu.Unlock()
}

func (h *fakeHost) set(entry protocol.HostAuthFileEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.roster = []protocol.HostAuthFileEntry{entry}
	h.runtime[entry.AuthIndex] = entry
}

func oauthEntry(index, provider, email, status, message string, unavailable bool) protocol.HostAuthFileEntry {
	return protocol.HostAuthFileEntry{
		AuthIndex:     index,
		ID:            "id-" + index,
		Name:          provider + "-" + index + ".json",
		Provider:      provider,
		Type:          provider,
		Email:         email,
		Label:         email,
		AccountType:   "oauth",
		Status:        status,
		StatusMessage: message,
		Unavailable:   unavailable,
	}
}

type mockPushover struct {
	server     *httptest.Server
	mu         sync.Mutex
	messages   []string
	priorities []string
	status     int
	body       string
}

func newMockPushover(t *testing.T) *mockPushover {
	t.Helper()
	mock := &mockPushover{status: http.StatusOK, body: `{"status":1}`}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mock.mu.Lock()
		mock.messages = append(mock.messages, r.PostForm.Get("title")+"\n"+r.PostForm.Get("message"))
		mock.priorities = append(mock.priorities, r.PostForm.Get("priority"))
		status, body := mock.status, mock.body
		mock.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *mockPushover) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.messages)
}

func (m *mockPushover) all() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.messages, "\n---\n")
}

func (m *mockPushover) lastPriority() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.priorities) == 0 {
		return ""
	}
	return m.priorities[len(m.priorities)-1]
}

func (m *mockPushover) respond(status int, body string) {
	m.mu.Lock()
	m.status = status
	m.body = body
	m.mu.Unlock()
}

func newTestMonitor(t *testing.T, host *fakeHost, mock *mockPushover, statePath string) *Monitor {
	t.Helper()
	return newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = statePath
		cfg.NotificationCoalesceWindow = 0
	})
}

func newConfiguredTestMonitor(t *testing.T, host *fakeHost, mock *mockPushover, configure func(*config.Config)) *Monitor {
	t.Helper()
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	cfg := config.Default()
	cfg.StartupGrace = 24 * time.Hour
	cfg.ScanInterval = 24 * time.Hour
	cfg.HTTPTimeout = time.Second
	if configure != nil {
		configure(&cfg)
	}
	client := notifier.NewClient(cfg, mock.server.URL, mock.server.Client())
	dispatcher := notifier.NewDispatcher(client, 64, cfg.NotificationCoalesceWindow)
	monitor := New(cfg, host, client, dispatcher)
	monitor.Start()
	t.Cleanup(monitor.Stop)
	return monitor
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestHealthyReauthDedupeAndRecovery(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	mock := newMockPushover(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	monitor := newTestMonitor(t, host, mock, statePath)

	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	if err := monitor.Reconcile(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if mock.count() != 0 {
		t.Fatal("healthy baseline sent an alert")
	}

	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	if err := monitor.Reconcile(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	if !strings.Contains(mock.all(), "reauth required") || !strings.Contains(mock.all(), "Manual sign-in is required") {
		t.Fatalf("unexpected failure message: %s", mock.all())
	}

	if err := monitor.Reconcile(context.Background(), "repeat"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if mock.count() != 1 {
		t.Fatalf("repeated incident sent duplicate; count=%d", mock.count())
	}

	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	if err := monitor.Reconcile(context.Background(), "recovery"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 2 })
	if !strings.Contains(mock.all(), "account recovered") || mock.lastPriority() != "0" {
		t.Fatalf("recovery message/priority missing: priority=%q messages=%s", mock.lastPriority(), mock.all())
	}
	if got := monitor.Snapshot().Accounts[0].Health; got != health.Healthy {
		t.Fatalf("health = %q", got)
	}
}

func TestQuotaAndDisabledDoNotAlertByDefault(t *testing.T) {
	for _, test := range []struct {
		name  string
		entry protocol.HostAuthFileEntry
		want  health.State
	}{
		{"quota", oauthEntry("one", "codex", "a@example.com", "error", "quota exhausted", true), health.QuotaLimited},
		{"five hour", oauthEntry("one", "claude", "a@example.com", "error", "5-hour limit reached", true), health.QuotaLimited},
		{"disabled", func() protocol.HostAuthFileEntry {
			entry := oauthEntry("one", "claude", "a@example.com", "disabled", "", false)
			entry.Disabled = true
			return entry
		}(), health.Disabled},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
			host.set(test.entry)
			mock := newMockPushover(t)
			monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
			if err := monitor.Reconcile(context.Background(), "test"); err != nil {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
			if mock.count() != 0 {
				t.Fatalf("non-alerting state sent notification: %s", mock.all())
			}
			if got := monitor.Snapshot().Accounts[0].Health; got != test.want {
				t.Fatalf("health=%q want=%q", got, test.want)
			}
		})
	}
}

func TestPersistentTransientBecomesCredentialDown(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	entry := oauthEntry("one", "codex", "a@example.com", "error", "transient upstream error", true)
	host.set(entry)
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }

	if err := monitor.Reconcile(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0].Health; got != health.Suspect {
		t.Fatalf("first health=%q", got)
	}
	if mock.count() != 0 {
		t.Fatal("single transient error alerted")
	}
	current = current.Add(11 * time.Minute)
	if err := monitor.Reconcile(context.Background(), "confirmed"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	status := monitor.Snapshot().Accounts[0]
	if status.Health != health.CredentialDown || !strings.HasPrefix(status.ReasonCode, "persistent_") {
		t.Fatalf("status=%+v", status)
	}
}

func TestRemovedAccountDoesNotSendRecovery(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	if err := monitor.Reconcile(context.Background(), "broken"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	host.mu.Lock()
	host.roster = nil
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "removed"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if mock.count() != 1 {
		t.Fatalf("removal sent recovery: %s", mock.all())
	}
	if got := monitor.Snapshot().Accounts[0].Health; got != health.Removed {
		t.Fatalf("health=%q", got)
	}
}

func TestRestartWithPersistedIncidentDoesNotDuplicate(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	first := newTestMonitor(t, host, mock, statePath)
	if err := first.Reconcile(context.Background(), "first-install"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	first.Stop()

	second := newTestMonitor(t, host, mock, statePath)
	if err := second.Reconcile(context.Background(), "restart"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if mock.count() != 1 {
		t.Fatalf("restart duplicated alert: %s", mock.all())
	}
}

func TestFailedDeliveryRetriesLaterAndOnlyThenMarksAlert(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "codex", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	mock.respond(http.StatusBadRequest, `{"status":0}`)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	if err := monitor.Reconcile(context.Background(), "failed-send"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	status := monitor.Snapshot().Accounts[0]
	if !status.LastAlertAt.IsZero() {
		t.Fatalf("failed send marked delivered: %+v", status)
	}

	mock.respond(http.StatusOK, `{"status":1}`)
	current = current.Add(notificationRetryAfter + time.Second)
	if err := monitor.Reconcile(context.Background(), "retry"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 2 })
	waitFor(t, time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })
}

func TestReminderOnlyAfterInterval(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	if err := monitor.Reconcile(context.Background(), "failure"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })
	current = current.Add(11 * time.Hour)
	if err := monitor.Reconcile(context.Background(), "too-soon"); err != nil {
		t.Fatal(err)
	}
	if mock.count() != 1 {
		t.Fatal("reminder sent too soon")
	}
	current = current.Add(2 * time.Hour)
	if err := monitor.Reconcile(context.Background(), "due"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 2 })
	if !strings.Contains(mock.all(), "still unresolved") {
		t.Fatalf("reminder content missing: %s", mock.all())
	}
}

func TestReplacementCorrelatesByExactEmailAndRecovers(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("old-index", "claude", "same@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	if err := monitor.Reconcile(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })

	newEntry := oauthEntry("new-index", "claude", "same@example.com", "active", "", false)
	host.set(newEntry)
	if err := monitor.Reconcile(context.Background(), "replacement"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 2 })
	rows := monitor.Snapshot().Accounts
	if len(rows) != 1 || rows[0].AuthIndex != "new-index" || rows[0].Health != health.Healthy {
		t.Fatalf("replacement was not safely correlated: %+v", rows)
	}
}

func TestHostListFailurePreservesStateAndMarksSnapshotStale(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	if err := monitor.Reconcile(context.Background(), "healthy"); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.listErr = context.DeadlineExceeded
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "failure"); err == nil {
		t.Fatal("expected list failure")
	}
	status := monitor.Snapshot()
	if !status.MonitoringStale || len(status.Accounts) != 1 || status.Accounts[0].Health != health.Healthy {
		t.Fatalf("state was not preserved: %+v", status)
	}
}

func TestSignalIsNonBlockingAndBurstCoalesced(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	start := time.Now()
	for i := 0; i < 100; i++ {
		if !monitor.Signal("one") {
			t.Fatal("signal was unexpectedly dropped")
		}
	}
	if time.Since(start) > 20*time.Millisecond {
		t.Fatal("usage signal callback path blocked")
	}
	waitFor(t, 2*time.Second, func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.listCalls == 1
	})
	time.Sleep(150 * time.Millisecond)
	host.mu.Lock()
	calls := host.listCalls
	host.mu.Unlock()
	if calls != 1 {
		t.Fatalf("duplicate event burst caused %d reconciliations", calls)
	}
}

func TestRepeatedReplacementCorrelationCannotCreateAliasCycle(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))

	for _, index := range []string{"index-a", "index-b", "index-a"} {
		host.set(oauthEntry(index, "claude", "same@example.com", "active", "", false))
		if err := monitor.Reconcile(context.Background(), "replacement"); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan struct{})
	go func() {
		monitor.deliveryCallback("claude:index-b", "disabled", 0)(notifier.DeliveryResult{Accepted: true, At: time.Now().UTC()})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("delivery callback deadlocked while resolving replacement aliases")
	}
	rows := monitor.Snapshot().Accounts
	if len(rows) != 1 || rows[0].AuthIndex != "index-a" {
		t.Fatalf("unexpected correlated account rows: %+v", rows)
	}
}

func TestStaleRecoveryCannotCorruptNewFailureIncident(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.NotificationCoalesceWindow = 200 * time.Millisecond
	})
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }

	if err := monitor.Reconcile(context.Background(), "first-failure"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })

	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	if err := monitor.Reconcile(context.Background(), "recovery-queued"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	oldGeneration := monitor.data.Accounts["claude:one"].IncidentGeneration
	monitor.stateMu.RUnlock()

	current = current.Add(time.Second)
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	if err := monitor.Reconcile(context.Background(), "failed-again"); err != nil {
		t.Fatal(err)
	}
	monitor.deliveryCallback("claude:one", "recovery", oldGeneration)(notifier.DeliveryResult{Accepted: true, At: current.Add(time.Second)})

	waitFor(t, 2*time.Second, func() bool { return mock.count() == 2 })
	status := monitor.Snapshot().Accounts[0]
	if status.Health != health.ReauthRequired || status.FirstDetectedAt.IsZero() {
		t.Fatalf("stale recovery corrupted current incident: %+v", status)
	}
	if strings.Contains(mock.all(), "2562047h") || !strings.Contains(mock.all(), "reauth required") {
		t.Fatalf("new incident message has an invalid duration or content: %s", mock.all())
	}
}

func TestReminderInFlightIsNotQueuedTwice(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.NotificationCoalesceWindow = 200 * time.Millisecond
	})
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	if err := monitor.Reconcile(context.Background(), "failure"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })

	current = current.Add(13 * time.Hour)
	if err := monitor.Reconcile(context.Background(), "reminder-one"); err != nil {
		t.Fatal(err)
	}
	if err := monitor.Reconcile(context.Background(), "reminder-two"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return mock.count() == 2 })
	time.Sleep(250 * time.Millisecond)
	if mock.count() != 2 {
		t.Fatalf("duplicate reminders were delivered: %s", mock.all())
	}
}

func TestStateLoadWaitsForDiscoveredAuthDirectory(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	mock := newMockPushover(t)
	authDir := t.TempDir()
	statePath := filepath.Join(authDir, ".plugin-state", "account-health-pushover", "state.json")
	data := state.NewData()
	firstDetected := time.Now().UTC().Add(-time.Hour)
	data.Accounts["claude:one"] = &state.Account{
		AccountKey:         "claude:one",
		Provider:           "claude",
		AuthIndex:          "one",
		Label:              "a@example.com",
		Health:             health.ReauthRequired,
		FirstDetectedAt:    firstDetected,
		AlertSent:          true,
		IncidentGeneration: 7,
	}
	if err := (state.Store{Path: statePath}).Save(data); err != nil {
		t.Fatal(err)
	}
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = ""
		cfg.NotificationCoalesceWindow = 0
	})
	if err := monitor.Reconcile(context.Background(), "empty-roster"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().StateFileHealth; got != "waiting_for_auth_path" {
		t.Fatalf("state health=%q, want waiting_for_auth_path", got)
	}

	entry := oauthEntry("one", "claude", "a@example.com", "active", "", false)
	entry.Path = filepath.Join(authDir, "claude-one.json")
	host.set(entry)
	if err := monitor.Reconcile(context.Background(), "auth-discovered"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	status := monitor.Snapshot()
	if status.StateFile != statePath || status.Accounts[0].Health != health.Healthy || !strings.Contains(mock.all(), "account recovered") {
		t.Fatalf("persisted incident was not loaded from discovered auth directory: status=%+v messages=%s", status, mock.all())
	}
}

func TestConfiguredDisabledAndRemovedNotifications(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "codex", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.NotificationCoalesceWindow = 0
		cfg.NotifyDisabled = true
		cfg.NotifyRemoved = true
		cfg.RemovedStateRetention = 0
	})
	if err := monitor.Reconcile(context.Background(), "healthy"); err != nil {
		t.Fatal(err)
	}
	disabled := oauthEntry("one", "codex", "a@example.com", "disabled", "", false)
	disabled.Disabled = true
	host.set(disabled)
	if err := monitor.Reconcile(context.Background(), "disabled"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	host.mu.Lock()
	host.roster = nil
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "removed"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 2 })
	if !strings.Contains(mock.all(), "disabled") || !strings.Contains(mock.all(), "removed") {
		t.Fatalf("configured informational notifications missing: %s", mock.all())
	}
}

func TestFailureMessageUsesConfiguredPriorityAndManagementURL(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.NotificationCoalesceWindow = 0
		cfg.FailurePriority = -1
		cfg.ManagementURL = "https://cpa.example.test/management"
	})
	if err := monitor.Reconcile(context.Background(), "failure"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return mock.count() == 1 })
	if mock.lastPriority() != "-1" || !strings.Contains(mock.all(), "Management: https://cpa.example.test/management") {
		t.Fatalf("priority=%q message=%s", mock.lastPriority(), mock.all())
	}
}
