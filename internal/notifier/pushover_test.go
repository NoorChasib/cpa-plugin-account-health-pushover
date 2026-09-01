package notifier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
	snapshot := client.Snapshot()
	if !strings.Contains(snapshot.LastError, "quota") || !snapshot.LastSuccessfulSend.IsZero() {
		t.Fatalf("snapshot does not report the 429 delivery as unhealthy: %+v", snapshot)
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

func TestDispatcherRejectsEnqueueAfterStoppingStarts(t *testing.T) {
	cfg := configuredTestConfig(t)
	client := NewClient(cfg, ProductionEndpoint, nil)
	dispatcher := NewDispatcher(client, 4, 0)
	dispatcher.Start()
	dispatcher.Stop()

	before := len(dispatcher.queue)
	if dispatcher.Enqueue(Job{Message: Message{Body: "must not be queued"}}) {
		t.Fatal("dispatcher accepted work after stopping started")
	}
	if after := len(dispatcher.queue); after != before {
		t.Fatalf("stopped dispatcher queue length changed from %d to %d", before, after)
	}
}

func TestDispatcherBeginDrainFlushesCoalescingAndRejectsNewWork(t *testing.T) {
	cfg := configuredTestConfig(t)
	request := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request <- struct{}{}
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	dispatcher := NewDispatcher(client, 4, time.Hour)
	dispatcher.Start()
	defer dispatcher.Stop()

	callback := make(chan DeliveryResult, 1)
	if !dispatcher.Enqueue(Job{
		Message:  Message{Kind: "failure", Body: "accepted before drain"},
		Callback: func(result DeliveryResult) { callback <- result },
	}) {
		t.Fatal("could not enqueue pre-drain job")
	}
	dispatcher.BeginDrain()
	if dispatcher.Enqueue(Job{Message: Message{Body: "must be rejected"}}) {
		t.Fatal("dispatcher accepted work after draining started")
	}
	select {
	case <-request:
	case <-time.After(time.Second):
		t.Fatal("drain did not flush the active coalescing window")
	}
	select {
	case result := <-callback:
		if !result.Accepted {
			t.Fatalf("pre-drain job was not delivered: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-drain job callback did not complete")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !dispatcher.WaitIdle(ctx) {
		t.Fatal("dispatcher did not become idle after draining accepted work")
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
	invalidCallback := make(chan struct{}, 1)
	if !dispatcher.Enqueue(Job{
		Message: Message{Kind: "stale", Body: "stale"},
		Valid:   func() bool { return false },
		Callback: func(DeliveryResult) {
			invalidCallback <- struct{}{}
		},
	}) {
		t.Fatal("could not enqueue test job")
	}
	// The sentinel uses a different kind, so if the invalid job were sent it
	// would form its own, earlier group; the sentinel callback therefore only
	// fires after every send the invalid job could have caused.
	sentinelDone := make(chan DeliveryResult, 1)
	if !dispatcher.Enqueue(Job{
		Message: Message{Kind: "sentinel", Body: "sentinel"},
		Callback: func(result DeliveryResult) {
			sentinelDone <- result
		},
	}) {
		t.Fatal("could not enqueue sentinel job")
	}
	select {
	case result := <-sentinelDone:
		if !result.Accepted {
			t.Fatalf("sentinel delivery failed: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for sentinel delivery")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected only the sentinel send, Pushover was called %d times", calls.Load())
	}
	select {
	case <-invalidCallback:
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

func TestDispatcherSplitsOversizedCoalescedGroupsAndScopesCallbacks(t *testing.T) {
	cfg := configuredTestConfig(t)
	var requestMu sync.Mutex
	var bodies []string
	var requestAccepted []bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		requestMu.Lock()
		index := len(bodies)
		body := r.PostForm.Get("message")
		bodies = append(bodies, body)
		accepted := index%2 == 0
		requestAccepted = append(requestAccepted, accepted)
		requestMu.Unlock()
		if !accepted {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"status":0}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()

	client := NewClient(cfg, server.URL, server.Client())
	dispatcher := NewDispatcher(client, 64, 5*time.Second)
	dispatcher.Start()
	defer dispatcher.Stop()

	const jobCount = 32
	labels := make([]string, jobCount)
	results := make([]DeliveryResult, jobCount)
	callbackCounts := make([]int, jobCount)
	done := make(chan int, jobCount)
	for i := 0; i < jobCount; i++ {
		i := i
		labels[i] = fmt.Sprintf("account-%02d-%s", i, strings.Repeat("x", 56))
		if !dispatcher.Enqueue(Job{
			Message: Message{
				Kind:     "failure",
				Priority: 1,
				Provider: "Claude",
				Label:    labels[i],
				Reason:   "persistent_unauthorized_request",
			},
			Callback: func(result DeliveryResult) {
				results[i] = result
				callbackCounts[i]++
				done <- i
			},
		}) {
			t.Fatalf("could not enqueue job %d", i)
		}
	}
	for range jobCount {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for split-batch callbacks")
		}
	}

	requestMu.Lock()
	gotBodies := append([]string(nil), bodies...)
	gotAccepted := append([]bool(nil), requestAccepted...)
	requestMu.Unlock()
	if len(gotBodies) < 2 {
		t.Fatalf("oversized group used %d request; want multiple", len(gotBodies))
	}
	for requestIndex, body := range gotBodies {
		if len([]rune(body)) > 1024 {
			t.Fatalf("request %d has %d runes", requestIndex, len([]rune(body)))
		}
	}
	for i, label := range labels {
		includedIn := -1
		for requestIndex, body := range gotBodies {
			if strings.Contains(body, label) {
				if includedIn >= 0 {
					t.Fatalf("job %d appeared in multiple requests", i)
				}
				includedIn = requestIndex
			}
		}
		if includedIn < 0 {
			t.Fatalf("job %d, including the final account, was omitted from all requests", i)
		}
		if callbackCounts[i] != 1 {
			t.Fatalf("job %d callback count=%d, want exactly one", i, callbackCounts[i])
		}
		if results[i].Accepted != gotAccepted[includedIn] {
			t.Fatalf("job %d callback accepted=%v, request %d accepted=%v", i, results[i].Accepted, includedIn, gotAccepted[includedIn])
		}
	}
}

func TestDispatcherRevalidatesEachSplitGroupBeforeSending(t *testing.T) {
	cfg := configuredTestConfig(t)
	var requestMu sync.Mutex
	var bodies []string
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		requestMu.Lock()
		requestIndex := len(bodies)
		bodies = append(bodies, r.PostForm.Get("message"))
		requestMu.Unlock()
		if requestIndex == 0 {
			firstOnce.Do(func() { close(firstStarted) })
			<-releaseFirst
		}
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer server.Close()

	client := NewClient(cfg, server.URL, server.Client())
	dispatcher := NewDispatcher(client, 64, time.Hour)
	dispatcher.Start()
	defer dispatcher.Stop()

	const jobCount = 32
	labels := make([]string, jobCount)
	valid := make([]atomic.Bool, jobCount)
	callbacks := make([]atomic.Int32, jobCount)
	for i := 0; i < jobCount; i++ {
		i := i
		labels[i] = fmt.Sprintf("account-%02d-%s", i, strings.Repeat("x", 100))
		valid[i].Store(true)
		if !dispatcher.Enqueue(Job{
			Message: Message{Kind: "failure", Priority: 1, Provider: "Claude", Label: labels[i], Reason: strings.Repeat("r", 80)},
			Valid:   func() bool { return valid[i].Load() },
			Callback: func(DeliveryResult) {
				callbacks[i].Add(1)
			},
		}) {
			t.Fatalf("could not enqueue job %d", i)
		}
	}
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first split request did not start")
	}
	valid[jobCount-1].Store(false)
	close(releaseFirst)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !dispatcher.WaitIdle(ctx) {
		t.Fatal("split delivery did not become idle")
	}

	requestMu.Lock()
	gotBodies := append([]string(nil), bodies...)
	requestMu.Unlock()
	if len(gotBodies) < 2 {
		t.Fatalf("test did not create multiple split requests: %d", len(gotBodies))
	}
	for _, body := range gotBodies {
		if strings.Contains(body, labels[jobCount-1]) {
			t.Fatal("job invalidated during an earlier split request was still sent")
		}
	}
	if got := callbacks[jobCount-1].Load(); got != 0 {
		t.Fatalf("invalidated split job callback count=%d, want zero", got)
	}
}

func TestDispatcherStopCancelsInFlightHTTPDelivery(t *testing.T) {
	cfg := configuredTestConfig(t)
	requestStarted := make(chan struct{})
	handlerRelease := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Consume the body so the server arms client-disconnect detection.
		_, _ = io.Copy(io.Discard, r.Body)
		startedOnce.Do(func() { close(requestStarted) })
		// Never respond; the canceled client's disconnect ends the request.
		// The release channel is a failsafe so server.Close can never hang.
		select {
		case <-r.Context().Done():
		case <-handlerRelease:
		}
	}))
	defer server.Close()
	defer close(handlerRelease)
	// No client timeout: cancellation via Stop must be what aborts the request.
	client := NewClient(cfg, server.URL, &http.Client{})
	dispatcher := NewDispatcher(client, 4, 0)
	dispatcher.Start()
	delivered := make(chan DeliveryResult, 1)
	if !dispatcher.Enqueue(Job{
		Message:  Message{Body: "in-flight"},
		Callback: func(result DeliveryResult) { delivered <- result },
	}) {
		t.Fatal("could not enqueue test job")
	}
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery request never reached the test server")
	}
	stopReturned := make(chan struct{})
	go func() {
		dispatcher.Stop()
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return while an HTTP request was in flight")
	}
	select {
	case result := <-delivered:
		if result.Accepted || result.Error == "" {
			t.Fatalf("canceled in-flight delivery reported success: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight job callback was not invoked after cancellation")
	}
	select {
	case <-dispatcher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher worker did not exit after cancellation")
	}
}

func TestDispatcherStopCancelsRetryBackoff(t *testing.T) {
	cfg := configuredTestConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"status":0}`)
	}))
	defer server.Close()
	client := NewClient(cfg, server.URL, server.Client())
	sleeping := make(chan struct{})
	var sleepOnce sync.Once
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		sleepOnce.Do(func() { close(sleeping) })
		// Only cancellation can end the backoff; a missed cancel hangs here.
		<-ctx.Done()
		return ctx.Err()
	}
	dispatcher := NewDispatcher(client, 4, 0)
	dispatcher.Start()
	delivered := make(chan DeliveryResult, 1)
	if !dispatcher.Enqueue(Job{
		Message:  Message{Body: "retrying"},
		Callback: func(result DeliveryResult) { delivered <- result },
	}) {
		t.Fatal("could not enqueue test job")
	}
	select {
	case <-sleeping:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery never entered its retry backoff")
	}
	stopReturned := make(chan struct{})
	go func() {
		dispatcher.Stop()
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return while a retry backoff was in progress")
	}
	select {
	case result := <-delivered:
		if result.Accepted || !strings.Contains(result.Error, "canceled") {
			t.Fatalf("canceled backoff did not surface as canceled: %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job callback was not invoked after backoff cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("delivery retried after cancellation: calls=%d", calls.Load())
	}
}

func TestDispatcherStopIsBoundedWhenDeliveryIgnoresCancellation(t *testing.T) {
	cfg := configuredTestConfig(t)
	transport := &failingRoundTripper{}
	client := NewClient(cfg, ProductionEndpoint, &http.Client{Transport: transport})
	sleeping := make(chan struct{})
	stuck := make(chan struct{})
	var sleepOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(stuck) }) }
	defer release()
	client.sleep = func(context.Context, time.Duration) error {
		sleepOnce.Do(func() { close(sleeping) })
		// Deliberately ignore cancellation to simulate a stuck delivery.
		<-stuck
		return nil
	}
	dispatcher := NewDispatcher(client, 4, 0)
	dispatcher.stopTimeout = 50 * time.Millisecond
	dispatcher.Start()
	if !dispatcher.Enqueue(Job{Message: Message{Body: "stuck"}}) {
		t.Fatal("could not enqueue test job")
	}
	select {
	case <-sleeping:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery never got stuck in its backoff")
	}
	stopReturned := make(chan struct{})
	go func() {
		dispatcher.Stop()
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("bounded Stop hung behind a delivery that ignores cancellation")
	}
	select {
	case <-dispatcher.done:
		t.Fatal("worker exited while still stuck; Stop should have abandoned it")
	default:
	}
	release()
	select {
	case <-dispatcher.done:
	case <-time.After(2 * time.Second):
		t.Fatal("abandoned worker did not exit once its blocking call returned")
	}
}

func TestDispatcherStopWithoutStartReturnsImmediately(t *testing.T) {
	cfg := configuredTestConfig(t)
	client := NewClient(cfg, ProductionEndpoint, nil)
	dispatcher := NewDispatcher(client, 4, 0)
	stopReturned := make(chan struct{})
	go func() {
		dispatcher.Stop()
		close(stopReturned)
	}()
	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked for a dispatcher that was never started")
	}
}
