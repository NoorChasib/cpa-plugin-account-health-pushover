package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/monitor"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/notifier"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

const (
	ID                        = "account-health-pushover"
	defaultMonitorStopTimeout = 5 * time.Second

	// actionRequestHeader makes browser-originated mutating requests non-simple
	// so an ambient reverse-proxy management credential cannot be ridden by a
	// cross-origin HTML form POST. Non-browser clients (no Origin and no fetch
	// metadata) authenticate with the management key alone.
	actionRequestHeader      = "X-Account-Health-Action"
	actionRequestHeaderValue = "1"
)

var Version = "0.3.2"

type Host = monitor.Host

type usageFailureRecord struct {
	AuthIndex string `json:"AuthIndex"`
	Failed    bool   `json:"Failed"`
	Failure   struct {
		StatusCode int `json:"StatusCode"`
	} `json:"Failure"`
}

type Plugin struct {
	host     Host
	endpoint string
	now      func() time.Time

	lifecycleMu            sync.Mutex
	mu                     sync.RWMutex
	monitor                *monitor.Monitor
	detached               []*monitor.Monitor
	transitioning          bool
	monitorStopTimeout     time.Duration
	managementCheckTimeout time.Duration
	beforeManagementCheck  func()
	beforeMonitorPublish   func()
	beforeFinalMonitorWait func()
}

func New(host Host, endpoint string) *Plugin {
	return &Plugin{
		host:                   host,
		endpoint:               endpoint,
		now:                    time.Now,
		monitorStopTimeout:     defaultMonitorStopTimeout,
		managementCheckTimeout: 30 * time.Second,
	}
}

func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	switch method {
	case protocol.MethodPluginRegister, protocol.MethodPluginReconfigure:
		return p.configure(raw)
	case protocol.MethodPluginQuiesce:
		p.quiesce()
		return map[string]any{}, nil
	case protocol.MethodManagementRegister:
		return managementRegistration(), nil
	case protocol.MethodManagementHandle:
		return p.handleManagement(raw)
	case protocol.MethodUsageHandle:
		return p.handleUsage(raw)
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

func (p *Plugin) Shutdown() {
	p.stopFinal()
}

func (p *Plugin) configure(raw []byte) (protocol.Registration, error) {
	var request protocol.LifecycleRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return protocol.Registration{}, fmt.Errorf("decode lifecycle request: %w", err)
	}
	cfg, err := config.Parse(request.ConfigYAML)
	if err != nil {
		return protocol.Registration{}, err
	}
	client := notifier.NewClient(cfg, p.endpoint, nil)
	dispatcher := notifier.NewDispatcher(client, 64, cfg.NotificationCoalesceWindow)
	newMonitor := monitor.New(cfg, p.host, client, dispatcher)

	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	oldMonitor := p.monitor
	p.monitor = nil
	p.transitioning = true
	p.mu.Unlock()
	if oldMonitor != nil {
		oldMonitor.StopWithin(p.monitorStopTimeout)
		// StopWithin reports host-operation drain, not completion of every
		// cancellation-ignoring delivery or asynchronous retirement write. Retain
		// every retired monitor so final native shutdown can join all plugin-owned
		// work before the shared object is unloaded.
		p.detached = append(p.detached, oldMonitor)
	}
	if p.beforeMonitorPublish != nil {
		p.beforeMonitorPublish()
	}
	newMonitor.Start()
	p.mu.Lock()
	p.monitor = newMonitor
	p.transitioning = false
	p.mu.Unlock()
	return registration(), nil
}

func (p *Plugin) quiesce() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.mu.Lock()
	current := p.monitor
	p.monitor = nil
	p.transitioning = true
	p.mu.Unlock()
	if current != nil {
		current.StopWithin(p.monitorStopTimeout)
		p.detached = append(p.detached, current)
	}
	p.mu.Lock()
	p.transitioning = false
	p.mu.Unlock()
}

func (p *Plugin) stopFinal() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.mu.Lock()
	current := p.monitor
	p.monitor = nil
	p.transitioning = true
	p.mu.Unlock()
	monitors := append([]*monitor.Monitor(nil), p.detached...)
	p.detached = nil
	if current != nil {
		monitors = append(monitors, current)
	}
	if p.beforeFinalMonitorWait != nil {
		p.beforeFinalMonitorWait()
	}
	for _, oldMonitor := range monitors {
		oldMonitor.Stop()
	}
	p.mu.Lock()
	p.transitioning = false
	p.mu.Unlock()
}

func (p *Plugin) handleUsage(raw []byte) (map[string]any, error) {
	// Decode only the fields account-health needs. In particular, Failure.Body
	// is intentionally never materialized as a Go string, logged, persisted,
	// rendered, classified, or copied into a notification.
	var usage usageFailureRecord
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil, fmt.Errorf("decode usage record: %w", err)
	}
	if !usage.Failed || strings.TrimSpace(usage.AuthIndex) == "" {
		return map[string]any{}, nil
	}
	p.mu.RLock()
	current := p.monitor
	p.mu.RUnlock()
	if current != nil {
		current.ObserveUsageFailure(usage.AuthIndex, usage.Failure.StatusCode)
	}
	return map[string]any{}, nil
}

func (p *Plugin) handleManagement(raw []byte) (protocol.ManagementResponse, error) {
	var request protocol.ManagementRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid management request"}), nil
	}
	path := strings.TrimSuffix(request.Path, "/")
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method == "" {
		method = http.MethodGet
	}
	authenticatedRoute := strings.Contains(path, "/management/plugins/") && !strings.Contains(path, "/v0/resource/")
	p.mu.RLock()
	current := p.monitor
	transitioning := p.transitioning
	p.mu.RUnlock()
	if current == nil {
		if transitioning && method == http.MethodPost && strings.HasSuffix(path, "/check") {
			return lifecycleRetryResponse(), nil
		}
		return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": "plugin is not configured"}), nil
	}
	switch {
	case method == http.MethodGet && strings.HasSuffix(path, "/status/html"):
		// Authenticated browser view. The host has already enforced management
		// authentication for /v0/management routes; anything else (including an
		// unexpected resource-tree path) gets the redacted page instead.
		if authenticatedRoute {
			return htmlResponse(http.StatusOK, renderStatusPage(current.Snapshot(), true, p.now())), nil
		}
		return htmlResponse(http.StatusOK, renderStatusPage(redactResourceStatus(current.Snapshot()), false, p.now())), nil
	case method == http.MethodGet && strings.HasSuffix(path, "/status"):
		if authenticatedRoute {
			return jsonResponse(http.StatusOK, current.Snapshot()), nil
		}
		// Unauthenticated resource page: redacted snapshot only. Its inline
		// script may upgrade the view client-side with credentials the browser
		// already holds; the server never authenticates this response.
		return htmlResponse(http.StatusOK, renderStatusPage(redactResourceStatus(current.Snapshot()), false, p.now())), nil
	case method == http.MethodPost && strings.HasSuffix(path, "/check"):
		if !actionRequestAllowed(request.Headers) {
			return actionForbiddenResponse(), nil
		}
		if p.beforeManagementCheck != nil {
			p.beforeManagementCheck()
		}
		ctx, cancel := context.WithTimeout(context.Background(), p.managementCheckTimeout)
		status, checkErr := current.CheckNow(ctx)
		cancel()
		if errors.Is(checkErr, monitor.ErrStopping) {
			p.mu.RLock()
			replacement := p.monitor
			p.mu.RUnlock()
			if replacement != nil && replacement != current {
				retryCtx, cancelRetry := context.WithTimeout(context.Background(), p.managementCheckTimeout)
				status, checkErr = replacement.CheckNow(retryCtx)
				cancelRetry()
			}
		}
		if errors.Is(checkErr, monitor.ErrStopping) || errors.Is(checkErr, context.Canceled) || errors.Is(checkErr, context.DeadlineExceeded) {
			return lifecycleRetryResponse(), nil
		}
		return jsonResponse(http.StatusOK, status), nil
	case method == http.MethodPost && strings.HasSuffix(path, "/test"):
		if !actionRequestAllowed(request.Headers) {
			return actionForbiddenResponse(), nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), current.NotificationTimeout())
		result := current.TestNotification(ctx)
		cancel()
		if !result.Accepted {
			statusCode := http.StatusBadGateway
			errorText := strings.ToLower(result.Error)
			if strings.Contains(errorText, "not configured") || strings.Contains(errorText, "invalid format") {
				statusCode = http.StatusServiceUnavailable
			}
			return jsonResponse(statusCode, map[string]any{"accepted": false, "error": result.Error}), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{"accepted": true, "sent_at": result.At}), nil
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "route not found"}), nil
	}
}

// actionRequestAllowed gates the mutating management actions against browser
// cross-site requests.
//
//   - Requests carrying browser fetch metadata must be same-origin (or a
//     top-level navigation, "none") and include the plugin action header.
//   - Browsers omit fetch metadata for requests to non-secure (plain http://,
//     non-loopback) URLs. In that case a single well-formed http:// Origin
//     plus the action header is accepted: mixed-content blocking prevents an
//     https:// page from posting here, so that is the only origin a
//     legitimate browser page can produce. https://, "null", multi-valued,
//     and malformed origins without metadata are rejected.
//   - Non-browser clients send neither Origin nor fetch metadata and pass on
//     the management key alone.
func actionRequestAllowed(headers http.Header) bool {
	fetchSite, hasFetchSite := headerTokens(headers, "Sec-Fetch-Site")
	if hasFetchSite {
		if len(fetchSite) == 0 {
			return false
		}
		for _, site := range fetchSite {
			if !strings.EqualFold(site, "same-origin") && !strings.EqualFold(site, "none") {
				return false
			}
		}
		return headerHasToken(headers, actionRequestHeader, actionRequestHeaderValue)
	}
	origins, hasOrigin := headerTokens(headers, "Origin")
	if !hasOrigin {
		return true
	}
	if len(origins) != 1 || !isPlainHTTPOrigin(origins[0]) {
		return false
	}
	return headerHasToken(headers, actionRequestHeader, actionRequestHeaderValue)
}

// isPlainHTTPOrigin reports whether value is a well-formed http:// origin
// (scheme and host only). Browsers omit fetch metadata for requests to such
// origins, so this is the shape a legitimate plain-HTTP status page produces.
func isPlainHTTPOrigin(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "http") &&
		parsed.Host != "" &&
		parsed.User == nil &&
		parsed.Path == "" &&
		parsed.RawQuery == "" &&
		parsed.Fragment == "" &&
		!parsed.ForceQuery
}

func headerHasToken(headers http.Header, name, token string) bool {
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		for _, value := range values {
			for _, part := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(part), token) {
					return true
				}
			}
		}
	}
	return false
}

func headerTokens(headers http.Header, name string) ([]string, bool) {
	var tokens []string
	present := false
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		present = true
		for _, value := range values {
			for _, part := range strings.Split(value, ",") {
				if token := strings.TrimSpace(part); token != "" {
					tokens = append(tokens, token)
				}
			}
		}
	}
	return tokens, present
}

func headerPresent(headers http.Header, name string) bool {
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return true
		}
	}
	return false
}

func actionForbiddenResponse() protocol.ManagementResponse {
	return jsonResponse(http.StatusForbidden, map[string]any{
		"error": "browser requests must come from the same origin (or a plain-http origin when fetch metadata is unavailable) and include the " + actionRequestHeader + " header",
	})
}

func lifecycleRetryResponse() protocol.ManagementResponse {
	response := jsonResponse(http.StatusServiceUnavailable, map[string]any{
		"error":     "monitor lifecycle transition in progress",
		"retryable": true,
	})
	response.Headers.Set("Retry-After", "1")
	return response
}

func registration() protocol.Registration {
	return protocol.Registration{
		SchemaVersion: protocol.SchemaVersion,
		Metadata: protocol.Metadata{
			Name:             "Account Health Pushover",
			Version:          Version,
			Author:           "NoorChasib",
			GitHubRepository: "https://github.com/NoorChasib/cpa-plugin-account-health-pushover",
			ConfigFields:     configFields(),
		},
		Capabilities: protocol.RegistrationCapabilities{
			UsagePlugin:   true,
			ManagementAPI: true,
		},
	}
}

func managementRegistration() protocol.ManagementRegistration {
	base := "/plugins/" + ID
	return protocol.ManagementRegistration{
		Routes: []protocol.ManagementRoute{
			{Method: http.MethodGet, Path: base + "/status", Description: "Returns sanitized account-health and notifier status."},
			{Method: http.MethodGet, Path: base + "/status/html", Description: "Authenticated browser status view with Check now and Test notification actions."},
			{Method: http.MethodPost, Path: base + "/check", Description: "Runs an immediate health reconciliation."},
			{Method: http.MethodPost, Path: base + "/test", Description: "Sends a safe Pushover test notification."},
		},
		Resources: []protocol.ResourceRoute{
			{Path: "/status", Menu: "Account Health Pushover", Description: "Shows Claude, Codex, and Grok OAuth health and Pushover delivery state."},
		},
	}
}

func configFields() []protocol.ConfigField {
	field := func(name, typ, description string) protocol.ConfigField {
		return protocol.ConfigField{Name: name, Type: typ, Description: description}
	}
	return []protocol.ConfigField{
		field("providers", "array", "OAuth providers to monitor. Supported values: claude, codex, xai (Grok)."),
		field("scan-interval", "string", "Full health reconciliation interval (default 1m)."),
		field("startup-grace", "string", "Delay before the first baseline scan; usage events retain status evidence but cannot bypass it (default 30s)."),
		field("transient-confirm-after", "string", "How long transient and ambiguous failures remain suspect before credential_down (default 10m)."),
		field("unauthorized-confirm-after", "string", "How long continuing recent request-level 401 evidence must remain unresolved before reauthentication is required; evidence expires after 2m unless repeated 401s refresh it (default 1m)."),
		field("usage-recheck-delay", "string", "Delay that allows CPA OAuth refresh to finish before usage-triggered reconciliation; must not exceed scan-interval (default 10s)."),
		field("notify-recovery", "boolean", "Send one recovery notification after an alerted incident (default true)."),
		field("notify-disabled", "boolean", "Send informational disabled notifications (default false)."),
		field("notify-removed", "boolean", "Send informational removed notifications (default false)."),
		field("reminder-interval", "string", "Minimum interval for unresolved-incident reminders; 0 disables (default 12h)."),
		field("notification-coalesce-window", "string", "Short window for batching simultaneous transitions (default 5s)."),
		field("removed-state-retention", "string", "Retention for removed-account metadata (default 7d)."),
		field("failure-notification-priority", "integer", "Pushover priority for failures, -2 through 1 (default 1)."),
		field("recovery-notification-priority", "integer", "Pushover priority for recoveries, -2 through 1 (default 0)."),
		field("pushover-app-token-env", "string", "Environment variable name containing the Pushover application token."),
		field("pushover-user-key-env", "string", "Environment variable name containing the Pushover user/group key."),
		field("pushover-app-token-file", "string", "Optional Docker/Kubernetes secret file containing the application token."),
		field("pushover-user-key-file", "string", "Optional Docker/Kubernetes secret file containing the user/group key."),
		field("pushover-device", "string", "Optional Pushover device target; blank sends to all active devices."),
		field("management-url", "string", "Optional private CPA Management URL appended to incident alerts."),
		field("state-file", "string", "Optional state file override; defaults to <auth dir>/.plugin-state/account-health-pushover/state.ahp. Avoid *.json paths beneath the auth directory, which CPA lists as credentials."),
		field("pushover-http-timeout", "string", "Timeout for each Pushover HTTP request (default 10s)."),
		field("max-concurrent-checks", "integer", "Maximum concurrent host runtime reads (default 4)."),
		field("display-timezone", "string", "IANA time zone for timestamps on the status page and in Pushover messages, e.g. America/Los_Angeles, or \"local\" for the host zone (default UTC). Presentation only."),
	}
}

func jsonResponse(status int, value any) protocol.ManagementResponse {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		body = []byte(`{"error":"response encoding failed"}`)
		status = http.StatusInternalServerError
	}
	return protocol.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"content-type":  []string{"application/json; charset=utf-8"},
			"cache-control": []string{"no-store"},
		},
		Body: body,
	}
}

func htmlResponse(status int, body []byte) protocol.ManagementResponse {
	return protocol.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"content-type":            []string{"text/html; charset=utf-8"},
			"cache-control":           []string{"no-store"},
			"content-security-policy": []string{"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
			"x-content-type-options":  []string{"nosniff"},
			"referrer-policy":         []string{"no-referrer"},
		},
		Body: body,
	}
}
