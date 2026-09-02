package health

import (
	"strings"
	"testing"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/config"
	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

func TestDiscoverDynamicOAuthAccounts(t *testing.T) {
	cfg := config.Default()
	roster := []protocol.HostAuthFileEntry{
		{AuthIndex: "claude-1", Provider: "claude", AccountType: "oauth", Email: "a@example.com"},
		{AuthIndex: "codex-1", Provider: "codex", AccountType: "oauth", Email: "b@example.com"},
		{AuthIndex: "gemini-1", Provider: "gemini", AccountType: "oauth", Email: "c@example.com"},
		{AuthIndex: "xai-1", Provider: "xai", AccountType: "oauth", Email: "grok@example.com"},
		{AuthIndex: "xai-key", Provider: "xai", AccountType: "api_key", Account: "xai-secret-must-not-leak"},
		{AuthIndex: "claude-key", Provider: "claude", AccountType: "api_key", Account: "must-not-leak"},
		{AuthIndex: "", Provider: "codex", AccountType: "oauth", Email: "missing-index@example.com"},
		{AuthIndex: "claude-1", Provider: "claude", AccountType: "oauth", Email: "duplicate@example.com"},
	}
	got := Discover(roster, cfg)
	if len(got) != 3 {
		t.Fatalf("discovered %d accounts, want 3: %+v", len(got), got)
	}
	if got[0].AuthIndex != "claude-1" || got[1].AuthIndex != "codex-1" || got[2].AuthIndex != "xai-1" {
		t.Fatalf("unexpected accounts: %+v", got)
	}
}

func TestDiscoverAddedAndRemovedAccounts(t *testing.T) {
	cfg := config.Default()
	first := Discover([]protocol.HostAuthFileEntry{
		{AuthIndex: "one", Provider: "claude", AccountType: "oauth", Email: "one@example.com"},
	}, cfg)
	second := Discover([]protocol.HostAuthFileEntry{
		{AuthIndex: "one", Provider: "claude", AccountType: "oauth", Email: "one@example.com"},
		{AuthIndex: "two", Provider: "codex", AccountType: "oauth", Email: "two@example.com"},
	}, cfg)
	third := Discover([]protocol.HostAuthFileEntry{
		{AuthIndex: "two", Provider: "codex", AccountType: "oauth", Email: "two@example.com"},
	}, cfg)
	if len(first) != 1 || len(second) != 2 || len(third) != 1 || third[0].AuthIndex != "two" {
		t.Fatalf("dynamic discovery mismatch: first=%+v second=%+v third=%+v", first, second, third)
	}
}

func TestIdentityFingerprintUsesSafeExactIdentifiers(t *testing.T) {
	entry := protocol.HostAuthFileEntry{Provider: "claude", Email: " Account@Example.COM ", Label: "ignored"}
	if got := IdentityFingerprint(entry); got != "claude|email|account@example.com" {
		t.Fatalf("fingerprint = %q", got)
	}
	entry.Email = ""
	if got := IdentityFingerprint(entry); got != "claude|label|ignored" {
		t.Fatalf("label fingerprint = %q", got)
	}
}

func TestSafeLabelRemovesControlsAndNeverUsesAccountField(t *testing.T) {
	entry := protocol.HostAuthFileEntry{
		Provider:    "codex",
		Label:       "user\nname\tlabel",
		AccountType: "api_key",
		Account:     "SECRET-API-KEY",
	}
	got := SafeLabel(entry)
	if got != "user name label" {
		t.Fatalf("safe label = %q", got)
	}
	entry.Label = ""
	entry.Name = ""
	entry.AuthIndex = ""
	if got := SafeLabel(entry); got != "unknown account" {
		t.Fatalf("empty safe identifiers used an unsafe fallback: %q", got)
	} else if strings.Contains(got, entry.Account) {
		t.Fatal("safe label used the Account field, which may contain an API key")
	}
}
