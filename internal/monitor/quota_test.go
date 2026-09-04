package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/notifier"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

// quotaFakeHost extends fakeHost with the callbacks quota polling needs and
// serves a canned usage response per auth index.
type quotaFakeHost struct {
	*fakeHost
	mu        sync.Mutex
	percent   map[string]float64
	resetAt   map[string]time.Time
	httpErr   map[string]error
	status    map[string]int
	authErr   map[string]error
	requests  []protocol.HostHTTPRequest
	authCalls int
}

func newQuotaHost() *quotaFakeHost {
	return &quotaFakeHost{
		fakeHost: &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)},
		percent:  make(map[string]float64),
		resetAt:  make(map[string]time.Time),
		httpErr:  make(map[string]error),
		status:   make(map[string]int),
		authErr:  make(map[string]error),
	}
}

func (h *quotaFakeHost) GetAuth(_ context.Context, authIndex string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.authCalls++
	if err := h.authErr[authIndex]; err != nil {
		return nil, err
	}
	// The access token encodes the auth index so HTTPDo can route the canned
	// response without a second lookup table.
	return []byte(fmt.Sprintf(`{"access_token":"tok-%s","account_id":"acct-%s","sub":"sub-%s","refresh_token":"RAW-REFRESH-SECRET"}`, authIndex, authIndex, authIndex)), nil
}

func (h *quotaFakeHost) HTTPDo(_ context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, request)
	authIndex := strings.TrimPrefix(strings.Join(request.Headers["Authorization"], ""), "Bearer tok-")
	if err := h.httpErr[authIndex]; err != nil {
		return protocol.HostHTTPResponse{}, err
	}
	if status := h.status[authIndex]; status != 0 {
		return protocol.HostHTTPResponse{StatusCode: status, Body: []byte(`{"error":"UPSTREAM-BODY-SECRET"}`)}, nil
	}
	percent := h.percent[authIndex]
	reset := h.resetAt[authIndex]
	var body string
	switch {
	case strings.Contains(request.URL, "anthropic.com"):
		body = fmt.Sprintf(`{"five_hour":{"utilization":99},"seven_day":{"utilization":%g,"resets_at":%q}}`, percent, reset.Format(time.RFC3339Nano))
	case strings.Contains(request.URL, "chatgpt.com"):
		body = fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%g,"limit_window_seconds":604800,"reset_at":%d}}}`, percent, reset.Unix())
	case strings.Contains(request.URL, "grok.com"):
		body = fmt.Sprintf(`{"config":{"creditUsagePercent":%g,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":%q}}}`, percent, reset.Format(time.RFC3339Nano))
	default:
		return protocol.HostHTTPResponse{StatusCode: 404}, nil
	}
	return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}, nil
}

func (h *quotaFakeHost) setQuota(authIndex string, percent float64, reset time.Time) {
	h.mu.Lock()
	h.percent[authIndex] = percent
	h.resetAt[authIndex] = reset
	h.mu.Unlock()
}

func (h *quotaFakeHost) requestCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

func newQuotaMonitor(t *testing.T, host Host, mock *mockPushover, configure func(*config.Config)) *Monitor {
	t.Helper()
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	cfg := config.Default()
	cfg.StartupGrace = 24 * time.Hour
	cfg.ScanInterval = 24 * time.Hour
	cfg.HTTPTimeout = time.Second
	cfg.NotificationCoalesceWindow = 0
	cfg.QuotaAlerts = true
	cfg.QuotaPollInterval = 24 * time.Hour
	cfg.StateFile = filepath.Join(t.TempDir(), "state.ahp")
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

var weekReset = time.Date(2026, time.September, 9, 10, 0, 0, 0, time.UTC)

func TestQuotaWarningAndExhaustionAlertOncePerWindowAcrossRestart(t *testing.T) {
	host := newQuotaHost()
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	host.setQuota("one", 40, weekReset)
	mock := newMockPushover(t)
	statePath := filepath.Join(t.TempDir(), "state.ahp")
	monitor := newQuotaMonitor(t, host, mock, func(cfg *config.Config) { cfg.StateFile = statePath })

	ctx := context.Background()
	if err := monitor.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if mock.count() != 0 {
		t.Fatalf("40%% must not alert: %s", mock.all())
	}
	row := monitor.Snapshot().Accounts[0]
	if row.Quota == nil || row.Quota.Percent == nil || *row.Quota.Percent != 40 || !row.Quota.ResetAt.Equal(weekReset) {
		t.Fatalf("quota status=%+v", row.Quota)
	}

	// Crossing 95% warns exactly once, even across repeated polls.
	host.setQuota("one", 96.5, weekReset)
	for i := 0; i < 3; i++ {
		if err := monitor.PollQuota(ctx, "test"); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	if all := mock.all(); !strings.Contains(all, "Claude weekly limit almost used") || !strings.Contains(all, "96.5%") || !strings.Contains(all, "3.5% remaining") || !strings.Contains(all, "Wed Sep 9 2026") {
		t.Fatalf("warning message=%q", all)
	}
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if mock.count() != 1 {
		t.Fatalf("warning repeated: %s", mock.all())
	}

	// Exhaustion sends its own single message.
	host.setQuota("one", 100, weekReset)
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	if all := mock.all(); !strings.Contains(all, "Claude weekly limit reached") || !strings.Contains(all, "used 100%") {
		t.Fatalf("exhausted message=%q", all)
	}
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if mock.count() != 2 {
		t.Fatalf("exhausted repeated: %s", mock.all())
	}

	// Restart with the same window: the persisted latches hold.
	monitor.Stop()
	restarted := newQuotaMonitor(t, host, mock, func(cfg *config.Config) { cfg.StateFile = statePath })
	if err := restarted.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if mock.count() != 2 {
		t.Fatalf("restart re-alerted: %s", mock.all())
	}

	// A new weekly window clears the latches and alerts again.
	host.setQuota("one", 100, weekReset.Add(7*24*time.Hour))
	if err := restarted.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 3 })
}

func TestQuotaExhaustionWithoutPriorWarningSendsOnlyExhausted(t *testing.T) {
	host := newQuotaHost()
	host.set(oauthEntry("one", "codex", "c@example.com", "active", "", false))
	host.setQuota("one", 100, weekReset)
	mock := newMockPushover(t)
	monitor := newQuotaMonitor(t, host, mock, nil)
	ctx := context.Background()
	if err := monitor.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	if all := mock.all(); !strings.Contains(all, "Codex weekly limit reached") || strings.Contains(all, "almost used") {
		t.Fatalf("messages=%q", all)
	}
	// Dropping below the warning threshold later (provider reports a reset
	// without changing the reset field) re-arms the latches.
	host.setQuota("one", 2, weekReset)
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	host.setQuota("one", 99, weekReset)
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 2 })
	if !strings.Contains(mock.all(), "almost used") {
		t.Fatalf("re-armed warning missing: %s", mock.all())
	}
}

func TestQuotaPollCoversAllProvidersAndIsolatesFailures(t *testing.T) {
	host := newQuotaHost()
	claude := oauthEntry("c1", "claude", "a@example.com", "active", "", false)
	codex := oauthEntry("o1", "codex", "b@example.com", "active", "", false)
	grok := oauthEntry("x1", "xai", "g@example.com", "active", "", false)
	host.roster = []protocol.HostAuthFileEntry{claude, codex, grok}
	for _, entry := range host.roster {
		host.runtime[entry.AuthIndex] = entry
	}
	host.setQuota("c1", 10, weekReset)
	host.setQuota("o1", 99, weekReset)
	host.setQuota("x1", 28, weekReset)
	host.status["c1"] = 401
	mock := newMockPushover(t)
	monitor := newQuotaMonitor(t, host, mock, nil)
	ctx := context.Background()
	if err := monitor.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	if !strings.Contains(mock.all(), "Codex weekly limit almost used") {
		t.Fatalf("messages=%q", mock.all())
	}
	status := monitor.Snapshot()
	if !status.QuotaAlerts || status.LastQuotaPoll.IsZero() || !strings.Contains(status.LastQuotaPollError, "1 of 3") {
		t.Fatalf("status=%+v", status)
	}
	encoded, _ := json.Marshal(status)
	for _, forbidden := range []string{"tok-", "RAW-REFRESH-SECRET", "UPSTREAM-BODY-SECRET", "Bearer"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("status leaked %q: %s", forbidden, encoded)
		}
	}
	byIndex := make(map[string]AccountStatus)
	for _, row := range status.Accounts {
		byIndex[row.AuthIndex] = row
	}
	if q := byIndex["c1"].Quota; q == nil || q.Percent != nil || !strings.Contains(q.LastError, "HTTP 401") {
		t.Fatalf("claude quota row=%+v", q)
	}
	if q := byIndex["x1"].Quota; q == nil || q.Percent == nil || *q.Percent != 28 || q.LastError != "" {
		t.Fatalf("grok quota row=%+v", q)
	}
	if q := byIndex["o1"].Quota; q == nil || q.WarningSentAt.IsZero() {
		t.Fatalf("codex quota row=%+v", q)
	}
	// Each provider hit its own endpoint exactly once.
	seen := make(map[string]int)
	host.mu.Lock()
	for _, request := range host.requests {
		seen[request.URL]++
	}
	host.mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("endpoints=%v", seen)
	}
}

func TestQuotaPollIsSkippedWhenDisabledOrUnsupportedHost(t *testing.T) {
	host := newQuotaHost()
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	host.setQuota("one", 100, weekReset)
	mock := newMockPushover(t)
	disabled := newQuotaMonitor(t, host, mock, func(cfg *config.Config) { cfg.QuotaAlerts = false })
	ctx := context.Background()
	if err := disabled.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if err := disabled.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if host.requestCount() != 0 || mock.count() != 0 || disabled.Snapshot().QuotaAlerts {
		t.Fatalf("disabled quota alerts still polled: requests=%d messages=%d", host.requestCount(), mock.count())
	}

	// A host without host.auth.get/host.http.do degrades with a warning.
	plain := &fakeHost{runtime: make(map[string]protocol.HostAuthFileEntry), runtimeErr: make(map[string]error)}
	plain.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	unsupported := newTestMonitor(t, plain, mock, filepath.Join(t.TempDir(), "state.ahp"))
	unsupported.cfg.QuotaAlerts = true
	if err := unsupported.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.QuotaAlerts = true
	cfg.StateFile = filepath.Join(t.TempDir(), "state.ahp")
	client := notifier.NewClient(cfg, mock.endpoint, mock.server.Client())
	warned := New(cfg, plain, client, notifier.NewDispatcher(client, 4, 0))
	if status := warned.Snapshot(); status.QuotaAlerts || len(status.Warnings) == 0 || !strings.Contains(status.Warnings[0], "host.auth.get") {
		t.Fatalf("unsupported host status=%+v", status)
	}
}

func TestQuotaFailedDeliveryRetriesLaterAndLatchesOnlyOnAccept(t *testing.T) {
	host := newQuotaHost()
	host.set(oauthEntry("one", "xai", "g@example.com", "active", "", false))
	host.setQuota("one", 100, weekReset)
	mock := newMockPushover(t)
	// A 4xx is not retried by the notifier, so the failure surfaces promptly.
	mock.respond(400, `{"status":0}`)
	monitor := newQuotaMonitor(t, host, mock, nil)
	base := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return base }
	ctx := context.Background()
	if err := monitor.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return mock.count() == 1 })
	waitFor(t, 5*time.Second, func() bool { return monitor.Snapshot().Notifier.LastError != "" })
	row := monitor.Snapshot().Accounts[0]
	if row.Quota == nil || !row.Quota.ExhaustedSentAt.IsZero() {
		t.Fatalf("failed delivery latched: %+v", row.Quota)
	}
	// Within the retry window nothing is re-queued.
	before := mock.count()
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if mock.count() != before {
		t.Fatalf("re-queued inside retry window")
	}
	mock.respond(200, `{"status":1}`)
	monitor.now = func() time.Time { return base.Add(notificationRetryAfter + time.Second) }
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		row := monitor.Snapshot().Accounts[0]
		return row.Quota != nil && !row.Quota.ExhaustedSentAt.IsZero()
	})
	if !strings.Contains(mock.all(), "Grok weekly limit reached") {
		t.Fatalf("messages=%q", mock.all())
	}
}

func TestQuotaPollErrorsAreStaticAndHostFailuresDoNotAbort(t *testing.T) {
	host := newQuotaHost()
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	host.authErr["one"] = errors.New("host callback host.auth.get failed: /secret/path/claude-a@example.com.json")
	mock := newMockPushover(t)
	monitor := newQuotaMonitor(t, host, mock, nil)
	ctx := context.Background()
	if err := monitor.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	row := monitor.Snapshot().Accounts[0]
	if row.Quota == nil || row.Quota.LastError != "host.auth.get failed" {
		t.Fatalf("quota row=%+v", row.Quota)
	}
	delete(host.authErr, "one")
	host.httpErr["one"] = errors.New("host callback host.http.do failed: dial tcp 1.2.3.4")
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if row := monitor.Snapshot().Accounts[0]; row.Quota.LastError != "host.http.do failed" {
		t.Fatalf("quota row=%+v", row.Quota)
	}
}

func TestQuotaPollTimesOutOnStuckHostCallback(t *testing.T) {
	host := &stuckQuotaHost{quotaFakeHost: newQuotaHost(), release: make(chan struct{})}
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	mock := newMockPushover(t)
	monitor := newQuotaMonitor(t, host, mock, func(cfg *config.Config) { cfg.QuotaHTTPTimeout = time.Second })
	ctx := context.Background()
	if err := monitor.Reconcile(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := monitor.PollQuota(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("poll ignored quota-http-timeout: %s", elapsed)
	}
	if row := monitor.Snapshot().Accounts[0]; row.Quota == nil || row.Quota.LastError != "usage request timed out" {
		t.Fatalf("quota row=%+v", row.Quota)
	}
	close(host.release)
}

type stuckQuotaHost struct {
	*quotaFakeHost
	release chan struct{}
}

func (h *stuckQuotaHost) HTTPDo(_ context.Context, _ protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	<-h.release
	return protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{}`)}, nil
}

func TestQuotaLoopRunsAfterBaselineAndCheckNowTriggersPoll(t *testing.T) {
	host := newQuotaHost()
	host.set(oauthEntry("one", "claude", "a@example.com", "active", "", false))
	host.setQuota("one", 5, weekReset)
	mock := newMockPushover(t)
	monitor := newQuotaMonitor(t, host, mock, func(cfg *config.Config) { cfg.StartupGrace = 10 * time.Millisecond })
	waitFor(t, 5*time.Second, func() bool { return host.requestCount() == 1 })
	if status := monitor.Snapshot(); status.LastQuotaPoll.IsZero() || status.NextQuotaPoll.IsZero() {
		t.Fatalf("status after startup poll=%+v", status)
	}
	if _, err := monitor.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return host.requestCount() == 2 })
}
