package protocol

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLifecycleRequestWireContract(t *testing.T) {
	raw, err := json.Marshal(LifecycleRequest{
		ConfigYAML:    []byte("enabled: true\n"),
		SchemaVersion: SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEquals(t, raw, `{"config_yaml":"ZW5hYmxlZDogdHJ1ZQo=","schema_version":4}`)
}

func TestRegistrationWireContractUsesMixedUpstreamCasing(t *testing.T) {
	raw, err := json.Marshal(Registration{
		SchemaVersion: SchemaVersion,
		Metadata: Metadata{
			Name:             "Account Health Pushover",
			Version:          "0.1.0",
			Author:           "Noor Chasib",
			GitHubRepository: "https://github.com/NoorChasib/cpa-plugin-account-health-pushover",
			Logo:             "bell",
			ConfigFields: []ConfigField{{
				Name: "providers", Type: "array", EnumValues: []string{"claude", "codex"}, Description: "providers",
			}},
		},
		Capabilities: RegistrationCapabilities{UsagePlugin: true, ManagementAPI: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, document, "capabilities", "metadata", "schema_version")
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(document["metadata"], &metadata); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, metadata, "Author", "ConfigFields", "GitHubRepository", "Logo", "Name", "Version")
	var capabilities map[string]json.RawMessage
	if err := json.Unmarshal(document["capabilities"], &capabilities); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, capabilities, "management_api", "usage_plugin")
}

func TestManagementRequestResponseWireContract(t *testing.T) {
	fixture := `{
		"Method":"POST",
		"Path":"/v0/management/plugins/account-health-pushover/check",
		"Headers":{"X-Test":["one"]},
		"Query":{"force":["true"]},
		"Body":"e30=",
		"host_callback_id":"callback-1"
	}`
	var request ManagementRequest
	if err := json.Unmarshal([]byte(fixture), &request); err != nil {
		t.Fatal(err)
	}
	if request.Method != http.MethodPost || request.Path == "" || request.Headers.Get("X-Test") != "one" || request.Query.Get("force") != "true" || string(request.Body) != "{}" || request.HostCallbackID != "callback-1" {
		t.Fatalf("decoded ManagementRequest = %+v", request)
	}

	raw, err := json.Marshal(ManagementResponse{
		StatusCode: http.StatusAccepted,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       []byte(`{"accepted":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, response, "Body", "Headers", "StatusCode")
}

func TestHostCallbackWireContract(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	fixture := `{"files":[{"id":"id-1","auth_index":"auth-1","name":"claude.json","type":"claude","provider":"claude","status":"error","status_message":"unauthorized","unavailable":true,"updated_at":"2026-09-01T12:00:00Z","next_retry_after":"0001-01-01T00:00:00Z","email":"safe@example.com","account_type":"oauth","success":2,"failed":3}]}`
	var response HostAuthListResponse
	if err := json.Unmarshal([]byte(fixture), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Files) != 1 {
		t.Fatalf("files = %d", len(response.Files))
	}
	entry := response.Files[0]
	if entry.AuthIndex != "auth-1" || entry.StatusMessage != "unauthorized" || !entry.Unavailable || !entry.UpdatedAt.Equal(now) || entry.Success != 2 || entry.Failed != 3 {
		t.Fatalf("decoded auth entry = %+v", entry)
	}

	requestRaw, err := json.Marshal(HostAuthGetRequest{AuthIndex: "auth-1"})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEquals(t, requestRaw, `{"auth_index":"auth-1"}`)

	logRaw, err := json.Marshal(HostLogRequest{
		HostCallbackID: "callback-1",
		Level:          "warn",
		Message:        "sanitized message",
		Fields:         map[string]any{"account_key": "claude:auth-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var logRequest map[string]json.RawMessage
	if err := json.Unmarshal(logRaw, &logRequest); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, logRequest, "fields", "host_callback_id", "level", "message")
}

func TestManagementRequestUsesStandardHeaderAndQueryTypes(t *testing.T) {
	request := ManagementRequest{
		Headers: http.Header{"X-Test": {"one", "two"}},
		Query:   url.Values{"provider": {"claude", "codex"}},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"Headers":{"X-Test":["one","two"]}`) || !strings.Contains(string(raw), `"Query":{"provider":["claude","codex"]}`) {
		t.Fatalf("unexpected management wire shape: %s", raw)
	}
}

func assertJSONEquals(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue any
	var wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	gotCanonical, _ := json.Marshal(gotValue)
	wantCanonical, _ := json.Marshal(wantValue)
	if string(gotCanonical) != string(wantCanonical) {
		t.Fatalf("JSON = %s, want %s", gotCanonical, wantCanonical)
	}
}

func assertKeys(t *testing.T, values map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(values) != len(want) {
		t.Fatalf("keys = %v, want %v", mapKeys(values), want)
	}
	for _, key := range want {
		if _, ok := values[key]; !ok {
			t.Fatalf("missing key %q in %v", key, mapKeys(values))
		}
	}
}

func mapKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
