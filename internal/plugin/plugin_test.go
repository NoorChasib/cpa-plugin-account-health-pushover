package plugin

import (
	"context"
	"encoding/json"
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
