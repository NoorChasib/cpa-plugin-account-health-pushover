package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

type hostLog struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type fakeHost struct {
	mu         sync.Mutex
	roster     []protocol.HostAuthFileEntry
	runtime    map[string]protocol.HostAuthFileEntry
	listErr    error
	runtimeErr map[string]error
	listCalls  int
	logs       []hostLog
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

func (h *fakeHost) Log(_ context.Context, level string, message string, fields map[string]any) {
	h.mu.Lock()
	copied := make(map[string]any, len(fields))
	for key, value := range fields {
		copied[key] = value
	}
	h.logs = append(h.logs, hostLog{Level: level, Message: message, Fields: copied})
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
	endpoint   string
	path       string
	mu         sync.Mutex
	messages   []string
	priorities []string
	status     int
	body       string
}

func newMockPushover(t *testing.T) *mockPushover {
	t.Helper()
	mock := &mockPushover{status: http.StatusOK, body: `{"status":1}`}
	mock.path = fmt.Sprintf("/pushover-%p", mock)
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A unique endpoint prevents a canceled request from a prior test from
		// being counted if the OS reuses an httptest listener port.
		if r.URL.Path != mock.path {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		mock.mu.Lock()
		mock.messages = append(mock.messages, r.PostForm.Get("title")+"\n"+r.PostForm.Get("message"))
		mock.priorities = append(mock.priorities, r.PostForm.Get("priority"))
		status, body := mock.status, mock.body
		mock.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	mock.endpoint = mock.server.URL + mock.path
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
	client := notifier.NewClient(cfg, mock.endpoint, mock.server.Client())
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

type blockingRuntimeHost struct {
	entry   protocol.HostAuthFileEntry
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *blockingRuntimeHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return []protocol.HostAuthFileEntry{h.entry}, nil
}

func (h *blockingRuntimeHost) GetRuntime(context.Context, string) (protocol.HostAuthFileEntry, error) {
	h.once.Do(func() { close(h.started) })
	<-h.release
	return h.entry, nil
}

func (*blockingRuntimeHost) Log(context.Context, string, string, map[string]any) {}

func TestStopWaitsForBlockedHostCallback(t *testing.T) {
	host := &blockingRuntimeHost{
		entry:   oauthEntry("one", "claude", "a@example.com", "active", "", false),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	mock := newMockPushover(t)
	cfg := config.Default()
	cfg.StartupGrace = 24 * time.Hour
	cfg.ScanInterval = 24 * time.Hour
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.NotificationCoalesceWindow = 0
	client := notifier.NewClient(cfg, mock.endpoint, mock.server.Client())
	dispatcher := notifier.NewDispatcher(client, 4, 0)
	monitor := New(cfg, host, client, dispatcher)
	monitor.Start()

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- monitor.Reconcile(context.Background(), "blocked") }()
	select {
	case <-host.started:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime callback did not start")
	}

	stopDone := make(chan struct{})
	go func() {
		monitor.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned while a host callback was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(host.release)
	select {
	case err := <-reconcileDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconciliation did not finish after releasing the host callback")
	}
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the host callback completed")
	}
}

func TestBoundedStopRetiresStuckHostCallButFinalStopStillWaits(t *testing.T) {
	host := &blockingRuntimeHost{
		entry:   oauthEntry("one", "claude", "a@example.com", "active", "", false),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	mock := newMockPushover(t)
	cfg := config.Default()
	cfg.StartupGrace = 24 * time.Hour
	cfg.ScanInterval = 24 * time.Hour
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	client := notifier.NewClient(cfg, mock.endpoint, mock.server.Client())
	dispatcher := notifier.NewDispatcher(client, 4, 0)
	monitor := New(cfg, host, client, dispatcher)
	monitor.Start()
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- monitor.Reconcile(context.Background(), "blocked") }()
	select {
	case <-host.started:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime callback did not start")
	}

	if monitor.StopWithin(20 * time.Millisecond) {
		t.Fatal("bounded stop reported a stuck host callback as drained")
	}
	if !monitor.isRetired() {
		t.Fatal("bounded stop did not retire the monitor")
	}
	finalWaitStarted := make(chan struct{})
	monitor.beforeFinalWait = func() { close(finalWaitStarted) }
	finalDone := make(chan struct{})
	go func() {
		monitor.Stop()
		close(finalDone)
	}()
	select {
	case <-finalWaitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("final stop did not reach the admitted-operation wait")
	}
	select {
	case <-finalDone:
		t.Fatal("final stop returned while the host callback was still in flight")
	default:
	}

	close(host.release)
	select {
	case err := <-reconcileDone:
		if !errors.Is(err, ErrStopping) {
			t.Fatalf("retired reconciliation error=%v, want ErrStopping", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconciliation did not finish after host release")
	}
	select {
	case <-finalDone:
	case <-time.After(5 * time.Second):
		t.Fatal("final stop did not return after host release")
	}
}

func TestReconcileSerializationHonorsCallerDeadline(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	monitor.reconcileMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := monitor.Reconcile(ctx, "queued-management")
	monitor.reconcileMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued reconciliation error=%v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("queued reconciliation ignored its deadline for %s", elapsed)
	}
}

func TestReconcileSerializationStopsWithMonitorLifecycle(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	monitor.reconcileMu.Lock()
	lockWaitStarted := make(chan struct{})
	monitor.beforeReconcileLock = func() { close(lockWaitStarted) }
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- monitor.Reconcile(context.Background(), "queued-management") }()
	select {
	case <-lockWaitStarted:
	case <-time.After(5 * time.Second):
		monitor.reconcileMu.Unlock()
		t.Fatal("queued reconciliation did not reach the serialization lock")
	}
	if !monitor.StopWithin(50 * time.Millisecond) {
		monitor.reconcileMu.Unlock()
		t.Fatal("bounded stop did not cancel a reconciliation waiting only for serialization")
	}
	select {
	case err := <-reconcileDone:
		if !errors.Is(err, ErrStopping) {
			monitor.reconcileMu.Unlock()
			t.Fatalf("queued reconciliation error=%v, want ErrStopping", err)
		}
	case <-time.After(5 * time.Second):
		monitor.reconcileMu.Unlock()
		t.Fatal("queued reconciliation did not stop with the monitor lifecycle")
	}
	monitor.reconcileMu.Unlock()
}

type cancellationIgnoringTransport struct {
	started chan struct{}
	release chan struct{}
	body    chan string
	once    sync.Once
}

func (t *cancellationIgnoringTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.body != nil {
		raw, _ := io.ReadAll(request.Body)
		form, _ := url.ParseQuery(string(raw))
		t.body <- form.Get("message")
	}
	t.once.Do(func() { close(t.started) })
	<-t.release
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"status":1}`)),
		Request:    request,
	}, nil
}

func TestRetiredMonitorCannotOverwriteReplacementStateAfterWorkerResumes(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config.Default()
	cfg.Enabled = true
	cfg.StartupGrace = 24 * time.Hour
	cfg.ScanInterval = 24 * time.Hour
	cfg.StateFile = statePath
	cfg.NotificationCoalesceWindow = 0
	cfg.NotifyRecovery = false

	oldHost := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	oldHost.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	transport := &cancellationIgnoringTransport{started: make(chan struct{}), release: make(chan struct{})}
	oldClient := notifier.NewClient(cfg, notifier.ProductionEndpoint, &http.Client{Transport: transport})
	oldDispatcher := notifier.NewDispatcher(oldClient, 4, 0)
	oldMonitor := New(cfg, oldHost, oldClient, oldDispatcher)
	oldMonitor.Start()
	if err := oldMonitor.Reconcile(context.Background(), "old-failure"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("old notifier worker did not start")
	}
	if !oldMonitor.StopWithin(20 * time.Millisecond) {
		t.Fatal("bounded stop unexpectedly left a host operation in flight")
	}

	replacementHost := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	replacementHost.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	replacementClient := notifier.NewClient(cfg, mock.endpoint, mock.server.Client())
	replacementDispatcher := notifier.NewDispatcher(replacementClient, 4, 0)
	replacement := New(cfg, replacementHost, replacementClient, replacementDispatcher)
	replacement.Start()
	defer replacement.Stop()
	if err := replacement.Reconcile(context.Background(), "replacement-healthy"); err != nil {
		t.Fatal(err)
	}
	beforeResume, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(beforeResume), `"health": "healthy"`) {
		t.Fatalf("replacement state was not persisted: %s", beforeResume)
	}

	close(transport.release)
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	if !oldDispatcher.WaitStopped(waitCtx) {
		t.Fatal("old notifier worker did not finish after release")
	}
	afterResume, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterResume) != string(beforeResume) {
		t.Fatalf("retired notifier worker overwrote replacement state:\nbefore=%s\nafter=%s", beforeResume, afterResume)
	}
	oldMonitor.Stop()
}

func TestFinalStopWaitsForCancellationIgnoringDeliveryWorker(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	cfg := config.Default()
	cfg.Enabled = true
	cfg.StartupGrace = 24 * time.Hour
	cfg.ScanInterval = 24 * time.Hour
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.NotificationCoalesceWindow = 0
	cfg.NotifyRecovery = false

	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	transport := &cancellationIgnoringTransport{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(transport.release) }) }
	defer release()
	client := notifier.NewClient(cfg, notifier.ProductionEndpoint, &http.Client{Transport: transport})
	dispatcher := notifier.NewDispatcher(client, 4, 0)
	monitor := New(cfg, host, client, dispatcher)
	monitor.Start()
	if err := monitor.Reconcile(context.Background(), "failure"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery worker did not start")
	}
	if !monitor.StopWithin(20 * time.Millisecond) {
		t.Fatal("bounded stop unexpectedly left a host operation in flight")
	}

	finalWaitStarted := make(chan struct{})
	monitor.beforeFinalDispatcherWait = func() { close(finalWaitStarted) }
	finalDone := make(chan struct{})
	go func() {
		monitor.Stop()
		close(finalDone)
	}()
	select {
	case <-finalWaitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("final stop did not reach the delivery-worker wait")
	}
	select {
	case <-finalDone:
		t.Fatal("final stop returned while delivery code was still in flight")
	default:
	}

	release()
	select {
	case <-finalDone:
	case <-time.After(5 * time.Second):
		t.Fatal("final stop did not return after the delivery worker exited")
	}
}

func TestBoundedStopClearsUnattemptedPartitionMarkersBeforeRetirement(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config.Default()
	cfg.Enabled = true
	cfg.StartupGrace = 24 * time.Hour
	cfg.ScanInterval = 24 * time.Hour
	cfg.StateFile = statePath
	cfg.NotificationCoalesceWindow = time.Hour

	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	const accountCount = 32
	labels := make([]string, accountCount)
	for i := 0; i < accountCount; i++ {
		index := fmt.Sprintf("account-%02d", i)
		labels[i] = fmt.Sprintf("account-%02d-%s@example.com", i, strings.Repeat("x", 80))
		entry := oauthEntry(index, "claude", labels[i], "error", "unauthorized", true)
		host.roster = append(host.roster, entry)
		host.runtime[index] = entry
	}
	transport := &cancellationIgnoringTransport{
		started: make(chan struct{}),
		release: make(chan struct{}),
		body:    make(chan string, 1),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(transport.release) }) }
	defer release()
	client := notifier.NewClient(cfg, notifier.ProductionEndpoint, &http.Client{Transport: transport})
	dispatcher := notifier.NewDispatcher(client, 64, cfg.NotificationCoalesceWindow)
	monitor := New(cfg, host, client, dispatcher)
	monitor.Start()
	if err := monitor.Reconcile(context.Background(), "oversized-failures"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first oversized notification partition did not start")
	}
	firstBody := <-transport.body
	if !monitor.StopWithin(100 * time.Millisecond) {
		t.Fatal("bounded stop unexpectedly left a host operation in flight")
	}

	loaded, err := (state.Store{Path: statePath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	unattempted := 0
	for i, label := range labels {
		account := loaded.Accounts[health.AccountKey("claude", fmt.Sprintf("account-%02d", i))]
		if account == nil {
			t.Fatalf("account %d missing from persisted state", i)
		}
		if strings.Contains(firstBody, label) {
			if account.LastAlertAttemptAt.IsZero() || account.LastAlertAttemptGeneration == 0 {
				t.Fatalf("attempted partition marker was cleared for account %d: %+v", i, account)
			}
			continue
		}
		unattempted++
		if !account.LastAlertAttemptAt.IsZero() || account.LastAlertAttemptGeneration != 0 {
			t.Fatalf("unattempted partition retained suppression for account %d: %+v", i, account)
		}
	}
	if unattempted == 0 {
		t.Fatal("test did not create a later oversized partition")
	}

	release()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	if !dispatcher.WaitStopped(waitCtx) {
		t.Fatal("old dispatcher did not stop after the attempted request resumed")
	}
	select {
	case body := <-transport.body:
		t.Fatalf("canceled dispatcher sent a later partition: %s", body)
	default:
	}
	monitor.Stop()
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })
	if !strings.Contains(mock.all(), "reauth required") || !strings.Contains(mock.all(), "Manual sign-in is required") {
		t.Fatalf("unexpected failure message: %s", mock.all())
	}

	if err := monitor.Reconcile(context.Background(), "repeat"); err != nil {
		t.Fatal(err)
	}
	if mock.count() != 1 {
		t.Fatalf("repeated incident sent duplicate; count=%d", mock.count())
	}

	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	if err := monitor.Reconcile(context.Background(), "recovery"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
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
	entry := oauthEntry("one", "codex", "a@example.com", "error", `{"raw":"provider 503 body"}`, true)
	host.set(entry)
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	monitor.ObserveUsageFailure("one", 503)

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
	monitor.ObserveUsageFailure("one", 503)
	if err := monitor.Reconcile(context.Background(), "confirmed"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	status := monitor.Snapshot().Accounts[0]
	if status.Health != health.CredentialDown || !strings.HasPrefix(status.ReasonCode, "persistent_") {
		t.Fatalf("status=%+v", status)
	}

	current = current.Add(time.Minute)
	monitor.ObserveUsageFailure("one", 503)
	if err := monitor.Reconcile(context.Background(), "still-confirmed"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0]; got.Health != health.CredentialDown || got.FirstDetectedAt.IsZero() {
		t.Fatalf("confirmed transient incident regressed: %+v", got)
	}
	if mock.count() != 1 {
		t.Fatalf("stable confirmed incident duplicated its alert: %s", mock.all())
	}

	current = current.Add(13 * time.Hour)
	monitor.ObserveUsageFailure("one", 502)
	if err := monitor.Reconcile(context.Background(), "confirmed-reminder"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	if !strings.Contains(mock.all(), "still unresolved") {
		t.Fatalf("confirmed transient incident did not send its due reminder: %s", mock.all())
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })
	host.mu.Lock()
	host.roster = nil
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "removed"); err != nil {
		t.Fatal(err)
	}
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !first.Snapshot().Accounts[0].LastAlertAt.IsZero() })
	first.Stop()

	second := newTestMonitor(t, host, mock, statePath)
	if err := second.Reconcile(context.Background(), "restart"); err != nil {
		t.Fatal(err)
	}
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	status := monitor.Snapshot().Accounts[0]
	if !status.LastAlertAt.IsZero() {
		t.Fatalf("failed send marked delivered: %+v", status)
	}

	mock.respond(http.StatusOK, `{"status":1}`)
	current = current.Add(notificationRetryAfter + time.Second)
	if err := monitor.Reconcile(context.Background(), "retry"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })
}

func TestRejectedEnqueueDoesNotRecordNotificationAttempt(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	monitor.dispatcher.Stop()

	if err := monitor.Reconcile(context.Background(), "dispatcher-stopped"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	account := monitor.data.Accounts["claude:one"]
	lastError := monitor.data.LastNotificationErr
	monitor.stateMu.RUnlock()
	if account == nil {
		t.Fatal("failure account was not created")
	}
	if !account.LastAlertAttemptAt.IsZero() || account.LastAlertAttemptGeneration != 0 {
		t.Fatalf("rejected queue admission recorded a five-minute suppression attempt: %+v", account)
	}
	if lastError == "" {
		t.Fatal("rejected queue admission was not surfaced in notifier state")
	}
	if mock.count() != 0 {
		t.Fatalf("stopped dispatcher unexpectedly delivered %d notifications", mock.count())
	}
}

func TestUnattemptedFailureClearsExactSuppressionMarker(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	cfg := config.Default()
	cfg.Enabled = true
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.NotificationCoalesceWindow = time.Hour
	client := notifier.NewClient(cfg, mock.endpoint, mock.server.Client())
	dispatcher := notifier.NewDispatcher(client, 4, cfg.NotificationCoalesceWindow)
	monitor := New(cfg, host, client, dispatcher)
	t.Cleanup(monitor.Stop)
	attemptAt := time.Now().UTC()
	monitor.now = func() time.Time { return attemptAt }

	if err := monitor.Reconcile(context.Background(), "accepted-before-start"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	queued := *monitor.data.Accounts["claude:one"]
	monitor.stateMu.RUnlock()
	if !queued.LastAlertAttemptAt.Equal(attemptAt) || queued.LastAlertAttemptGeneration != queued.IncidentGeneration {
		t.Fatalf("accepted job did not record its suppression marker: %+v", queued)
	}

	dispatcher.Stop()
	monitor.stateMu.RLock()
	account := *monitor.data.Accounts["claude:one"]
	lastError := monitor.data.LastNotificationErr
	monitor.stateMu.RUnlock()
	if !account.LastAlertAttemptAt.IsZero() || account.LastAlertAttemptGeneration != 0 {
		t.Fatalf("unattempted job retained a five-minute suppression marker: %+v", account)
	}
	if account.AlertSent || !account.LastAlertAt.IsZero() || lastError != "" {
		t.Fatalf("unattempted job changed delivery state: account=%+v lastError=%q", account, lastError)
	}
	if mock.count() != 0 {
		t.Fatalf("unattempted job made %d HTTP requests", mock.count())
	}
}

func TestBoundedStopDoesNotWaitForStateLockAndCarriesAbandonmentForward(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := config.Default()
	cfg.Enabled = true
	cfg.StateFile = statePath
	cfg.NotificationCoalesceWindow = time.Hour
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	client := notifier.NewClient(cfg, mock.endpoint, mock.server.Client())
	dispatcher := notifier.NewDispatcher(client, 4, cfg.NotificationCoalesceWindow)
	monitor := New(cfg, host, client, dispatcher)
	attemptAt := time.Now().UTC()
	monitor.now = func() time.Time { return attemptAt }
	if err := monitor.Reconcile(context.Background(), "queued-before-start"); err != nil {
		t.Fatal(err)
	}

	monitor.stateMu.Lock()
	stopDone := make(chan bool, 1)
	go func() { stopDone <- monitor.StopWithin(20 * time.Millisecond) }()
	select {
	case <-stopDone:
	case <-time.After(500 * time.Millisecond):
		monitor.stateMu.Unlock()
		<-stopDone
		t.Fatal("bounded stop waited indefinitely for the monitor state lock")
	}

	loaded, err := (state.Store{Path: statePath}).Load()
	if err != nil {
		monitor.stateMu.Unlock()
		t.Fatal(err)
	}
	account := loaded.Accounts[health.AccountKey("claude", "one")]
	if account == nil || !account.LastAlertAttemptAt.IsZero() || account.LastAlertAttemptGeneration != 0 {
		monitor.stateMu.Unlock()
		t.Fatalf("replacement load retained an abandoned suppression marker: %+v", account)
	}
	monitor.stateMu.Unlock()
	monitor.Stop()
}

func TestBoundedStopDoesNotWaitForRetirementSave(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	cfg := config.Default()
	cfg.Enabled = true
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.NotificationCoalesceWindow = time.Hour
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	client := notifier.NewClient(cfg, mock.endpoint, mock.server.Client())
	dispatcher := notifier.NewDispatcher(client, 4, cfg.NotificationCoalesceWindow)
	monitor := New(cfg, host, client, dispatcher)
	if err := monitor.Reconcile(context.Background(), "queued-before-start"); err != nil {
		t.Fatal(err)
	}

	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSave) }) }
	defer release()
	monitor.beforeRetirementSave = func() {
		close(saveStarted)
		<-releaseSave
	}
	stopDone := make(chan bool, 1)
	go func() { stopDone <- monitor.StopWithin(50 * time.Millisecond) }()
	select {
	case <-saveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("retirement save did not start")
	}
	select {
	case <-stopDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("bounded stop waited for retirement filesystem I/O")
	}
	select {
	case <-monitor.retirementDone:
		t.Fatal("retirement completed while its save was still blocked")
	default:
	}
	release()
	monitor.Stop()
}

func TestUnattemptedCallbackClearsOnlyItsExactAttempt(t *testing.T) {
	cfg := config.Default()
	client := notifier.NewClient(cfg, notifier.ProductionEndpoint, nil)
	dispatcher := notifier.NewDispatcher(client, 4, 0)
	monitor := New(cfg, &fakeHost{}, client, dispatcher)
	oldAttempt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	newAttempt := oldAttempt.Add(time.Minute)
	failure := &state.Account{
		AccountKey:                 "claude:failure",
		Health:                     health.ReauthRequired,
		IncidentGeneration:         4,
		LastAlertAttemptAt:         newAttempt,
		LastAlertAttemptGeneration: 4,
	}
	recovery := &state.Account{
		AccountKey:            "claude:recovery",
		Health:                health.Healthy,
		IncidentGeneration:    7,
		RecoveryPendingFrom:   health.ReauthRequired,
		LastRecoveryAttemptAt: newAttempt,
	}
	monitor.data.Accounts[failure.AccountKey] = failure
	monitor.data.Accounts[recovery.AccountKey] = recovery

	monitor.deliveryCallback(failure, "failure", 3, newAttempt)(notifier.DeliveryResult{Unattempted: true})
	monitor.deliveryCallback(recovery, "recovery", 6, newAttempt)(notifier.DeliveryResult{Unattempted: true})
	monitor.deliveryCallback(failure, "failure", 4, oldAttempt)(notifier.DeliveryResult{Unattempted: true})
	monitor.deliveryCallback(recovery, "recovery", 7, oldAttempt)(notifier.DeliveryResult{Unattempted: true})
	if !failure.LastAlertAttemptAt.Equal(newAttempt) || failure.LastAlertAttemptGeneration != 4 {
		t.Fatalf("stale failure callback cleared a newer attempt: %+v", failure)
	}
	if !recovery.LastRecoveryAttemptAt.Equal(newAttempt) {
		t.Fatalf("stale recovery callback cleared a newer attempt: %+v", recovery)
	}

	monitor.deliveryCallback(failure, "failure", 4, newAttempt)(notifier.DeliveryResult{Unattempted: true})
	monitor.deliveryCallback(recovery, "recovery", 7, newAttempt)(notifier.DeliveryResult{Unattempted: true})
	if !failure.LastAlertAttemptAt.IsZero() || failure.LastAlertAttemptGeneration != 0 {
		t.Fatalf("matching failure callback did not clear its attempt: %+v", failure)
	}
	if !recovery.LastRecoveryAttemptAt.IsZero() {
		t.Fatalf("matching recovery callback did not clear its attempt: %+v", recovery)
	}
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })

	newEntry := oauthEntry("new-index", "claude", "same@example.com", "active", "", false)
	host.set(newEntry)
	if err := monitor.Reconcile(context.Background(), "replacement"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	rows := monitor.Snapshot().Accounts
	if len(rows) != 1 || rows[0].AuthIndex != "new-index" || rows[0].Health != health.Healthy {
		t.Fatalf("replacement was not safely correlated: %+v", rows)
	}
}

func TestReplacementUsesUniqueRuntimeIdentityWhenRosterIsLessSpecific(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("old-index", "claude", "same@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	if err := monitor.Reconcile(context.Background(), "old-failure"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })

	rosterEntry := oauthEntry("new-index", "claude", "", "active", "", false)
	rosterEntry.Label = "replacement roster label"
	runtimeEntry := rosterEntry
	runtimeEntry.Email = "same@example.com"
	host.mu.Lock()
	host.roster = []protocol.HostAuthFileEntry{rosterEntry}
	host.runtime = map[string]protocol.HostAuthFileEntry{runtimeEntry.AuthIndex: runtimeEntry}
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "runtime-enriched-replacement"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	rows := monitor.Snapshot().Accounts
	if len(rows) != 1 || rows[0].AuthIndex != "new-index" || rows[0].Health != health.Healthy {
		t.Fatalf("unique runtime identity was not correlated: %+v", rows)
	}
}

func TestRuntimeDuplicateIdentityBlocksRosterOnlyPreCorrelation(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("old-index", "claude", "same@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	if err := monitor.Reconcile(context.Background(), "old-failure"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })

	firstRoster := oauthEntry("new-index-a", "claude", "same@example.com", "active", "", false)
	secondRoster := oauthEntry("new-index-b", "claude", "", "active", "", false)
	secondRoster.Label = "less-specific-roster-entry"
	firstRuntime := firstRoster
	secondRuntime := secondRoster
	secondRuntime.Email = "same@example.com"
	host.mu.Lock()
	host.roster = []protocol.HostAuthFileEntry{firstRoster, secondRoster}
	host.runtime = map[string]protocol.HostAuthFileEntry{
		firstRuntime.AuthIndex:  firstRuntime,
		secondRuntime.AuthIndex: secondRuntime,
	}
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "runtime-duplicate-replacements"); err != nil {
		t.Fatal(err)
	}
	monitor.dispatcher.BeginDrain()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDrain()
	if !monitor.dispatcher.WaitIdle(drainCtx) {
		t.Fatal("runtime duplicate notifications did not drain")
	}

	monitor.stateMu.RLock()
	old := monitor.data.Accounts["claude:old-index"]
	newA := monitor.data.Accounts["claude:new-index-a"]
	newB := monitor.data.Accounts["claude:new-index-b"]
	monitor.stateMu.RUnlock()
	if old == nil || old.Health != health.Removed {
		t.Fatalf("runtime-ambiguous prior account was not retained as removed: %+v", old)
	}
	if newA == nil || newA.Health != health.Healthy || newA.IncidentGeneration != 0 || newA.AlertSent {
		t.Fatalf("roster-specific duplicate inherited prior incident state: %+v", newA)
	}
	if newB == nil || newB.Health != health.Healthy || newB.IncidentGeneration != 0 || newB.AlertSent {
		t.Fatalf("runtime-enriched duplicate inherited prior incident state: %+v", newB)
	}
	if mock.count() != 1 {
		t.Fatalf("runtime duplicate identity sent a spurious recovery: %s", mock.all())
	}
}

func TestDuplicateReplacementIdentityDoesNotArbitrarilyRecover(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("old-index", "claude", "same@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	if err := monitor.Reconcile(context.Background(), "old-failure"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })

	first := oauthEntry("new-index-a", "claude", "same@example.com", "active", "", false)
	second := oauthEntry("new-index-b", "claude", "same@example.com", "active", "", false)
	host.mu.Lock()
	host.roster = []protocol.HostAuthFileEntry{first, second}
	host.runtime[first.AuthIndex] = first
	host.runtime[second.AuthIndex] = second
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "duplicate-replacements"); err != nil {
		t.Fatal(err)
	}
	monitor.dispatcher.BeginDrain()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDrain()
	if !monitor.dispatcher.WaitIdle(drainCtx) {
		t.Fatal("duplicate replacement notifications did not drain")
	}

	monitor.stateMu.RLock()
	old := monitor.data.Accounts["claude:old-index"]
	newA := monitor.data.Accounts["claude:new-index-a"]
	newB := monitor.data.Accounts["claude:new-index-b"]
	monitor.stateMu.RUnlock()
	if old == nil || old.Health != health.Removed {
		t.Fatalf("ambiguous prior account was not retained as removed: %+v", old)
	}
	if newA == nil || newA.Health != health.Healthy || newA.IncidentGeneration != 0 || newA.AlertSent {
		t.Fatalf("first duplicate inherited prior incident state: %+v", newA)
	}
	if newB == nil || newB.Health != health.Healthy || newB.IncidentGeneration != 0 || newB.AlertSent {
		t.Fatalf("second duplicate inherited prior incident state: %+v", newB)
	}
	if mock.count() != 1 {
		t.Fatalf("ambiguous duplicate identity sent a spurious recovery: %s", mock.all())
	}
}

func TestReplacementRuntimeFailurePreservesIncidentUntilRecovery(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("old-index", "claude", "same@example.com", "error", "unauthorized", true))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	if err := monitor.Reconcile(context.Background(), "old-failure"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })

	monitor.stateMu.RLock()
	old := monitor.data.Accounts["claude:old-index"]
	generation := old.IncidentGeneration
	alertAt := old.LastAlertAt
	monitor.stateMu.RUnlock()

	replacement := oauthEntry("new-index", "claude", "same@example.com", "active", "", false)
	unrelated := oauthEntry("old-index", "claude", "other@example.com", "active", "", false)
	host.mu.Lock()
	host.roster = []protocol.HostAuthFileEntry{replacement, unrelated}
	host.runtime[replacement.AuthIndex] = replacement
	host.runtime[unrelated.AuthIndex] = unrelated
	host.runtimeErr[replacement.AuthIndex] = context.DeadlineExceeded
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "replacement-runtime-failed"); err == nil {
		t.Fatal("expected temporary replacement runtime failure")
	}
	monitor.stateMu.RLock()
	preserved := monitor.data.Accounts["claude:new-index"]
	reused := monitor.data.Accounts["claude:old-index"]
	monitor.stateMu.RUnlock()
	if preserved == nil || preserved.Health != health.ReauthRequired || !preserved.AlertSent || preserved.IncidentGeneration != generation || !preserved.LastAlertAt.Equal(alertAt) {
		t.Fatalf("temporary replacement read failure discarded incident state: %+v", preserved)
	}
	if reused == nil || reused.Health != health.Healthy || reused.AlertSent {
		t.Fatalf("unrelated account reusing the old key inherited incident state: %+v", reused)
	}

	host.mu.Lock()
	delete(host.runtimeErr, replacement.AuthIndex)
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "replacement-runtime-recovered"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	monitor.stateMu.RLock()
	recovered := monitor.data.Accounts["claude:new-index"]
	reused = monitor.data.Accounts["claude:old-index"]
	monitor.stateMu.RUnlock()
	if recovered == nil || recovered.Health != health.Healthy || reused == nil || reused.Health != health.Healthy || !strings.Contains(mock.all(), "account recovered") {
		t.Fatalf("replacement recovery did not preserve and close the incident: recovered=%+v reused=%+v messages=%s", recovered, reused, mock.all())
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

func TestUsageSignalIsNonBlockingAndBurstCoalesced(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.StartupGrace = 0
		cfg.UsageRecheckDelay = 20 * time.Millisecond
	})
	waitFor(t, 5*time.Second, func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.listCalls == 1
	})
	for i := 0; i < 100; i++ {
		if !monitor.ObserveUsageFailure("one", 503) {
			t.Fatal("usage failure was unexpectedly rejected")
		}
	}
	waitFor(t, 5*time.Second, func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.listCalls == 2
	})
	host.mu.Lock()
	calls := host.listCalls
	host.mu.Unlock()
	if calls != 2 {
		t.Fatalf("event burst caused %d total reconciliations; want baseline plus one batch", calls)
	}
}

func TestRepeatedReplacementCorrelationKeepsOneLogicalAccount(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))

	for _, index := range []string{"index-a", "index-b", "index-a"} {
		host.set(oauthEntry(index, "claude", "same@example.com", "active", "", false))
		if err := monitor.Reconcile(context.Background(), "replacement"); err != nil {
			t.Fatal(err)
		}
	}

	rows := monitor.Snapshot().Accounts
	if len(rows) != 1 || rows[0].AuthIndex != "index-a" || rows[0].Health != health.Healthy {
		t.Fatalf("unexpected correlated account rows: %+v", rows)
	}
}

func TestOldKeyReuseDoesNotInheritUncorrelatedIncidentState(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))

	host.set(oauthEntry("index-x", "claude", "account-a@example.com", "error", "unauthorized", true))
	if err := monitor.Reconcile(context.Background(), "original-failure"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })
	monitor.stateMu.RLock()
	original := monitor.data.Accounts["claude:index-x"]
	monitor.stateMu.RUnlock()

	host.set(oauthEntry("index-x", "claude", "account-c@example.com", "active", "", false))
	if err := monitor.Reconcile(context.Background(), "unrelated-key-reuse"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	reused := monitor.data.Accounts["claude:index-x"]
	monitor.stateMu.RUnlock()
	if reused == nil || reused == original || reused.Health != health.Healthy || reused.AlertSent || reused.IncidentGeneration != 0 || reused.RecoveryPendingFrom != "" {
		t.Fatalf("unrelated key reuse inherited prior incident state: original=%+v reused=%+v", original, reused)
	}
	if mock.count() != 1 {
		t.Fatalf("unrelated key reuse sent a spurious recovery: %s", mock.all())
	}
}

func TestQueuedIncidentSurvivesReplacementAndOldKeyReuse(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.NotificationCoalesceWindow = time.Hour
	})

	accountAOld := oauthEntry("index-x", "claude", "account-a@example.com", "error", "unauthorized", true)
	host.set(accountAOld)
	if err := monitor.Reconcile(context.Background(), "account-a-old-key"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	originalAccount := monitor.data.Accounts["claude:index-x"]
	originalGeneration := originalAccount.IncidentGeneration
	monitor.stateMu.RUnlock()
	if mock.count() != 0 {
		t.Fatal("queued incident delivered before replacement correlation was exercised")
	}

	accountANew := oauthEntry("index-y", "claude", "account-a@example.com", "error", "unauthorized", true)
	accountC := oauthEntry("index-x", "claude", "account-c@example.com", "active", "", false)
	host.mu.Lock()
	host.roster = []protocol.HostAuthFileEntry{accountANew, accountC}
	host.runtime[accountANew.AuthIndex] = accountANew
	host.runtime[accountC.AuthIndex] = accountC
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "replacement-and-old-key-reuse"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	movedAccount := monitor.data.Accounts["claude:index-y"]
	unrelatedBeforeDelivery := monitor.data.Accounts["claude:index-x"]
	monitor.stateMu.RUnlock()
	if movedAccount == nil || movedAccount != originalAccount {
		t.Fatalf("replacement did not retain the original queued logical account: original=%p moved=%p", originalAccount, movedAccount)
	}
	if movedAccount.IncidentGeneration != originalGeneration {
		t.Fatalf("replacement changed queued incident generation: got=%d want=%d", movedAccount.IncidentGeneration, originalGeneration)
	}
	if unrelatedBeforeDelivery == nil || unrelatedBeforeDelivery == originalAccount || unrelatedBeforeDelivery.Health != health.Healthy {
		t.Fatalf("old-key reuse did not create an unrelated healthy account: %+v", unrelatedBeforeDelivery)
	}
	monitor.dispatcher.BeginDrain()

	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool {
		monitor.stateMu.RLock()
		defer monitor.stateMu.RUnlock()
		account := monitor.data.Accounts["claude:index-y"]
		return account != nil && account.AlertSent
	})
	if messages := mock.all(); !strings.Contains(messages, "account-a@example.com") || strings.Contains(messages, "account-c@example.com") {
		t.Fatalf("queued incident was delivered for the wrong logical account: %s", messages)
	}
	monitor.stateMu.RLock()
	accountA := monitor.data.Accounts["claude:index-y"]
	unrelatedC := monitor.data.Accounts["claude:index-x"]
	monitor.stateMu.RUnlock()
	if accountA == nil || !accountA.AlertSent || accountA.Health != health.ReauthRequired {
		t.Fatalf("replacement account did not receive its queued delivery state: %+v", accountA)
	}
	if unrelatedC == nil || unrelatedC.AlertSent || unrelatedC.Health != health.Healthy {
		t.Fatalf("reused old key was mutated by another account's callback: %+v", unrelatedC)
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })

	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	if err := monitor.Reconcile(context.Background(), "recovery-queued"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	account := monitor.data.Accounts["claude:one"]
	oldGeneration := account.IncidentGeneration
	monitor.stateMu.RUnlock()

	current = current.Add(time.Second)
	host.set(oauthEntry("one", "claude", "a@example.com", "error", "unauthorized", true))
	if err := monitor.Reconcile(context.Background(), "failed-again"); err != nil {
		t.Fatal(err)
	}
	monitor.deliveryCallback(account, "recovery", oldGeneration, time.Time{})(notifier.DeliveryResult{Accepted: true, At: current.Add(time.Second)})

	waitFor(t, 15*time.Second, func() bool { return mock.count() == 2 })
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() >= 1 })
	if count := mock.count(); count != 1 {
		t.Fatalf("initial failure was delivered %d times, want exactly once", count)
	}
	waitFor(t, 5*time.Second, func() bool { return !monitor.Snapshot().Accounts[0].LastAlertAt.IsZero() })

	current = current.Add(13 * time.Hour)
	if err := monitor.Reconcile(context.Background(), "reminder-one"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	firstAttempt := monitor.data.Accounts["claude:one"].LastAlertAttemptAt
	monitor.stateMu.RUnlock()
	if err := monitor.Reconcile(context.Background(), "reminder-two"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	secondAttempt := monitor.data.Accounts["claude:one"].LastAlertAttemptAt
	monitor.stateMu.RUnlock()
	if !secondAttempt.Equal(firstAttempt) {
		t.Fatalf("duplicate in-flight reminder was queued: first=%s second=%s", firstAttempt, secondAttempt)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() >= 2 })
	if count := mock.count(); count != 2 {
		t.Fatalf("reminder was delivered %d times total, want exactly twice", count)
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	host.mu.Lock()
	host.roster = nil
	host.mu.Unlock()
	if err := monitor.Reconcile(context.Background(), "removed"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
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
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	if mock.lastPriority() != "-1" || !strings.Contains(mock.all(), "Management: https://cpa.example.test/management") {
		t.Fatalf("priority=%q message=%s", mock.lastPriority(), mock.all())
	}
}

func TestStructured429NeverBecomesCredentialFailure(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	entry := oauthEntry("one", "claude", "a@example.com", "error", `{"opaque":"raw provider response"}`, true)
	entry.NextRetryAfter = time.Now().UTC().Add(time.Hour)
	host.set(entry)
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	monitor.ObserveUsageFailure("one", 429)
	if err := monitor.Reconcile(context.Background(), "429"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0]; got.Health != health.QuotaLimited || got.ReasonCode != string(health.ReasonHTTP429Quota) {
		t.Fatalf("429 classification=%+v", got)
	}
	current = current.Add(30 * time.Minute)
	entry.NextRetryAfter = current.Add(time.Hour)
	host.set(entry)
	monitor.ObserveUsageFailure("one", 429)
	if err := monitor.Reconcile(context.Background(), "429-still-limited"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0].Health; got != health.QuotaLimited {
		t.Fatalf("429 promoted to %q", got)
	}
	if mock.count() != 0 {
		t.Fatalf("quota limitation sent a credential alert: %s", mock.all())
	}
}

func TestAvailableModelSuccessClears429WithoutPromotingResidualError(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	entry := oauthEntry("one", "claude", "a@example.com", "error", `{"model":"fable","status":"quota"}`, true)
	host.set(entry)
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.NotificationCoalesceWindow = 0
		cfg.TransientConfirmAfter = 10 * time.Minute
	})
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	monitor.ObserveUsageFailure("one", 429)
	if err := monitor.Reconcile(context.Background(), "fable-429"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0].Health; got != health.QuotaLimited {
		t.Fatalf("initial model-scoped 429 health=%q", got)
	}

	current = current.Add(time.Minute)
	entry.Unavailable = false
	entry.Status = "error"
	entry.StatusMessage = `{"model":"fable","status":"quota","opus":"succeeded"}`
	entry.NextRetryAfter = time.Time{}
	entry.UpdatedAt = current
	entry.Success = 1
	host.set(entry)
	if err := monitor.Reconcile(context.Background(), "opus-success"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0]; got.Health != health.Suspect || got.ReasonCode != string(health.ReasonAvailableResidualError) {
		t.Fatalf("available residual status=%+v", got)
	}

	current = current.Add(11 * time.Minute)
	entry.UpdatedAt = current
	host.set(entry)
	if err := monitor.Reconcile(context.Background(), "residual-still-present"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0]; got.Health != health.Suspect || got.ReasonCode != string(health.ReasonAvailableResidualError) {
		t.Fatalf("available residual error promoted to account failure: %+v", got)
	}
	if mock.count() != 0 {
		t.Fatalf("available residual model error sent an account alert: %s", mock.all())
	}
}

func TestRequest401RequiresConfirmationAndRefreshSuccessWins(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	monitor.ObserveUsageFailure("one", 401)
	if err := monitor.Reconcile(context.Background(), "refresh-completed"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0].Health; got != health.Healthy {
		t.Fatalf("successful CPA refresh did not override request 401: %q", got)
	}
	if mock.count() != 0 {
		t.Fatal("single recovered request 401 sent an alert")
	}
}

func TestPersistentRequest401ConfirmsAsReauthRequired(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	entry := oauthEntry("one", "codex", "a@example.com", "error", `{"raw":"401 response"}`, true)
	host.set(entry)
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.NotificationCoalesceWindow = 0
		cfg.UnauthorizedConfirmAfter = time.Minute
	})
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	entry.NextRetryAfter = current.Add(5 * time.Minute)
	host.set(entry)
	monitor.ObserveUsageFailure("one", 401)
	if err := monitor.Reconcile(context.Background(), "first-401"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0].Health; got != health.Suspect {
		t.Fatalf("first 401 health=%q", got)
	}
	current = current.Add(time.Minute + time.Second)
	entry.NextRetryAfter = current.Add(5 * time.Minute)
	host.set(entry)
	monitor.ObserveUsageFailure("one", 401)
	if err := monitor.Reconcile(context.Background(), "confirmed-401"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	if got := monitor.Snapshot().Accounts[0]; got.Health != health.ReauthRequired || got.ReasonCode != string(health.ReasonPersistentUnauthorized) {
		t.Fatalf("persistent 401 status=%+v", got)
	}

	current = current.Add(30 * time.Second)
	entry.NextRetryAfter = current.Add(5 * time.Minute)
	host.set(entry)
	monitor.ObserveUsageFailure("one", 401)
	if err := monitor.Reconcile(context.Background(), "still-confirmed-401"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0].Health; got != health.ReauthRequired {
		t.Fatalf("confirmed 401 regressed to %q", got)
	}

	current = current.Add(3 * time.Minute)
	if err := monitor.Reconcile(context.Background(), "401-evidence-expired"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0].Health; got != health.ReauthRequired {
		t.Fatalf("ambiguous cooldown observation cleared confirmed reauth state: %q", got)
	}
	if mock.count() != 1 {
		t.Fatalf("stable confirmed 401 duplicated its alert: %s", mock.all())
	}
}

func TestUnknownFutureCooldownNeverPromotes(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	entry := oauthEntry("one", "claude", "a@example.com", "error", `{"raw":"unknown body"}`, true)
	host.set(entry)
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	entry.NextRetryAfter = current.Add(24 * time.Hour)
	host.set(entry)
	if err := monitor.Reconcile(context.Background(), "unknown-cooldown"); err != nil {
		t.Fatal(err)
	}
	current = current.Add(12 * time.Hour)
	if err := monitor.Reconcile(context.Background(), "unknown-cooldown-later"); err != nil {
		t.Fatal(err)
	}
	if got := monitor.Snapshot().Accounts[0]; got.Health != health.Suspect || got.ReasonCode != string(health.ReasonCooldownActive) {
		t.Fatalf("unknown cooldown status=%+v", got)
	}
	if mock.count() != 0 {
		t.Fatalf("unknown cooldown promoted and alerted: %s", mock.all())
	}
}

func TestChangingSuspicionClassResetsConfirmationClock(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	entry := oauthEntry("one", "claude", "a@example.com", "error", `{"raw":"failure"}`, true)
	host.set(entry)
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.TransientConfirmAfter = 10 * time.Minute
		cfg.UnauthorizedConfirmAfter = time.Minute
	})
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	monitor.ObserveUsageFailure("one", 503)
	if err := monitor.Reconcile(context.Background(), "503"); err != nil {
		t.Fatal(err)
	}
	current = current.Add(9*time.Minute + 59*time.Second)
	entry.NextRetryAfter = current.Add(5 * time.Minute)
	host.set(entry)
	monitor.ObserveUsageFailure("one", 401)
	if err := monitor.Reconcile(context.Background(), "401"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	account := *monitor.data.Accounts["claude:one"]
	monitor.stateMu.RUnlock()
	if account.Health != health.Suspect || account.SuspectClass != health.ConfirmationUnauthorized || !account.SuspectSince.Equal(current) {
		t.Fatalf("suspicion clock did not reset: %+v", account)
	}
	if mock.count() != 0 {
		t.Fatal("changed suspicion class immediately alerted")
	}
}

func TestChangingReasonWithinTransientClassDoesNotResetConfirmation(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	entry := oauthEntry("one", "claude", "a@example.com", "error", `{"raw":"failure"}`, true)
	host.set(entry)
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.TransientConfirmAfter = 10 * time.Minute
		cfg.NotificationCoalesceWindow = 0
	})
	current := time.Now().UTC()
	monitor.now = func() time.Time { return current }
	monitor.ObserveUsageFailure("one", 503)
	if err := monitor.Reconcile(context.Background(), "503"); err != nil {
		t.Fatal(err)
	}

	current = current.Add(9*time.Minute + 59*time.Second)
	monitor.ObserveUsageFailure("one", 502)
	if err := monitor.Reconcile(context.Background(), "502-same-class"); err != nil {
		t.Fatal(err)
	}
	monitor.stateMu.RLock()
	suspectSince := monitor.data.Accounts["claude:one"].SuspectSince
	monitor.stateMu.RUnlock()
	if !suspectSince.Equal(current.Add(-9*time.Minute - 59*time.Second)) {
		t.Fatalf("same-class reason change reset confirmation clock to %s", suspectSince)
	}

	current = current.Add(2 * time.Second)
	monitor.ObserveUsageFailure("one", 502)
	if err := monitor.Reconcile(context.Background(), "502-confirmed"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	if got := monitor.Snapshot().Accounts[0]; got.Health != health.CredentialDown || got.ReasonCode != string(health.ReasonPersistentHTTP502) {
		t.Fatalf("same-class persistent outage did not confirm: %+v", got)
	}
}

func TestLoopRunsStartupAndPeriodicReconciliations(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.StartupGrace = 0
		cfg.ScanInterval = 20 * time.Millisecond
	})
	waitFor(t, 5*time.Second, func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.listCalls >= 3
	})
	var status Status
	waitFor(t, 5*time.Second, func() bool {
		status = monitor.Snapshot()
		return !status.LastScan.IsZero() && !status.NextScan.IsZero() && status.NextScan.After(status.LastScan)
	})
	if mock.count() != 0 {
		t.Fatalf("healthy periodic scans sent notifications: %s", mock.all())
	}
}

func TestUsageSignalCannotBypassStartupGrace(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		cfg.StartupGrace = 150 * time.Millisecond
		cfg.UsageRecheckDelay = 20 * time.Millisecond
	})
	monitor.ObserveUsageFailure("one", 401)
	time.Sleep(50 * time.Millisecond)
	host.mu.Lock()
	beforeGrace := host.listCalls
	host.mu.Unlock()
	if beforeGrace != 0 {
		t.Fatalf("usage signal bypassed startup grace with %d scans", beforeGrace)
	}
	waitFor(t, 5*time.Second, func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.listCalls == 1
	})
}

type concurrencyHost struct {
	entries  []protocol.HostAuthFileEntry
	mu       sync.Mutex
	calls    map[string]int
	inFlight int
	max      int
	started  chan string
	release  chan struct{}
}

func (h *concurrencyHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return append([]protocol.HostAuthFileEntry(nil), h.entries...), nil
}

func (h *concurrencyHost) GetRuntime(ctx context.Context, authIndex string) (protocol.HostAuthFileEntry, error) {
	h.mu.Lock()
	h.inFlight++
	if h.inFlight > h.max {
		h.max = h.inFlight
	}
	h.calls[authIndex]++
	var entry protocol.HostAuthFileEntry
	for _, candidate := range h.entries {
		if candidate.AuthIndex == authIndex {
			entry = candidate
			break
		}
	}
	h.mu.Unlock()
	h.started <- authIndex
	select {
	case <-ctx.Done():
		return protocol.HostAuthFileEntry{}, ctx.Err()
	case <-h.release:
	}
	h.mu.Lock()
	h.inFlight--
	h.mu.Unlock()
	return entry, nil
}

func (h *concurrencyHost) Log(context.Context, string, string, map[string]any) {}

func TestReconcileManyAccountsRespectsConcurrencyLimit(t *testing.T) {
	entries := make([]protocol.HostAuthFileEntry, 17)
	for i := range entries {
		entries[i] = oauthEntry(fmt.Sprintf("account-%02d", i), "claude", fmt.Sprintf("user-%02d@example.com", i), "active", "", false)
	}
	host := &concurrencyHost{entries: entries, calls: make(map[string]int), started: make(chan string, len(entries)), release: make(chan struct{})}
	mock := newMockPushover(t)
	cfg := config.Default()
	cfg.Enabled = true
	cfg.StartupGrace = 24 * time.Hour
	cfg.ScanInterval = 24 * time.Hour
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	cfg.MaxConcurrentChecks = 3
	cfg.NotificationCoalesceWindow = 0
	client := notifier.NewClient(cfg, mock.endpoint, mock.server.Client())
	dispatcher := notifier.NewDispatcher(client, 64, 0)
	monitor := New(cfg, host, client, dispatcher)
	monitor.Start()
	t.Cleanup(monitor.Stop)
	done := make(chan error, 1)
	go func() { done <- monitor.Reconcile(context.Background(), "many") }()
	for i := 0; i < cfg.MaxConcurrentChecks; i++ {
		select {
		case <-host.started:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent runtime reads did not start")
		}
	}
	host.mu.Lock()
	maxBeforeRelease := host.max
	host.mu.Unlock()
	if maxBeforeRelease != cfg.MaxConcurrentChecks {
		t.Fatalf("max concurrency before release=%d want=%d", maxBeforeRelease, cfg.MaxConcurrentChecks)
	}
	close(host.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("many-account reconciliation did not finish")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.max > cfg.MaxConcurrentChecks || len(host.calls) != len(entries) {
		t.Fatalf("max=%d calls=%d want max<=%d calls=%d", host.max, len(host.calls), cfg.MaxConcurrentChecks, len(entries))
	}
	for index, count := range host.calls {
		if count != 1 {
			t.Fatalf("account %s checked %d times", index, count)
		}
	}
	if rows := monitor.Snapshot().Accounts; len(rows) != len(entries) {
		t.Fatalf("status rows=%d want=%d", len(rows), len(entries))
	}
}

func TestIndependentAccountsEachAlertOnce(t *testing.T) {
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	for _, entry := range []protocol.HostAuthFileEntry{
		oauthEntry("one", "claude", "one@example.com", "error", "unauthorized", true),
		oauthEntry("two", "claude", "two@example.com", "error", "invalid_grant", true),
		oauthEntry("three", "codex", "three@example.com", "active", "", false),
	} {
		host.roster = append(host.roster, entry)
		host.runtime[entry.AuthIndex] = entry
	}
	mock := newMockPushover(t)
	monitor := newTestMonitor(t, host, mock, filepath.Join(t.TempDir(), "state.json"))
	if err := monitor.Reconcile(context.Background(), "many-incidents"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	waitFor(t, 5*time.Second, func() bool {
		rows := monitor.Snapshot().Accounts
		return len(rows) == 3 && !rows[0].LastAlertAt.IsZero() && !rows[1].LastAlertAt.IsZero()
	})
	if err := monitor.Reconcile(context.Background(), "unchanged"); err != nil {
		t.Fatal(err)
	}
	if mock.count() != 2 {
		t.Fatalf("independent unchanged incidents duplicated: %s", mock.all())
	}
}

func TestMonitorLogsAndStateExcludeSecretSentinels(t *testing.T) {
	accountSecret := "RAW-ACCOUNT-SECRET-SENTINEL"
	appSecret := strings.Repeat("S", 30)
	userSecret := strings.Repeat("T", 30)
	t.Setenv(config.DefaultAppTokenEnv, appSecret)
	t.Setenv(config.DefaultUserKeyEnv, userSecret)
	host := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	entry := oauthEntry("one", "claude", "safe@example.com", "active", "", false)
	entry.Account = accountSecret
	host.set(entry)
	mock := newMockPushover(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	monitor := newTestMonitor(t, host, mock, statePath)
	if err := monitor.Reconcile(context.Background(), "secret-check"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "claude:one") {
		t.Fatal("state fixture is unexpectedly empty")
	}
	for _, secret := range []string{accountSecret, appSecret, userSecret} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("state leaked secret sentinel %q", secret)
		}
	}

	blockingParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	failing := newConfiguredTestMonitor(t, host, mock, func(cfg *config.Config) {
		cfg.StateFile = filepath.Join(blockingParent, "state.json")
		cfg.NotificationCoalesceWindow = 0
	})
	_ = failing.Reconcile(context.Background(), "trigger-without-secret")
	host.mu.Lock()
	logs, err := json.Marshal(host.logs)
	host.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) <= 2 {
		t.Fatal("expected at least one operational log entry")
	}
	for _, secret := range []string{accountSecret, appSecret, userSecret} {
		if strings.Contains(string(logs), secret) {
			t.Fatalf("logs leaked secret sentinel %q: %s", secret, logs)
		}
	}
}
