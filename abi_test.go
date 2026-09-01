package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

func TestABIEnvelopesMatchHostContract(t *testing.T) {
	raw, err := okEnvelope(map[string]any{"registered": true})
	if err != nil {
		t.Fatal(err)
	}
	var success protocol.Envelope
	if err := json.Unmarshal(raw, &success); err != nil {
		t.Fatal(err)
	}
	if !success.OK || success.Error != nil || string(success.Result) != `{"registered":true}` {
		t.Fatalf("success envelope = %s", raw)
	}

	raw = errorEnvelope("invalid_request", "request was rejected")
	var failure protocol.Envelope
	if err := json.Unmarshal(raw, &failure); err != nil {
		t.Fatal(err)
	}
	if failure.OK || failure.Error == nil || failure.Error.Code != "invalid_request" || failure.Error.Message != "request was rejected" || len(failure.Result) != 0 {
		t.Fatalf("failure envelope = %s", raw)
	}
}

func TestSanitizeErrorBoundsUntrustedText(t *testing.T) {
	if got := sanitizeError(nil); got != "plugin error" {
		t.Fatalf("nil error = %q", got)
	}
	input := strings.Repeat("x", 400)
	got := sanitizeError(errors.New(input))
	if len(got) != 300 || got != input[:300] {
		t.Fatalf("sanitized length = %d", len(got))
	}
}

func TestProductionBuildIgnoresEndpointOverride(t *testing.T) {
	original := allowTestEndpointOverride
	allowTestEndpointOverride = "false"
	t.Cleanup(func() { allowTestEndpointOverride = original })
	t.Setenv("CPA_PUSHOVER_TEST_ENDPOINT", "http://127.0.0.1:9876/test")
	if got := configuredNotifierEndpoint(); got != "" {
		t.Fatalf("production endpoint override = %q", got)
	}
}

func TestTestEndpointOverrideIsRestrictedToLocalHosts(t *testing.T) {
	original := allowTestEndpointOverride
	allowTestEndpointOverride = "true"
	t.Cleanup(func() { allowTestEndpointOverride = original })

	for _, endpoint := range []string{
		"http://localhost:9876/test",
		"https://127.0.0.1:9876/test",
		"http://[::1]:9876/test",
		"http://host.docker.internal:9876/test",
	} {
		t.Run("accept_"+strings.NewReplacer(":", "_", "/", "_").Replace(endpoint), func(t *testing.T) {
			t.Setenv("CPA_PUSHOVER_TEST_ENDPOINT", endpoint)
			if got := configuredNotifierEndpoint(); got != endpoint {
				t.Fatalf("endpoint = %q, want %q", got, endpoint)
			}
		})
	}

	for _, endpoint := range []string{
		"https://api.pushover.net/1/messages.json",
		"http://user:password@localhost:9876/test",
		"ftp://127.0.0.1/test",
		"not a URL",
	} {
		t.Run("reject_"+strings.NewReplacer(":", "_", "/", "_").Replace(endpoint), func(t *testing.T) {
			t.Setenv("CPA_PUSHOVER_TEST_ENDPOINT", endpoint)
			if got := configuredNotifierEndpoint(); got != "" {
				t.Fatalf("unsafe endpoint override = %q", got)
			}
		})
	}
}
