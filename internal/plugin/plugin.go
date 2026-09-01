package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/monitor"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/notifier"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

const ID = "account-health-pushover"

var Version = "0.1.0"

type Host = monitor.Host

type Plugin struct {
	host     Host
	endpoint string

	mu      sync.RWMutex
	monitor *monitor.Monitor
}

func New(host Host, endpoint string) *Plugin {
	return &Plugin{host: host, endpoint: endpoint}
}

func (p *Plugin) Handle(method string, raw []byte) (any, error) {
	switch method {
	case protocol.MethodPluginRegister, protocol.MethodPluginReconfigure:
		return p.configure(raw)
	case protocol.MethodPluginQuiesce:
		p.stop()
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
	p.stop()
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

	p.mu.Lock()
	oldMonitor := p.monitor
	p.monitor = newMonitor
	p.mu.Unlock()
	if oldMonitor != nil {
		oldMonitor.Stop()
	}
	newMonitor.Start()
	return registration(), nil
}

func (p *Plugin) stop() {
	p.mu.Lock()
	current := p.monitor
	p.monitor = nil
	p.mu.Unlock()
	if current != nil {
		current.Stop()
	}
}

func (p *Plugin) handleUsage(raw []byte) (map[string]any, error) {
	var usage protocol.UsageRecord
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil, fmt.Errorf("decode usage record: %w", err)
	}
	if !usage.Failed || strings.TrimSpace(usage.AuthIndex) == "" {
		return map[string]any{}, nil
	}
	switch usage.Failure.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		p.mu.RLock()
		current := p.monitor
		p.mu.RUnlock()
		if current != nil {
			current.Signal(usage.AuthIndex)
		}
	case http.StatusTooManyRequests:
		// Quota/rate limiting is intentionally not an account-down signal.
	}
	return map[string]any{}, nil
}

func (p *Plugin) handleManagement(raw []byte) (protocol.ManagementResponse, error) {
	var request protocol.ManagementRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid management request"}), nil
	}
	p.mu.RLock()
	current := p.monitor
	p.mu.RUnlock()
	if current == nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": "plugin is not configured"}), nil
	}
	path := strings.TrimSuffix(request.Path, "/")
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch {
	case method == http.MethodGet && strings.HasSuffix(path, "/status"):
		if strings.Contains(path, "/v0/management/plugins/") || strings.Contains(path, "/management/plugins/") {
			return jsonResponse(http.StatusOK, current.Snapshot()), nil
		}
		return htmlResponse(http.StatusOK, renderStatusPage(redactResourceStatus(current.Snapshot()))), nil
	case method == http.MethodPost && strings.HasSuffix(path, "/check"):
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		status := current.CheckNow(ctx)
		cancel()
		return jsonResponse(http.StatusOK, status), nil
	case method == http.MethodPost && strings.HasSuffix(path, "/test"):
		ctx, cancel := context.WithTimeout(context.Background(), current.NotificationTimeout())
		result := current.TestNotification(ctx)
		cancel()
		if !result.Accepted {
			statusCode := http.StatusBadGateway
			if strings.Contains(strings.ToLower(result.Error), "not configured") || strings.Contains(strings.ToLower(result.Error), "invalid format") {
				statusCode = http.StatusServiceUnavailable
			}
			return jsonResponse(statusCode, map[string]any{"accepted": false, "error": result.Error}), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{"accepted": true, "sent_at": result.At}), nil
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "route not found"}), nil
	}
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
			{Method: http.MethodPost, Path: base + "/check", Description: "Runs an immediate health reconciliation."},
			{Method: http.MethodPost, Path: base + "/test", Description: "Sends a safe Pushover test notification."},
		},
		Resources: []protocol.ResourceRoute{
			{Path: "/status", Menu: "Account Health Pushover", Description: "Shows Claude/Codex OAuth health and Pushover delivery state."},
		},
	}
}

func configFields() []protocol.ConfigField {
	field := func(name, typ, description string) protocol.ConfigField {
		return protocol.ConfigField{Name: name, Type: typ, Description: description}
	}
	return []protocol.ConfigField{
		field("providers", "array", "OAuth providers to monitor. Supported values: claude, codex."),
		field("scan-interval", "string", "Full health reconciliation interval (default 1m)."),
		field("startup-grace", "string", "Delay before the first baseline scan; current usage failures can trigger an earlier recheck (default 30s)."),
		field("transient-confirm-after", "string", "How long ambiguous failures remain suspect before credential_down (default 10m)."),
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
		field("state-file", "string", "Optional state file override; defaults below the detected CPA auth directory."),
		field("pushover-http-timeout", "string", "Timeout for each Pushover HTTP request (default 10s)."),
		field("max-concurrent-checks", "integer", "Maximum concurrent host runtime reads (default 4)."),
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
			"content-security-policy": []string{"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'"},
			"x-content-type-options":  []string{"nosniff"},
			"referrer-policy":         []string{"no-referrer"},
		},
		Body: body,
	}
}
