package plugin

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/monitor"
)

var statusTemplate = template.Must(template.New("status").Funcs(template.FuncMap{
	"formatTime": func(value time.Time) string {
		if value.IsZero() {
			return "—"
		}
		return value.Local().Format("2006-01-02 15:04:05 MST")
	},
	"stateClass": func(value health.State) string {
		switch value {
		case health.Healthy:
			return "healthy"
		case health.QuotaLimited:
			return "quota"
		case health.ReauthRequired, health.CredentialDown:
			return "down"
		case health.Disabled, health.Removed:
			return "muted"
		default:
			return "suspect"
		}
	},
	"provider": func(value string) string {
		if value == "" {
			return "OAuth"
		}
		return strings.ToUpper(value[:1]) + value[1:]
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Account Health Pushover</title>
<style>
:root{color-scheme:light dark;font-family:ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;--bg:#f7f8fa;--panel:#fff;--text:#18212f;--muted:#627083;--line:#dfe4ea;--healthy:#147d4f;--quota:#9a6500;--down:#b42318;--suspect:#7a4fb3}@media(prefers-color-scheme:dark){:root{--bg:#10151c;--panel:#171e27;--text:#ecf2f8;--muted:#aab6c5;--line:#303b49;--healthy:#57d49a;--quota:#f5c45b;--down:#ff8378;--suspect:#cba5ff}}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text)}main{max-width:1400px;margin:0 auto;padding:28px}h1{margin:0 0 6px;font-size:28px}.lead{color:var(--muted);margin:0 0 24px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(230px,1fr));gap:12px;margin-bottom:20px}.card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px}.card strong{display:block;font-size:13px;color:var(--muted);margin-bottom:5px}.actions{display:flex;gap:10px;margin:18px 0;flex-wrap:wrap}button{border:0;border-radius:8px;background:#2267d8;color:white;padding:10px 14px;font-weight:650;cursor:pointer}button.secondary{background:#526276}button:disabled{opacity:.55;cursor:wait}#action-result{min-height:24px;color:var(--muted)}.table-wrap{overflow:auto;background:var(--panel);border:1px solid var(--line);border-radius:10px}table{width:100%;border-collapse:collapse;font-size:13px}th,td{text-align:left;padding:10px 11px;border-bottom:1px solid var(--line);white-space:nowrap}th{color:var(--muted);font-weight:650}.state{font-weight:750}.state.healthy{color:var(--healthy)}.state.quota{color:var(--quota)}.state.down{color:var(--down)}.state.suspect{color:var(--suspect)}.state.muted{color:var(--muted)}code{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:12px}footer{margin-top:16px;color:var(--muted);font-size:12px}
</style>
</head>
<body><main>
<h1>Account Health Pushover</h1>
<p class="lead">Credential health is classified conservatively. Quota-limited accounts are credential-healthy and do not produce failure alerts.</p>
<section class="grid">
<div class="card"><strong>Pushover configuration</strong>{{.Notifier.Configuration.State}}</div>
<div class="card"><strong>Last successful Pushover send</strong>{{formatTime .Notifier.LastSuccessfulSend}}</div>
<div class="card"><strong>Last Pushover error</strong>{{if .Notifier.LastError}}{{.Notifier.LastError}}{{else}}—{{end}}</div>
<div class="card"><strong>Next health scan</strong>{{formatTime .NextScan}}</div>
<div class="card"><strong>State file health</strong>{{.StateFileHealth}}</div>
<div class="card"><strong>Monitoring snapshot</strong>{{if .MonitoringStale}}stale: {{.LastMonitoringError}}{{else}}current{{end}}</div>
</section>
<div class="actions"><button data-action="check">Check now</button><button class="secondary" data-action="test">Test notification</button></div>
<div id="action-result" role="status" aria-live="polite"></div>
<div class="table-wrap"><table>
<thead><tr><th>Provider</th><th>Account</th><th>Auth index</th><th>Health</th><th>CPA status</th><th>Unavailable</th><th>Quota limited</th><th>Reason</th><th>First detected</th><th>Last transition</th><th>Last healthy observation</th><th>Last alert</th><th>Next reminder</th></tr></thead>
<tbody>{{range .Accounts}}<tr><td>{{provider .Provider}}</td><td>{{.Label}}</td><td><code>{{.AuthIndex}}</code></td><td class="state {{stateClass .Health}}">{{.Health}}</td><td>{{if .CPAStatus}}{{.CPAStatus}}{{else}}—{{end}}</td><td>{{.CPAUnavailable}}</td><td>{{.QuotaLimited}}</td><td>{{if .ReasonCode}}{{.ReasonCode}}{{else}}—{{end}}</td><td>{{formatTime .FirstDetectedAt}}</td><td>{{formatTime .LastTransitionAt}}</td><td>{{formatTime .LastSuccessfulHealthObservation}}</td><td>{{formatTime .LastAlertAt}}</td><td>{{formatTime .NextReminderAt}}</td></tr>{{else}}<tr><td colspan="13">No monitored Claude/Codex OAuth accounts have been observed yet.</td></tr>{{end}}</tbody>
</table></div>
<footer>Management actions require the normal CPA management key. The key is used only for the request and is not stored by this page.</footer>
<script>
const result=document.getElementById('action-result');
for(const button of document.querySelectorAll('button[data-action]'))button.addEventListener('click',async()=>{
 const key=window.prompt('CPA management key'); if(!key)return;
 button.disabled=true; result.textContent='Working…';
 try{const response=await fetch('/v0/management/plugins/account-health-pushover/'+button.dataset.action,{method:'POST',headers:{'X-Management-Key':key}});const data=await response.json();if(!response.ok)throw new Error(data.error||('HTTP '+response.status));result.textContent=button.dataset.action==='test'?'Test notification accepted.':'Health check completed. Reloading…';if(button.dataset.action==='check')setTimeout(()=>location.reload(),500)}catch(error){result.textContent='Action failed: '+error.message}finally{button.disabled=false}
});
</script>
</main></body></html>`))

func redactResourceStatus(status monitor.Status) monitor.Status {
	counts := make(map[string]int)
	for i := range status.Accounts {
		provider := strings.ToLower(strings.TrimSpace(status.Accounts[i].Provider))
		counts[provider]++
		status.Accounts[i].Label = fmt.Sprintf("%s OAuth account %d", providerName(provider), counts[provider])
		status.Accounts[i].AuthIndex = "hidden"
	}
	status.StateFile = ""
	return status
}

func providerName(value string) string {
	if value == "" {
		return "OAuth"
	}
	runes := []rune(value)
	runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
	return string(runes)
}

func renderStatusPage(status monitor.Status) []byte {
	var output bytes.Buffer
	if err := statusTemplate.Execute(&output, status); err != nil {
		return []byte("<!doctype html><title>Account Health Pushover</title><p>Status rendering failed.</p>")
	}
	return output.Bytes()
}
