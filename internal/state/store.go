package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

const CurrentVersion = 1

var ErrWriterSuperseded = errors.New("state writer was superseded")

type SuppressionKind string

const (
	AlertSuppression    SuppressionKind = "alert"
	RecoverySuppression SuppressionKind = "recovery"
)

type SuppressionRollback struct {
	AccountKey string
	Identity   string
	Kind       SuppressionKind
	Generation uint64
	AttemptAt  time.Time
}

type suppressionRollbackSet struct {
	mu      sync.Mutex
	entries map[SuppressionRollback]struct{}
}

type writerLease struct {
	mu    sync.Mutex
	owner uint64
}

var (
	writerLeases         sync.Map
	writerGeneration     atomic.Uint64
	suppressionRollbacks sync.Map
)

func canonicalStatePath(path string) string {
	cleaned := filepath.Clean(path)
	if absolute, err := filepath.Abs(cleaned); err == nil {
		cleaned = absolute
	}

	// The state directory may not exist before the first save. Resolve the
	// nearest existing ancestor, then append the missing path components so
	// symlinked directory aliases share one process-local writer authority.
	// Resolve directory aliases only. Save atomically renames onto Path, so a
	// symlink in the final filename is intentionally replaced rather than written
	// through; treating that leaf as its target would change the lease key after
	// the first save.
	current := filepath.Dir(cleaned)
	missing := []string{filepath.Base(cleaned)}
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleaned
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func leaseFor(path string) *writerLease {
	path = canonicalStatePath(path)
	lease, _ := writerLeases.LoadOrStore(path, &writerLease{})
	return lease.(*writerLease)
}

func rollbackSetFor(path string) *suppressionRollbackSet {
	path = canonicalStatePath(path)
	set, _ := suppressionRollbacks.LoadOrStore(path, &suppressionRollbackSet{entries: make(map[SuppressionRollback]struct{})})
	return set.(*suppressionRollbackSet)
}

// RegisterSuppressionRollback records an exact, process-local rollback before a
// monitor relinquishes its writer lease. Store.Load and Store.Save both apply
// registered rollbacks, so a replacement cannot inherit a five-minute marker
// for accepted work that was never attempted even if old filesystem I/O is
// still unwinding.
func RegisterSuppressionRollback(path string, rollback SuppressionRollback) {
	if strings.TrimSpace(path) == "" || rollback.AttemptAt.IsZero() || rollback.Generation == 0 {
		return
	}
	set := rollbackSetFor(path)
	set.mu.Lock()
	cutoff := time.Now().Add(-24 * time.Hour)
	for existing := range set.entries {
		if existing.AttemptAt.Before(cutoff) {
			delete(set.entries, existing)
		}
	}
	set.entries[rollback] = struct{}{}
	set.mu.Unlock()
}

func applySuppressionRollbacks(path string, data *Data) {
	if data == nil || strings.TrimSpace(path) == "" || len(data.Accounts) == 0 {
		return
	}
	loaded, ok := suppressionRollbacks.Load(canonicalStatePath(path))
	if !ok {
		return
	}
	set := loaded.(*suppressionRollbackSet)
	set.mu.Lock()
	defer set.mu.Unlock()
	for rollback := range set.entries {
		account := data.Accounts[rollback.AccountKey]
		if account != nil && rollback.Identity != "" && account.Identity != rollback.Identity {
			account = nil
		}
		if account == nil && rollback.Identity != "" {
			var match *Account
			for _, candidate := range data.Accounts {
				if candidate == nil || candidate.Identity != rollback.Identity {
					continue
				}
				if match != nil {
					match = nil
					break
				}
				match = candidate
			}
			account = match
		}
		if account == nil || account.IncidentGeneration != rollback.Generation {
			continue
		}
		switch rollback.Kind {
		case AlertSuppression:
			if account.LastAlertAttemptGeneration == rollback.Generation && account.LastAlertAttemptAt.Equal(rollback.AttemptAt) {
				account.LastAlertAttemptAt = time.Time{}
				account.LastAlertAttemptGeneration = 0
			}
		case RecoverySuppression:
			if account.LastRecoveryAttemptAt.Equal(rollback.AttemptAt) {
				account.LastRecoveryAttemptAt = time.Time{}
			}
		}
	}
}

type Account struct {
	AccountKey                      string                   `json:"account_key"`
	Provider                        string                   `json:"provider"`
	AuthIndex                       string                   `json:"auth_index"`
	Label                           string                   `json:"label"`
	Identity                        string                   `json:"identity,omitempty"`
	Health                          health.State             `json:"health"`
	PreviousHealth                  health.State             `json:"previous_health,omitempty"`
	FirstDetectedAt                 time.Time                `json:"first_detected_at,omitempty"`
	LastChangedAt                   time.Time                `json:"last_changed_at,omitempty"`
	LastAlertAt                     time.Time                `json:"last_alert_at,omitempty"`
	LastAlertAttemptAt              time.Time                `json:"last_alert_attempt_at,omitempty"`
	LastAlertAttemptGeneration      uint64                   `json:"last_alert_attempt_generation,omitempty"`
	LastRecoveryAt                  time.Time                `json:"last_recovery_at,omitempty"`
	LastRecoveryAttemptAt           time.Time                `json:"last_recovery_attempt_at,omitempty"`
	LastReasonCode                  health.ReasonCode        `json:"last_reason_code,omitempty"`
	AlertSent                       bool                     `json:"alert_sent"`
	IncidentGeneration              uint64                   `json:"incident_generation,omitempty"`
	RecoveryPendingFrom             health.State             `json:"recovery_pending_from,omitempty"`
	SuspectSince                    time.Time                `json:"suspect_since,omitempty"`
	SuspectClass                    health.ConfirmationClass `json:"suspect_class,omitempty"`
	SuspectTarget                   health.State             `json:"suspect_target,omitempty"`
	SuspectReason                   health.ReasonCode        `json:"suspect_reason,omitempty"`
	RemovedAt                       time.Time                `json:"removed_at,omitempty"`
	LastSuccessfulHealthObservation time.Time                `json:"last_successful_health_observation,omitempty"`
	LastObservedAt                  time.Time                `json:"last_observed_at,omitempty"`
	CPAStatus                       string                   `json:"cpa_status,omitempty"`
	CPAUnavailable                  bool                     `json:"cpa_unavailable"`
	QuotaLimited                    bool                     `json:"quota_limited"`
}

// ClearSuspect resets confirmation metadata when an account is not carrying a
// valid typed suspicion.
func (a *Account) ClearSuspect() {
	a.SuspectSince = time.Time{}
	a.SuspectClass = health.ConfirmationNone
	a.SuspectTarget = health.Unknown
	a.SuspectReason = health.ReasonNone
}

type Data struct {
	Version             int                 `json:"version"`
	UpdatedAt           time.Time           `json:"updated_at"`
	Accounts            map[string]*Account `json:"accounts"`
	LastSuccessfulSend  time.Time           `json:"last_successful_send,omitempty"`
	LastNotificationErr string              `json:"last_notification_error,omitempty"`
}

type Store struct {
	Path         string
	writerToken  uint64
	beforeClaim  func()
	beforeCommit func()
}

func (s *Store) Claim() error {
	return s.ClaimContext(context.Background())
}

func (s *Store) ClaimContext(ctx context.Context) error {
	if strings.TrimSpace(s.Path) == "" {
		return errors.New("state file path is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.beforeClaim != nil {
		s.beforeClaim()
	}
	lease := leaseFor(s.Path)
	for !lease.mu.TryLock() {
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer lease.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	token := writerGeneration.Add(1)
	if token == 0 {
		token = writerGeneration.Add(1)
	}
	lease.owner = token
	s.writerToken = token
	return nil
}

func (s *Store) Release() {
	if s == nil || strings.TrimSpace(s.Path) == "" || s.writerToken == 0 {
		return
	}
	lease := leaseFor(s.Path)
	lease.mu.Lock()
	if lease.owner == s.writerToken {
		lease.owner = 0
	}
	lease.mu.Unlock()
}

func NewData() Data {
	return Data{Version: CurrentVersion, Accounts: make(map[string]*Account)}
}

func Clone(data Data) Data {
	clone := data
	clone.Accounts = make(map[string]*Account, len(data.Accounts))
	for key, account := range data.Accounts {
		if account == nil {
			clone.Accounts[key] = nil
			continue
		}
		accountClone := *account
		clone.Accounts[key] = &accountClone
	}
	return clone
}

func (s Store) Load() (Data, error) {
	data := NewData()
	if strings.TrimSpace(s.Path) == "" {
		return data, errors.New("state file path is not configured")
	}
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return data, nil
	}
	if err != nil {
		return data, fmt.Errorf("read state file: %w", err)
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return NewData(), fmt.Errorf("decode state file: %w", err)
	}
	if data.Version != CurrentVersion {
		return NewData(), fmt.Errorf("unsupported state version %d", data.Version)
	}
	if data.Accounts == nil {
		data.Accounts = make(map[string]*Account)
	}
	for key, account := range data.Accounts {
		if account == nil {
			delete(data.Accounts, key)
			continue
		}
		account.AccountKey = key
		account.LastReasonCode = health.NormalizeReasonCode(string(account.LastReasonCode))
		if account.Health == health.Suspect {
			validClass := account.SuspectClass == health.ConfirmationTransient || account.SuspectClass == health.ConfirmationUnauthorized
			validTarget := account.SuspectTarget == health.CredentialDown || account.SuspectTarget == health.ReauthRequired
			if !validClass || !validTarget || account.SuspectReason == health.ReasonNone {
				account.ClearSuspect()
			} else {
				account.SuspectReason = health.NormalizeReasonCode(string(account.SuspectReason))
			}
		} else {
			account.ClearSuspect()
		}
	}
	if data.LastNotificationErr != "" {
		data.LastNotificationErr = "previous notification delivery failed"
	}
	applySuppressionRollbacks(s.Path, &data)
	return data, nil
}

func (s Store) Save(data Data) error {
	if strings.TrimSpace(s.Path) == "" {
		return errors.New("state file path is not configured")
	}
	data.Version = CurrentVersion
	data.UpdatedAt = time.Now().UTC()
	if data.Accounts == nil {
		data.Accounts = make(map[string]*Account)
	}
	applySuppressionRollbacks(s.Path, &data)
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state file: %w", err)
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("set state permissions: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		cleanup()
		return fmt.Errorf("write temporary state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temporary state file: %w", err)
	}
	lease := leaseFor(s.Path)
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if (s.writerToken == 0 && lease.owner != 0) || (s.writerToken != 0 && lease.owner != s.writerToken) {
		_ = os.Remove(tmpName)
		return ErrWriterSuperseded
	}
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace state file: %w", err)
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

func ResolvePath(configured string, roster []protocol.HostAuthFileEntry) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		if filepath.IsAbs(configured) {
			return configured
		}
		absolute, err := filepath.Abs(configured)
		if err == nil {
			return absolute
		}
		return configured
	}
	for _, entry := range roster {
		if strings.TrimSpace(entry.Path) != "" {
			return filepath.Join(filepath.Dir(entry.Path), ".plugin-state", "account-health-pushover", "state.json")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".plugin-state", "account-health-pushover", "state.json")
	}
	return filepath.Join(home, ".cli-proxy-api", ".plugin-state", "account-health-pushover", "state.json")
}

func PruneRemoved(data *Data, now time.Time, retention time.Duration) int {
	if data == nil || retention < 0 {
		return 0
	}
	pruned := 0
	for key, account := range data.Accounts {
		if account == nil || account.Health != health.Removed || account.RemovedAt.IsZero() {
			continue
		}
		if retention == 0 || !account.RemovedAt.Add(retention).After(now) {
			delete(data.Accounts, key)
			pruned++
		}
	}
	return pruned
}
