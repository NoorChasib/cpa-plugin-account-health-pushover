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
	Accepted    bool
	Unattempted bool
	At          time.Time
	Error       string
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

type dispatchState struct {
	attempted        bool
	resolving        bool
	abandonmentState uint8
	done             chan struct{}
}

type Job struct {
	Message Message
	Valid   func() bool
	// Abandon is a nonblocking, idempotent internal bookkeeping hook. It is
	// completed before stop-side retirement and before an unattempted job's
	// validity check. A stop racing an already-running hook may invoke it again.
	Abandon  func()
	Callback func(DeliveryResult)
	state    *dispatchState
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

	stopMu                 sync.Mutex
	stateMu                sync.Mutex
	resolverWG             sync.WaitGroup
	accepting              bool
	started                bool
	stopping               bool
	pending                int
	pendingJobs            map[*dispatchState]Job
	idle                   chan struct{}
	flush                  chan struct{}
	flushOnce              sync.Once
	collectStarted         func()
	beforeMarkAttempted    func()
	beforeAbandon          func()
	beforeUnattemptedClaim func()
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
		pendingJobs:    make(map[*dispatchState]Job),
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
	d.StopWithin(d.stopTimeout)
}

func (d *Dispatcher) StopWithin(timeout time.Duration) {
	d.stopMu.Lock()
	defer d.stopMu.Unlock()
	deadline := time.Now().Add(timeout)
	jobs, started := d.beginStop()
	d.abandonJobsAsync(jobs)
	d.discardQueue()
	if timeout <= 0 || !d.waitJobsUntil(jobs, deadline) {
		return
	}
	if started {
		d.waitUntil(d.done, deadline)
	}
}

// StopAndWait performs the final, unbounded worker join required before native
// code can be unloaded. Bounded lifecycle paths use StopWithin and retain the
// dispatcher for this final join when a delivery ignores cancellation.
func (d *Dispatcher) StopAndWait() {
	d.stopMu.Lock()
	defer d.stopMu.Unlock()
	jobs, started := d.beginStop()
	d.abandonJobsAsync(jobs)
	d.discardQueue()
	for _, job := range jobs {
		<-job.state.done
	}
	if started {
		<-d.done
	}
	d.stateMu.Lock()
	idle := d.idle
	d.stateMu.Unlock()
	<-idle
	d.resolverWG.Wait()
}

func (d *Dispatcher) beginStop() ([]Job, bool) {
	d.BeginDrain()
	d.stateMu.Lock()
	d.stopping = true
	d.cancel()
	jobs := make([]Job, 0, len(d.pendingJobs))
	abandonments := make([]Job, 0, len(d.pendingJobs))
	for state, job := range d.pendingJobs {
		if state.attempted {
			continue
		}
		jobs = append(jobs, job)
		if state.abandonmentState < 2 {
			if state.abandonmentState == 0 {
				state.abandonmentState = 1
			}
			// State 1 may belong to a preempted worker. Re-running the
			// idempotent hook here guarantees completion before retirement.
			abandonments = append(abandonments, job)
		}
	}
	started := d.started
	d.stateMu.Unlock()
	for _, job := range abandonments {
		d.completeAbandonment(job)
	}
	return jobs, started
}

func (d *Dispatcher) waitJobsUntil(jobs []Job, deadline time.Time) bool {
	for _, job := range jobs {
		if !d.waitUntil(job.state.done, deadline) {
			return false
		}
	}
	return true
}

func (d *Dispatcher) waitUntil(done <-chan struct{}, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
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
	job.state = &dispatchState{done: make(chan struct{})}
	select {
	case d.queue <- job:
		if d.pending == 0 {
			d.idle = make(chan struct{})
		}
		d.pending++
		d.pendingJobs[job.state] = job
		d.updateQueueStatus()
		return true
	default:
		d.dropped.Add(1)
		d.updateQueueStatus()
		return false
	}
}

func (d *Dispatcher) WaitStopped(ctx context.Context) bool {
	select {
	case <-d.done:
		return true
	case <-ctx.Done():
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

func (d *Dispatcher) resolve(job Job, result DeliveryResult, callback bool) bool {
	if !d.claimResolution(job, false) {
		return false
	}
	defer d.finishResolution(job)
	if callback && job.Callback != nil {
		job.Callback(result)
	}
	return true
}

func (d *Dispatcher) claimResolution(job Job, unattempted bool) bool {
	if job.state == nil {
		return false
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if _, pending := d.pendingJobs[job.state]; !pending || job.state.resolving || (unattempted && job.state.attempted) {
		return false
	}
	job.state.resolving = true
	return true
}

func (d *Dispatcher) finishResolution(job Job) {
	d.stateMu.Lock()
	if _, pending := d.pendingJobs[job.state]; pending {
		delete(d.pendingJobs, job.state)
		if d.pending > 0 {
			d.pending--
		}
		close(job.state.done)
		if d.pending == 0 {
			close(d.idle)
		}
	}
	d.stateMu.Unlock()
}

func (d *Dispatcher) isPending(job Job) bool {
	if job.state == nil {
		return false
	}
	d.stateMu.Lock()
	_, pending := d.pendingJobs[job.state]
	pending = pending && !job.state.resolving
	d.stateMu.Unlock()
	return pending
}

func (d *Dispatcher) noteAbandonment(job Job) {
	if job.state == nil {
		return
	}
	d.stateMu.Lock()
	_, pending := d.pendingJobs[job.state]
	if !pending || job.state.attempted || job.state.abandonmentState != 0 {
		d.stateMu.Unlock()
		return
	}
	job.state.abandonmentState = 1
	d.stateMu.Unlock()
	if d.beforeAbandon != nil {
		d.beforeAbandon()
	}
	d.completeAbandonment(job)
}

func (d *Dispatcher) completeAbandonment(job Job) {
	if job.Abandon != nil {
		job.Abandon()
	}
	d.stateMu.Lock()
	if job.state != nil && job.state.abandonmentState != 2 {
		job.state.abandonmentState = 2
	}
	d.stateMu.Unlock()
}

func (d *Dispatcher) resolveUnattempted(job Job) {
	d.noteAbandonment(job)
	if d.beforeUnattemptedClaim != nil {
		d.beforeUnattemptedClaim()
	}
	if !d.claimResolution(job, true) {
		return
	}
	defer d.finishResolution(job)
	valid := job.Valid == nil || job.Valid()
	if valid && job.Callback != nil {
		job.Callback(DeliveryResult{Unattempted: true})
	}
}

func (d *Dispatcher) abandonJobs(jobs []Job) {
	for _, job := range jobs {
		d.resolveUnattempted(job)
	}
}

func (d *Dispatcher) abandonJobsAsync(jobs []Job) {
	d.resolverWG.Add(len(jobs))
	for _, job := range jobs {
		job := job
		go func() {
			defer d.resolverWG.Done()
			d.resolveUnattempted(job)
		}()
	}
}

func (d *Dispatcher) abandonUnattempted() {
	d.stateMu.Lock()
	jobs := make([]Job, 0, len(d.pendingJobs))
	for state, job := range d.pendingJobs {
		if !state.attempted {
			jobs = append(jobs, job)
		}
	}
	d.stateMu.Unlock()
	d.abandonJobs(jobs)
}

func (d *Dispatcher) drainQueue() {
	for {
		select {
		case job := <-d.queue:
			d.resolveUnattempted(job)
		default:
			d.updateQueueStatus()
			return
		}
	}
}

func (d *Dispatcher) discardQueue() {
	for {
		select {
		case <-d.queue:
		default:
			d.updateQueueStatus()
			return
		}
	}
}

func (d *Dispatcher) validPending(jobs []Job) []Job {
	valid := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if !d.isPending(job) {
			continue
		}
		if job.Valid != nil && !job.Valid() {
			d.noteAbandonment(job)
			d.resolve(job, DeliveryResult{}, false)
			continue
		}
		valid = append(valid, job)
	}
	return valid
}

func (d *Dispatcher) markAttempted(jobs []Job) []Job {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.stopping || d.ctx.Err() != nil {
		return nil
	}
	attempted := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		if _, pending := d.pendingJobs[job.state]; !pending || job.state.attempted || job.state.resolving {
			continue
		}
		job.state.attempted = true
		attempted = append(attempted, job)
	}
	return attempted
}

func (d *Dispatcher) run() {
	defer close(d.done)
	for {
		select {
		case <-d.ctx.Done():
			d.abandonUnattempted()
			d.drainQueue()
			return
		case first := <-d.queue:
			batch, ok := d.collect(first)
			if !ok {
				d.abandonJobs(batch)
				d.abandonUnattempted()
				d.drainQueue()
				return
			}
			d.updateQueueStatus()
			d.deliverBatch(d.ctx, batch)
			if d.ctx.Err() != nil {
				d.abandonUnattempted()
				d.drainQueue()
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
	if d.collectStarted != nil {
		d.collectStarted()
	}
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
	groups := make(map[string][]Job)
	order := make([]string, 0)
	for _, job := range d.validPending(batch) {
		key := job.Message.Kind + "|" + strconv.Itoa(job.Message.Priority)
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], job)
	}
	abandonGroups := func(keys []string) {
		for _, key := range keys {
			d.abandonJobs(groups[key])
		}
	}
	for groupIndex, key := range order {
		if parent.Err() != nil {
			abandonGroups(order[groupIndex:])
			return
		}
		jobs := d.validPending(groups[key])
		if len(jobs) == 0 {
			continue
		}

		messageGroups := [][]Job{jobs}
		if len(jobs) > 1 {
			messageGroups = splitCoalescedJobs(jobs)
		}
		for messageIndex, messageJobs := range messageGroups {
			if parent.Err() != nil {
				for _, remaining := range messageGroups[messageIndex:] {
					d.abandonJobs(remaining)
				}
				abandonGroups(order[groupIndex+1:])
				return
			}
			messageJobs = d.validPending(messageJobs)
			if len(messageJobs) == 0 {
				continue
			}
			if d.beforeMarkAttempted != nil {
				d.beforeMarkAttempted()
			}
			messageJobs = d.markAttempted(messageJobs)
			if len(messageJobs) == 0 {
				continue
			}
			message := messageJobs[0].Message
			if len(messageJobs) > 1 {
				message = coalescedMessage(messageJobs)
			}
			ctx, cancel := context.WithTimeout(parent, d.client.DeliveryTimeout())
			result := d.client.Send(ctx, message)
			cancel()
			for _, job := range messageJobs {
				d.resolve(job, result, true)
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
