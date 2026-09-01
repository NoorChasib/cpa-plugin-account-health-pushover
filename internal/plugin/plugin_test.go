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
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
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
	for _, required := range []string{"providers", "scan-interval", "startup-grace", "transient-confirm-after", "notify-recovery", "reminder-interval", "pushover-app-token-env", "pushover-user-key-env", "management-url"} {
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

func TestUsage401SchedulesRecheckAnd429DoesNot(t *testing.T) {
	host := &pluginFakeHost{entry: protocol.HostAuthFileEntry{
		AuthIndex: "one", Provider: "claude", Type: "claude", AccountType: "oauth", Email: "a@example.com", Status: "active",
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"status":1}`) }))
	defer server.Close()
	p := configurePlugin(t, host, server.URL)

	start := time.Now()
	usage401, _ := json.Marshal(protocol.UsageRecord{Failed: true, AuthIndex: "one", Failure: protocol.UsageFailure{StatusCode: 401, Body: "must be ignored"}})
	if _, err := p.Handle(protocol.MethodUsageHandle, usage401); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 20*time.Millisecond {
		t.Fatal("usage callback blocked")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		host.mu.Lock()
		calls := host.listCalls
		host.mu.Unlock()
		if calls == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	host.mu.Lock()
	before429 := host.listCalls
	host.mu.Unlock()
	if before429 != 1 {
		t.Fatalf("401 did not trigger exactly one recheck; calls=%d", before429)
	}
	usage429, _ := json.Marshal(protocol.UsageRecord{Failed: true, AuthIndex: "one", Failure: protocol.UsageFailure{StatusCode: 429}})
	if _, err := p.Handle(protocol.MethodUsageHandle, usage429); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	host.mu.Lock()
	after429 := host.listCalls
	host.mu.Unlock()
	if after429 != before429 {
		t.Fatalf("429 scheduled account-health recheck; before=%d after=%d", before429, after429)
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
