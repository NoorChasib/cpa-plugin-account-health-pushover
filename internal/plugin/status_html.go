package plugin

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/monitor"
)

// This file renders both browser views from one template:
//
//   - the UNAUTHENTICATED resource page (GET /v0/resource/plugins/<id>/status)
//     receives a redacted snapshot (see redactResourceStatus) and carries the
//     same-origin bootstrap script that swaps in the authenticated view when
//     the browser already holds a management session;
//   - the AUTHENTICATED management page (GET /v0/management/plugins/<id>/status/html)
//     receives the full sanitized snapshot plus the Check now / Test
//     notification actions.
//
// Every dynamic value passes through html/template's contextual escaping.
// Timestamps are rendered in the configured display timezone as
// "Tue Sep 1 2026 - 6:25:36 PM PDT"; the exact RFC3339 UTC instant is kept
// on each <time> element and shown on hover.

type statusPageData struct {
	Authenticated       bool
	Timezone            string
	GeneratedAt         pageTime
	PluginEnabled       bool
	MonitoringStale     bool
	LastMonitoringError string
	Warnings            []string
	Pushover            pagePushover
	NextScan            pageTime
	LastScan            pageTime
	LastSuccessfulScan  pageTime
	StateFileHealth     string
	StateFileClass      string
	StateFile           string
	AccountCount        int
	HealthyCount        int
	QuotaCount          int
	AttentionCount      int
	QuotaAlerts         bool
	QuotaWarningPercent string
	LastQuotaPoll       pageTime
	NextQuotaPoll       pageTime
	LastQuotaPollError  string
	QuotaNearLimitCount int
	QuotaExhaustedCount int
	Providers           []pageProvider
}

type pagePushover struct {
	State      string
	StateClass string
	Error      string
	LastSend   pageTime
	LastError  string
	Queue      int
	Dropped    uint64
}

type pageProvider struct {
	Name     string
	Accounts []pageAccount
}

type pageAccount struct {
	Label          string
	AuthIndex      string
	Health         string
	HealthClass    string
	CPAStatus      string
	Unavailable    bool
	QuotaLimited   bool
	ReasonCode     string
	FirstDetected  pageTime
	LastTransition pageTime
	LastHealthy    pageTime
	LastAlert      pageTime
	NextReminder   pageTime
	RemovedAt      pageTime
	Quota          pageQuota
}

// pageQuota is the per-account weekly usage cell. Set is false until the
// first successful poll; Class colors the usage pill by threshold.
type pageQuota struct {
	Set        bool
	Percent    string
	Class      string
	ResetAt    pageTime
	ObservedAt pageTime
	LastError  string
	Warned     bool
	Exhausted  bool
}

// pageTime carries one instant in the forms the template needs. A zero value
// (Set=false) renders as a placeholder dash.
type pageTime struct {
	Set      bool
	Date     string // "Tue Sep 1 2026"
	Clock    string // "6:25:36 PM PDT"
	Exact    string // RFC3339Nano UTC for datetime/title attributes
	Relative string // "in 2m" / "3h ago", relative to GeneratedAt
}

var statusTemplate = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Account Health Pushover</title>
<style>
:root{
  --bg:#f6f7f9;--surface:#fff;--border:#e4e7ec;--text:#101828;--muted:#667085;--faint:#98a2b3;
  --ok-bg:#ecfdf3;--ok-fg:#067647;--ok-bd:#abefc6;
  --warn-bg:#fffaeb;--warn-fg:#b54708;--warn-bd:#fedf89;
  --err-bg:#fef3f2;--err-fg:#b42318;--err-bd:#fecdca;
  --info-bg:#eff8ff;--info-fg:#175cd3;--info-bd:#b2ddff;
  --neutral-bg:#f2f4f7;--neutral-fg:#344054;--neutral-bd:#e4e7ec;
  --accent:#2563eb;--accent-hover:#1d4ed8;--accent-fg:#fff;
  --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#0c111d;--surface:#131a2b;--border:#1f2a3d;--text:#e6eaf2;--muted:#94a3b8;--faint:#64748b;
    --ok-bg:#052e21;--ok-fg:#5ee6a8;--ok-bd:#0f5c47;
    --warn-bg:#3a2a06;--warn-fg:#fcd34d;--warn-bd:#6b4d0b;
    --err-bg:#3b0f0f;--err-fg:#fca5a5;--err-bd:#7f1d1d;
    --info-bg:#0f2148;--info-fg:#93c5fd;--info-bd:#1e3a8a;
    --neutral-bg:#1a2436;--neutral-fg:#cbd5e1;--neutral-bd:#2c3a52;
    --accent:#3b82f6;--accent-hover:#60a5fa;
  }
}
*{box-sizing:border-box}
html,body{margin:0;background:var(--bg);color:var(--text)}
body{font:14px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;-webkit-font-smoothing:antialiased}
.page{max-width:1280px;margin:0 auto;padding:28px 24px 48px}
.header{display:flex;flex-wrap:wrap;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:22px}
h1{font-size:22px;font-weight:650;letter-spacing:-.01em;margin:0 0 4px}
.subtitle{margin:0;color:var(--muted)}
.pills{display:flex;flex-wrap:wrap;gap:6px;margin-top:10px}
.pill{display:inline-flex;align-items:center;gap:6px;border:1px solid var(--neutral-bd);background:var(--neutral-bg);color:var(--neutral-fg);border-radius:999px;padding:2px 10px;font-size:12px;font-weight:500;line-height:18px;white-space:nowrap}
.pill::before{content:"";width:6px;height:6px;border-radius:50%;background:currentColor;opacity:.85}
.pill.ok{background:var(--ok-bg);color:var(--ok-fg);border-color:var(--ok-bd)}
.pill.warn{background:var(--warn-bg);color:var(--warn-fg);border-color:var(--warn-bd)}
.pill.err{background:var(--err-bg);color:var(--err-fg);border-color:var(--err-bd)}
.pill.info{background:var(--info-bg);color:var(--info-fg);border-color:var(--info-bd)}
.actions{display:flex;flex-direction:column;align-items:flex-end;gap:8px}
.buttons{display:flex;gap:8px;flex-wrap:wrap;justify-content:flex-end}
button{font:inherit;font-weight:500;padding:7px 14px;border-radius:8px;border:1px solid var(--accent);background:var(--accent);color:var(--accent-fg);cursor:pointer;transition:background .12s,border-color .12s}
button:hover{background:var(--accent-hover);border-color:var(--accent-hover)}
button.secondary{background:var(--surface);color:var(--text);border-color:var(--border)}
button.secondary:hover{background:var(--neutral-bg)}
button:disabled{opacity:.55;cursor:default}
.result{font-size:12px;color:var(--muted);min-height:18px;text-align:right}
.result.ok{color:var(--ok-fg)}
.result.err{color:var(--err-fg)}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:12px;margin-bottom:12px}
.stat{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:12px 14px;min-width:0}
.stat .label{font-size:11px;font-weight:600;letter-spacing:.06em;text-transform:uppercase;color:var(--muted);margin-bottom:4px}
.stat .value{font-size:14px;font-weight:550;overflow-wrap:anywhere}
.stat .hint{font-size:12px;color:var(--muted);margin-top:2px}
.alert{border-radius:10px;padding:10px 14px;margin:12px 0;border:1px solid var(--err-bd);background:var(--err-bg);color:var(--err-fg)}
.alert.warn{border-color:var(--warn-bd);background:var(--warn-bg);color:var(--warn-fg)}
.alert.info{border-color:var(--info-bd);background:var(--info-bg);color:var(--info-fg)}
.alert ul{margin:4px 0 0;padding-left:18px}
.section{margin-top:26px}
.section-head{display:flex;align-items:baseline;gap:10px;margin-bottom:10px}
h2{font-size:16px;font-weight:650;margin:0}
.count{color:var(--muted);font-size:13px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:10px;overflow:hidden}
.table-wrap{overflow-x:auto}
table{border-collapse:collapse;width:100%;font-size:13px}
th,td{padding:10px 12px;text-align:left;vertical-align:top;border-bottom:1px solid var(--border)}
th{font-size:11px;font-weight:600;letter-spacing:.05em;text-transform:uppercase;color:var(--muted);background:var(--neutral-bg);white-space:nowrap}
tbody tr:last-child td{border-bottom:0}
tbody tr:hover td{background:color-mix(in srgb,var(--neutral-bg) 55%,transparent)}
td.account{min-width:180px;max-width:320px}
td.account .name{font-weight:550;overflow-wrap:anywhere}
code{font-family:var(--mono);font-size:12px;background:var(--neutral-bg);border:1px solid var(--neutral-bd);padding:1px 6px;border-radius:5px;overflow-wrap:anywhere}
.sub{display:block;font-size:12px;color:var(--muted);margin-top:2px}
.flags{display:flex;flex-wrap:wrap;gap:4px}
time{font-variant-numeric:tabular-nums}
time .d,time .t{white-space:nowrap}
.rel{display:block;font-size:12px;color:var(--muted)}
.dash{color:var(--faint)}
.usage{display:flex;flex-direction:column;gap:4px;min-width:120px}
.usage .bar{height:6px;border-radius:999px;background:var(--neutral-bg);border:1px solid var(--neutral-bd);overflow:hidden}
.usage .bar span{display:block;height:100%;background:var(--ok-fg)}
.usage .bar.warn span{background:var(--warn-fg)}
.usage .bar.err span{background:var(--err-fg)}
.usage .pct{font-weight:550;font-variant-numeric:tabular-nums}
.error-text{color:var(--err-fg)}
.empty{padding:18px 14px;color:var(--muted);font-style:italic}
.footnote{margin-top:28px;padding-top:14px;border-top:1px solid var(--border);font-size:12px;color:var(--muted);line-height:1.55}
.footnote code{font-size:11px}
</style>
</head>
<body>
<div class="page">
<div class="header">
  <div>
    <h1>Account Health Pushover</h1>
    <p class="subtitle">{{if .Authenticated}}Claude, Codex, and Grok OAuth credential health with Pushover delivery state.{{else}}Redacted read-only view. Quota-limited accounts are credential-healthy and never trigger failure alerts.{{end}}</p>
    <div class="pills">
      {{if .PluginEnabled}}<span class="pill ok">monitoring enabled</span>{{else}}<span class="pill warn">monitoring disabled</span>{{end}}
      {{if .MonitoringStale}}<span class="pill warn">snapshot stale</span>{{else}}<span class="pill ok">snapshot current</span>{{end}}
      <span class="pill {{.Pushover.StateClass}}">pushover {{.Pushover.State}}</span>
      {{if .Authenticated}}<span class="pill info">authenticated view</span>{{else}}<span class="pill">redacted view</span>{{end}}
    </div>
  </div>
  {{if .Authenticated}}
  <div class="actions">
    <div class="buttons">
      <button type="button" class="secondary" data-action="test">Test notification</button>
      <button type="button" data-action="check">Check now</button>
    </div>
    <span id="action-result" class="result"></span>
  </div>
  {{end}}
</div>

<div class="stats">
  <div class="stat">
    <div class="label">Accounts</div>
    <div class="value">{{.AccountCount}} monitored</div>
    <div class="hint">{{.HealthyCount}} healthy · {{.QuotaCount}} quota limited · {{.AttentionCount}} need attention</div>
  </div>
  <div class="stat">
    <div class="label">Weekly quota alerts</div>
    {{if .QuotaAlerts}}<div class="value">{{.QuotaExhaustedCount}} exhausted · {{.QuotaNearLimitCount}} near limit</div>
    <div class="hint">Warn at {{.QuotaWarningPercent}} used · next poll {{if .NextQuotaPoll.Set}}{{.NextQuotaPoll.Relative}}{{else}}—{{end}}</div>
    {{else}}<div class="value"><span class="pill">disabled</span></div>
    <div class="hint">Set quota-alerts: true to poll weekly usage</div>{{end}}
  </div>
  <div class="stat">
    <div class="label">Next health scan</div>
    <div class="value">{{template "when" .NextScan}}</div>
  </div>
  <div class="stat">
    <div class="label">Last successful scan</div>
    <div class="value">{{template "when" .LastSuccessfulScan}}</div>
  </div>
  <div class="stat">
    <div class="label">Last Pushover send</div>
    <div class="value">{{template "when" .Pushover.LastSend}}</div>
    {{if .Pushover.Queue}}<div class="hint">{{.Pushover.Queue}} queued</div>{{end}}
  </div>
  <div class="stat">
    <div class="label">State file</div>
    <div class="value"><span class="pill {{.StateFileClass}}">{{.StateFileHealth}}</span></div>
    {{if .StateFile}}<div class="hint"><code>{{.StateFile}}</code></div>{{end}}
  </div>
  <div class="stat">
    <div class="label">Generated</div>
    <div class="value">{{template "when" .GeneratedAt}}</div>
    <div class="hint">Times shown in {{.Timezone}}</div>
  </div>
</div>

{{if .LastMonitoringError}}<div class="alert warn"><strong>Monitoring:</strong> {{.LastMonitoringError}}</div>{{end}}
{{if .LastQuotaPollError}}<div class="alert warn"><strong>Weekly quota poll:</strong> {{.LastQuotaPollError}}</div>{{end}}
{{if .Pushover.Error}}<div class="alert"><strong>Pushover configuration:</strong> {{.Pushover.Error}}</div>{{end}}
{{if .Pushover.LastError}}<div class="alert"><strong>Last Pushover error:</strong> {{.Pushover.LastError}}</div>{{end}}
{{if .Warnings}}<div class="alert warn"><strong>Warnings</strong><ul>{{range .Warnings}}<li>{{.}}</li>{{end}}</ul></div>{{end}}
{{if not .Authenticated}}<p class="footnote" id="session-note"></p>{{end}}

{{range .Providers}}
<div class="section">
  <div class="section-head">
    <h2>{{.Name}}</h2>
    <span class="count">{{len .Accounts}} account{{if ne (len .Accounts) 1}}s{{end}}</span>
  </div>
  <div class="card">
  <div class="table-wrap">
  <table>
    <thead>
      <tr>
        <th>Account</th>
        <th>Health</th>
        <th>CPA status</th>
        {{if $.QuotaAlerts}}<th>Weekly usage</th>{{end}}
        <th>First detected</th>
        <th>Last transition</th>
        <th>Last healthy</th>
        <th>Last alert</th>
        <th>Next reminder</th>
      </tr>
    </thead>
    <tbody>
    {{range .Accounts}}
      <tr>
        <td class="account"><span class="name">{{.Label}}</span>{{if $.Authenticated}}<span class="sub"><code>{{.AuthIndex}}</code></span>{{end}}</td>
        <td><span class="pill {{.HealthClass}}">{{.Health}}</span>{{if .ReasonCode}}<span class="sub">{{.ReasonCode}}</span>{{end}}</td>
        <td>{{if .CPAStatus}}{{.CPAStatus}}{{else}}<span class="dash">—</span>{{end}}{{if or .Unavailable .QuotaLimited}}<span class="sub flags">{{if .Unavailable}}<span class="pill warn">unavailable</span>{{end}}{{if .QuotaLimited}}<span class="pill info">quota limited</span>{{end}}</span>{{end}}</td>
        {{if $.QuotaAlerts}}<td>{{template "usage" .Quota}}</td>{{end}}
        <td>{{template "when" .FirstDetected}}</td>
        <td>{{template "when" .LastTransition}}</td>
        <td>{{template "when" .LastHealthy}}</td>
        <td>{{template "when" .LastAlert}}</td>
        <td>{{if .RemovedAt.Set}}<span class="sub">removed</span>{{template "when" .RemovedAt}}{{else}}{{template "when" .NextReminder}}{{end}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  </div>
</div>
{{else}}
<div class="section"><div class="card"><p class="empty">No monitored Claude, Codex, or Grok OAuth accounts have been observed yet.</p></div></div>
{{end}}

<p class="footnote">{{if .Authenticated}}Check now runs an immediate reconciliation and Test notification sends a safe Pushover test. Both post to same-origin management routes with the plugin action header; CPA must receive the management authentication header through the browser session or reverse proxy.{{else}}Account labels, auth indexes, reason codes, and error details are redacted on this unauthenticated page. When opened from the same origin as a signed-in management console, the full authenticated view loads in place.{{end}} Hover any timestamp for the exact UTC instant.</p>
</div>
<script>` + browserAuthScript + `{{if .Authenticated}}` + managementActionsScript + `{{else}}` + resourceBootstrapScript + `{{end}}</script>
</body>
</html>
{{define "usage"}}{{if .Set}}<div class="usage"><span class="pct">{{.Percent}} used</span><div class="bar {{.Class}}"><span style="width:{{.Percent}}"></span></div>{{if .ResetAt.Set}}<span class="sub">resets {{.ResetAt.Relative}}</span>{{end}}{{if or .Warned .Exhausted}}<span class="sub flags">{{if .Exhausted}}<span class="pill err">exhausted alert sent</span>{{else}}<span class="pill warn">warning sent</span>{{end}}</span>{{end}}{{if .LastError}}<span class="sub error-text">{{.LastError}}</span>{{end}}</div>{{else if .LastError}}<span class="dash">—</span><span class="sub error-text">{{.LastError}}</span>{{else}}<span class="dash">—</span>{{end}}{{end}}
{{define "when"}}{{if .Set}}<time datetime="{{.Exact}}" title="{{.Exact}}"><span class="d">{{.Date}}</span> - <span class="t">{{.Clock}}</span></time>{{if .Relative}}<span class="rel">{{.Relative}}</span>{{end}}{{else}}<span class="dash">—</span>{{end}}{{end}}
`))

// renderStatusPage renders the shared browser view. authenticated selects the
// management variant (actions, exact identifiers); the caller is responsible
// for passing a redacted snapshot when authenticated is false.
func renderStatusPage(status monitor.Status, authenticated bool, now time.Time) []byte {
	var output bytes.Buffer
	if err := statusTemplate.Execute(&output, buildStatusPageData(status, authenticated, now)); err != nil {
		return []byte("<!doctype html><title>Account Health Pushover</title><p>Status rendering failed.</p>")
	}
	return output.Bytes()
}

func buildStatusPageData(status monitor.Status, authenticated bool, now time.Time) statusPageData {
	loc := config.LoadDisplayLocation(status.DisplayTimezone)
	when := func(t time.Time) pageTime { return newPageTime(t, now, loc) }

	data := statusPageData{
		Authenticated:       authenticated,
		Timezone:            displayZoneName(status.DisplayTimezone, loc),
		GeneratedAt:         when(now),
		PluginEnabled:       status.PluginEnabled,
		MonitoringStale:     status.MonitoringStale,
		LastMonitoringError: status.LastMonitoringError,
		Warnings:            status.Warnings,
		NextScan:            when(status.NextScan),
		LastScan:            when(status.LastScan),
		LastSuccessfulScan:  when(status.LastSuccessfulScan),
		QuotaAlerts:         status.QuotaAlerts,
		QuotaWarningPercent: formatPagePercent(status.QuotaWarningPercent),
		LastQuotaPoll:       when(status.LastQuotaPoll),
		NextQuotaPoll:       when(status.NextQuotaPoll),
		LastQuotaPollError:  status.LastQuotaPollError,
		StateFileHealth:     status.StateFileHealth,
		StateFileClass:      stateFileClass(status.StateFileHealth),
		StateFile:           status.StateFile,
		Pushover: pagePushover{
			State:      status.Notifier.Configuration.State,
			StateClass: pushoverClass(status.Notifier.Configuration.State),
			Error:      status.Notifier.Configuration.Error,
			LastSend:   when(status.Notifier.LastSuccessfulSend),
			LastError:  status.Notifier.LastError,
			Queue:      status.Notifier.NotificationQueue,
			Dropped:    status.Notifier.NotificationDropped,
		},
	}
	data.GeneratedAt.Relative = ""
	if data.Pushover.State == "" {
		data.Pushover.State = "unknown"
	}
	if data.StateFileHealth == "" {
		data.StateFileHealth = "unknown"
	}

	groups := make(map[string]int)
	for _, account := range status.Accounts {
		name := providerName(account.Provider)
		index, ok := groups[name]
		if !ok {
			index = len(data.Providers)
			groups[name] = index
			data.Providers = append(data.Providers, pageProvider{Name: name})
		}
		data.Providers[index].Accounts = append(data.Providers[index].Accounts, pageAccount{
			Label:          account.Label,
			AuthIndex:      account.AuthIndex,
			Health:         string(account.Health),
			HealthClass:    healthClass(account.Health),
			CPAStatus:      account.CPAStatus,
			Unavailable:    account.CPAUnavailable,
			QuotaLimited:   account.QuotaLimited,
			ReasonCode:     account.ReasonCode,
			FirstDetected:  when(account.FirstDetectedAt),
			LastTransition: when(account.LastTransitionAt),
			LastHealthy:    when(account.LastSuccessfulHealthObservation),
			LastAlert:      when(account.LastAlertAt),
			NextReminder:   when(account.NextReminderAt),
			RemovedAt:      when(account.RemovedAt),
			Quota:          buildPageQuota(account.Quota, status.QuotaWarningPercent, when),
		})
		if account.Quota != nil && account.Quota.Percent != nil {
			switch {
			case *account.Quota.Percent >= 100:
				data.QuotaExhaustedCount++
			case status.QuotaWarningPercent > 0 && *account.Quota.Percent >= status.QuotaWarningPercent:
				data.QuotaNearLimitCount++
			}
		}
		data.AccountCount++
		switch {
		case account.Health == health.Healthy:
			data.HealthyCount++
		case account.Health == health.QuotaLimited:
			data.QuotaCount++
		case health.IsFailure(account.Health) || account.Health == health.Suspect:
			data.AttentionCount++
		}
	}
	return data
}

func buildPageQuota(q *monitor.QuotaStatus, warnAt float64, when func(time.Time) pageTime) pageQuota {
	if q == nil {
		return pageQuota{}
	}
	out := pageQuota{
		LastError:  q.LastError,
		ResetAt:    when(q.ResetAt),
		ObservedAt: when(q.ObservedAt),
		Warned:     !q.WarningSentAt.IsZero(),
		Exhausted:  !q.ExhaustedSentAt.IsZero(),
	}
	if q.Percent == nil {
		return out
	}
	out.Set = true
	out.Percent = formatPagePercent(*q.Percent)
	switch {
	case *q.Percent >= 100:
		out.Class = "err"
	case warnAt > 0 && *q.Percent >= warnAt:
		out.Class = "warn"
	}
	return out
}

func formatPagePercent(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d%%", int64(value))
	}
	return fmt.Sprintf("%.1f%%", value)
}

func newPageTime(t, now time.Time, loc *time.Location) pageTime {
	if t.IsZero() {
		return pageTime{}
	}
	local := t.In(loc)
	return pageTime{
		Set:      true,
		Date:     local.Format(config.DisplayDateLayout),
		Clock:    local.Format(config.DisplayClockLayout),
		Exact:    t.UTC().Format(time.RFC3339Nano),
		Relative: relativeTime(t, now),
	}
}

// relativeTime renders a coarse human hint ("in 2m", "3h 12m ago") relative
// to the page generation time. It is intentionally approximate.
func relativeTime(t, now time.Time) string {
	if now.IsZero() {
		return ""
	}
	d := t.Sub(now)
	future := d >= 0
	if !future {
		d = -d
	}
	if d < time.Second {
		return "now"
	}
	var s string
	switch {
	case d < time.Minute:
		s = fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		s = fmt.Sprintf("%dh %dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	default:
		s = fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int((d%(24*time.Hour))/time.Hour))
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

func displayZoneName(configured string, loc *time.Location) string {
	if strings.EqualFold(strings.TrimSpace(configured), "local") {
		return "host local time (" + loc.String() + ")"
	}
	if loc == nil || loc.String() == "" {
		return "UTC"
	}
	return loc.String()
}

func healthClass(value health.State) string {
	switch value {
	case health.Healthy:
		return "ok"
	case health.QuotaLimited:
		return "info"
	case health.ReauthRequired, health.CredentialDown:
		return "err"
	case health.Disabled, health.Removed:
		return ""
	default:
		return "warn"
	}
}

func pushoverClass(state string) string {
	switch state {
	case "configured":
		return "ok"
	case "missing":
		return "warn"
	case "", "unknown":
		return ""
	default:
		return "err"
	}
}

func stateFileClass(healthValue string) string {
	switch healthValue {
	case "healthy":
		return "ok"
	case "not_initialized", "waiting_for_auth_path", "":
		return ""
	default:
		return "warn"
	}
}

func redactResourceStatus(status monitor.Status) monitor.Status {
	counts := make(map[string]int)
	status.Accounts = append([]monitor.AccountStatus(nil), status.Accounts...)
	for i := range status.Accounts {
		provider := strings.ToLower(strings.TrimSpace(status.Accounts[i].Provider))
		counts[provider]++
		name := providerName(provider)
		if name == "OAuth" {
			status.Accounts[i].Label = fmt.Sprintf("OAuth account %d", counts[provider])
		} else {
			status.Accounts[i].Label = fmt.Sprintf("%s OAuth account %d", name, counts[provider])
		}
		status.Accounts[i].AuthIndex = "hidden"
		status.Accounts[i].ReasonCode = ""
		if quota := status.Accounts[i].Quota; quota != nil {
			redactedQuota := *quota
			redactedQuota.LastError = ""
			status.Accounts[i].Quota = &redactedQuota
		}
	}
	status.StateFile = ""
	status.LastQuotaPollError = ""
	status.LastMonitoringError = ""
	status.Warnings = nil
	status.Notifier.LastError = ""
	status.Notifier.Configuration.Error = ""
	return status
}

func providerName(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "xai", "grok":
		return "Grok"
	default:
		return "OAuth"
	}
}
