package monitor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/notifier"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/quota"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/state"
)

// QuotaHost is the optional host surface required for weekly-quota polling.
// It is a separate interface so hosts (and test fakes) that only support
// health monitoring keep working; quota alerts are then reported as
// unavailable rather than failing the plugin.
type QuotaHost interface {
	// GetAuth returns the raw physical credential JSON. The document contains
	// OAuth tokens and is passed straight to the quota package, which decodes
	// only the fields needed for one usage request.
	GetAuth(context.Context, string) ([]byte, error)
	HTTPDo(context.Context, protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error)
}

const (
	quotaWarningKind   = "quota_warning"
	quotaExhaustedKind = "quota_exhausted"
	quotaHostWarning   = "quota-alerts is enabled but the host does not expose host.auth.get/host.http.do; weekly quota polling is disabled"
)

// QuotaStatus is the account-level quota view in status JSON.
type QuotaStatus struct {
	Percent         *float64  `json:"percent,omitempty"`
	ResetAt         time.Time `json:"reset_at,omitempty"`
	ObservedAt      time.Time `json:"observed_at,omitempty"`
	LastPollAt      time.Time `json:"last_poll_at,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	WarningSentAt   time.Time `json:"warning_sent_at,omitempty"`
	ExhaustedSentAt time.Time `json:"exhausted_sent_at,omitempty"`
}

func quotaStatusOf(account *state.Account) *QuotaStatus {
	q := account.Quota
	if q.ObservedAt.IsZero() && q.LastPollAt.IsZero() {
		return nil
	}
	status := &QuotaStatus{
		ResetAt:         q.ResetAt,
		ObservedAt:      q.ObservedAt,
		LastPollAt:      q.LastPollAt,
		LastError:       q.LastError,
		WarningSentAt:   q.WarningSentAt,
		ExhaustedSentAt: q.ExhaustedSentAt,
	}
	if !q.ObservedAt.IsZero() {
		percent := q.Percent
		status.Percent = &percent
	}
	return status
}

// quotaLoop polls provider usage endpoints on the configured interval. It
// starts after the baseline health reconciliation so every account row
// already exists, then wakes on the ticker or on an explicit request.
func (m *Monitor) quotaLoop() {
	defer m.loopWG.Done()
	select {
	case <-m.ctx.Done():
		return
	case <-m.baselined:
	}
	// Publish the schedule before the first poll so a snapshot taken during
	// the startup read already shows when the next one is due.
	m.setNextQuotaPoll(m.now().Add(m.cfg.QuotaPollInterval))
	_ = m.PollQuota(m.ctx, "startup")
	ticker := time.NewTicker(m.cfg.QuotaPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			_ = m.PollQuota(m.ctx, "periodic")
			m.setNextQuotaPoll(m.now().Add(m.cfg.QuotaPollInterval))
		case <-m.quotaSignals:
			_ = m.PollQuota(m.ctx, "management")
		}
	}
}

// RequestQuotaPoll asks the quota loop to poll soon without blocking the
// caller. It is a no-op when quota alerts are disabled.
func (m *Monitor) RequestQuotaPoll() {
	if m.quotaHost == nil || !m.cfg.QuotaAlerts {
		return
	}
	select {
	case m.quotaSignals <- struct{}{}:
	default:
	}
}

func (m *Monitor) setNextQuotaPoll(next time.Time) {
	m.stateMu.Lock()
	if !m.retiring.Load() && !m.retired {
		m.status.NextQuotaPoll = next.UTC()
	}
	m.stateMu.Unlock()
}

// PollQuota reads every supported account's weekly window once and applies
// threshold latches. Provider errors are recorded per account and never abort
// the pass; a roster failure is recorded on the status snapshot.
func (m *Monitor) PollQuota(ctx context.Context, trigger string) error {
	if m.quotaHost == nil || !m.cfg.QuotaAlerts {
		return nil
	}
	if !m.beginOperation() {
		return ErrStopping
	}
	defer m.operations.Done()
	if m.isRetired() {
		return ErrStopping
	}
	now := m.now().UTC()
	roster, err := m.host.ListAuth(ctx)
	if err != nil {
		if m.isRetired() {
			return ErrStopping
		}
		m.recordQuotaPollFailure(now, "host.auth.list failed; previous quota state was retained")
		return err
	}
	candidates := health.Discover(roster, m.cfg)

	m.stateMu.RLock()
	loaded := m.stateLoaded
	targets := make([]protocol.HostAuthFileEntry, 0, len(candidates))
	for _, candidate := range candidates {
		if !quota.Supported(candidate.Provider) {
			continue
		}
		if _, ok := m.data.Accounts[health.AccountKey(candidate.Provider, candidate.AuthIndex)]; ok {
			targets = append(targets, candidate)
		}
	}
	m.stateMu.RUnlock()
	if !loaded {
		return nil
	}

	type result struct {
		entry       protocol.HostAuthFileEntry
		observation quota.Observation
		err         error
	}
	results := make(chan result, len(targets))
	semaphore := make(chan struct{}, m.cfg.MaxConcurrentChecks)
	var wg sync.WaitGroup
	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				results <- result{entry: target, err: ctx.Err()}
				return
			}
			defer func() { <-semaphore }()
			observation, fetchErr := m.fetchQuota(ctx, target, now)
			results <- result{entry: target, observation: observation, err: fetchErr}
		}()
	}
	wg.Wait()
	close(results)
	if m.isRetired() {
		return ErrStopping
	}
	items := make([]result, 0, len(targets))
	for item := range results {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return health.AccountKey(items[i].entry.Provider, items[i].entry.AuthIndex) < health.AccountKey(items[j].entry.Provider, items[j].entry.AuthIndex)
	})

	m.stateMu.Lock()
	if m.retiring.Load() || m.retired {
		m.stateMu.Unlock()
		return ErrStopping
	}
	failed := 0
	for _, item := range items {
		account := m.data.Accounts[health.AccountKey(item.entry.Provider, item.entry.AuthIndex)]
		if account == nil {
			continue
		}
		account.Quota.LastPollAt = now
		if item.err != nil {
			failed++
			account.Quota.LastError = sanitizeQuotaError(item.err)
			continue
		}
		account.Quota.LastError = ""
		m.applyQuotaLocked(account, item.observation, now)
	}
	persistErr := m.persistLocked()
	m.status.LastQuotaPoll = now
	m.status.Accounts = accountStatuses(m.data, m.cfg.ReminderInterval)
	if failed > 0 {
		m.status.LastQuotaPollError = fmt.Sprintf("%d of %d usage reads failed; see account rows", failed, len(items))
	} else {
		m.status.LastQuotaPollError = ""
	}
	m.stateMu.Unlock()
	if persistErr != nil {
		m.host.Log(ctx, "warn", "account-health quota state persistence failed", map[string]any{"trigger": safeField(trigger)})
	}
	if failed > 0 {
		m.host.Log(ctx, "warn", "one or more weekly quota reads failed", map[string]any{"trigger": safeField(trigger), "failed": failed})
	}
	return persistErr
}

// fetchQuota reads one account's usage window, bounding the wait on the
// synchronous host callbacks with the configured timeout. A callback that
// outlives the timeout keeps its native admission and drains at shutdown.
func (m *Monitor) fetchQuota(ctx context.Context, entry protocol.HostAuthFileEntry, now time.Time) (quota.Observation, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, m.cfg.QuotaHTTPTimeout)
	defer cancel()
	type outcome struct {
		observation quota.Observation
		err         error
	}
	done := make(chan outcome, 1)
	go func() {
		raw, err := m.quotaHost.GetAuth(timeoutCtx, entry.AuthIndex)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		observation, err := quota.Fetch(timeoutCtx, m.quotaHost, entry.Provider, raw, now)
		done <- outcome{observation: observation, err: err}
	}()
	select {
	case result := <-done:
		return result.observation, result.err
	case <-timeoutCtx.Done():
		return quota.Observation{}, timeoutCtx.Err()
	}
}

// applyQuotaLocked records an observation and enqueues threshold
// notifications. Caller holds stateMu.
func (m *Monitor) applyQuotaLocked(account *state.Account, observation quota.Observation, now time.Time) {
	q := &account.Quota
	windowKey := quotaWindowKey(observation)
	previousPercent := q.Percent
	hadObservation := !q.ObservedAt.IsZero()
	q.Percent = observation.Percent
	q.ResetAt = observation.ResetAt
	q.ObservedAt = now

	// A new window (different reset instant, or a large drop in usage when the
	// provider reports no reset instant) clears the latches so the next window
	// alerts again.
	newWindow := q.WindowKey != windowKey
	if !newWindow && hadObservation && observation.Percent < m.cfg.QuotaWarningPercent && previousPercent >= m.cfg.QuotaWarningPercent {
		newWindow = true
	}
	if newWindow {
		q.WindowKey = windowKey
		q.WarningSentAt = time.Time{}
		q.ExhaustedSentAt = time.Time{}
		q.WarningAttemptAt = time.Time{}
		q.ExhaustedAttemptAt = time.Time{}
	}

	switch {
	case observation.Percent >= m.cfg.QuotaExhaustedPercent:
		if q.ExhaustedSentAt.IsZero() && due(q.ExhaustedAttemptAt, notificationRetryAfter, now) {
			if m.enqueueQuotaLocked(account, quotaExhaustedKind, windowKey, now) {
				q.ExhaustedAttemptAt = now
			}
		}
	case observation.Percent >= m.cfg.QuotaWarningPercent:
		if q.WarningSentAt.IsZero() && q.ExhaustedSentAt.IsZero() && due(q.WarningAttemptAt, notificationRetryAfter, now) {
			if m.enqueueQuotaLocked(account, quotaWarningKind, windowKey, now) {
				q.WarningAttemptAt = now
			}
		}
	}
}

func quotaWindowKey(observation quota.Observation) string {
	if observation.ResetAt.IsZero() {
		return ""
	}
	return observation.ResetAt.UTC().Format(time.RFC3339)
}

func (m *Monitor) enqueueQuotaLocked(account *state.Account, kind, windowKey string, now time.Time) bool {
	if m.retiring.Load() || m.retired {
		return false
	}
	message := m.quotaMessage(account, kind)
	job := notifier.Job{
		Message: message,
		Valid: func() bool {
			m.stateMu.RLock()
			defer m.stateMu.RUnlock()
			if !m.accountCurrentLocked(account) || account.Quota.WindowKey != windowKey {
				return false
			}
			if kind == quotaExhaustedKind {
				return account.Quota.ExhaustedSentAt.IsZero()
			}
			return account.Quota.WarningSentAt.IsZero() && account.Quota.ExhaustedSentAt.IsZero()
		},
		Callback: m.quotaDeliveryCallback(account, kind, windowKey, now),
	}
	if !m.dispatcher.Enqueue(job) {
		m.data.LastNotificationErr = "notification queue is unavailable"
		return false
	}
	return true
}

func (m *Monitor) quotaDeliveryCallback(account *state.Account, kind, windowKey string, attemptAt time.Time) func(notifier.DeliveryResult) {
	return func(result notifier.DeliveryResult) {
		m.stateMu.Lock()
		defer m.stateMu.Unlock()
		if m.retiring.Load() || m.retired {
			return
		}
		current := m.accountCurrentLocked(account) && account.Quota.WindowKey == windowKey
		q := &account.Quota
		switch {
		case result.Unattempted:
			if current {
				if kind == quotaExhaustedKind && q.ExhaustedAttemptAt.Equal(attemptAt) {
					q.ExhaustedAttemptAt = time.Time{}
				} else if kind == quotaWarningKind && q.WarningAttemptAt.Equal(attemptAt) {
					q.WarningAttemptAt = time.Time{}
				}
			}
			return
		case result.Accepted:
			m.data.LastSuccessfulSend = result.At
			m.data.LastNotificationErr = ""
			if current {
				if kind == quotaExhaustedKind {
					q.ExhaustedSentAt = result.At
				} else {
					q.WarningSentAt = result.At
				}
			}
		default:
			m.data.LastNotificationErr = result.Error
		}
		_ = m.persistLocked()
		m.status.Accounts = accountStatuses(m.data, m.cfg.ReminderInterval)
	}
}

func (m *Monitor) quotaMessage(account *state.Account, kind string) notifier.Message {
	provider := titleProvider(account.Provider)
	percent := account.Quota.Percent
	remaining := 100 - percent
	if remaining < 0 {
		remaining = 0
	}
	reset := "unknown"
	if !account.Quota.ResetAt.IsZero() {
		reset = m.formatTime(account.Quota.ResetAt)
	}
	var title, body string
	if kind == quotaExhaustedKind {
		title = fmt.Sprintf("CLIProxyAPI: %s weekly limit reached", provider)
		body = fmt.Sprintf("%s has used %s of its weekly limit.\nResets: %s\nOther healthy accounts will continue routing if available.", account.Label, formatPercent(percent), reset)
	} else {
		title = fmt.Sprintf("CLIProxyAPI: %s weekly limit almost used", provider)
		body = fmt.Sprintf("%s has used %s of its weekly limit (%s remaining).\nResets: %s", account.Label, formatPercent(percent), formatPercent(remaining), reset)
	}
	if m.cfg.ManagementURL != "" {
		body += "\nManagement: " + m.cfg.ManagementURL
	}
	return notifier.Message{
		Kind:       kind,
		Title:      title,
		Body:       body,
		Priority:   m.cfg.QuotaNotificationPriority,
		Provider:   provider,
		AccountKey: account.AccountKey,
		Label:      account.Label,
		Reason:     kind,
	}
}

func formatPercent(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d%%", int64(value))
	}
	return fmt.Sprintf("%.1f%%", value)
}

func (m *Monitor) recordQuotaPollFailure(now time.Time, message string) {
	m.stateMu.Lock()
	if !m.retiring.Load() && !m.retired {
		m.status.LastQuotaPoll = now
		m.status.LastQuotaPollError = message
	}
	m.stateMu.Unlock()
}

// sanitizeQuotaError maps provider/host errors to closed, static text. The
// quota package already returns static errors; host callback and context
// errors are reduced here so no upstream or filesystem detail reaches status.
func sanitizeQuotaError(err error) string {
	var statusErr quota.HTTPStatusError
	switch {
	case errors.As(err, &statusErr):
		switch {
		case statusErr.StatusCode == 401 || statusErr.StatusCode == 403:
			return fmt.Sprintf("usage endpoint rejected the credential (HTTP %d)", statusErr.StatusCode)
		case statusErr.StatusCode == 429:
			return "usage endpoint rate limited the poll (HTTP 429)"
		case statusErr.StatusCode >= 500:
			return fmt.Sprintf("usage endpoint unavailable (HTTP %d)", statusErr.StatusCode)
		default:
			return statusErr.Error()
		}
	case errors.Is(err, context.DeadlineExceeded):
		return "usage request timed out"
	case errors.Is(err, context.Canceled), errors.Is(err, ErrStopping):
		return "usage request cancelled"
	case errors.Is(err, quota.ErrUnsupportedProvider), errors.Is(err, quota.ErrNoAccessToken), errors.Is(err, quota.ErrNoAccountID), errors.Is(err, quota.ErrInvalidResponse), errors.Is(err, quota.ErrNoWeeklyWindow):
		return err.Error()
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "host.auth.get"):
		return "host.auth.get failed"
	case strings.Contains(text, "host.http.do"):
		return "host.http.do failed"
	case strings.Contains(text, "shutting down"):
		return "usage request cancelled"
	}
	return "usage request failed"
}
