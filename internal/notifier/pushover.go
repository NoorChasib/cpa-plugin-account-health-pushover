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
	message.Body = BoundMessage(message.Body, 1024)
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

type Dispatcher struct {
	client         *Client
	queue          chan Job
	coalesceWindow time.Duration
	stop           chan struct{}
	done           chan struct{}
	stopOnce       sync.Once

	mu      sync.Mutex
	dropped uint64
}

func NewDispatcher(client *Client, queueSize int, coalesceWindow time.Duration) *Dispatcher {
	if queueSize < 1 {
		queueSize = 64
	}
	return &Dispatcher{
		client:         client,
		queue:          make(chan Job, queueSize),
		coalesceWindow: coalesceWindow,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
}

func (d *Dispatcher) Start() {
	go d.run()
}

func (d *Dispatcher) Stop() {
	d.stopOnce.Do(func() { close(d.stop) })
	<-d.done
}

func (d *Dispatcher) Enqueue(job Job) bool {
	select {
	case d.queue <- job:
		d.updateQueueStatus()
		return true
	default:
		d.mu.Lock()
		d.dropped++
		d.mu.Unlock()
		d.updateQueueStatus()
		return false
	}
}

func (d *Dispatcher) run() {
	defer close(d.done)
	for {
		select {
		case <-d.stop:
			return
		case first := <-d.queue:
			batch := []Job{first}
			if d.coalesceWindow > 0 {
				timer := time.NewTimer(d.coalesceWindow)
			collect:
				for {
					select {
					case job := <-d.queue:
						batch = append(batch, job)
						if len(batch) >= 32 {
							break collect
						}
					case <-timer.C:
						break collect
					case <-d.stop:
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
						return
					}
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			d.updateQueueStatus()
			d.deliverBatch(batch)
		}
	}
}

func (d *Dispatcher) deliverBatch(batch []Job) {
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
		message := jobs[0].Message
		if len(jobs) > 1 {
			message = coalescedMessage(jobs)
		}
		ctx, cancel := context.WithTimeout(context.Background(), d.client.DeliveryTimeout())
		result := d.client.Send(ctx, message)
		cancel()
		for _, job := range jobs {
			if job.Callback != nil {
				job.Callback(result)
			}
		}
	}
}

func (d *Dispatcher) updateQueueStatus() {
	d.mu.Lock()
	dropped := d.dropped
	d.mu.Unlock()
	d.client.setQueue(len(d.queue), dropped)
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
		body.WriteString(job.Message.Provider)
		body.WriteString(": ")
		body.WriteString(job.Message.Label)
		if job.Message.Reason != "" {
			body.WriteString(" (")
			body.WriteString(job.Message.Reason)
			body.WriteString(")")
		}
		body.WriteByte('\n')
	}
	first.Title = title
	first.Body = BoundMessage(body.String(), 1024)
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
