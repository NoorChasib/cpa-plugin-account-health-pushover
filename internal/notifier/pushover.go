package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
)

const ProductionEndpoint = "https://api.pushover.net/1/messages.json"

var retryBackoffs = []time.Duration{0, 5 * time.Second, 10 * time.Second}

type Message struct {
	Kind       string
	Title      string
	Body       string
	Priority   int
	Provider   string
	AccountKey string
	Label      string
	Reason     string
}

type DeliveryResult struct {
	Accepted bool
	At       time.Time
	Error    string
}

type Status struct {
	Configuration       config.CredentialStatus `json:"configuration"`
	LastSuccessfulSend  time.Time               `json:"last_successful_send,omitempty"`
	LastError           string                  `json:"last_error,omitempty"`
	APILimitRemaining   string                  `json:"api_limit_remaining,omitempty"`
	NotificationQueue   int                     `json:"notification_queue"`
	NotificationDropped uint64                  `json:"notification_dropped"`
}

type Client struct {
	cfg      config.Config
	endpoint string
	http     *http.Client
	sleep    func(context.Context, time.Duration) error
	now      func() time.Time

	mu     sync.RWMutex
	status Status
}

func NewClient(cfg config.Config, endpoint string, httpClient *http.Client) *Client {
	if endpoint == "" {
		endpoint = ProductionEndpoint
	}
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: cfg.HTTPTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	client := &Client{
		cfg:      cfg,
		endpoint: endpoint,
		http:     httpClient,
		sleep:    sleepContext,
		now:      time.Now,
	}
	_, status := cfg.ResolveCredentials(nil)
	client.status.Configuration = status
	return client
}

func (c *Client) DeliveryTimeout() time.Duration {
	budget := c.cfg.HTTPTimeout * time.Duration(len(retryBackoffs))
	for _, backoff := range retryBackoffs {
		budget += backoff
	}
	return budget + time.Second
}

func (c *Client) Send(ctx context.Context, message Message) DeliveryResult {
	credentials, credentialStatus := c.cfg.ResolveCredentials(nil)
	c.setConfiguration(credentialStatus)
	if credentialStatus.State != "configured" {
		result := DeliveryResult{Error: credentialStatus.Error}
		c.record(result, "")
		return result
	}
	message.Title = BoundText(message.Title, 250)
	message.Body = BoundMessage(message.Body, maxPushoverMessageRunes)
	if message.Body == "" {
		message.Body = "CLIProxyAPI account health notification"
	}

	form := url.Values{
		"token":    []string{credentials.AppToken},
		"user":     []string{credentials.UserKey},
		"message":  []string{message.Body},
		"title":    []string{message.Title},
		"priority": []string{strconv.Itoa(message.Priority)},
	}
	if credentials.Device != "" {
		form.Set("device", credentials.Device)
	}

	var final DeliveryResult
	var remaining string
	for attempt, backoff := range retryBackoffs {
		if backoff > 0 {
			if err := c.sleep(ctx, backoff); err != nil {
				final = DeliveryResult{Error: "Pushover send canceled"}
				break
			}
		}
		result, retry, apiRemaining := c.sendOnce(ctx, form)
		remaining = apiRemaining
		final = result
		if result.Accepted || !retry || attempt == len(retryBackoffs)-1 {
			break
		}
	}
	c.record(final, remaining)
	return final
}

func (c *Client) sendOnce(ctx context.Context, form url.Values) (DeliveryResult, bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return DeliveryResult{Error: "could not create Pushover request"}, false, ""
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return DeliveryResult{Error: "Pushover network request failed"}, true, ""
	}
	defer resp.Body.Close()
	remaining := sanitizeHeader(resp.Header.Get("X-Limit-App-Remaining"))
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if readErr != nil {
		return DeliveryResult{Error: "could not read Pushover response"}, resp.StatusCode >= 500, remaining
	}
	var payload struct {
		Status int `json:"status"`
	}
	jsonErr := json.Unmarshal(body, &payload)
	if resp.StatusCode == http.StatusOK && jsonErr == nil && payload.Status == 1 {
		return DeliveryResult{Accepted: true, At: c.now().UTC()}, false, remaining
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout {
		return DeliveryResult{Error: fmt.Sprintf("Pushover service error (HTTP %d)", resp.StatusCode)}, true, remaining
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return DeliveryResult{Error: "Pushover message quota exceeded (HTTP 429)"}, false, remaining
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return DeliveryResult{Error: fmt.Sprintf("Pushover rejected request (HTTP %d)", resp.StatusCode)}, false, remaining
	}
	if jsonErr != nil {
		return DeliveryResult{Error: "Pushover returned an invalid response"}, false, remaining
	}
	return DeliveryResult{Error: "Pushover did not accept the message"}, false, remaining
}

func (c *Client) Snapshot() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

func (c *Client) setQueue(size int, dropped uint64) {
	c.mu.Lock()
	c.status.NotificationQueue = size
	c.status.NotificationDropped = dropped
	c.mu.Unlock()
}

func (c *Client) setConfiguration(status config.CredentialStatus) {
	c.mu.Lock()
	c.status.Configuration = status
	c.mu.Unlock()
}

func (c *Client) record(result DeliveryResult, remaining string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if result.Accepted {
		c.status.LastSuccessfulSend = result.At
		c.status.LastError = ""
	} else {
		c.status.LastError = result.Error
	}
	if remaining != "" {
		c.status.APILimitRemaining = remaining
	}
}

type Job struct {
	Message  Message
	Valid    func() bool
	Callback func(DeliveryResult)
}

// defaultStopTimeout bounds how long Stop waits for the delivery worker to
// exit after cancellation. Cancellation aborts in-flight HTTP requests and
// retry backoffs almost immediately, so this bound only matters when a
// delivery ignores cancellation; the worker is then safely abandoned.
const defaultStopTimeout = 5 * time.Second

type Dispatcher struct {
	client         *Client
	queue          chan Job
	coalesceWindow time.Duration
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	stopTimeout    time.Duration
	dropped        atomic.Uint64

	stateMu   sync.Mutex
	accepting bool
	started   bool
	pending   int
	idle      chan struct{}
	flush     chan struct{}
	flushOnce sync.Once
}

func NewDispatcher(client *Client, queueSize int, coalesceWindow time.Duration) *Dispatcher {
	if queueSize < 1 {
		queueSize = 64
	}
	ctx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &Dispatcher{
		client:         client,
		queue:          make(chan Job, queueSize),
		coalesceWindow: coalesceWindow,
		ctx:            ctx,
		cancel:         cancel,
		done:           make(chan struct{}),
		stopTimeout:    defaultStopTimeout,
		accepting:      true,
		idle:           idle,
		flush:          make(chan struct{}),
	}
}

func (d *Dispatcher) Start() {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if !d.accepting || d.started {
		return
	}
	d.started = true
	go d.run()
}

// BeginDrain closes enqueue admission and flushes any active coalescing window
// without canceling accepted work. Callers can then wait on WaitIdle with a
// separate bound before using Stop to cancel a stuck delivery.
func (d *Dispatcher) BeginDrain() {
	d.stateMu.Lock()
	d.accepting = false
	d.stateMu.Unlock()
	d.flushOnce.Do(func() { close(d.flush) })
}

// Stop cancels any in-flight delivery (aborting the HTTP request or retry
// backoff) and waits for the delivery worker to exit. The wait is hard-bounded
// by stopTimeout so host lifecycle calls never hang behind a stuck delivery: a
// worker that does not exit in time is safely abandoned — it holds no locks
// Stop's caller needs, cannot start new deliveries, and exits on its own once
// its blocking call returns.
func (d *Dispatcher) Stop() {
	d.BeginDrain()
	d.cancel()
	d.stateMu.Lock()
	started := d.started
	d.stateMu.Unlock()
	if !started {
		d.abandonQueued()
		return
	}
	timer := time.NewTimer(d.stopTimeout)
	defer timer.Stop()
	select {
	case <-d.done:
	case <-timer.C:
	}
}

func (d *Dispatcher) Enqueue(job Job) bool {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if !d.accepting {
		d.dropped.Add(1)
		d.updateQueueStatus()
		return false
	}
	select {
	case d.queue <- job:
		if d.pending == 0 {
			d.idle = make(chan struct{})
		}
		d.pending++
		d.updateQueueStatus()
		return true
	default:
		d.dropped.Add(1)
		d.updateQueueStatus()
		return false
	}
}

func (d *Dispatcher) WaitIdle(ctx context.Context) bool {
	d.stateMu.Lock()
	idle := d.idle
	d.stateMu.Unlock()
	select {
	case <-idle:
		return true
	case <-ctx.Done():
		return false
	}
}

func (d *Dispatcher) finishJobs(count int) {
	if count <= 0 {
		return
	}
	d.stateMu.Lock()
	if count > d.pending {
		count = d.pending
	}
	if count == 0 {
		d.stateMu.Unlock()
		return
	}
	d.pending -= count
	if d.pending == 0 {
		close(d.idle)
	}
	d.stateMu.Unlock()
}

func (d *Dispatcher) abandonQueued() {
	for {
		select {
		case <-d.queue:
			d.finishJobs(1)
		default:
			d.updateQueueStatus()
			return
		}
	}
}

func (d *Dispatcher) run() {
	defer close(d.done)
	for {
		select {
		case <-d.ctx.Done():
			d.abandonQueued()
			return
		case first := <-d.queue:
			batch, ok := d.collect(first)
			if !ok {
				d.finishJobs(len(batch))
				d.abandonQueued()
				return
			}
			d.updateQueueStatus()
			d.deliverBatch(d.ctx, batch)
			if d.ctx.Err() != nil {
				d.abandonQueued()
				return
			}
		}
	}
}

// collect gathers jobs that arrive within the coalesce window. It reports
// false when the dispatcher was stopped while collecting; the batch is then
// abandoned undelivered.
func (d *Dispatcher) collect(first Job) ([]Job, bool) {
	batch := []Job{first}
	if d.coalesceWindow <= 0 {
		return batch, true
	}
	timer := time.NewTimer(d.coalesceWindow)
	defer timer.Stop()
	for {
		select {
		case job := <-d.queue:
			batch = append(batch, job)
			if len(batch) >= 32 {
				return batch, true
			}
		case <-timer.C:
			return batch, true
		case <-d.flush:
			return batch, true
		case <-d.ctx.Done():
			return batch, false
		}
	}
}

// deliverBatch sends each coalesced group with a delivery context derived from
// parent, so stopping the dispatcher cancels the in-flight HTTP request or
// retry backoff. The interrupted group's callbacks still receive the final
// (canceled) result; groups not yet started are abandoned.
func (d *Dispatcher) deliverBatch(parent context.Context, batch []Job) {
	defer d.finishJobs(len(batch))
	groups := make(map[string][]Job)
	order := make([]string, 0)
	for _, job := range batch {
		if job.Valid != nil && !job.Valid() {
			continue
		}
		key := job.Message.Kind + "|" + strconv.Itoa(job.Message.Priority)
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], job)
	}
	for _, key := range order {
		if parent.Err() != nil {
			return
		}
		jobs := groups[key]
		stillValid := jobs[:0]
		for _, job := range jobs {
			if job.Valid == nil || job.Valid() {
				stillValid = append(stillValid, job)
			}
		}
		jobs = stillValid
		if len(jobs) == 0 {
			continue
		}

		coalesced := len(jobs) > 1
		messageGroups := [][]Job{jobs}
		if coalesced {
			messageGroups = splitCoalescedJobs(jobs)
		}
		for _, messageJobs := range messageGroups {
			if parent.Err() != nil {
				return
			}
			stillValid = messageJobs[:0]
			for _, job := range messageJobs {
				if job.Valid == nil || job.Valid() {
					stillValid = append(stillValid, job)
				}
			}
			messageJobs = stillValid
			if len(messageJobs) == 0 {
				continue
			}
			message := messageJobs[0].Message
			if coalesced {
				message = coalescedMessage(messageJobs)
			}
			ctx, cancel := context.WithTimeout(parent, d.client.DeliveryTimeout())
			result := d.client.Send(ctx, message)
			cancel()
			for _, job := range messageJobs {
				if job.Callback != nil {
					job.Callback(result)
				}
			}
		}
	}
}

func (d *Dispatcher) updateQueueStatus() {
	d.client.setQueue(len(d.queue), d.dropped.Load())
}

const maxPushoverMessageRunes = 1024

func splitCoalescedJobs(jobs []Job) [][]Job {
	groups := make([][]Job, 0, 1)
	current := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		candidate := append(append([]Job(nil), current...), job)
		if len(current) > 0 && utf8.RuneCountInString(coalescedMessage(candidate).Body) > maxPushoverMessageRunes {
			groups = append(groups, current)
			current = []Job{job}
			continue
		}
		current = candidate
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

func coalescedMessage(jobs []Job) Message {
	first := jobs[0].Message
	var title string
	switch first.Kind {
	case "recovery":
		title = "CLIProxyAPI: multiple accounts recovered"
	case "reminder":
		title = "CLIProxyAPI: unresolved account incidents"
	default:
		title = "CLIProxyAPI: multiple account health alerts"
	}
	var body strings.Builder
	body.WriteString(strconv.Itoa(len(jobs)))
	body.WriteString(" account transitions were detected:\n")
	for _, job := range jobs {
		body.WriteString("- ")
		body.WriteString(BoundText(job.Message.Provider, 32))
		body.WriteString(": ")
		body.WriteString(BoundText(job.Message.Label, 120))
		if reason := BoundText(job.Message.Reason, 80); reason != "" {
			body.WriteString(" (")
			body.WriteString(reason)
			body.WriteString(")")
		}
		body.WriteByte('\n')
	}
	first.Title = title
	first.Body = strings.TrimSpace(body.String())
	return first
}

func BoundText(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	return truncate(value, maxRunes)
}

func BoundMessage(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	return truncate(value, maxRunes)
}

func truncate(value string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	if maxRunes == 1 {
		return string(runes[:1])
	}
	return string(runes[:maxRunes-1]) + "…"
}

func sanitizeHeader(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 32 {
		value = value[:32]
	}
	for _, r := range value {
		if !unicode.IsDigit(r) {
			return ""
		}
	}
	return value
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
