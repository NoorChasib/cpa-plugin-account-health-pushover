package health

import (
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

type State string

const (
	Unknown        State = "unknown"
	Healthy        State = "healthy"
	QuotaLimited   State = "quota_limited"
	Suspect        State = "suspect"
	CredentialDown State = "credential_down"
	ReauthRequired State = "reauth_required"
	Disabled       State = "disabled"
	Removed        State = "removed"
	Recovering     State = "recovering"
)

type RuntimeSnapshot struct {
	AuthKey          string
	AuthID           string
	AuthIndex        string
	Provider         string
	Label            string
	Identity         string
	Path             string
	Disabled         bool
	Unavailable      bool
	Status           string
	StatusMessage    string
	LastErrorCode    string
	LastErrorMessage string
	LastHTTPStatus   int
	QuotaExceeded    bool
	QuotaReason      string
	NextRecoverAt    time.Time
	NextRetryAfter   time.Time
	NextRefreshAfter time.Time
	LastRefresh      time.Time
	UpdatedAt        time.Time
	Success          int64
	Failed           int64
	ObservedAt       time.Time
}

type Observation struct {
	State      State
	ReasonCode string
	Detail     string
	ObservedAt time.Time
	Definitive bool
}

type ProviderClassifier interface {
	ProviderID() string
	Classify(RuntimeSnapshot) Observation
}

func FromHostEntry(entry protocol.HostAuthFileEntry, now time.Time) RuntimeSnapshot {
	provider := strings.ToLower(strings.TrimSpace(entry.Provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(entry.Type))
	}
	index := strings.TrimSpace(entry.AuthIndex)
	return RuntimeSnapshot{
		AuthKey:        AccountKey(provider, index),
		AuthID:         safeText(entry.ID, 128),
		AuthIndex:      safeText(index, 128),
		Provider:       safeText(provider, 32),
		Label:          SafeLabel(entry),
		Identity:       IdentityFingerprint(entry),
		Path:           entry.Path,
		Disabled:       entry.Disabled,
		Unavailable:    entry.Unavailable,
		Status:         strings.ToLower(strings.TrimSpace(entry.Status)),
		StatusMessage:  safeReason(entry.StatusMessage),
		NextRetryAfter: entry.NextRetryAfter,
		LastRefresh:    entry.LastRefresh,
		UpdatedAt:      entry.UpdatedAt,
		Success:        entry.Success,
		Failed:         entry.Failed,
		ObservedAt:     now,
	}
}

func AccountKey(provider, authIndex string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + ":" + strings.TrimSpace(authIndex)
}

func IsOAuthCredential(entry protocol.HostAuthFileEntry) bool {
	accountType := strings.ToLower(strings.TrimSpace(entry.AccountType))
	if accountType == "api_key" {
		return false
	}
	if accountType == "oauth" {
		return true
	}
	if strings.TrimSpace(entry.Email) != "" {
		return true
	}
	typeName := strings.ToLower(strings.TrimSpace(entry.Type))
	return strings.Contains(typeName, "oauth")
}

func SafeLabel(entry protocol.HostAuthFileEntry) string {
	for _, candidate := range []string{entry.Email, entry.Label, entry.Name, entry.AuthIndex} {
		if value := safeText(candidate, 120); value != "" {
			return value
		}
	}
	return "unknown account"
}

func IdentityFingerprint(entry protocol.HostAuthFileEntry) string {
	provider := strings.ToLower(strings.TrimSpace(entry.Provider))
	if email := strings.ToLower(strings.TrimSpace(entry.Email)); email != "" {
		return provider + "|email|" + safeText(email, 160)
	}
	if label := strings.ToLower(strings.TrimSpace(entry.Label)); label != "" {
		return provider + "|label|" + safeText(label, 160)
	}
	if name := strings.ToLower(strings.TrimSuffix(filepath.Base(strings.TrimSpace(entry.Name)), filepath.Ext(entry.Name))); name != "" {
		return provider + "|name|" + safeText(name, 160)
	}
	return ""
}

func IsFailure(state State) bool {
	return state == ReauthRequired || state == CredentialDown
}

func IsCredentialHealthy(state State) bool {
	return state == Healthy || state == QuotaLimited
}

var whitespacePattern = regexp.MustCompile(`\s+`)

func safeText(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = whitespacePattern.ReplaceAllString(strings.TrimSpace(value), " ")
	return truncateRunes(value, maxRunes)
}

func safeReason(value string) string {
	return strings.ToLower(safeText(value, 160))
}

func truncateRunes(value string, max int) string {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}
