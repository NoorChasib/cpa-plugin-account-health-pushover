package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/monitor"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/notifier"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

type pluginFakeHost struct {
	mu        sync.Mutex
	entry     protocol.HostAuthFileEntry
	listCalls int
}

func (h *pluginFakeHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listCalls++
	if h.entry.AuthIndex == "" {
		return nil, nil
	}
	return []protocol.HostAuthFileEntry{h.entry}, nil
}

func (h *pluginFakeHost) GetRuntime(context.Context, string) (protocol.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.entry, nil
}

func (h *pluginFakeHost) Log(context.Context, string, string, map[string]any) {}

func configurePlugin(t *testing.T, host *pluginFakeHost, endpoint string) *Plugin {
	t.Helper()
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	p := New(host, endpoint)
	request, err := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(
		"enabled: true\nstartup-grace: 24h\nscan-interval: 24h\nnotification-coalesce-window: 0\nstate-file: " + filepath.Join(t.TempDir(), "state.json") + "\n",
	), SchemaVersion: protocol.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, request); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Shutdown)
	return p
}

type reconfigureBlockingHost struct {
	mu      sync.Mutex
	entry   protocol.HostAuthFileEntry
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *reconfigureBlockingHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return []protocol.HostAuthFileEntry{h.entry}, nil
}

func (h *reconfigureBlockingHost) GetRuntime(context.Context, string) (protocol.HostAuthFileEntry, error) {
	h.once.Do(func() { close(h.started) })
	<-h.release
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.entry, nil
}

func (*reconfigureBlockingHost) Log(context.Context, string, string, map[string]any) {}

type expiredRetryHost struct {
	mu           sync.Mutex
	entry        protocol.HostAuthFileEntry
	listCalls    int
	runtimeCalls int
	firstStarted chan struct{}
	firstExpired chan struct{}
	releaseFirst chan struct{}
}

func (h *expiredRetryHost) ListAuth(ctx context.Context) ([]protocol.HostAuthFileEntry, error) {
	h.mu.Lock()
	h.listCalls++
	call := h.listCalls
	entry := h.entry
	h.mu.Unlock()
	if call > 1 && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return []protocol.HostAuthFileEntry{entry}, nil
}

func (h *expiredRetryHost) GetRuntime(ctx context.Context, _ string) (protocol.HostAuthFileEntry, error) {
	h.mu.Lock()
	h.runtimeCalls++
	call := h.runtimeCalls
	entry := h.entry
	h.mu.Unlock()
	if call == 1 {
		close(h.firstStarted)
		<-ctx.Done()
		close(h.firstExpired)
		<-h.releaseFirst
	}
	return entry, nil
}

func (*expiredRetryHost) Log(context.Context, string, string, map[string]any) {}

type deadlineListHost struct{}

func (*deadlineListHost) ListAuth(ctx context.Context) ([]protocol.HostAuthFileEntry, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*deadlineListHost) GetRuntime(context.Context, string) (protocol.HostAuthFileEntry, error) {
	return protocol.HostAuthFileEntry{}, errors.New("unexpected runtime read")
}

func (*deadlineListHost) Log(context.Context, string, string, map[string]any) {}

type deadlineRuntimeHost struct {
	entry protocol.HostAuthFileEntry
}

func (h *deadlineRuntimeHost) ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error) {
	return []protocol.HostAuthFileEntry{h.entry}, nil
}

func (*deadlineRuntimeHost) GetRuntime(ctx context.Context, _ string) (protocol.HostAuthFileEntry, error) {
	<-ctx.Done()
	return protocol.HostAuthFileEntry{}, ctx.Err()
}

func (*deadlineRuntimeHost) Log(context.Context, string, string, map[string]any) {}

type pluginCancellationIgnoringTransport struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (t *pluginCancellationIgnoringTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.once.Do(func() { close(t.started) })
	<-t.release
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"status":1}`)),
		Request:    request,
	}, nil
}

func TestReconfigureWaitsForManagementCheckAndDeliversItsAlert(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	host := &reconfigureBlockingHost{
		entry: protocol.HostAuthFileEntry{
			AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com", Status: "active",
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	notification := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		notification <- struct{}{}
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()

	p := New(host, server.URL)
	t.Cleanup(p.Shutdown)
	statePath := filepath.Join(t.TempDir(), "state.json")
	lifecycleRequest, err := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(
		"enabled: true\nstartup-grace: 24h\nscan-interval: 24h\nstate-file: " + statePath + "\n",
	), SchemaVersion: protocol.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycleRequest); err != nil {
		t.Fatal(err)
	}

	checkRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/check"})
	checkDone := make(chan error, 1)
	go func() {
		_, err := p.Handle(protocol.MethodManagementHandle, checkRequest)
		checkDone <- err
	}()
	select {
	case <-host.started:
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not block in the old runtime callback")
	}

	reconfigureDone := make(chan error, 1)
	go func() {
		_, err := p.Handle(protocol.MethodPluginReconfigure, lifecycleRequest)
		reconfigureDone <- err
	}()
	select {
	case err := <-reconfigureDone:
		t.Fatalf("reconfigure returned before the old management check: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.RLock()
		current := p.monitor
		p.mu.RUnlock()
		if current == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reconfigure published a replacement monitor before draining the old one")
		}
		time.Sleep(5 * time.Millisecond)
	}

	host.mu.Lock()
	host.entry.Status = "error"
	host.entry.StatusMessage = "unauthorized"
	host.entry.Unavailable = true
	host.mu.Unlock()
	close(host.release)
	select {
	case err := <-checkDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not finish after runtime release")
	}
	select {
	case <-notification:
	case <-time.After(5 * time.Second):
		t.Fatal("alert from the admitted old-monitor check was stranded during reconfigure")
	}
	select {
	case err := <-reconfigureDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconfigure did not finish after the management check")
	}
}

func TestReconfigureDetachesStuckHostCallbackAndPublishesReplacement(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	host := &reconfigureBlockingHost{
		entry: protocol.HostAuthFileEntry{
			AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com", Status: "active",
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(host.release) }) }
	defer release()
	p := New(host, "")
	p.monitorStopTimeout = 20 * time.Millisecond
	lifecycleRequest, err := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(
		"enabled: true\nstartup-grace: 24h\nscan-interval: 24h\nstate-file: " + filepath.Join(t.TempDir(), "state.json") + "\n",
	), SchemaVersion: protocol.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycleRequest); err != nil {
		t.Fatal(err)
	}
	p.mu.RLock()
	oldMonitor := p.monitor
	p.mu.RUnlock()

	checkRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/check"})
	checkDone := make(chan error, 1)
	go func() {
		_, err := p.Handle(protocol.MethodManagementHandle, checkRequest)
		checkDone <- err
	}()
	select {
	case <-host.started:
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not enter the stuck host callback")
	}

	reconfigureDone := make(chan error, 1)
	go func() {
		_, err := p.Handle(protocol.MethodPluginReconfigure, lifecycleRequest)
		reconfigureDone <- err
	}()
	select {
	case err := <-reconfigureDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconfigure hung behind a host callback that ignored cancellation")
	}
	p.mu.RLock()
	replacement := p.monitor
	p.mu.RUnlock()
	if replacement == nil || replacement == oldMonitor {
		t.Fatal("reconfigure did not publish a replacement monitor")
	}
	if len(p.detached) != 1 || p.detached[0] != oldMonitor {
		t.Fatalf("stuck old monitor was not retained for final shutdown: %+v", p.detached)
	}

	release()
	select {
	case err := <-checkDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not unwind after host release")
	}
	shutdownDone := make(chan struct{})
	go func() {
		p.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("final shutdown did not reap the detached monitor")
	}
}

func TestQuiesceDetachesStuckHostCallbackButFinalShutdownWaits(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	host := &reconfigureBlockingHost{
		entry: protocol.HostAuthFileEntry{
			AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com", Status: "active",
		},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(host.release) }) }
	defer release()
	p := New(host, "")
	p.monitorStopTimeout = 20 * time.Millisecond
	lifecycleRequest, err := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(
		"enabled: true\nstartup-grace: 24h\nscan-interval: 24h\nstate-file: " + filepath.Join(t.TempDir(), "state.json") + "\n",
	), SchemaVersion: protocol.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycleRequest); err != nil {
		t.Fatal(err)
	}

	checkRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/check"})
	checkDone := make(chan error, 1)
	go func() {
		_, err := p.Handle(protocol.MethodManagementHandle, checkRequest)
		checkDone <- err
	}()
	select {
	case <-host.started:
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not enter the stuck host callback")
	}
	quiesceDone := make(chan error, 1)
	go func() {
		_, err := p.Handle(protocol.MethodPluginQuiesce, nil)
		quiesceDone <- err
	}()
	select {
	case err := <-quiesceDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quiesce hung behind a host callback that ignored cancellation")
	}
	p.mu.RLock()
	current := p.monitor
	p.mu.RUnlock()
	if current != nil || len(p.detached) != 1 {
		t.Fatalf("quiesce did not detach the stuck monitor: current=%p detached=%d", current, len(p.detached))
	}

	finalWaitStarted := make(chan struct{})
	p.beforeFinalMonitorWait = func() { close(finalWaitStarted) }
	shutdownDone := make(chan struct{})
	go func() {
		p.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-finalWaitStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("final shutdown did not reach the detached-monitor wait")
	}
	select {
	case <-shutdownDone:
		t.Fatal("final shutdown returned while the detached host callback was in flight")
	default:
	}

	release()
	select {
	case err := <-checkDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not unwind after host release")
	}
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("final shutdown did not finish after host release")
	}
}

func TestQuiesceRetainsDrainedMonitorUntilDeliveryWorkerExits(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	host := &pluginFakeHost{entry: protocol.HostAuthFileEntry{
		AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com",
		Status: "error", StatusMessage: "unauthorized", Unavailable: true,
	}}
	cfg := config.Default()
	cfg.Enabled = true
	cfg.StartupGrace = 24 * time.Hour
	cfg.ScanInterval = 24 * time.Hour
	cfg.NotificationCoalesceWindow = 0
	cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
	transport := &pluginCancellationIgnoringTransport{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(transport.release) }) }
	defer release()
	client := notifier.NewClient(cfg, notifier.ProductionEndpoint, &http.Client{Transport: transport})
	dispatcher := notifier.NewDispatcher(client, 4, 0)
	oldMonitor := monitor.New(cfg, host, client, dispatcher)
	oldMonitor.Start()
	if err := oldMonitor.Reconcile(context.Background(), "failure"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.started:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery worker did not start")
	}

	p := New(host, "")
	p.monitor = oldMonitor
	p.monitorStopTimeout = 20 * time.Millisecond
	p.quiesce()
	if len(p.detached) != 1 || p.detached[0] != oldMonitor {
		t.Fatalf("quiesce discarded a drained monitor with live delivery work: %+v", p.detached)
	}

	shutdownDone := make(chan struct{})
	go func() {
		p.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		t.Fatal("final shutdown returned while retired delivery code was in flight")
	default:
	}
	release()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("final shutdown did not finish after retired delivery worker exited")
	}
}

func TestManagementCheckTimeoutIsRetryable(t *testing.T) {
	runtimeEntry := protocol.HostAuthFileEntry{
		AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com", Status: "active",
	}
	for _, test := range []struct {
		name string
		host Host
	}{
		{name: "auth list", host: &deadlineListHost{}},
		{name: "runtime auth", host: &deadlineRuntimeHost{entry: runtimeEntry}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
			t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
			p := New(test.host, "")
			p.managementCheckTimeout = 20 * time.Millisecond
			defer p.Shutdown()
			lifecycleRequest, err := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(
				"enabled: true\nstartup-grace: 24h\nscan-interval: 24h\nstate-file: " + filepath.Join(t.TempDir(), "state.json") + "\n",
			), SchemaVersion: protocol.SchemaVersion})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Handle(protocol.MethodPluginRegister, lifecycleRequest); err != nil {
				t.Fatal(err)
			}

			checkRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/check"})
			value, err := p.Handle(protocol.MethodManagementHandle, checkRequest)
			if err != nil {
				t.Fatal(err)
			}
			response := value.(protocol.ManagementResponse)
			if response.StatusCode != http.StatusServiceUnavailable || response.Headers.Get("Retry-After") != "1" || !strings.Contains(string(response.Body), `"retryable": true`) {
				t.Fatalf("timed-out management check was not retryable: status=%d headers=%v body=%s", response.StatusCode, response.Headers, response.Body)
			}
		})
	}
}

func TestManagementCheckUsesFreshTimeoutWhenRetryingReplacement(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	host := &expiredRetryHost{
		entry: protocol.HostAuthFileEntry{
			AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com", Status: "active",
		},
		firstStarted: make(chan struct{}),
		firstExpired: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(host.releaseFirst) }) }
	defer releaseFirst()
	p := New(host, "")
	p.monitorStopTimeout = 5 * time.Millisecond
	p.managementCheckTimeout = 20 * time.Millisecond
	defer p.Shutdown()
	lifecycleRequest, err := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(
		"enabled: true\nstartup-grace: 24h\nscan-interval: 24h\nstate-file: " + filepath.Join(t.TempDir(), "state.json") + "\n",
	), SchemaVersion: protocol.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycleRequest); err != nil {
		t.Fatal(err)
	}

	checkRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/check"})
	checkDone := make(chan protocol.ManagementResponse, 1)
	go func() {
		value, _ := p.Handle(protocol.MethodManagementHandle, checkRequest)
		checkDone <- value.(protocol.ManagementResponse)
	}()
	select {
	case <-host.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("old management check did not start")
	}
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycleRequest); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.firstExpired:
	case <-time.After(5 * time.Second):
		t.Fatal("old management timeout did not expire")
	}
	releaseFirst()

	select {
	case response := <-checkDone:
		body := string(response.Body)
		if response.StatusCode != http.StatusOK || !strings.Contains(body, `"health": "healthy"`) || strings.Contains(body, `"monitoring_stale": true`) {
			t.Fatalf("replacement retry reused the expired context: status=%d body=%s", response.StatusCode, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not retry after its old context expired")
	}
	host.mu.Lock()
	listCalls := host.listCalls
	runtimeCalls := host.runtimeCalls
	host.mu.Unlock()
	if listCalls != 2 || runtimeCalls != 2 {
		t.Fatalf("host calls list=%d runtime=%d, want two complete checks", listCalls, runtimeCalls)
	}
}

func TestManagementCheckDuringReconfigureSwapIsRetryable(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	host := &pluginFakeHost{entry: protocol.HostAuthFileEntry{
		AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com", Status: "active",
	}}
	p := New(host, "")
	defer p.Shutdown()
	lifecycleRequest, err := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(
		"enabled: true\nstartup-grace: 24h\nscan-interval: 24h\nstate-file: " + filepath.Join(t.TempDir(), "state.json") + "\n",
	), SchemaVersion: protocol.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycleRequest); err != nil {
		t.Fatal(err)
	}

	swapStarted := make(chan struct{})
	releaseSwap := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSwap) }) }
	defer release()
	p.beforeMonitorPublish = func() {
		close(swapStarted)
		<-releaseSwap
	}
	reconfigureDone := make(chan error, 1)
	go func() {
		_, err := p.Handle(protocol.MethodPluginReconfigure, lifecycleRequest)
		reconfigureDone <- err
	}()
	select {
	case <-swapStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("reconfigure did not enter the unpublished swap window")
	}

	checkRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/check"})
	value, err := p.Handle(protocol.MethodManagementHandle, checkRequest)
	if err != nil {
		t.Fatal(err)
	}
	response := value.(protocol.ManagementResponse)
	body := string(response.Body)
	if response.StatusCode != http.StatusServiceUnavailable || response.Headers.Get("Retry-After") != "1" || !strings.Contains(body, `"retryable": true`) {
		t.Fatalf("swap-window check was not retryable: status=%d headers=%v body=%s", response.StatusCode, response.Headers, body)
	}
	release()
	select {
	case err := <-reconfigureDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconfigure did not finish after releasing publication")
	}
}

func TestManagementCheckRetriesReplacementWhenReconfigureWins(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	host := &pluginFakeHost{entry: protocol.HostAuthFileEntry{
		AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com", Status: "active",
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()
	p := New(host, server.URL)
	defer p.Shutdown()
	lifecycleRequest, err := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(
		"enabled: true\nstartup-grace: 24h\nscan-interval: 24h\nnotification-coalesce-window: 0\nstate-file: " + filepath.Join(t.TempDir(), "state.json") + "\n",
	), SchemaVersion: protocol.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycleRequest); err != nil {
		t.Fatal(err)
	}

	capturedOld := make(chan struct{})
	releaseCheck := make(chan struct{})
	p.beforeManagementCheck = func() {
		close(capturedOld)
		<-releaseCheck
	}
	checkRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/check"})
	checkDone := make(chan protocol.ManagementResponse, 1)
	go func() {
		value, _ := p.Handle(protocol.MethodManagementHandle, checkRequest)
		checkDone <- value.(protocol.ManagementResponse)
	}()
	select {
	case <-capturedOld:
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not capture the old monitor")
	}

	host.mu.Lock()
	host.entry.Status = "error"
	host.entry.StatusMessage = "unauthorized"
	host.entry.Unavailable = true
	host.mu.Unlock()
	if _, err := p.Handle(protocol.MethodPluginReconfigure, lifecycleRequest); err != nil {
		t.Fatal(err)
	}
	close(releaseCheck)

	select {
	case response := <-checkDone:
		if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"health": "reauth_required"`) {
			t.Fatalf("check did not retry the replacement monitor: status=%d body=%s", response.StatusCode, response.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not finish after reconfigure")
	}
	host.mu.Lock()
	listCalls := host.listCalls
	host.mu.Unlock()
	if listCalls != 1 {
		t.Fatalf("replacement monitor performed %d checks, want exactly one", listCalls)
	}
}

func TestManagementCheckReturnsRetryable503WhenQuiesceWins(t *testing.T) {
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	host := &pluginFakeHost{entry: protocol.HostAuthFileEntry{
		AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com", Status: "active",
	}}
	p := New(host, "")
	lifecycleRequest, err := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte(
		"enabled: true\nstartup-grace: 24h\nscan-interval: 24h\nstate-file: " + filepath.Join(t.TempDir(), "state.json") + "\n",
	), SchemaVersion: protocol.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(protocol.MethodPluginRegister, lifecycleRequest); err != nil {
		t.Fatal(err)
	}

	capturedOld := make(chan struct{})
	releaseCheck := make(chan struct{})
	p.beforeManagementCheck = func() {
		close(capturedOld)
		<-releaseCheck
	}
	checkRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/check"})
	checkDone := make(chan protocol.ManagementResponse, 1)
	go func() {
		value, _ := p.Handle(protocol.MethodManagementHandle, checkRequest)
		checkDone <- value.(protocol.ManagementResponse)
	}()
	select {
	case <-capturedOld:
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not capture the old monitor")
	}
	if _, err := p.Handle(protocol.MethodPluginQuiesce, nil); err != nil {
		t.Fatal(err)
	}
	close(releaseCheck)

	select {
	case response := <-checkDone:
		body := string(response.Body)
		if response.StatusCode != http.StatusServiceUnavailable || response.Headers.Get("Retry-After") != "1" || !strings.Contains(body, `"retryable": true`) {
			t.Fatalf("lifecycle rejection was not retryable: status=%d headers=%v body=%s", response.StatusCode, response.Headers, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("management check did not finish after quiesce")
	}
	host.mu.Lock()
	listCalls := host.listCalls
	host.mu.Unlock()
	if listCalls != 0 {
		t.Fatalf("stopped monitor performed %d stale checks", listCalls)
	}
}

func TestRegistrationUsesCurrentABIContract(t *testing.T) {
	p := New(&pluginFakeHost{}, "")
	request, _ := json.Marshal(protocol.LifecycleRequest{ConfigYAML: []byte("enabled: false\n")})
	value, err := p.Handle(protocol.MethodPluginRegister, request)
	if err != nil {
		t.Fatal(err)
	}
	registration := value.(protocol.Registration)
	if registration.SchemaVersion != 4 || !registration.Capabilities.UsagePlugin || !registration.Capabilities.ManagementAPI {
		t.Fatalf("registration=%+v", registration)
	}
	if registration.Metadata.Version != Version || registration.Metadata.GitHubRepository != "https://github.com/NoorChasib/cpa-plugin-account-health-pushover" {
		t.Fatalf("metadata=%+v", registration.Metadata)
	}
	fieldNames := make(map[string]bool)
	for _, field := range registration.Metadata.ConfigFields {
		fieldNames[field.Name] = true
	}
	for _, required := range []string{"providers", "scan-interval", "startup-grace", "transient-confirm-after", "unauthorized-confirm-after", "usage-recheck-delay", "notify-recovery", "reminder-interval", "pushover-app-token-env", "pushover-user-key-env", "management-url"} {
		if !fieldNames[required] {
			t.Fatalf("missing ConfigField %q", required)
		}
	}
	for _, forbidden := range []string{"pushover-app-token", "pushover-user-key"} {
		if fieldNames[forbidden] {
			t.Fatalf("direct secret field %q must not be exposed", forbidden)
		}
	}
}

func TestManagementRegistrationPaths(t *testing.T) {
	value, err := New(&pluginFakeHost{}, "").Handle(protocol.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	registration := value.(protocol.ManagementRegistration)
	if len(registration.Routes) != 3 || len(registration.Resources) != 1 {
		t.Fatalf("management registration=%+v", registration)
	}
	if registration.Routes[0].Path != "/plugins/account-health-pushover/status" || registration.Resources[0].Path != "/status" {
		t.Fatalf("unexpected paths: %+v", registration)
	}
}

func TestUsageEvidenceIsDelayedByStartupGraceAndBodyIsDiscarded(t *testing.T) {
	bodySecret := "RAW-USAGE-BODY-SECRET-SENTINEL"
	host := &pluginFakeHost{entry: protocol.HostAuthFileEntry{
		AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com", Status: "active",
	}}
	var notificationCount atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		notificationCount.Add(1)
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()
	p := configurePlugin(t, host, server.URL)

	usage401 := []byte(`{"Failed":true,"AuthIndex":"one","Failure":{"StatusCode":401,"Body":"` + bodySecret + `"}}`)
	if _, err := p.Handle(protocol.MethodUsageHandle, usage401); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	callsBeforeManualCheck := host.listCalls
	host.mu.Unlock()
	if callsBeforeManualCheck != 0 {
		t.Fatalf("usage event bypassed startup grace with %d host scans", callsBeforeManualCheck)
	}

	checkRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/check"})
	value, err := p.Handle(protocol.MethodManagementHandle, checkRequest)
	if err != nil {
		t.Fatal(err)
	}
	if body := string(value.(protocol.ManagementResponse).Body); !strings.Contains(body, `"health": "healthy"`) || strings.Contains(body, bodySecret) {
		t.Fatalf("active runtime did not override 401 or leaked body: %s", body)
	}

	host.mu.Lock()
	host.entry.Status = "error"
	host.entry.Unavailable = true
	host.entry.StatusMessage = `{"provider":"opaque text"}`
	host.entry.NextRetryAfter = time.Now().Add(time.Hour)
	host.mu.Unlock()
	usage429 := []byte(`{"Failed":true,"AuthIndex":"one","Failure":{"StatusCode":429,"Body":"` + bodySecret + `"}}`)
	if _, err := p.Handle(protocol.MethodUsageHandle, usage429); err != nil {
		t.Fatal(err)
	}
	value, err = p.Handle(protocol.MethodManagementHandle, checkRequest)
	if err != nil {
		t.Fatal(err)
	}
	managementBody := string(value.(protocol.ManagementResponse).Body)
	if !strings.Contains(managementBody, `"health": "quota_limited"`) || strings.Contains(managementBody, bodySecret) {
		t.Fatalf("429 evidence was not quota-safe or leaked body: %s", managementBody)
	}
	resourceRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/account-health-pushover/status"})
	resource, err := p.Handle(protocol.MethodManagementHandle, resourceRequest)
	if err != nil {
		t.Fatal(err)
	}
	if resourceBody := string(resource.(protocol.ManagementResponse).Body); strings.Contains(resourceBody, bodySecret) || strings.Contains(resourceBody, "http_429_quota") {
		t.Fatalf("resource page leaked diagnostics: %s", resourceBody)
	}
	if count := notificationCount.Load(); count != 0 {
		t.Fatalf("401/429 evidence sent %d credential notifications", count)
	}
}

func TestManagementStatusAndTestNotificationAreSecretSafe(t *testing.T) {
	appSecret := strings.Repeat("A", 30)
	userSecret := strings.Repeat("B", 30)
	host := &pluginFakeHost{entry: protocol.HostAuthFileEntry{
		AuthIndex: "one", Provider: "codex", Type: "codex", AccountType: "oauth", Email: "safe@example.com", Status: "active", Account: "OAUTH-OR-API-SECRET-MUST-NOT-RENDER",
	}}
	var receivedMessage string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		receivedMessage = r.PostForm.Get("message")
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()
	p := configurePlugin(t, host, server.URL)

	checkRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/check"})
	checkValue, err := p.Handle(protocol.MethodManagementHandle, checkRequest)
	if err != nil {
		t.Fatal(err)
	}
	if checkBody := string(checkValue.(protocol.ManagementResponse).Body); !strings.Contains(checkBody, "safe@example.com") || strings.Contains(checkBody, host.entry.Account) {
		t.Fatalf("authenticated management status omitted safe identity or leaked raw account data: %s", checkBody)
	}
	resourceRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/account-health-pushover/status"})
	value, err := p.Handle(protocol.MethodManagementHandle, resourceRequest)
	if err != nil {
		t.Fatal(err)
	}
	body := string(value.(protocol.ManagementResponse).Body)
	for _, secret := range []string{appSecret, userSecret, host.entry.Account} {
		if strings.Contains(body, secret) {
			t.Fatalf("status page leaked secret %q", secret)
		}
	}
	if strings.Contains(body, "safe@example.com") || !strings.Contains(body, "Codex OAuth account 1") || !strings.Contains(body, "Quota-limited accounts") {
		t.Fatalf("resource page did not redact account identity or omitted expected content: %s", body)
	}
	fallbackRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodGet, Path: "/plugins/account-health-pushover/status"})
	fallbackValue, err := p.Handle(protocol.MethodManagementHandle, fallbackRequest)
	if err != nil {
		t.Fatal(err)
	}
	fallbackBody := string(fallbackValue.(protocol.ManagementResponse).Body)
	if strings.Contains(fallbackBody, "safe@example.com") || !strings.Contains(fallbackBody, "Codex OAuth account 1") {
		t.Fatalf("unrecognized status path was not redacted by default: %s", fallbackBody)
	}

	testRequest, _ := json.Marshal(protocol.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/account-health-pushover/test"})
	value, err = p.Handle(protocol.MethodManagementHandle, testRequest)
	if err != nil {
		t.Fatal(err)
	}
	if value.(protocol.ManagementResponse).StatusCode != http.StatusOK || receivedMessage != "CLIProxyAPI Pushover test successful" {
		t.Fatalf("test response=%+v message=%q", value, receivedMessage)
	}
	for _, forbidden := range []string{"safe@example.com", host.entry.Account, appSecret, userSecret} {
		if strings.Contains(receivedMessage, forbidden) {
			t.Fatalf("test message leaked %q", forbidden)
		}
	}
}

func TestResourceStatusRedactsAllDiagnosticsAndUsesClosedProviderNames(t *testing.T) {
	secret := "RAW-DIAGNOSTIC-SECRET-SENTINEL"
	status := monitor.Status{
		MonitoringStale:     true,
		LastMonitoringError: secret,
		StateFile:           "/secret/state/path",
		Notifier: notifier.Status{
			LastError:     secret,
			Configuration: config.CredentialStatus{State: "error", Error: secret},
		},
		Accounts: []monitor.AccountStatus{
			{Provider: "claude", Label: "person@example.com", AuthIndex: "secret-index", Health: health.Suspect, ReasonCode: secret},
			{Provider: "ßprovider", Label: "second@example.com", AuthIndex: "other-index", Health: health.Healthy, ReasonCode: secret},
		},
	}
	redacted := redactResourceStatus(status)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "/secret/state/path", "person@example.com", "second@example.com", "secret-index", "other-index"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("redacted resource retained %q: %s", forbidden, encoded)
		}
	}
	page := string(renderStatusPage(redacted))
	if !strings.Contains(page, "Claude OAuth account 1") || !strings.Contains(page, "OAuth account 1") {
		t.Fatalf("provider names were not mapped to the closed display set: %s", page)
	}
	if strings.Contains(page, "window.prompt") || strings.Contains(page, "X-Management-Key") || strings.Contains(page, "<script") {
		t.Fatalf("unauthenticated resource still collects a management key: %s", page)
	}
	if strings.Contains(page, "stale:") || !strings.Contains(page, ">stale<") {
		t.Fatalf("resource stale marker exposed a dangling diagnostic: %s", page)
	}
}
