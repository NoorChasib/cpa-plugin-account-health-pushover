package notifier

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
)

func configuredTestConfig(t *testing.T) config.Config {
	t.Helper()
	t.Setenv(config.DefaultAppTokenEnv, strings.Repeat("A", 30))
	t.Setenv(config.DefaultUserKeyEnv, strings.Repeat("B", 30))
	cfg := config.Default()
	cfg.HTTPTimeout = time.Second
	return cfg
}

func TestPushoverSuccessAndBlankDeviceOmitted(t *testing.T) {
	cfg := configuredTestConfig(t)
	var received url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/messages.json" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		received = r.PostForm
		w.Header().Set("X-Limit-App-Remaining", "9999")
		_, _ = io.WriteString(w, `{"status":1,"request":"safe-id"}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL+"/1/messages.json", server.Client())
	result := client.Send(context.Background(), Message{Title: "test", Body: "hello", Priority: 1})
	if !result.Accepted {
		t.Fatalf("send failed: %+v", result)
	}
	if received.Get("token") != strings.Repeat("A", 30) || received.Get("user") != strings.Repeat("B", 30) {
		t.Fatal("credentials not sent to mock endpoint")
	}
	if _, exists := received["device"]; exists {
		t.Fatal("blank device should be omitted")
	}
	if received.Get("priority") != "1" {
		t.Fatalf("priority = %q", received.Get("priority"))
	}
	if got := client.Snapshot().APILimitRemaining; got != "9999" {
		t.Fatalf("remaining = %q", got)
	}
}

func TestPushoverConfiguredDeviceIncluded(t *testing.T) {
	cfg := configuredTestConfig(t)
	cfg.PushoverDevice = "phone"
	var device string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		device = r.PostForm.Get("device")
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	if result := client.Send(context.Background(), Message{Body: "test"}); !result.Accepted {
		t.Fatalf("send failed: %+v", result)
	}
	if device != "phone" {
		t.Fatalf("device = %q", device)
	}
}

func TestPushoverStatusZeroFailsWithoutRetry(t *testing.T) {
	cfg := configuredTestConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"status":0,"errors":["invalid"]}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	result := client.Send(context.Background(), Message{Body: "test"})
	if result.Accepted || calls.Load() != 1 || result.Error == "" {
		t.Fatalf("result=%+v calls=%d", result, calls.Load())
	}
}

func TestPushover400DoesNotRetryOrLeakSecrets(t *testing.T) {
	cfg := configuredTestConfig(t)
	secret := strings.Repeat("A", 30)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"status":0,"errors":["token `+secret+` is invalid"]}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	result := client.Send(context.Background(), Message{Body: "test"})
	if calls.Load() != 1 || result.Accepted {
		t.Fatalf("result=%+v calls=%d", result, calls.Load())
	}
	if strings.Contains(result.Error, secret) || strings.Contains(client.Snapshot().LastError, secret) {
		t.Fatal("sanitized error leaked the app token")
	}
}

func TestPushover429DoesNotBlindlyRetry(t *testing.T) {
	cfg := configuredTestConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"status":0}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	result := client.Send(context.Background(), Message{Body: "test"})
	if calls.Load() != 1 || !strings.Contains(result.Error, "quota") {
		t.Fatalf("result=%+v calls=%d", result, calls.Load())
	}
}

func TestPushover500RetriesAtMostThreeAttempts(t *testing.T) {
	cfg := configuredTestConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"status":0}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	var sleeps atomic.Int32
	client.sleep = func(context.Context, time.Duration) error {
		sleeps.Add(1)
		return nil
	}
	result := client.Send(context.Background(), Message{Body: "test"})
	if result.Accepted || calls.Load() != 3 || sleeps.Load() != 2 {
		t.Fatalf("result=%+v calls=%d sleeps=%d", result, calls.Load(), sleeps.Load())
	}
}

type failingRoundTripper struct {
	calls atomic.Int32
}

func (f *failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls.Add(1)
	return nil, errors.New("network unavailable with SECRET body")
}

func TestPushoverNetworkFailureRetriesAndSanitizes(t *testing.T) {
	cfg := configuredTestConfig(t)
	transport := &failingRoundTripper{}
	client := NewClient(cfg, ProductionEndpoint, &http.Client{Transport: transport, Timeout: time.Second})
	client.sleep = func(context.Context, time.Duration) error { return nil }
	result := client.Send(context.Background(), Message{Body: "test"})
	if transport.calls.Load() != 3 || result.Accepted {
		t.Fatalf("result=%+v calls=%d", result, transport.calls.Load())
	}
	if strings.Contains(result.Error, "SECRET") {
		t.Fatal("network error detail leaked")
	}
}

func TestPushoverMessageLengthsAreBounded(t *testing.T) {
	cfg := configuredTestConfig(t)
	var title, message string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		title = r.PostForm.Get("title")
		message = r.PostForm.Get("message")
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	result := client.Send(context.Background(), Message{
		Title: strings.Repeat("界", 300),
		Body:  strings.Repeat("界", 1200),
	})
	if !result.Accepted {
		t.Fatal(result.Error)
	}
	if len([]rune(title)) != 250 || len([]rune(message)) != 1024 {
		t.Fatalf("title=%d message=%d", len([]rune(title)), len([]rune(message)))
	}
}

func TestDispatcherQueueIsBoundedAndNonBlocking(t *testing.T) {
	cfg := configuredTestConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	dispatcher := NewDispatcher(client, 1, 0)
	start := time.Now()
	first := dispatcher.Enqueue(Job{Message: Message{Body: "one"}})
	second := dispatcher.Enqueue(Job{Message: Message{Body: "two"}})
	if time.Since(start) > 20*time.Millisecond {
		t.Fatal("enqueue blocked")
	}
	if !first || second {
		t.Fatalf("enqueue results = %v, %v", first, second)
	}
	if client.Snapshot().NotificationDropped != 1 {
		t.Fatalf("dropped = %d", client.Snapshot().NotificationDropped)
	}
}

func TestDispatcherDropsInvalidJobBeforeSend(t *testing.T) {
	cfg := configuredTestConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	dispatcher := NewDispatcher(client, 4, 10*time.Millisecond)
	dispatcher.Start()
	defer dispatcher.Stop()
	callbackCalled := make(chan struct{}, 1)
	if !dispatcher.Enqueue(Job{
		Message: Message{Body: "stale"},
		Valid:   func() bool { return false },
		Callback: func(DeliveryResult) {
			callbackCalled <- struct{}{}
		},
	}) {
		t.Fatal("could not enqueue test job")
	}
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("invalid job reached Pushover %d times", calls.Load())
	}
	select {
	case <-callbackCalled:
		t.Fatal("invalid job callback was invoked")
	default:
	}
}

func TestDeliveryTimeoutCoversAllAttemptsAndBackoffs(t *testing.T) {
	cfg := configuredTestConfig(t)
	cfg.HTTPTimeout = 2 * time.Second
	client := NewClient(cfg, ProductionEndpoint, nil)
	wantMinimum := 3*cfg.HTTPTimeout + 15*time.Second
	if got := client.DeliveryTimeout(); got <= wantMinimum {
		t.Fatalf("delivery timeout=%s, must exceed request and backoff budget %s", got, wantMinimum)
	}
}

func TestDispatcherCoalescesSameKindAndPriority(t *testing.T) {
	cfg := configuredTestConfig(t)
	var calls atomic.Int32
	var receivedTitle, receivedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		receivedTitle = r.PostForm.Get("title")
		receivedBody = r.PostForm.Get("message")
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	dispatcher := NewDispatcher(client, 4, 25*time.Millisecond)
	dispatcher.Start()
	defer dispatcher.Stop()
	callbacks := make(chan DeliveryResult, 2)
	for _, label := range []string{"account one", "account two"} {
		if !dispatcher.Enqueue(Job{
			Message: Message{Kind: "failure", Title: "single", Body: "single", Priority: 1, Provider: "Claude", Label: label, Reason: "unauthorized"},
			Callback: func(result DeliveryResult) {
				callbacks <- result
			},
		}) {
			t.Fatal("could not enqueue coalescing test job")
		}
	}
	for range 2 {
		select {
		case result := <-callbacks:
			if !result.Accepted {
				t.Fatalf("coalesced delivery failed: %+v", result)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for coalesced callback")
		}
	}
	if calls.Load() != 1 || receivedTitle != "CLIProxyAPI: multiple account health alerts" || !strings.Contains(receivedBody, "2 account transitions") {
		t.Fatalf("calls=%d title=%q body=%q", calls.Load(), receivedTitle, receivedBody)
	}
}
