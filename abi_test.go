package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

func TestHostCallGateDrainsBeforeNativeUnload(t *testing.T) {
	waitStarted := make(chan struct{})
	gate := hostCallGate{waitStarted: func() { close(waitStarted) }}
	done, ok := gate.begin()
	if !ok {
		t.Fatal("fresh host-call gate rejected admission")
	}
	gate.stopAccepting()

	drained := make(chan struct{})
	go func() {
		gate.wait()
		close(drained)
	}()
	select {
	case <-waitStarted:
	case <-time.After(time.Second):
		t.Fatal("host-call drain did not reach its wait")
	}
	select {
	case <-drained:
		t.Fatal("host-call drain returned while a callback was still in flight")
	default:
	}
	if _, accepted := gate.begin(); accepted {
		t.Fatal("host-call gate admitted a callback after shutdown started")
	}

	done()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("host-call drain did not finish after the callback returned")
	}
}

func TestNativeShutdownDrainsCallbacksBeforeClearingHostAPI(t *testing.T) {
	nativeLifecycleMu.Lock()
	globalMu.Lock()
	originalPlugin := globalPlugin
	globalPlugin = nil
	globalMu.Unlock()
	waitStarted := make(chan struct{})
	hostCalls = hostCallGate{waitStarted: func() { close(waitStarted) }}
	originalClear := clearStoredHostAPI
	cleared := make(chan struct{})
	clearStoredHostAPI = func() { close(cleared) }
	nativeLifecycleMu.Unlock()
	t.Cleanup(func() {
		nativeLifecycleMu.Lock()
		globalMu.Lock()
		globalPlugin = originalPlugin
		globalMu.Unlock()
		hostCalls = hostCallGate{}
		clearStoredHostAPI = originalClear
		nativeLifecycleMu.Unlock()
	})

	finish, ok := hostCalls.begin()
	if !ok {
		t.Fatal("fresh native host-call gate rejected admission")
	}
	shutdownDone := make(chan struct{})
	go func() {
		cliproxyPluginShutdown()
		close(shutdownDone)
	}()
	select {
	case <-waitStarted:
	case <-time.After(time.Second):
		t.Fatal("native shutdown did not reach the host-callback wait")
	}
	select {
	case <-cleared:
		t.Fatal("native shutdown cleared the host API while a callback was in flight")
	case <-shutdownDone:
		t.Fatal("native shutdown returned while a callback was in flight")
	default:
	}

	finish()
	select {
	case <-cleared:
	case <-time.After(time.Second):
		t.Fatal("native shutdown did not clear the host API after callback drain")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("native shutdown did not return after clearing the host API")
	}
}

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
