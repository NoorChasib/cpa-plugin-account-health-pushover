package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScanInterval != time.Minute || cfg.StartupGrace != 30*time.Second || cfg.TransientConfirmAfter != 10*time.Minute {
		t.Fatalf("unexpected default durations: %+v", cfg)
	}
	if !cfg.NotifyRecovery || cfg.NotifyDisabled || cfg.NotifyRemoved {
		t.Fatalf("unexpected notification defaults: %+v", cfg)
	}
	if strings.Join(cfg.Providers, ",") != "claude,codex" {
		t.Fatalf("providers = %v", cfg.Providers)
	}
}

func TestParseCompleteConfig(t *testing.T) {
	raw := []byte(`enabled: true
priority: 20
providers: [Codex, claude, codex]
scan-interval: 2m
startup-grace: 5s
transient-confirm-after: 15m
notify-recovery: false
notify-disabled: true
notify-removed: true
reminder-interval: 0
notification-coalesce-window: 2s
removed-state-retention: 48h
failure-notification-priority: 0
recovery-notification-priority: -1
pushover-app-token-env: CUSTOM_APP
pushover-user-key-env: CUSTOM_USER
pushover-app-token-file: /run/secrets/app
pushover-user-key-file: /run/secrets/user
pushover-device: phone
management-url: https://cpa.example.test/management
state-file: /var/lib/cpa-plugin/state.json
pushover-http-timeout: 7s
max-concurrent-checks: 8
`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Providers, ",") != "claude,codex" {
		t.Fatalf("normalized providers = %v", cfg.Providers)
	}
	if cfg.ScanInterval != 2*time.Minute || cfg.StartupGrace != 5*time.Second || cfg.HTTPTimeout != 7*time.Second {
		t.Fatalf("durations not parsed: %+v", cfg)
	}
	if cfg.NotifyRecovery || !cfg.NotifyDisabled || !cfg.NotifyRemoved || cfg.ReminderInterval != 0 {
		t.Fatalf("booleans/reminder not parsed: %+v", cfg)
	}
	if cfg.FailurePriority != 0 || cfg.RecoveryPriority != -1 || cfg.MaxConcurrentChecks != 8 {
		t.Fatalf("numeric config not parsed: %+v", cfg)
	}
}

func TestParseRejectsUnsafeOrInvalidValues(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{"unsupported provider", "providers: [gemini]"},
		{"invalid duration", "scan-interval: tomorrow"},
		{"zero scan", "scan-interval: 0s"},
		{"emergency priority", "failure-notification-priority: 2"},
		{"bad env name", "pushover-app-token-env: BAD-NAME"},
		{"embedded URL credentials", "management-url: https://user:pass@example.com"},
		{"too many workers", "max-concurrent-checks: 99"},
		{"negative workers", "max-concurrent-checks: -5"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse([]byte(test.raw)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestResolveCredentialsPrefersEnvironment(t *testing.T) {
	cfg := Default()
	temp := t.TempDir()
	cfg.PushoverAppTokenFile = filepath.Join(temp, "app")
	cfg.PushoverUserKeyFile = filepath.Join(temp, "user")
	if err := os.WriteFile(cfg.PushoverAppTokenFile, []byte(strings.Repeat("F", 30)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.PushoverUserKeyFile, []byte(strings.Repeat("G", 30)), 0o600); err != nil {
		t.Fatal(err)
	}
	envApp := strings.Repeat("A", 30)
	envUser := strings.Repeat("B", 30)
	credentials, status := cfg.ResolveCredentials(func(name string) string {
		if name == cfg.PushoverAppTokenEnv {
			return envApp
		}
		if name == cfg.PushoverUserKeyEnv {
			return envUser
		}
		return ""
	})
	if status.State != "configured" || credentials.AppToken != envApp || credentials.UserKey != envUser {
		t.Fatalf("unexpected resolution: credentials=%+v status=%+v", credentials, status)
	}
}

func TestResolveCredentialsMissingAndInvalidAreSanitized(t *testing.T) {
	cfg := Default()
	_, missing := cfg.ResolveCredentials(func(string) string { return "" })
	if missing.State != "missing" || strings.Contains(missing.Error, DefaultAppTokenEnv) {
		t.Fatalf("missing status leaked details: %+v", missing)
	}
	secret := "short-secret"
	_, invalid := cfg.ResolveCredentials(func(string) string { return secret })
	if invalid.State != "invalid" || strings.Contains(invalid.Error, secret) {
		t.Fatalf("invalid status leaked secret: %+v", invalid)
	}
}
