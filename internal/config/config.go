package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultAppTokenEnv = "CPA_PUSHOVER_APP_TOKEN"
	DefaultUserKeyEnv  = "CPA_PUSHOVER_USER_KEY"
)

var (
	envNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	pushoverKeyExpr = regexp.MustCompile(`^[A-Za-z0-9]{30}$`)
)

type Config struct {
	Enabled                    bool
	Priority                   int
	Providers                  []string
	ScanInterval               time.Duration
	StartupGrace               time.Duration
	TransientConfirmAfter      time.Duration
	UnauthorizedConfirmAfter   time.Duration
	UsageRecheckDelay          time.Duration
	NotifyRecovery             bool
	NotifyDisabled             bool
	NotifyRemoved              bool
	ReminderInterval           time.Duration
	NotificationCoalesceWindow time.Duration
	RemovedStateRetention      time.Duration
	FailurePriority            int
	RecoveryPriority           int
	PushoverAppTokenEnv        string
	PushoverUserKeyEnv         string
	PushoverAppTokenFile       string
	PushoverUserKeyFile        string
	PushoverDevice             string
	ManagementURL              string
	StateFile                  string
	HTTPTimeout                time.Duration
	MaxConcurrentChecks        int
}

type rawConfig struct {
	Enabled                    *bool    `yaml:"enabled"`
	Priority                   int      `yaml:"priority"`
	Providers                  []string `yaml:"providers"`
	ScanInterval               string   `yaml:"scan-interval"`
	StartupGrace               string   `yaml:"startup-grace"`
	TransientConfirmAfter      string   `yaml:"transient-confirm-after"`
	UnauthorizedConfirmAfter   string   `yaml:"unauthorized-confirm-after"`
	UsageRecheckDelay          string   `yaml:"usage-recheck-delay"`
	NotifyRecovery             *bool    `yaml:"notify-recovery"`
	NotifyDisabled             *bool    `yaml:"notify-disabled"`
	NotifyRemoved              *bool    `yaml:"notify-removed"`
	ReminderInterval           string   `yaml:"reminder-interval"`
	NotificationCoalesceWindow string   `yaml:"notification-coalesce-window"`
	RemovedStateRetention      string   `yaml:"removed-state-retention"`
	FailurePriority            *int     `yaml:"failure-notification-priority"`
	RecoveryPriority           *int     `yaml:"recovery-notification-priority"`
	PushoverAppTokenEnv        string   `yaml:"pushover-app-token-env"`
	PushoverUserKeyEnv         string   `yaml:"pushover-user-key-env"`
	PushoverAppTokenFile       string   `yaml:"pushover-app-token-file"`
	PushoverUserKeyFile        string   `yaml:"pushover-user-key-file"`
	PushoverDevice             string   `yaml:"pushover-device"`
	ManagementURL              string   `yaml:"management-url"`
	StateFile                  string   `yaml:"state-file"`
	HTTPTimeout                string   `yaml:"pushover-http-timeout"`
	MaxConcurrentChecks        *int     `yaml:"max-concurrent-checks"`
}

type Credentials struct {
	AppToken string
	UserKey  string
	Device   string
}

type CredentialStatus struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

func Default() Config {
	return Config{
		Enabled:                    true,
		Providers:                  []string{"claude", "codex"},
		ScanInterval:               time.Minute,
		StartupGrace:               30 * time.Second,
		TransientConfirmAfter:      10 * time.Minute,
		UnauthorizedConfirmAfter:   time.Minute,
		UsageRecheckDelay:          10 * time.Second,
		NotifyRecovery:             true,
		NotifyDisabled:             false,
		NotifyRemoved:              false,
		ReminderInterval:           12 * time.Hour,
		NotificationCoalesceWindow: 5 * time.Second,
		RemovedStateRetention:      7 * 24 * time.Hour,
		FailurePriority:            1,
		RecoveryPriority:           0,
		PushoverAppTokenEnv:        DefaultAppTokenEnv,
		PushoverUserKeyEnv:         DefaultUserKeyEnv,
		HTTPTimeout:                10 * time.Second,
		MaxConcurrentChecks:        4,
	}
}

func Parse(data []byte) (Config, error) {
	cfg := Default()
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse plugin config: %w", err)
	}
	if raw.Enabled != nil {
		cfg.Enabled = *raw.Enabled
	}
	cfg.Priority = raw.Priority
	if len(raw.Providers) > 0 {
		cfg.Providers = raw.Providers
	}
	var err error
	if cfg.ScanInterval, err = parseDuration(raw.ScanInterval, cfg.ScanInterval, "scan-interval"); err != nil {
		return Config{}, err
	}
	if cfg.StartupGrace, err = parseDuration(raw.StartupGrace, cfg.StartupGrace, "startup-grace"); err != nil {
		return Config{}, err
	}
	if cfg.TransientConfirmAfter, err = parseDuration(raw.TransientConfirmAfter, cfg.TransientConfirmAfter, "transient-confirm-after"); err != nil {
		return Config{}, err
	}
	if cfg.UnauthorizedConfirmAfter, err = parseDuration(raw.UnauthorizedConfirmAfter, cfg.UnauthorizedConfirmAfter, "unauthorized-confirm-after"); err != nil {
		return Config{}, err
	}
	if cfg.UsageRecheckDelay, err = parseDuration(raw.UsageRecheckDelay, cfg.UsageRecheckDelay, "usage-recheck-delay"); err != nil {
		return Config{}, err
	}
	if cfg.ReminderInterval, err = parseDuration(raw.ReminderInterval, cfg.ReminderInterval, "reminder-interval"); err != nil {
		return Config{}, err
	}
	if cfg.NotificationCoalesceWindow, err = parseDuration(raw.NotificationCoalesceWindow, cfg.NotificationCoalesceWindow, "notification-coalesce-window"); err != nil {
		return Config{}, err
	}
	if cfg.RemovedStateRetention, err = parseDuration(raw.RemovedStateRetention, cfg.RemovedStateRetention, "removed-state-retention"); err != nil {
		return Config{}, err
	}
	if cfg.HTTPTimeout, err = parseDuration(raw.HTTPTimeout, cfg.HTTPTimeout, "pushover-http-timeout"); err != nil {
		return Config{}, err
	}
	if raw.NotifyRecovery != nil {
		cfg.NotifyRecovery = *raw.NotifyRecovery
	}
	if raw.NotifyDisabled != nil {
		cfg.NotifyDisabled = *raw.NotifyDisabled
	}
	if raw.NotifyRemoved != nil {
		cfg.NotifyRemoved = *raw.NotifyRemoved
	}
	if raw.FailurePriority != nil {
		cfg.FailurePriority = *raw.FailurePriority
	}
	if raw.RecoveryPriority != nil {
		cfg.RecoveryPriority = *raw.RecoveryPriority
	}
	if strings.TrimSpace(raw.PushoverAppTokenEnv) != "" {
		cfg.PushoverAppTokenEnv = strings.TrimSpace(raw.PushoverAppTokenEnv)
	}
	if strings.TrimSpace(raw.PushoverUserKeyEnv) != "" {
		cfg.PushoverUserKeyEnv = strings.TrimSpace(raw.PushoverUserKeyEnv)
	}
	cfg.PushoverAppTokenFile = strings.TrimSpace(raw.PushoverAppTokenFile)
	cfg.PushoverUserKeyFile = strings.TrimSpace(raw.PushoverUserKeyFile)
	cfg.PushoverDevice = strings.TrimSpace(raw.PushoverDevice)
	cfg.ManagementURL = strings.TrimSpace(raw.ManagementURL)
	cfg.StateFile = strings.TrimSpace(raw.StateFile)
	if raw.MaxConcurrentChecks != nil {
		cfg.MaxConcurrentChecks = *raw.MaxConcurrentChecks
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return errors.New("providers must contain at least one provider")
	}
	seen := make(map[string]struct{}, len(c.Providers))
	for _, provider := range c.Providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			return errors.New("providers must not contain an empty value")
		}
		if provider != "claude" && provider != "codex" {
			return fmt.Errorf("unsupported provider %q", provider)
		}
		seen[provider] = struct{}{}
	}
	if c.ScanInterval <= 0 {
		return errors.New("scan-interval must be greater than zero")
	}
	if c.StartupGrace < 0 || c.TransientConfirmAfter < 0 || c.ReminderInterval < 0 || c.NotificationCoalesceWindow < 0 || c.RemovedStateRetention < 0 {
		return errors.New("duration settings must not be negative")
	}
	if c.UsageRecheckDelay < time.Second {
		return errors.New("usage-recheck-delay must be at least 1s")
	}
	if c.UsageRecheckDelay > c.ScanInterval {
		return errors.New("usage-recheck-delay must not exceed scan-interval")
	}
	if c.UnauthorizedConfirmAfter < time.Second {
		return errors.New("unauthorized-confirm-after must be at least 1s")
	}
	if c.HTTPTimeout <= 0 {
		return errors.New("pushover-http-timeout must be greater than zero")
	}
	if c.MaxConcurrentChecks < 1 || c.MaxConcurrentChecks > 32 {
		return errors.New("max-concurrent-checks must be between 1 and 32")
	}
	if c.FailurePriority < -2 || c.FailurePriority > 1 {
		return errors.New("failure-notification-priority must be between -2 and 1")
	}
	if c.RecoveryPriority < -2 || c.RecoveryPriority > 1 {
		return errors.New("recovery-notification-priority must be between -2 and 1")
	}
	if !envNamePattern.MatchString(c.PushoverAppTokenEnv) || !envNamePattern.MatchString(c.PushoverUserKeyEnv) {
		return errors.New("Pushover environment variable names are invalid")
	}
	if len(c.PushoverDevice) > 25 {
		return errors.New("pushover-device exceeds 25 characters")
	}
	if c.ManagementURL != "" {
		u, err := url.Parse(c.ManagementURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return errors.New("management-url must be an absolute HTTP(S) URL without embedded credentials")
		}
	}
	c.Providers = slices.Sorted(maps.Keys(seen))
	return nil
}

func (c Config) ProviderEnabled(provider string) bool {
	return slices.Contains(c.Providers, strings.ToLower(strings.TrimSpace(provider)))
}

func (c Config) ResolveCredentials(getenv func(string) string) (Credentials, CredentialStatus) {
	if getenv == nil {
		getenv = os.Getenv
	}
	appToken, appErr := secretValue(c.PushoverAppTokenEnv, c.PushoverAppTokenFile, getenv)
	userKey, userErr := secretValue(c.PushoverUserKeyEnv, c.PushoverUserKeyFile, getenv)
	if appErr != nil || userErr != nil {
		return Credentials{}, CredentialStatus{State: "invalid", Error: "configured Pushover secret file could not be read"}
	}
	if appToken == "" || userKey == "" {
		return Credentials{}, CredentialStatus{State: "missing", Error: "Pushover credentials are not configured"}
	}
	if !pushoverKeyExpr.MatchString(appToken) || !pushoverKeyExpr.MatchString(userKey) {
		return Credentials{}, CredentialStatus{State: "invalid", Error: "Pushover credentials have an invalid format"}
	}
	return Credentials{AppToken: appToken, UserKey: userKey, Device: c.PushoverDevice}, CredentialStatus{State: "configured"}
}

func parseDuration(raw string, fallback time.Duration, name string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}

func secretValue(envName, fileName string, getenv func(string) string) (string, error) {
	if value := strings.TrimSpace(getenv(envName)); value != "" {
		return value, nil
	}
	if fileName == "" {
		return "", nil
	}
	info, err := os.Stat(fileName)
	if err != nil {
		return "", err
	}
	if info.Size() > 64*1024 {
		return "", errors.New("secret file is too large")
	}
	data, err := os.ReadFile(fileName)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
