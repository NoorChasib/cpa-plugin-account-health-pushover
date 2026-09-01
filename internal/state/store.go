package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/health"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

const CurrentVersion = 1

type Account struct {
	AccountKey                      string       `json:"account_key"`
	Provider                        string       `json:"provider"`
	AuthIndex                       string       `json:"auth_index"`
	Label                           string       `json:"label"`
	Identity                        string       `json:"identity,omitempty"`
	Health                          health.State `json:"health"`
	PreviousHealth                  health.State `json:"previous_health,omitempty"`
	FirstDetectedAt                 time.Time    `json:"first_detected_at,omitempty"`
	LastChangedAt                   time.Time    `json:"last_changed_at,omitempty"`
	LastAlertAt                     time.Time    `json:"last_alert_at,omitempty"`
	LastAlertAttemptAt              time.Time    `json:"last_alert_attempt_at,omitempty"`
	LastAlertAttemptGeneration      uint64       `json:"last_alert_attempt_generation,omitempty"`
	LastRecoveryAt                  time.Time    `json:"last_recovery_at,omitempty"`
	LastRecoveryAttemptAt           time.Time    `json:"last_recovery_attempt_at,omitempty"`
	LastReasonCode                  string       `json:"last_reason_code,omitempty"`
	AlertSent                       bool         `json:"alert_sent"`
	IncidentGeneration              uint64       `json:"incident_generation,omitempty"`
	RecoveryPendingFrom             health.State `json:"recovery_pending_from,omitempty"`
	SuspectSince                    time.Time    `json:"suspect_since,omitempty"`
	RemovedAt                       time.Time    `json:"removed_at,omitempty"`
	LastSuccessfulHealthObservation time.Time    `json:"last_successful_health_observation,omitempty"`
	LastObservedAt                  time.Time    `json:"last_observed_at,omitempty"`
	CPAStatus                       string       `json:"cpa_status,omitempty"`
	CPAUnavailable                  bool         `json:"cpa_unavailable"`
	QuotaLimited                    bool         `json:"quota_limited"`
}

type Data struct {
	Version             int                 `json:"version"`
	UpdatedAt           time.Time           `json:"updated_at"`
	Accounts            map[string]*Account `json:"accounts"`
	LastSuccessfulSend  time.Time           `json:"last_successful_send,omitempty"`
	LastNotificationErr string              `json:"last_notification_error,omitempty"`
}

type Store struct {
	Path string
}

func NewData() Data {
	return Data{Version: CurrentVersion, Accounts: make(map[string]*Account)}
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
	}
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
