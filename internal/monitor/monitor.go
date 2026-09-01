package monitor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/notifier"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/state"
)

var ErrStopping = errors.New("monitor is stopping")

const (
	notificationRetryAfter = 5 * time.Minute
	failureEvidenceTTL     = 2 * time.Minute
	notificationDrainMin   = 5 * time.Second
	notificationDrainMax   = time.Minute
)

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

	reconcileMu         sync.Mutex
	beforeReconcileLock func()
	stateMu             sync.RWMutex
	data                state.Data
	store               state.Store
	stateLoaded         bool
	rollbackPending     atomic.Bool
	status              Status

	ctx    context.Context
	cancel context.CancelFunc
	loopWG sync.WaitGroup

	lifecycleMu               sync.Mutex
	operations                sync.WaitGroup
	stopping                  bool
	stopStartOnce             sync.Once
	stopFinishOnce            sync.Once
	drained                   chan struct{}
	retirementDone            chan struct{}
	retiring                  atomic.Bool
	retired                   bool
	beforeFinalWait           func()
	beforeFinalDispatcherWait func()
	beforeRetirementSave      func()

	signals   chan struct{}
	pendingMu sync.Mutex
	pending   map[string]struct{}
	evidence  map[string]health.FailureEvidence
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
		ctx:            ctx,
		cancel:         cancel,
		drained:        make(chan struct{}),
		retirementDone: make(chan struct{}),
		signals:        make(chan struct{}, 1),
		pending:        make(map[string]struct{}),
		evidence:       make(map[string]health.FailureEvidence),
	}
}

func (m *Monitor) Start() {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.stopping {
		return
	}
	m.dispatcher.Start()
	if !m.cfg.Enabled {
		return
	}
	m.loopWG.Add(1)
	go m.loop()
}

// Stop closes reconciliation admission before waiting, so WaitGroup.Add can
// never race with Wait. Final shutdown waits without a bound for every admitted
// reconciliation, including a host callback that ignores context cancellation.
func (m *Monitor) Stop() {
	m.beginStop()
	if m.beforeFinalWait != nil {
		m.beforeFinalWait()
	}
	<-m.drained
	m.finishStop(time.Time{})
	if m.beforeFinalDispatcherWait != nil {
		m.beforeFinalDispatcherWait()
	}
	m.dispatcher.StopAndWait()
	<-m.retirementDone
}

// StopWithin closes reconciliation admission and retires the monitor within a
// bounded lifecycle budget. A false return means an admitted reconciliation is
// still unwinding and must be waited during final shutdown.
func (m *Monitor) StopWithin(timeout time.Duration) bool {
	m.beginStop()
	deadline := time.Now().Add(timeout)
	drained := false
	if timeout > 0 {
		// Reserve half of the lifecycle budget for notifier cancellation,
		// unattempted terminal bookkeeping, and retirement handoff.
		operationBudget := timeout - timeout/2
		timer := time.NewTimer(operationBudget)
		select {
		case <-m.drained:
			drained = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	} else {
		select {
		case <-m.drained:
			drained = true
		default:
		}
	}
	m.finishStop(deadline)
	if !drained {
		select {
		case <-m.drained:
			drained = true
		default:
		}
	}
	return drained
}

func (m *Monitor) beginStop() {
	m.stopStartOnce.Do(func() {
		m.lifecycleMu.Lock()
		m.stopping = true
		m.cancel()
		m.lifecycleMu.Unlock()
		go func() {
			m.loopWG.Wait()
			m.operations.Wait()
			close(m.drained)
		}()
	})
}

func (m *Monitor) finishStop(deadline time.Time) {
	m.stopFinishOnce.Do(func() {
		m.dispatcher.BeginDrain()
		if deadline.IsZero() {
			drainCtx, cancelDrain := context.WithTimeout(context.Background(), m.notificationDrainTimeout())
			m.dispatcher.WaitIdle(drainCtx)
			cancelDrain()
			m.dispatcher.Stop()
		} else {
			remaining := time.Until(deadline)
			if remaining > 0 {
				// A quick graceful drain preserves alerts whose reconciliation won
				// admission, while retaining a cancellation budget for queued jobs
				// behind a delivery that ignores context cancellation.
				drainCtx, cancelDrain := context.WithTimeout(context.Background(), remaining/2)
				m.dispatcher.WaitIdle(drainCtx)
				cancelDrain()
			}
			m.dispatcher.StopWithin(time.Until(deadline))
		}
		m.retiring.Store(true)
		m.startRetirement(deadline)
	})
}

func (m *Monitor) startRetirement(deadline time.Time) {
	if deadline.IsZero() {
		go m.completeRetirement()
		return
	}
	for {
		if m.stateMu.TryLock() {
			m.completeRetirementLocked()
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			go m.completeRetirement()
			return
		}
		pause := time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		<-timer.C
	}
}

func (m *Monitor) completeRetirement() {
	m.stateMu.Lock()
	m.completeRetirementLocked()
}

func (m *Monitor) completeRetirementLocked() {
	loaded := m.stateLoaded
	store := m.store
	shouldSave := loaded && m.rollbackPending.Load()
	var snapshot state.Data
	if shouldSave {
		snapshot = state.Clone(m.data)
	}
	m.status.Accounts = accountStatuses(m.data, m.cfg.ReminderInterval)
	m.retired = true
	m.stateMu.Unlock()

	go func() {
		if shouldSave {
			if m.beforeRetirementSave != nil {
				m.beforeRetirementSave()
			}
			_ = store.Save(snapshot)
		}
		if loaded {
			store.Release()
		}
		close(m.retirementDone)
	}()
}

func (m *Monitor) isRetired() bool {
	if m.retiring.Load() {
		return true
	}
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.retired
}

func (m *Monitor) beginOperation() bool {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.stopping {
		return false
	}
	m.operations.Add(1)
	return true
}

func (m *Monitor) loop() {
	defer m.loopWG.Done()
	startup := time.NewTimer(m.cfg.StartupGrace)
	defer startup.Stop()
	select {
	case <-m.ctx.Done():
		return
	case <-startup.C:
	}

	// Usage events received during startup grace retain their structured status
	// evidence, but cannot bypass the baseline delay. Drain the old wake before
	// clearing its pending markers: an event arriving after the drain then leaves
	// a wake queued for a follow-up scan, while an earlier event is included in
	// the baseline through its retained evidence.
	m.drainSignalWake()
	m.clearPending()
	_ = m.Reconcile(m.ctx, "startup")

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
		case <-m.signals:
			if !m.processSignals() {
				return
			}
		}
	}
}

func (m *Monitor) processSignals() bool {
	timer := time.NewTimer(m.cfg.UsageRecheckDelay)
	defer timer.Stop()
	select {
	case <-m.ctx.Done():
		return false
	case <-timer.C:
	}
	m.clearPending()
	_ = m.Reconcile(m.ctx, "usage_failure")
	return true
}

func (m *Monitor) clearPending() {
	m.pendingMu.Lock()
	m.pending = make(map[string]struct{})
	m.pendingMu.Unlock()
}

func (m *Monitor) drainSignalWake() {
	select {
	case <-m.signals:
	default:
	}
}

func (m *Monitor) ObserveUsageFailure(authIndex string, statusCode int) bool {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || !m.cfg.Enabled || !health.IsClassifiableFailureStatus(statusCode) {
		return false
	}
	m.pendingMu.Lock()
	m.evidence[authIndex] = health.FailureEvidence{HTTPStatus: statusCode, ObservedAt: m.now().UTC()}
	wasEmpty := len(m.pending) == 0
	m.pending[authIndex] = struct{}{}
	m.pendingMu.Unlock()
	if wasEmpty {
		select {
		case m.signals <- struct{}{}:
		default:
		}
	}
	return true
}

func (m *Monitor) failureEvidenceFor(entry protocol.HostAuthFileEntry, now time.Time) health.FailureEvidence {
	authIndex := strings.TrimSpace(entry.AuthIndex)
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	evidence := m.evidence[authIndex]
	if evidence.HTTPStatus == 0 {
		return health.FailureEvidence{}
	}
	if now.Sub(evidence.ObservedAt) > failureEvidenceTTL || (!entry.UpdatedAt.IsZero() && entry.UpdatedAt.After(evidence.ObservedAt)) || (strings.EqualFold(strings.TrimSpace(entry.Status), "active") && !entry.Unavailable) {
		delete(m.evidence, authIndex)
		return health.FailureEvidence{}
	}
	return evidence
}

func (m *Monitor) pruneFailureEvidence(candidates []protocol.HostAuthFileEntry, now time.Time) {
	active := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		active[strings.TrimSpace(candidate.AuthIndex)] = struct{}{}
	}
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	for authIndex, evidence := range m.evidence {
		_, exists := active[authIndex]
		if !exists || now.Sub(evidence.ObservedAt) > failureEvidenceTTL {
			delete(m.evidence, authIndex)
			delete(m.pending, authIndex)
		}
	}
}

func (m *Monitor) lockReconcile(ctx context.Context) error {
	if m.beforeReconcileLock != nil {
		m.beforeReconcileLock()
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if m.reconcileMu.TryLock() {
			if m.ctx.Err() != nil {
				m.reconcileMu.Unlock()
				return ErrStopping
			}
			if err := ctx.Err(); err != nil {
				m.reconcileMu.Unlock()
				return err
			}
			return nil
		}
		select {
		case <-m.ctx.Done():
			return ErrStopping
		case <-ctx.Done():
			if m.ctx.Err() != nil {
				return ErrStopping
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Monitor) Reconcile(ctx context.Context, trigger string) error {
	if !m.beginOperation() {
		return ErrStopping
	}
	defer m.operations.Done()

	if err := m.lockReconcile(ctx); err != nil {
		return err
	}
	defer m.reconcileMu.Unlock()
	if m.isRetired() {
		return ErrStopping
	}
	now := m.now().UTC()
	roster, err := m.host.ListAuth(ctx)
	if err != nil {
		if m.isRetired() {
			return ErrStopping
		}
		message := "host.auth.list failed; previous account state was retained"
		m.recordMonitoringFailure(now, message)
		m.host.Log(ctx, "warn", message, map[string]any{"trigger": safeField(trigger)})
		return err
	}
	candidates := health.Discover(roster, m.cfg)
	m.pruneFailureEvidence(candidates, now)
	loadWarning, loadErr := m.ensureStateLoaded(ctx, roster)
	if loadErr != nil {
		return loadErr
	}
	if loadWarning {
		m.host.Log(ctx, "warn", "account-health state could not be loaded; starting with an empty safe state", nil)
		if m.isRetired() {
			return ErrStopping
		}
	}

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
			evidence := m.failureEvidenceFor(runtimeEntry, now)
			snapshot := health.FromHostEntry(runtimeEntry, evidence, now)
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
	if m.isRetired() {
		return ErrStopping
	}

	items := make([]result, 0, len(candidates))
	for item := range results {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return health.AccountKey(items[i].entry.Provider, items[i].entry.AuthIndex) < health.AccountKey(items[j].entry.Provider, items[j].entry.AuthIndex)
	})

	activeKeys := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		activeKeys[health.AccountKey(candidate.Provider, candidate.AuthIndex)] = struct{}{}
	}

	// Correlation authority comes from the complete scan. Count each identity at
	// most once per candidate even when auth.list and get_runtime report the same
	// value, while allowing runtime enrichment to expose cross-candidate
	// ambiguity before either the exact pre-pass or fallback can move state.
	scanIdentityCounts := make(map[string]int, len(items))
	runtimeIdentities := make(map[string]string, len(items))
	runtimeObserved := make(map[string]bool, len(items))
	for _, item := range items {
		candidateIdentities := make(map[string]struct{}, 2)
		if identity := health.IdentityFingerprint(item.entry); identity != "" {
			candidateIdentities[identityCountKey(item.entry.Provider, identity)] = struct{}{}
		}
		if item.err == nil {
			accountKey := health.AccountKey(item.entry.Provider, item.entry.AuthIndex)
			runtimeObserved[accountKey] = true
			runtimeIdentities[accountKey] = item.snapshot.Identity
			if item.snapshot.Identity != "" {
				candidateIdentities[identityCountKey(item.snapshot.Provider, item.snapshot.Identity)] = struct{}{}
			}
		}
		for key := range candidateIdentities {
			scanIdentityCounts[key]++
		}
	}
	m.correlateReplacementCandidates(candidates, scanIdentityCounts, runtimeIdentities, runtimeObserved)

	partialFailure := false
	var runtimeContextErr error
	failedEntries := make([]protocol.HostAuthFileEntry, 0)
	for _, item := range items {
		if item.err != nil {
			partialFailure = true
			if errors.Is(item.err, context.DeadlineExceeded) {
				runtimeContextErr = context.DeadlineExceeded
			} else if runtimeContextErr == nil && errors.Is(item.err, context.Canceled) {
				runtimeContextErr = context.Canceled
			}
			failedEntries = append(failedEntries, item.entry)
			continue
		}
		m.applyObservation(item.snapshot, item.observation, activeKeys, scanIdentityCounts, now)
	}
	for _, entry := range failedEntries {
		identity := health.IdentityFingerprint(entry)
		if identity != "" && scanIdentityCounts[identityCountKey(entry.Provider, identity)] == 1 {
			m.preserveReplacementCandidate(entry, activeKeys)
		}
	}
	m.markRemoved(activeKeys, now)

	m.stateMu.Lock()
	if m.retiring.Load() || m.retired {
		m.stateMu.Unlock()
		return ErrStopping
	}
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
		if runtimeContextErr != nil {
			return runtimeContextErr
		}
		return errors.New("one or more host.auth.get_runtime callbacks failed")
	}
	return persistErr
}

func (m *Monitor) CheckNow(ctx context.Context) (Status, error) {
	err := m.Reconcile(ctx, "management")
	return m.Snapshot(), err
}

func (m *Monitor) notificationDrainTimeout() time.Duration {
	timeout := m.client.DeliveryTimeout()
	if timeout < notificationDrainMin {
		return notificationDrainMin
	}
	if timeout > notificationDrainMax {
		return notificationDrainMax
	}
	return timeout
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

func (m *Monitor) ensureStateLoaded(ctx context.Context, roster []protocol.HostAuthFileEntry) (bool, error) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	if m.retiring.Load() || m.retired {
		return false, ErrStopping
	}
	if m.stateLoaded {
		return false, nil
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
			return false, nil
		}
	}
	m.store = state.Store{Path: state.ResolvePath(m.cfg.StateFile, roster)}
	m.status.StateFile = m.store.Path

	// A replacement monitor may encounter an old writer whose validated commit
	// is still in flight. Bound lease acquisition by both the caller and monitor
	// lifecycle contexts so management checks can retry and stop can detach.
	claimCtx, cancelClaim := context.WithCancel(ctx)
	stopClaimCancel := context.AfterFunc(m.ctx, cancelClaim)
	err := m.store.ClaimContext(claimCtx)
	stopClaimCancel()
	cancelClaim()
	if err != nil {
		if m.ctx.Err() != nil || m.retiring.Load() || m.retired {
			m.status.StateFileHealth = "writer_claim_stopping"
			return false, ErrStopping
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			m.status.StateFileHealth = "writer_claim_wait_timeout"
			return false, err
		}
		m.data = state.NewData()
		m.status.StateFileHealth = "writer_claim_error_in_memory_only"
		m.stateLoaded = true
		return false, nil
	}
	if m.ctx.Err() != nil || m.retiring.Load() || m.retired {
		m.store.Release()
		m.status.StateFileHealth = "writer_claim_stopping"
		return false, ErrStopping
	}
	data, err := m.store.Load()
	if err != nil {
		m.data = state.NewData()
		m.status.StateFileHealth = "load_error_in_memory_only"
	} else {
		m.data = data
		m.status.StateFileHealth = "healthy"
	}
	m.stateLoaded = true
	return err != nil, nil
}

func (m *Monitor) applyObservation(snapshot health.RuntimeSnapshot, observation health.Observation, activeKeys map[string]struct{}, identityCounts map[string]int, now time.Time) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	account := m.data.Accounts[snapshot.AuthKey]
	if account != nil && account.Identity != "" && snapshot.Identity != "" && account.Identity != snapshot.Identity {
		// An auth index can be reused after replacement. Without a safe identity
		// correlation, the new credential must not inherit the prior logical
		// account's incident generation, delivery state, or recovery state.
		account = nil
	}
	if account == nil {
		if oldKey := m.correlatedKeyLocked(snapshot, activeKeys, identityCounts); oldKey != "" {
			account = m.data.Accounts[oldKey]
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
	if health.IsCredentialHealthy(observation.State) {
		account.LastSuccessfulHealthObservation = now
	}

	newState := observation.State
	firstDetected := time.Time{}
	if newState == health.Suspect {
		if observation.Confirmation == health.ConfirmationNone || !health.IsFailure(observation.ConfirmAs) {
			account.ClearSuspect()
		} else {
			// A sustained outage can alternate among status codes within the same
			// confirmation class (for example 502/503). Reset only when the class
			// or target meaningfully changes, while retaining the latest closed
			// reason code for a future promotion.
			if account.SuspectSince.IsZero() || account.SuspectClass != observation.Confirmation || account.SuspectTarget != observation.ConfirmAs {
				account.SuspectSince = now
				account.SuspectClass = observation.Confirmation
				account.SuspectTarget = observation.ConfirmAs
			}
			account.SuspectReason = observation.ReasonCode
			confirmAfter := m.cfg.TransientConfirmAfter
			if observation.Confirmation == health.ConfirmationUnauthorized {
				confirmAfter = m.cfg.UnauthorizedConfirmAfter
			}
			if confirmAfter == 0 || !account.SuspectSince.Add(confirmAfter).After(now) {
				firstDetected = account.SuspectSince
				newState = observation.ConfirmAs
				observation.ReasonCode = health.PersistentReason(observation.ReasonCode)
				account.ClearSuspect()
			}
		}
		// An ambiguous observation cannot prove that an already-confirmed
		// credential incident recovered. Keep the confirmed external state (and
		// reminder/dedupe generation) until a credential-healthy, disabled,
		// removed, or differently confirmed observation provides a transition.
		if newState == health.Suspect && health.IsFailure(account.Health) {
			newState = account.Health
			observation.ReasonCode = account.LastReasonCode
		}
	} else {
		account.ClearSuspect()
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
				if !firstDetected.IsZero() {
					account.FirstDetectedAt = firstDetected
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
				m.beginRecoveryLocked(account, previous, now)
			}
			if !health.IsFailure(previous) && account.RecoveryPendingFrom == "" {
				account.FirstDetectedAt = time.Time{}
			}
		} else if newState == health.Disabled {
			account.AlertSent = false
			account.RecoveryPendingFrom = ""
			if m.cfg.NotifyDisabled {
				m.enqueueInformationalLocked(account, "disabled")
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

func identityCountKey(provider, identity string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "\x00" + identity
}

func (m *Monitor) correlateReplacementCandidates(candidates []protocol.HostAuthFileEntry, identityCounts map[string]int, runtimeIdentities map[string]string, runtimeObserved map[string]bool) {
	type replacementMove struct {
		oldKey    string
		newKey    string
		authIndex string
		account   *state.Account
	}

	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	moves := make([]replacementMove, 0)
	for _, candidate := range candidates {
		identity := health.IdentityFingerprint(candidate)
		provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if identity == "" || identityCounts[identityCountKey(provider, identity)] != 1 {
			continue
		}
		newKey := health.AccountKey(provider, candidate.AuthIndex)
		if runtimeObserved[newKey] && runtimeIdentities[newKey] != "" && runtimeIdentities[newKey] != identity {
			// A successful runtime read that contradicts the roster identity vetoes
			// roster-only continuity. The runtime fallback may still correlate its
			// own identity if that identity is unique across the complete scan.
			continue
		}
		if current := m.data.Accounts[newKey]; current != nil && current.Provider == provider && current.Identity == identity {
			continue
		}

		oldKey := ""
		var matched *state.Account
		for key, account := range m.data.Accounts {
			if key == newKey || account == nil || account.Provider != provider || account.Identity != identity {
				continue
			}
			if matched != nil {
				matched = nil
				oldKey = ""
				break
			}
			oldKey = key
			matched = account
		}
		if matched != nil {
			moves = append(moves, replacementMove{oldKey: oldKey, newKey: newKey, authIndex: strings.TrimSpace(candidate.AuthIndex), account: matched})
		}
	}
	if len(moves) == 0 {
		return
	}

	validBySource := make(map[string]replacementMove, len(moves))
	for _, move := range moves {
		validBySource[move.oldKey] = move
	}
	for changed := true; changed; {
		changed = false
		for source, move := range validBySource {
			if occupant := m.data.Accounts[move.newKey]; occupant != nil {
				if _, moving := validBySource[move.newKey]; !moving {
					delete(validBySource, source)
					changed = true
				}
			}
		}
	}
	valid := make([]replacementMove, 0, len(validBySource))
	for _, move := range moves {
		if _, ok := validBySource[move.oldKey]; ok {
			valid = append(valid, move)
		}
	}
	for _, move := range valid {
		delete(m.data.Accounts, move.oldKey)
	}
	for _, move := range valid {
		move.account.AccountKey = move.newKey
		move.account.AuthIndex = move.authIndex
		move.account.RemovedAt = time.Time{}
		m.data.Accounts[move.newKey] = move.account
	}
}

func (m *Monitor) preserveReplacementCandidate(candidate protocol.HostAuthFileEntry, activeKeys map[string]struct{}) {
	identity := health.IdentityFingerprint(candidate)
	if identity == "" {
		return
	}
	candidateKey := health.AccountKey(candidate.Provider, candidate.AuthIndex)
	match := ""
	m.stateMu.RLock()
	for key, account := range m.data.Accounts {
		if key == candidateKey || account == nil || account.Provider != candidate.Provider || account.Identity != identity {
			continue
		}
		if _, active := activeKeys[key]; active {
			continue
		}
		if match != "" {
			match = ""
			break
		}
		match = key
	}
	m.stateMu.RUnlock()
	if match != "" {
		// A roster-visible replacement with an exact, unique safe identity keeps
		// the old logical account active while its runtime read is temporarily
		// unavailable. A later successful read can then correlate and recover it.
		activeKeys[match] = struct{}{}
	}
}

func (m *Monitor) correlatedKeyLocked(snapshot health.RuntimeSnapshot, activeKeys map[string]struct{}, identityCounts map[string]int) string {
	if snapshot.Identity == "" || identityCounts[identityCountKey(snapshot.Provider, snapshot.Identity)] != 1 {
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
		account.LastReasonCode = health.ReasonRemoved
		account.AlertSent = false
		account.RecoveryPendingFrom = ""
		if m.cfg.NotifyRemoved {
			m.enqueueInformationalLocked(account, "removed")
		}
	}
}

func (m *Monitor) beginRecoveryLocked(account *state.Account, from health.State, now time.Time) {
	account.RecoveryPendingFrom = from
	if m.cfg.NotifyRecovery {
		m.enqueueRecoveryLocked(account, now)
	} else {
		account.AlertSent = false
		account.RecoveryPendingFrom = ""
	}
}

func (m *Monitor) enqueueFailureLocked(account *state.Account, reminder bool, now time.Time) {
	kind := "failure"
	if reminder {
		kind = "reminder"
	}
	generation := account.IncidentGeneration
	if m.enqueueLocked(account, kind, generation, now, m.failureMessage(account, kind, now)) {
		account.LastAlertAttemptAt = now
		account.LastAlertAttemptGeneration = generation
	}
}

func (m *Monitor) enqueueRecoveryLocked(account *state.Account, now time.Time) {
	generation := account.IncidentGeneration
	if m.enqueueLocked(account, "recovery", generation, now, m.recoveryMessage(account, now)) {
		account.LastRecoveryAttemptAt = now
	}
}

func (m *Monitor) enqueueInformationalLocked(account *state.Account, kind string) {
	provider := titleProvider(account.Provider)
	message := notifier.Message{
		Kind:       kind,
		Title:      fmt.Sprintf("CLIProxyAPI: %s account %s", provider, kind),
		Body:       fmt.Sprintf("%s account %s is now %s.", provider, account.Label, kind),
		Priority:   0,
		Provider:   provider,
		AccountKey: account.AccountKey,
		Label:      account.Label,
		Reason:     kind,
	}
	m.enqueueLocked(account, kind, account.IncidentGeneration, time.Time{}, message)
}

func (m *Monitor) enqueueLocked(account *state.Account, kind string, generation uint64, attemptAt time.Time, message notifier.Message) bool {
	if m.retiring.Load() || m.retired {
		return false
	}
	job := notifier.Job{
		Message:  message,
		Valid:    m.deliveryValid(account, kind, generation),
		Abandon:  m.deliveryAbandon(account, kind, generation, attemptAt),
		Callback: m.deliveryCallback(account, kind, generation, attemptAt),
	}
	if !m.dispatcher.Enqueue(job) {
		m.data.LastNotificationErr = "notification queue is unavailable"
		return false
	}
	return true
}

func (m *Monitor) accountCurrentLocked(target *state.Account) bool {
	if target == nil {
		return false
	}
	for _, account := range m.data.Accounts {
		if account == target {
			return true
		}
	}
	return false
}

func (m *Monitor) deliveryAbandon(account *state.Account, kind string, generation uint64, attemptAt time.Time) func() {
	if account == nil || attemptAt.IsZero() || generation == 0 {
		return nil
	}
	rollbackKind := state.AlertSuppression
	if kind == "recovery" {
		rollbackKind = state.RecoverySuppression
	} else if kind != "failure" && kind != "reminder" {
		return nil
	}
	path := m.store.Path
	accountKey := account.AccountKey
	identity := account.Identity
	return func() {
		m.rollbackPending.Store(true)
		state.RegisterSuppressionRollback(path, state.SuppressionRollback{
			AccountKey: accountKey,
			Identity:   identity,
			Kind:       rollbackKind,
			Generation: generation,
			AttemptAt:  attemptAt,
		})
	}
}

func (m *Monitor) deliveryValid(account *state.Account, kind string, generation uint64) func() bool {
	return func() bool {
		m.stateMu.RLock()
		defer m.stateMu.RUnlock()
		if !m.accountCurrentLocked(account) {
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

func (m *Monitor) deliveryCallback(account *state.Account, kind string, generation uint64, attemptAt time.Time) func(notifier.DeliveryResult) {
	return func(result notifier.DeliveryResult) {
		m.stateMu.Lock()
		defer m.stateMu.Unlock()
		if m.retiring.Load() || m.retired {
			return
		}
		current := m.accountCurrentLocked(account)
		if result.Unattempted {
			if !current {
				return
			}
			changed := false
			switch kind {
			case "failure", "reminder":
				if account.IncidentGeneration == generation && account.LastAlertAttemptGeneration == generation && account.LastAlertAttemptAt.Equal(attemptAt) {
					account.LastAlertAttemptAt = time.Time{}
					account.LastAlertAttemptGeneration = 0
					changed = true
				}
			case "recovery":
				if account.IncidentGeneration == generation && account.LastRecoveryAttemptAt.Equal(attemptAt) {
					account.LastRecoveryAttemptAt = time.Time{}
					changed = true
				}
			}
			if changed {
				// Retirement batches all unattempted marker clears into one
				// asynchronous state write. Keeping terminal callbacks free of
				// filesystem I/O preserves the bounded lifecycle budget.
				m.rollbackPending.Store(true)
				m.status.Accounts = accountStatuses(m.data, m.cfg.ReminderInterval)
			}
			return
		}
		if result.Accepted {
			m.data.LastSuccessfulSend = result.At
			m.data.LastNotificationErr = ""
			if current {
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
						m.beginRecoveryLocked(account, account.PreviousHealth, result.At)
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

func (m *Monitor) failureMessage(account *state.Account, notificationKind string, now time.Time) notifier.Message {
	provider := titleProvider(account.Provider)
	incidentKind := string(account.Health)
	var title, body string
	if notificationKind == "reminder" {
		title = fmt.Sprintf("CLIProxyAPI: %s incident still unresolved", provider)
		body = fmt.Sprintf("%s is still unavailable.\nIncident: %s\nReason: %s\nFirst detected: %s", account.Label, incidentKind, account.LastReasonCode, formatTime(account.FirstDetectedAt))
	} else if account.Health == health.ReauthRequired {
		title = fmt.Sprintf("CLIProxyAPI: %s reauth required", provider)
		body = fmt.Sprintf("%s is unavailable because its OAuth credentials were rejected.\nManual sign-in is required.\nIncident: %s\nReason: %s\nDetected: %s\nOther healthy accounts will continue routing if available.", account.Label, incidentKind, account.LastReasonCode, formatTime(account.FirstDetectedAt))
	} else {
		title = fmt.Sprintf("CLIProxyAPI: %s account down", provider)
		duration := time.Duration(0)
		if !account.FirstDetectedAt.IsZero() {
			duration = now.Sub(account.FirstDetectedAt).Round(time.Second)
		}
		if duration < 0 {
			duration = 0
		}
		body = fmt.Sprintf("%s has been unusable for %s because of a persistent credential error.\nIncident: %s\nReason: %s\nCheck the CLIProxyAPI auth status.", account.Label, duration, incidentKind, account.LastReasonCode)
	}
	if m.cfg.ManagementURL != "" {
		body += "\nManagement: " + m.cfg.ManagementURL
	}
	return notifier.Message{
		Kind:       notificationKind,
		Title:      title,
		Body:       body,
		Priority:   m.cfg.FailurePriority,
		Provider:   provider,
		AccountKey: account.AccountKey,
		Label:      account.Label,
		Reason:     string(account.LastReasonCode),
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
	if m.retiring.Load() || m.retired {
		return state.ErrWriterSuperseded
	}
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
	if m.retiring.Load() || m.retired {
		m.stateMu.Unlock()
		return
	}
	m.status.LastScan = now
	m.status.MonitoringStale = true
	m.status.LastMonitoringError = message
	m.stateMu.Unlock()
}

func (m *Monitor) setNextScan(next time.Time) {
	m.stateMu.Lock()
	if m.retiring.Load() || m.retired {
		m.stateMu.Unlock()
		return
	}
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
			ReasonCode:                      string(account.LastReasonCode),
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
