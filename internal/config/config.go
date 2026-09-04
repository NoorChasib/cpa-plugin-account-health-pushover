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

	// DefaultDisplayTimezone is the zone used to render human-readable
	// timestamps on the HTML status view and in Pushover message bodies.
	DefaultDisplayTimezone = "UTC"

	// DisplayTimeLayout renders instants as "Tue Sep 1 2026 - 6:25:36 PM PDT".
	DisplayTimeLayout = DisplayDateLayout + " - " + DisplayClockLayout
	// DisplayDateLayout is the date half of DisplayTimeLayout.
	DisplayDateLayout = "Mon Jan 2 2006"
	// DisplayClockLayout is the clock half of DisplayTimeLayout.
	DisplayClockLayout = "3:04:05 PM MST"

	// DefaultQuotaPollInterval is how often provider usage endpoints are read
	// when quota alerts are enabled.
	DefaultQuotaPollInterval = 15 * time.Minute
	// MinQuotaPollInterval keeps the plugin from hammering provider usage
	// endpoints, which are undocumented and rate limited.
	MinQuotaPollInterval = time.Minute
	// DefaultQuotaWarningPercent is the used-percentage that triggers the
	// "almost out" warning: 95 used means 5 remaining.
	DefaultQuotaWarningPercent = 95.0
	// DefaultQuotaExhaustedPercent is the used-percentage that counts as
	// "ran out".
	DefaultQuotaExhaustedPercent = 100.0
	// DefaultQuotaHTTPTimeout bounds one provider usage request.
	DefaultQuotaHTTPTimeout = 15 * time.Second
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
	// QuotaAlerts enables weekly-quota polling and threshold notifications.
	QuotaAlerts bool
	// QuotaPollInterval is how often each account's provider usage endpoint is
	// read while quota alerts are enabled.
	QuotaPollInterval time.Duration
	// QuotaWarningPercent is the used-percentage at which one warning is sent
	// per weekly window (default 95, i.e. 5% remaining).
	QuotaWarningPercent float64
	// QuotaExhaustedPercent is the used-percentage that counts as "ran out"
	// (default 100).
	QuotaExhaustedPercent float64
	// QuotaNotificationPriority is the Pushover priority for quota messages.
	QuotaNotificationPriority int
	// QuotaHTTPTimeout bounds each provider usage request.
	QuotaHTTPTimeout time.Duration
	// DisplayTimezone is the validated IANA zone name, "UTC", or "Local". It is
	// presentation-only: JSON status stays RFC3339 UTC.
	DisplayTimezone string
	// DisplayTimezoneWarning is set when display-timezone was unknown and the
	// plugin fell back to UTC instead of rejecting the whole configuration.
	DisplayTimezoneWarning string
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
	DisplayTimezone            string   `yaml:"display-timezone"`
	QuotaAlerts                *bool    `yaml:"quota-alerts"`
	QuotaPollInterval          string   `yaml:"quota-poll-interval"`
	QuotaWarningPercent        *float64 `yaml:"quota-warning-percent"`
	QuotaExhaustedPercent      *float64 `yaml:"quota-exhausted-percent"`
	QuotaPriority              *int     `yaml:"quota-notification-priority"`
	QuotaHTTPTimeout           string   `yaml:"quota-http-timeout"`
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
		Providers:                  []string{"claude", "codex", "xai"},
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
		QuotaAlerts:                false,
		QuotaPollInterval:          DefaultQuotaPollInterval,
		QuotaWarningPercent:        DefaultQuotaWarningPercent,
		QuotaExhaustedPercent:      DefaultQuotaExhaustedPercent,
		QuotaNotificationPriority:  0,
		QuotaHTTPTimeout:           DefaultQuotaHTTPTimeout,
		DisplayTimezone:            DefaultDisplayTimezone,
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
	if raw.QuotaAlerts != nil {
		cfg.QuotaAlerts = *raw.QuotaAlerts
	}
	if cfg.QuotaPollInterval, err = parseDuration(raw.QuotaPollInterval, cfg.QuotaPollInterval, "quota-poll-interval"); err != nil {
		return Config{}, err
	}
	if cfg.QuotaHTTPTimeout, err = parseDuration(raw.QuotaHTTPTimeout, cfg.QuotaHTTPTimeout, "quota-http-timeout"); err != nil {
		return Config{}, err
	}
	if raw.QuotaWarningPercent != nil {
		cfg.QuotaWarningPercent = *raw.QuotaWarningPercent
	}
	if raw.QuotaExhaustedPercent != nil {
		cfg.QuotaExhaustedPercent = *raw.QuotaExhaustedPercent
	}
	if raw.QuotaPriority != nil {
		cfg.QuotaNotificationPriority = *raw.QuotaPriority
	}
	cfg.DisplayTimezone, cfg.DisplayTimezoneWarning = resolveDisplayTimezone(raw.DisplayTimezone)
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
		if !SupportedProvider(provider) {
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
	if c.QuotaNotificationPriority < -2 || c.QuotaNotificationPriority > 1 {
		return errors.New("quota-notification-priority must be between -2 and 1")
	}
	if c.QuotaPollInterval < MinQuotaPollInterval {
		return fmt.Errorf("quota-poll-interval must be at least %s", MinQuotaPollInterval)
	}
	if c.QuotaHTTPTimeout <= 0 || c.QuotaHTTPTimeout > time.Minute {
		return errors.New("quota-http-timeout must be between 1s and 1m")
	}
	if c.QuotaWarningPercent <= 0 || c.QuotaWarningPercent > 100 {
		return errors.New("quota-warning-percent must be between 1 and 100")
	}
	if c.QuotaExhaustedPercent <= 0 || c.QuotaExhaustedPercent > 100 {
		return errors.New("quota-exhausted-percent must be between 1 and 100")
	}
	if c.QuotaWarningPercent >= c.QuotaExhaustedPercent {
		return errors.New("quota-warning-percent must be lower than quota-exhausted-percent")
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

// resolveDisplayTimezone validates a display-timezone value. It accepts IANA
// names ("America/Los_Angeles"), "UTC", and "local" (the host process zone).
// Because the zone only affects presentation, an unknown name degrades to UTC
// with a status warning instead of rejecting the whole configuration.
func resolveDisplayTimezone(value string) (string, string) {
	trimmed := strings.TrimSpace(value)
	switch strings.ToLower(trimmed) {
	case "", "utc", "z":
		return DefaultDisplayTimezone, ""
	case "local":
		return "Local", ""
	}
	loc, err := time.LoadLocation(trimmed)
	if err != nil {
		return DefaultDisplayTimezone, fmt.Sprintf("display-timezone %q is not a known IANA zone; timestamps are shown in UTC", trimmed)
	}
	return loc.String(), ""
}

// DisplayLocation returns the *time.Location for DisplayTimezone, falling back
// to UTC when the name is empty or cannot be loaded.
func (c Config) DisplayLocation() *time.Location {
	return LoadDisplayLocation(c.DisplayTimezone)
}

// LoadDisplayLocation resolves a validated display zone name to a location,
// treating any failure as UTC so rendering never fails on presentation input.
func LoadDisplayLocation(name string) *time.Location {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "utc", "z":
		return time.UTC
	case "local":
		return time.Local
	}
	loc, err := time.LoadLocation(strings.TrimSpace(name))
	if err != nil {
		return time.UTC
	}
	return loc
}

// FormatDisplayTime renders an instant in the configured display zone using
// DisplayTimeLayout, or the fallback text for a zero time.
func (c Config) FormatDisplayTime(value time.Time, fallback string) string {
	if value.IsZero() {
		return fallback
	}
	return value.In(c.DisplayLocation()).Format(DisplayTimeLayout)
}

// SupportedProviders lists the CPA OAuth provider IDs this plugin can monitor.
// "xai" is CPA's provider ID for Grok Build OAuth accounts.
var SupportedProviders = []string{"claude", "codex", "xai"}

// SupportedProvider reports whether a normalized provider ID is monitorable.
func SupportedProvider(provider string) bool {
	return slices.Contains(SupportedProviders, strings.ToLower(strings.TrimSpace(provider)))
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
