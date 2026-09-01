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
)

type RuntimeMessageCode string

const (
	RuntimeMessageUnknown             RuntimeMessageCode = ""
	RuntimeMessageUnauthorized        RuntimeMessageCode = "unauthorized"
	RuntimeMessageInvalidGrant        RuntimeMessageCode = "invalid_grant"
	RuntimeMessageInvalidToken        RuntimeMessageCode = "invalid_token"
	RuntimeMessagePaymentRequired     RuntimeMessageCode = "payment_required"
	RuntimeMessageNotFound            RuntimeMessageCode = "not_found"
	RuntimeMessageQuotaExhausted      RuntimeMessageCode = "quota_exhausted"
	RuntimeMessageCloudflareChallenge RuntimeMessageCode = "cloudflare_challenge"
	RuntimeMessageTransientError      RuntimeMessageCode = "transient_upstream_error"
	RuntimeMessageRequestFailed       RuntimeMessageCode = "request_failed"
	RuntimeMessageRefreshRevoked      RuntimeMessageCode = "refresh_token_revoked"
	RuntimeMessageRefreshRejected     RuntimeMessageCode = "refresh_token_rejected"
)

type ReasonCode string

const (
	ReasonNone                        ReasonCode = ""
	ReasonOperatorDisabled            ReasonCode = "operator_disabled"
	ReasonHTTP429Quota                ReasonCode = "http_429_quota"
	ReasonQuotaExhausted              ReasonCode = "quota_exhausted"
	ReasonUnauthorizedRequest         ReasonCode = "unauthorized_request"
	ReasonUnauthorizedRefreshFailed   ReasonCode = "unauthorized_refresh_failed"
	ReasonInvalidGrant                ReasonCode = "invalid_grant"
	ReasonInvalidToken                ReasonCode = "invalid_token"
	ReasonRefreshTokenRevoked         ReasonCode = "refresh_token_revoked"
	ReasonRefreshTokenRejected        ReasonCode = "refresh_token_rejected"
	ReasonPaymentRequired             ReasonCode = "payment_required"
	ReasonNotFound                    ReasonCode = "not_found"
	ReasonHTTP403                     ReasonCode = "http_403"
	ReasonHTTP408                     ReasonCode = "http_408"
	ReasonHTTP500                     ReasonCode = "http_500"
	ReasonHTTP502                     ReasonCode = "http_502"
	ReasonHTTP503                     ReasonCode = "http_503"
	ReasonHTTP504                     ReasonCode = "http_504"
	ReasonTransientUpstreamError      ReasonCode = "transient_upstream_error"
	ReasonCloudflareChallenge         ReasonCode = "cloudflare_challenge"
	ReasonRequestFailed               ReasonCode = "request_failed"
	ReasonCooldownActive              ReasonCode = "cooldown_active"
	ReasonCooldownRecoveryUnconfirmed ReasonCode = "cooldown_recovery_unconfirmed"
	ReasonAvailableResidualError      ReasonCode = "available_residual_error"
	ReasonUnclassifiedCredentialError ReasonCode = "unclassified_credential_error"
	ReasonRemoved                     ReasonCode = "removed"
	ReasonDisabled                    ReasonCode = "disabled"
	ReasonPersistentUnauthorized      ReasonCode = "persistent_unauthorized_request"
	ReasonPersistentHTTP403           ReasonCode = "persistent_http_403"
	ReasonPersistentHTTP408           ReasonCode = "persistent_http_408"
	ReasonPersistentHTTP500           ReasonCode = "persistent_http_500"
	ReasonPersistentHTTP502           ReasonCode = "persistent_http_502"
	ReasonPersistentHTTP503           ReasonCode = "persistent_http_503"
	ReasonPersistentHTTP504           ReasonCode = "persistent_http_504"
	ReasonPersistentTransient         ReasonCode = "persistent_transient_upstream_error"
	ReasonPersistentCloudflare        ReasonCode = "persistent_cloudflare_challenge"
	ReasonPersistentRequestFailed     ReasonCode = "persistent_request_failed"
	ReasonPersistentPaymentRequired   ReasonCode = "persistent_payment_required"
	ReasonPersistentNotFound          ReasonCode = "persistent_not_found"
	ReasonPersistentUnclassified      ReasonCode = "persistent_unclassified_credential_error"
)

type ConfirmationClass string

const (
	ConfirmationNone         ConfirmationClass = ""
	ConfirmationTransient    ConfirmationClass = "transient"
	ConfirmationUnauthorized ConfirmationClass = "unauthorized"
)

type FailureEvidence struct {
	HTTPStatus int
	ObservedAt time.Time
}

type RuntimeSnapshot struct {
	AuthKey        string
	AuthIndex      string
	Provider       string
	Label          string
	Identity       string
	Disabled       bool
	Unavailable    bool
	Status         string
	MessageCode    RuntimeMessageCode
	NextRetryAfter time.Time
	Evidence       FailureEvidence
	ObservedAt     time.Time
}

type Observation struct {
	State        State
	ReasonCode   ReasonCode
	Detail       string
	ObservedAt   time.Time
	Confirmation ConfirmationClass
	ConfirmAs    State
}

type ProviderClassifier interface {
	Classify(RuntimeSnapshot) Observation
}

func providerOf(entry protocol.HostAuthFileEntry) string {
	provider := strings.ToLower(strings.TrimSpace(entry.Provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(entry.Type))
	}
	return provider
}

func FromHostEntry(entry protocol.HostAuthFileEntry, evidence FailureEvidence, now time.Time) RuntimeSnapshot {
	provider := providerOf(entry)
	index := strings.TrimSpace(entry.AuthIndex)
	return RuntimeSnapshot{
		AuthKey:        AccountKey(provider, index),
		AuthIndex:      safeText(index, 128),
		Provider:       safeText(provider, 32),
		Label:          SafeLabel(entry),
		Identity:       IdentityFingerprint(entry),
		Disabled:       entry.Disabled,
		Unavailable:    entry.Unavailable,
		Status:         strings.ToLower(strings.TrimSpace(entry.Status)),
		MessageCode:    CanonicalRuntimeMessage(entry.StatusMessage),
		NextRetryAfter: entry.NextRetryAfter,
		Evidence:       evidence,
		ObservedAt:     now,
	}
}

func CanonicalRuntimeMessage(value string) RuntimeMessageCode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "unauthorized":
		return RuntimeMessageUnauthorized
	case "invalid_grant", "invalid grant":
		return RuntimeMessageInvalidGrant
	case "invalid_token", "invalid token":
		return RuntimeMessageInvalidToken
	case "payment_required", "payment required":
		return RuntimeMessagePaymentRequired
	case "not_found", "not found":
		return RuntimeMessageNotFound
	case "quota exhausted":
		return RuntimeMessageQuotaExhausted
	case "cloudflare challenge":
		return RuntimeMessageCloudflareChallenge
	case "transient upstream error":
		return RuntimeMessageTransientError
	case "request failed":
		return RuntimeMessageRequestFailed
	case "refresh token revoked", "refresh_token_revoked":
		return RuntimeMessageRefreshRevoked
	case "refresh token rejected", "refresh_token_rejected":
		return RuntimeMessageRefreshRejected
	default:
		return RuntimeMessageUnknown
	}
}

func NormalizeReasonCode(value string) ReasonCode {
	candidate := ReasonCode(strings.TrimSpace(value))
	for _, known := range []ReasonCode{
		ReasonNone, ReasonOperatorDisabled, ReasonHTTP429Quota, ReasonQuotaExhausted,
		ReasonUnauthorizedRequest, ReasonUnauthorizedRefreshFailed, ReasonInvalidGrant,
		ReasonInvalidToken, ReasonRefreshTokenRevoked, ReasonRefreshTokenRejected,
		ReasonPaymentRequired, ReasonNotFound, ReasonHTTP403, ReasonHTTP408,
		ReasonHTTP500, ReasonHTTP502, ReasonHTTP503, ReasonHTTP504,
		ReasonTransientUpstreamError, ReasonCloudflareChallenge, ReasonRequestFailed,
		ReasonCooldownActive, ReasonCooldownRecoveryUnconfirmed, ReasonAvailableResidualError,
		ReasonUnclassifiedCredentialError, ReasonRemoved, ReasonDisabled,
		ReasonPersistentUnauthorized, ReasonPersistentHTTP403, ReasonPersistentHTTP408,
		ReasonPersistentHTTP500, ReasonPersistentHTTP502, ReasonPersistentHTTP503,
		ReasonPersistentHTTP504, ReasonPersistentTransient, ReasonPersistentCloudflare,
		ReasonPersistentRequestFailed, ReasonPersistentPaymentRequired,
		ReasonPersistentNotFound, ReasonPersistentUnclassified,
	} {
		if candidate == known {
			return candidate
		}
	}
	return ReasonUnclassifiedCredentialError
}

func PersistentReason(reason ReasonCode) ReasonCode {
	switch reason {
	case ReasonUnauthorizedRequest:
		return ReasonPersistentUnauthorized
	case ReasonHTTP403:
		return ReasonPersistentHTTP403
	case ReasonHTTP408:
		return ReasonPersistentHTTP408
	case ReasonHTTP500:
		return ReasonPersistentHTTP500
	case ReasonHTTP502:
		return ReasonPersistentHTTP502
	case ReasonHTTP503:
		return ReasonPersistentHTTP503
	case ReasonHTTP504:
		return ReasonPersistentHTTP504
	case ReasonTransientUpstreamError:
		return ReasonPersistentTransient
	case ReasonCloudflareChallenge:
		return ReasonPersistentCloudflare
	case ReasonRequestFailed:
		return ReasonPersistentRequestFailed
	case ReasonPaymentRequired:
		return ReasonPersistentPaymentRequired
	case ReasonNotFound:
		return ReasonPersistentNotFound
	default:
		return ReasonPersistentUnclassified
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

func truncateRunes(value string, max int) string {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}
