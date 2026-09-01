package monitor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/notifier"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/state"
)

const notificationRetryAfter = 5 * time.Minute

// stopWaitBound caps how long Stop waits for the reconcile loop to exit after
// cancellation, so host lifecycle calls (quiesce, reconfigure, shutdown)
// return within a hard bounded deadline even if a host callback ignores
// context cancellation.
const stopWaitBound = 5 * time.Second

type Host interface {
	ListAuth(context.Context) ([]protocol.HostAuthFileEntry, error)
	GetRuntime(context.Context, string) (protocol.HostAuthFileEntry, error)
	Log(context.Context, string, string, map[string]any)
}

type Status struct {
	PluginEnabled       bool            `json:"plugin_enabled"`
	MonitoringStale     bool            `json:"monitoring_stale"`
	LastMonitoringError string          `json:"last_monitoring_error,omitempty"`
	LastScan            time.Time       `json:"last_scan,omitempty"`
	LastSuccessfulScan  time.Time       `json:"last_successful_scan,omitempty"`
	NextScan            time.Time       `json:"next_scan,omitempty"`
	StateFile           string          `json:"state_file,omitempty"`
	StateFileHealth     string          `json:"state_file_health"`
	Notifier            notifier.Status `json:"pushover"`
	Accounts            []AccountStatus `json:"accounts"`
}

type AccountStatus struct {
	Provider                        string       `json:"provider"`
	Label                           string       `json:"label"`
	AuthIndex                       string       `json:"auth_index"`
	Health                          health.State `json:"health"`
	CPAStatus                       string       `json:"cpa_status,omitempty"`
	CPAUnavailable                  bool         `json:"cpa_unavailable"`
	QuotaLimited                    bool         `json:"quota_limited"`
	ReasonCode                      string       `json:"reason_code,omitempty"`
	FirstDetectedAt                 time.Time    `json:"first_detected_at,omitempty"`
	LastTransitionAt                time.Time    `json:"last_transition_at,omitempty"`
	LastSuccessfulHealthObservation time.Time    `json:"last_successful_health_observation,omitempty"`
	LastAlertAt                     time.Time    `json:"last_alert_at,omitempty"`
	NextReminderAt                  time.Time    `json:"next_reminder_at,omitempty"`
	RemovedAt                       time.Time    `json:"removed_at,omitempty"`
}

type Monitor struct {
	cfg         config.Config
	host        Host
	classifiers map[string]health.ProviderClassifier
	client      *notifier.Client
	dispatcher  *notifier.Dispatcher
	now         func() time.Time

	reconcileMu sync.Mutex
	stateMu     sync.RWMutex
	data        state.Data
	store       state.Store
	stateLoaded bool
	status      Status

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	signals   chan string
	pendingMu sync.Mutex
	pending   map[string]struct{}
	aliases   map[string]string
}

func New(cfg config.Config, host Host, client *notifier.Client, dispatcher *notifier.Dispatcher) *Monitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Monitor{
		cfg:         cfg,
		host:        host,
		classifiers: health.Classifiers(),
		client:      client,
		dispatcher:  dispatcher,
		now:         time.Now,
		data:        state.NewData(),
		status: Status{
			PluginEnabled:   cfg.Enabled,
			StateFileHealth: "not_initialized",
		},
		ctx:     ctx,
		cancel:  cancel,
		signals: make(chan string, 64),
		pending: make(map[string]struct{}),
		aliases: make(map[string]string),
	}
}

func (m *Monitor) Start() {
	m.dispatcher.Start()
	if !m.cfg.Enabled {
		return
	}
	m.wg.Add(1)
	go m.loop()
}

// Stop cancels monitoring and notification delivery. Both waits are bounded:
// the reconcile loop is abandoned after stopWaitBound if it is stuck inside a
// host callback that ignored cancellation, and the dispatcher's Stop bounds
// its own wait. An abandoned goroutine holds no locks Stop's caller needs and
// exits on its own once its blocking call returns; state writes stay safe
// because persistence is an atomic rename.
func (m *Monitor) Stop() {
	m.cancel()
	loopDone := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(loopDone)
	}()
	timer := time.NewTimer(stopWaitBound)
	defer timer.Stop()
	select {
	case <-loopDone:
	case <-timer.C:
	}
	m.dispatcher.Stop()
}

func (m *Monitor) loop() {
	defer m.wg.Done()
	startup := time.NewTimer(m.cfg.StartupGrace)
	defer startup.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-startup.C:
			_ = m.Reconcile(m.ctx, "startup")
			goto periodic
		case authIndex := <-m.signals:
			if !m.processSignal(authIndex) {
				return
			}
		}
	}

periodic:
	ticker := time.NewTicker(m.cfg.ScanInterval)
	defer ticker.Stop()
	m.setNextScan(m.now().Add(m.cfg.ScanInterval))
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			_ = m.Reconcile(m.ctx, "periodic")
			m.setNextScan(m.now().Add(m.cfg.ScanInterval))
		case authIndex := <-m.signals:
			if !m.processSignal(authIndex) {
				return
			}
		}
	}
}

func (m *Monitor) processSignal(authIndex string) bool {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-m.ctx.Done():
		return false
	case <-timer.C:
	}
	_ = m.Reconcile(m.ctx, "usage_failure")
	m.pendingMu.Lock()
	delete(m.pending, authIndex)
	m.pendingMu.Unlock()
	return true
}

func (m *Monitor) Signal(authIndex string) bool {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || !m.cfg.Enabled {
		return false
	}
	m.pendingMu.Lock()
	if _, exists := m.pending[authIndex]; exists {
		m.pendingMu.Unlock()
		return true
	}
	m.pending[authIndex] = struct{}{}
	m.pendingMu.Unlock()
	select {
	case m.signals <- authIndex:
		return true
	default:
		m.pendingMu.Lock()
		delete(m.pending, authIndex)
		m.pendingMu.Unlock()
		return false
	}
}

func (m *Monitor) Reconcile(ctx context.Context, trigger string) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	now := m.now().UTC()
	roster, err := m.host.ListAuth(ctx)
	if err != nil {
		message := "host.auth.list failed; previous account state was retained"
		m.recordMonitoringFailure(now, message)
		m.host.Log(ctx, "warn", message, map[string]any{"trigger": safeField(trigger)})
		return err
	}
	candidates := health.Discover(roster, m.cfg)
	m.ensureStateLoaded(roster)

	type result struct {
		entry       protocol.HostAuthFileEntry
		snapshot    health.RuntimeSnapshot
		observation health.Observation
		err         error
	}
	results := make(chan result, len(candidates))
	semaphore := make(chan struct{}, m.cfg.MaxConcurrentChecks)
	var wg sync.WaitGroup
	for _, candidate := range candidates {
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				results <- result{entry: candidate, err: ctx.Err()}
				return
			}
			defer func() { <-semaphore }()
			runtimeEntry, runtimeErr := m.host.GetRuntime(ctx, candidate.AuthIndex)
			if runtimeErr != nil {
				results <- result{entry: candidate, err: runtimeErr}
				return
			}
			if runtimeEntry.Provider == "" {
				runtimeEntry.Provider = candidate.Provider
			}
			if runtimeEntry.AuthIndex == "" {
				runtimeEntry.AuthIndex = candidate.AuthIndex
			}
			snapshot := health.FromHostEntry(runtimeEntry, now)
			classifier := m.classifiers[snapshot.Provider]
			if classifier == nil {
				results <- result{entry: candidate, err: fmt.Errorf("no classifier for provider %s", snapshot.Provider)}
				return
			}
			results <- result{entry: candidate, snapshot: snapshot, observation: classifier.Classify(snapshot)}
		}()
	}
	wg.Wait()
	close(results)

	activeKeys := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		activeKeys[health.AccountKey(candidate.Provider, candidate.AuthIndex)] = struct{}{}
	}
	partialFailure := false
	for item := range results {
		if item.err != nil {
			partialFailure = true
			continue
		}
		m.applyObservation(item.snapshot, item.observation, activeKeys, now)
	}
	m.markRemoved(activeKeys, now)

	m.stateMu.Lock()
	state.PruneRemoved(&m.data, now, m.effectiveRemovedStateRetention())
	persistErr := m.persistLocked()
	m.status.LastScan = now
	m.status.Accounts = accountStatuses(m.data, m.cfg.ReminderInterval)
	if partialFailure {
		m.status.MonitoringStale = true
		m.status.LastMonitoringError = "one or more runtime auth reads failed; retained their prior state"
	} else {
		m.status.MonitoringStale = false
		m.status.LastMonitoringError = ""
		m.status.LastSuccessfulScan = now
	}
	m.stateMu.Unlock()
	if persistErr != nil {
		m.host.Log(ctx, "warn", "account-health state persistence failed", map[string]any{"trigger": safeField(trigger)})
	}
	if partialFailure {
		return errors.New("one or more host.auth.get_runtime callbacks failed")
	}
	return persistErr
}

func (m *Monitor) CheckNow(ctx context.Context) Status {
	_ = m.Reconcile(ctx, "management")
	return m.Snapshot()
}

func (m *Monitor) NotificationTimeout() time.Duration {
	return m.client.DeliveryTimeout()
}

func (m *Monitor) TestNotification(ctx context.Context) notifier.DeliveryResult {
	return m.client.Send(ctx, notifier.Message{
		Kind:     "test",
		Title:    "CLIProxyAPI Pushover test",
		Body:     "CLIProxyAPI Pushover test successful",
		Priority: 0,
	})
}

func (m *Monitor) Snapshot() Status {
	m.stateMu.RLock()
	status := m.status
	status.Accounts = append([]AccountStatus(nil), m.status.Accounts...)
	m.stateMu.RUnlock()
	status.Notifier = m.client.Snapshot()
	return status
}

func (m *Monitor) ensureStateLoaded(roster []protocol.HostAuthFileEntry) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.stateLoaded {
		return
	}
	if strings.TrimSpace(m.cfg.StateFile) == "" {
		hasAuthPath := false
		for _, entry := range roster {
			if strings.TrimSpace(entry.Path) != "" {
				hasAuthPath = true
				break
			}
		}
		if !hasAuthPath {
			m.status.StateFile = ""
			m.status.StateFileHealth = "waiting_for_auth_path"
			return
		}
	}
	m.store = state.Store{Path: state.ResolvePath(m.cfg.StateFile, roster)}
	m.status.StateFile = m.store.Path
	data, err := m.store.Load()
	if err != nil {
		m.data = state.NewData()
		m.status.StateFileHealth = "load_error_in_memory_only"
		m.host.Log(context.Background(), "warn", "account-health state could not be loaded; starting with an empty safe state", nil)
	} else {
		m.data = data
		m.status.StateFileHealth = "healthy"
	}
	m.stateLoaded = true
}

func (m *Monitor) applyObservation(snapshot health.RuntimeSnapshot, observation health.Observation, activeKeys map[string]struct{}, now time.Time) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	account := m.data.Accounts[snapshot.AuthKey]
	if account == nil {
		if oldKey := m.correlatedKeyLocked(snapshot, activeKeys); oldKey != "" {
			account = m.data.Accounts[oldKey]
			m.setAliasLocked(oldKey, snapshot.AuthKey)
			delete(m.data.Accounts, oldKey)
			account.AccountKey = snapshot.AuthKey
			account.AuthIndex = snapshot.AuthIndex
			account.RemovedAt = time.Time{}
			m.data.Accounts[snapshot.AuthKey] = account
		} else {
			account = &state.Account{
				AccountKey: snapshot.AuthKey,
				Provider:   snapshot.Provider,
				AuthIndex:  snapshot.AuthIndex,
				Health:     health.Unknown,
			}
			m.data.Accounts[snapshot.AuthKey] = account
		}
	}
	account.Provider = snapshot.Provider
	account.AuthIndex = snapshot.AuthIndex
	account.Label = snapshot.Label
	account.Identity = snapshot.Identity
	account.CPAStatus = snapshot.Status
	account.CPAUnavailable = snapshot.Unavailable
	account.QuotaLimited = observation.State == health.QuotaLimited
	account.LastObservedAt = now
	account.RemovedAt = time.Time{}
	if observation.State == health.Healthy || observation.State == health.QuotaLimited {
		account.LastSuccessfulHealthObservation = now
	}

	newState := observation.State
	if newState == health.Suspect {
		if account.SuspectSince.IsZero() {
			account.SuspectSince = now
		}
		if m.cfg.TransientConfirmAfter == 0 || !account.SuspectSince.Add(m.cfg.TransientConfirmAfter).After(now) {
			newState = health.CredentialDown
			observation.ReasonCode = confirmedReason(observation.ReasonCode)
		}
	} else {
		account.SuspectSince = time.Time{}
	}

	previous := account.Health
	if previous != newState {
		account.PreviousHealth = previous
		account.Health = newState
		account.LastChangedAt = now
		account.LastReasonCode = observation.ReasonCode
		if health.IsFailure(newState) {
			if !health.IsFailure(previous) {
				account.IncidentGeneration++
				if account.IncidentGeneration == 0 {
					account.IncidentGeneration = 1
				}
				if previous == health.Suspect && !account.SuspectSince.IsZero() {
					account.FirstDetectedAt = account.SuspectSince
				} else {
					account.FirstDetectedAt = now
				}
			}
			account.RecoveryPendingFrom = ""
			account.LastRecoveryAttemptAt = time.Time{}
			if !account.AlertSent {
				m.enqueueFailureLocked(account, false, now)
			}
		} else if health.IsCredentialHealthy(newState) {
			if account.AlertSent {
				account.RecoveryPendingFrom = previous
				if m.cfg.NotifyRecovery {
					m.enqueueRecoveryLocked(account, now)
				} else {
					account.AlertSent = false
					account.RecoveryPendingFrom = ""
				}
			}
			if !health.IsFailure(previous) && account.RecoveryPendingFrom == "" {
				account.FirstDetectedAt = time.Time{}
			}
		} else if newState == health.Disabled {
			account.AlertSent = false
			account.RecoveryPendingFrom = ""
			if m.cfg.NotifyDisabled {
				m.enqueueInformationalLocked(account, "disabled", now)
			}
		}
	} else {
		account.LastReasonCode = observation.ReasonCode
		if health.IsFailure(newState) {
			if !account.AlertSent && due(account.LastAlertAttemptAt, notificationRetryAfter, now) {
				m.enqueueFailureLocked(account, false, now)
			} else if account.AlertSent && m.cfg.ReminderInterval > 0 && due(account.LastAlertAt, m.cfg.ReminderInterval, now) && due(account.LastAlertAttemptAt, notificationRetryAfter, now) {
				m.enqueueFailureLocked(account, true, now)
			}
		}
		if health.IsCredentialHealthy(newState) && account.RecoveryPendingFrom != "" && due(account.LastRecoveryAttemptAt, notificationRetryAfter, now) {
			m.enqueueRecoveryLocked(account, now)
		}
	}
}

func (m *Monitor) correlatedKeyLocked(snapshot health.RuntimeSnapshot, activeKeys map[string]struct{}) string {
	if snapshot.Identity == "" {
		return ""
	}
	match := ""
	for key, account := range m.data.Accounts {
		if key == snapshot.AuthKey || account == nil || account.Provider != snapshot.Provider || account.Identity != snapshot.Identity {
			continue
		}
		if _, active := activeKeys[key]; active {
			return ""
		}
		if match != "" {
			return ""
		}
		match = key
	}
	return match
}

func (m *Monitor) setAliasLocked(oldKey, newKey string) {
	if oldKey == "" || newKey == "" || oldKey == newKey {
		return
	}
	delete(m.aliases, newKey)
	for key, target := range m.aliases {
		if target == oldKey {
			m.aliases[key] = newKey
		}
		if key == target {
			delete(m.aliases, key)
		}
	}
	m.aliases[oldKey] = newKey
}

func (m *Monitor) resolveAccountKeyLocked(accountKey string) string {
	visited := make(map[string]struct{}, len(m.aliases)+1)
	for hops := 0; hops <= len(m.aliases); hops++ {
		if _, exists := m.data.Accounts[accountKey]; exists {
			return accountKey
		}
		if _, seen := visited[accountKey]; seen {
			break
		}
		visited[accountKey] = struct{}{}
		next, exists := m.aliases[accountKey]
		if !exists || next == "" || next == accountKey {
			break
		}
		accountKey = next
	}
	return accountKey
}

func (m *Monitor) markRemoved(activeKeys map[string]struct{}, now time.Time) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	for key, account := range m.data.Accounts {
		if account == nil {
			continue
		}
		if _, active := activeKeys[key]; active {
			continue
		}
		if account.Health == health.Removed {
			continue
		}
		account.PreviousHealth = account.Health
		account.Health = health.Removed
		account.RemovedAt = now
		account.LastChangedAt = now
		account.LastReasonCode = "removed"
		account.AlertSent = false
		account.RecoveryPendingFrom = ""
		if m.cfg.NotifyRemoved {
			m.enqueueInformationalLocked(account, "removed", now)
		}
	}
}

func (m *Monitor) enqueueFailureLocked(account *state.Account, reminder bool, now time.Time) {
	kind := "failure"
	if reminder {
		kind = "reminder"
	}
	generation := account.IncidentGeneration
	account.LastAlertAttemptAt = now
	account.LastAlertAttemptGeneration = generation
	job := notifier.Job{
		Message:  m.failureMessage(account, reminder, now),
		Valid:    m.deliveryValid(account.AccountKey, kind, generation),
		Callback: m.deliveryCallback(account.AccountKey, kind, generation),
	}
	if !m.dispatcher.Enqueue(job) {
		m.data.LastNotificationErr = "notification queue is full"
	}
}

func (m *Monitor) enqueueRecoveryLocked(account *state.Account, now time.Time) {
	generation := account.IncidentGeneration
	account.LastRecoveryAttemptAt = now
	job := notifier.Job{
		Message:  m.recoveryMessage(account, now),
		Valid:    m.deliveryValid(account.AccountKey, "recovery", generation),
		Callback: m.deliveryCallback(account.AccountKey, "recovery", generation),
	}
	if !m.dispatcher.Enqueue(job) {
		m.data.LastNotificationErr = "notification queue is full"
	}
}

func (m *Monitor) enqueueInformationalLocked(account *state.Account, kind string, now time.Time) {
	message := notifier.Message{
		Kind:       kind,
		Title:      fmt.Sprintf("CLIProxyAPI: %s account %s", titleProvider(account.Provider), kind),
		Body:       fmt.Sprintf("%s account %s is now %s.", titleProvider(account.Provider), account.Label, kind),
		Priority:   0,
		Provider:   titleProvider(account.Provider),
		AccountKey: account.AccountKey,
		Label:      account.Label,
		Reason:     kind,
	}
	_ = now
	job := notifier.Job{
		Message:  message,
		Valid:    m.deliveryValid(account.AccountKey, kind, account.IncidentGeneration),
		Callback: m.deliveryCallback(account.AccountKey, kind, account.IncidentGeneration),
	}
	if !m.dispatcher.Enqueue(job) {
		m.data.LastNotificationErr = "notification queue is full"
	}
}

func (m *Monitor) deliveryValid(accountKey, kind string, generation uint64) func() bool {
	return func() bool {
		m.stateMu.RLock()
		defer m.stateMu.RUnlock()
		resolved := m.resolveAccountKeyLocked(accountKey)
		account := m.data.Accounts[resolved]
		if account == nil {
			return false
		}
		switch kind {
		case "failure", "reminder":
			return account.IncidentGeneration == generation && health.IsFailure(account.Health)
		case "recovery":
			return account.IncidentGeneration == generation && health.IsCredentialHealthy(account.Health) && account.RecoveryPendingFrom != ""
		case "disabled":
			return account.Health == health.Disabled
		case "removed":
			return account.Health == health.Removed
		default:
			return true
		}
	}
}

func (m *Monitor) deliveryCallback(accountKey, kind string, generation uint64) func(notifier.DeliveryResult) {
	return func(result notifier.DeliveryResult) {
		m.stateMu.Lock()
		defer m.stateMu.Unlock()
		accountKey = m.resolveAccountKeyLocked(accountKey)
		account := m.data.Accounts[accountKey]
		if result.Accepted {
			m.data.LastSuccessfulSend = result.At
			m.data.LastNotificationErr = ""
			if account != nil {
				switch kind {
				case "failure", "reminder":
					if account.IncidentGeneration != generation {
						break
					}
					if health.IsFailure(account.Health) || health.IsCredentialHealthy(account.Health) {
						account.AlertSent = true
						account.LastAlertAt = result.At
					}
					if health.IsCredentialHealthy(account.Health) {
						account.RecoveryPendingFrom = account.PreviousHealth
						if m.cfg.NotifyRecovery {
							m.enqueueRecoveryLocked(account, result.At)
						} else {
							account.AlertSent = false
							account.RecoveryPendingFrom = ""
						}
					} else if account.Health == health.Disabled || account.Health == health.Removed {
						account.AlertSent = false
					}
				case "recovery":
					if account.IncidentGeneration != generation {
						if health.IsFailure(account.Health) && account.LastAlertAttemptGeneration != account.IncidentGeneration {
							account.AlertSent = false
							m.enqueueFailureLocked(account, false, result.At)
						}
						break
					}
					if health.IsCredentialHealthy(account.Health) && account.RecoveryPendingFrom != "" {
						account.LastRecoveryAt = result.At
						account.RecoveryPendingFrom = ""
						account.AlertSent = false
						account.FirstDetectedAt = time.Time{}
					}
				}
			}
		} else {
			m.data.LastNotificationErr = result.Error
		}
		_ = m.persistLocked()
		m.status.Accounts = accountStatuses(m.data, m.cfg.ReminderInterval)
	}
}

func (m *Monitor) failureMessage(account *state.Account, reminder bool, now time.Time) notifier.Message {
	provider := titleProvider(account.Provider)
	kind := string(account.Health)
	var title, body string
	if reminder {
		title = fmt.Sprintf("CLIProxyAPI: %s incident still unresolved", provider)
		body = fmt.Sprintf("%s is still unavailable.\nIncident: %s\nReason: %s\nFirst detected: %s", account.Label, kind, account.LastReasonCode, formatTime(account.FirstDetectedAt))
	} else if account.Health == health.ReauthRequired {
		title = fmt.Sprintf("CLIProxyAPI: %s reauth required", provider)
		body = fmt.Sprintf("%s is unavailable because its OAuth credentials were rejected.\nManual sign-in is required.\nIncident: %s\nReason: %s\nDetected: %s\nOther healthy accounts will continue routing if available.", account.Label, kind, account.LastReasonCode, formatTime(account.FirstDetectedAt))
	} else {
		title = fmt.Sprintf("CLIProxyAPI: %s account down", provider)
		duration := time.Duration(0)
		if !account.FirstDetectedAt.IsZero() {
			duration = now.Sub(account.FirstDetectedAt).Round(time.Second)
		}
		if duration < 0 {
			duration = 0
		}
		body = fmt.Sprintf("%s has been unusable for %s because of a persistent credential error.\nIncident: %s\nReason: %s\nCheck the CLIProxyAPI auth status.", account.Label, duration, kind, account.LastReasonCode)
	}
	if m.cfg.ManagementURL != "" {
		body += "\nManagement: " + m.cfg.ManagementURL
	}
	return notifier.Message{
		Kind:       map[bool]string{true: "reminder", false: "failure"}[reminder],
		Title:      title,
		Body:       body,
		Priority:   m.cfg.FailurePriority,
		Provider:   provider,
		AccountKey: account.AccountKey,
		Label:      account.Label,
		Reason:     account.LastReasonCode,
	}
}

func (m *Monitor) recoveryMessage(account *state.Account, now time.Time) notifier.Message {
	provider := titleProvider(account.Provider)
	downtime := time.Duration(0)
	if !account.FirstDetectedAt.IsZero() {
		downtime = now.Sub(account.FirstDetectedAt).Round(time.Second)
	}
	if downtime < 0 {
		downtime = 0
	}
	body := fmt.Sprintf("%s is healthy and available again.\nDowntime: %s", account.Label, downtime)
	if account.Health == health.QuotaLimited {
		body = fmt.Sprintf("%s is authenticated again and currently quota limited; its credential is healthy.\nDowntime: %s", account.Label, downtime)
	}
	return notifier.Message{
		Kind:       "recovery",
		Title:      fmt.Sprintf("CLIProxyAPI: %s account recovered", provider),
		Body:       body,
		Priority:   m.cfg.RecoveryPriority,
		Provider:   provider,
		AccountKey: account.AccountKey,
		Label:      account.Label,
		Reason:     "recovered",
	}
}

func (m *Monitor) persistLocked() error {
	if !m.stateLoaded {
		return nil
	}
	if err := m.store.Save(m.data); err != nil {
		m.status.StateFileHealth = "write_error_in_memory_only"
		return err
	}
	m.status.StateFileHealth = "healthy"
	return nil
}

func (m *Monitor) recordMonitoringFailure(now time.Time, message string) {
	m.stateMu.Lock()
	m.status.LastScan = now
	m.status.MonitoringStale = true
	m.status.LastMonitoringError = message
	m.stateMu.Unlock()
}

func (m *Monitor) setNextScan(next time.Time) {
	m.stateMu.Lock()
	m.status.NextScan = next.UTC()
	m.stateMu.Unlock()
}

func accountStatuses(data state.Data, reminderInterval time.Duration) []AccountStatus {
	rows := make([]AccountStatus, 0, len(data.Accounts))
	for _, account := range data.Accounts {
		if account == nil {
			continue
		}
		row := AccountStatus{
			Provider:                        account.Provider,
			Label:                           account.Label,
			AuthIndex:                       account.AuthIndex,
			Health:                          account.Health,
			CPAStatus:                       account.CPAStatus,
			CPAUnavailable:                  account.CPAUnavailable,
			QuotaLimited:                    account.QuotaLimited,
			ReasonCode:                      account.LastReasonCode,
			FirstDetectedAt:                 account.FirstDetectedAt,
			LastTransitionAt:                account.LastChangedAt,
			LastSuccessfulHealthObservation: account.LastSuccessfulHealthObservation,
			LastAlertAt:                     account.LastAlertAt,
			RemovedAt:                       account.RemovedAt,
		}
		if reminderInterval > 0 && account.AlertSent && !account.LastAlertAt.IsZero() && health.IsFailure(account.Health) {
			row.NextReminderAt = account.LastAlertAt.Add(reminderInterval)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider == rows[j].Provider {
			if rows[i].Label == rows[j].Label {
				return rows[i].AuthIndex < rows[j].AuthIndex
			}
			return rows[i].Label < rows[j].Label
		}
		return rows[i].Provider < rows[j].Provider
	})
	return rows
}

func (m *Monitor) effectiveRemovedStateRetention() time.Duration {
	retention := m.cfg.RemovedStateRetention
	if !m.cfg.NotifyRemoved {
		return retention
	}
	minimum := m.cfg.NotificationCoalesceWindow + m.client.DeliveryTimeout()
	if retention < minimum {
		return minimum
	}
	return retention
}

func due(last time.Time, interval time.Duration, now time.Time) bool {
	return last.IsZero() || !last.Add(interval).After(now)
}

func confirmedReason(reason string) string {
	if reason == "" {
		return "persistent_credential_error"
	}
	if strings.HasPrefix(reason, "persistent_") {
		return reason
	}
	return "persistent_" + reason
}

func titleProvider(provider string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return "OAuth"
	}
	runes := []rune(strings.ToLower(provider))
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.Local().Format("2006-01-02 15:04 MST")
}

func safeField(value string) string {
	value = strings.TrimSpace(value)
	value = filepath.Base(value)
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}
