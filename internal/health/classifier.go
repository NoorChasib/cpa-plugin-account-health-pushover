package health

import (
	"fmt"
	"strings"
	"time"
)

type classifier struct {
	provider string
}

func ClaudeClassifier() ProviderClassifier { return classifier{provider: "claude"} }
func CodexClassifier() ProviderClassifier  { return classifier{provider: "codex"} }

func Classifiers() map[string]ProviderClassifier {
	return map[string]ProviderClassifier{
		"claude": ClaudeClassifier(),
		"codex":  CodexClassifier(),
	}
}

func (c classifier) ProviderID() string { return c.provider }

func (c classifier) Classify(snapshot RuntimeSnapshot) Observation {
	now := snapshot.ObservedAt
	if now.IsZero() {
		now = time.Now()
	}
	observation := Observation{ObservedAt: now}
	if snapshot.Disabled {
		observation.State = Disabled
		observation.ReasonCode = "operator_disabled"
		observation.Detail = "credential is disabled by the operator"
		observation.Definitive = true
		return observation
	}

	status := strings.ToLower(strings.TrimSpace(snapshot.Status))
	reasonText := strings.ToLower(strings.Join([]string{
		snapshot.StatusMessage,
		snapshot.LastErrorCode,
		snapshot.LastErrorMessage,
		snapshot.QuotaReason,
	}, " "))

	if quotaEvidence(snapshot, reasonText, now) {
		observation.State = QuotaLimited
		observation.ReasonCode = quotaReason(reasonText)
		observation.Detail = "credential is authenticated but temporarily quota limited"
		observation.Definitive = true
		return observation
	}

	// Current active/available state overrides a stale historic LastError.
	if status == "active" && !snapshot.Unavailable {
		observation.State = Healthy
		observation.ReasonCode = ""
		observation.Detail = "credential is active"
		observation.Definitive = true
		return observation
	}

	if definitiveUnauthorized(snapshot, status, reasonText) {
		observation.State = ReauthRequired
		observation.ReasonCode = unauthorizedReason(reasonText, snapshot.LastHTTPStatus)
		observation.Detail = "OAuth credentials were rejected and manual sign-in is required"
		observation.Definitive = true
		return observation
	}

	if transientEvidence(snapshot, reasonText) {
		observation.State = Suspect
		observation.ReasonCode = transientReason(snapshot.LastHTTPStatus, reasonText)
		observation.Detail = "temporary provider or network failure requires confirmation"
		return observation
	}

	if snapshot.LastHTTPStatus == 403 || strings.Contains(reasonText, "payment_required") || strings.Contains(reasonText, "forbidden") {
		observation.State = Suspect
		observation.ReasonCode = "ambiguous_403"
		observation.Detail = "provider returned a non-definitive forbidden or entitlement error"
		return observation
	}

	if status == "error" || snapshot.Unavailable {
		if !snapshot.NextRetryAfter.IsZero() && !snapshot.NextRetryAfter.After(now) && status != "error" {
			observation.State = Healthy
			observation.Detail = "temporary unavailability window has elapsed"
			observation.Definitive = true
			return observation
		}
		observation.State = Suspect
		observation.ReasonCode = genericReason(snapshot.StatusMessage, snapshot.LastErrorCode)
		observation.Detail = "credential error requires sustained confirmation"
		return observation
	}

	observation.State = Healthy
	observation.Detail = "no current credential failure is reported"
	observation.Definitive = true
	return observation
}

func quotaEvidence(snapshot RuntimeSnapshot, text string, now time.Time) bool {
	if snapshot.LastHTTPStatus == 429 || snapshot.QuotaExceeded {
		return true
	}
	if !snapshot.NextRecoverAt.IsZero() && snapshot.NextRecoverAt.After(now) {
		return true
	}
	for _, keyword := range []string{
		"quota", "rate limit", "rate_limit", "usage limit", "usage_limit",
		"weekly limit", "weekly_limit", "5-hour", "5 hour", "five hour",
		"fable limit", "model-scoped", "model scoped", "limit reached",
	} {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

func quotaReason(text string) string {
	for _, candidate := range []struct {
		needle string
		code   string
	}{
		{"fable", "model_quota"},
		{"weekly", "weekly_limit"},
		{"5-hour", "five_hour_limit"},
		{"5 hour", "five_hour_limit"},
		{"rate limit", "rate_limit"},
		{"rate_limit", "rate_limit"},
		{"quota", "quota"},
	} {
		if strings.Contains(text, candidate.needle) {
			return candidate.code
		}
	}
	return "quota"
}

func definitiveUnauthorized(snapshot RuntimeSnapshot, status, text string) bool {
	currentError := status == "error" || snapshot.Unavailable
	if !currentError {
		return false
	}
	if snapshot.LastHTTPStatus == 401 {
		return true
	}
	for _, keyword := range []string{
		"invalid_grant", "invalid grant", "unauthorized", "invalid_token", "invalid token",
		"refresh token revoked", "refresh_token_revoked", "refresh token rejected",
		"credential revoked", "authentication required", "login required",
	} {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

func unauthorizedReason(text string, status int) string {
	for _, candidate := range []struct {
		needle string
		code   string
	}{
		{"invalid_grant", "invalid_grant"},
		{"invalid grant", "invalid_grant"},
		{"refresh token revoked", "refresh_token_revoked"},
		{"refresh_token_revoked", "refresh_token_revoked"},
		{"invalid_token", "invalid_token"},
		{"invalid token", "invalid_token"},
		{"unauthorized", "unauthorized"},
	} {
		if strings.Contains(text, candidate.needle) {
			return candidate.code
		}
	}
	if status == 401 {
		return "unauthorized"
	}
	return "invalid_credential"
}

func transientEvidence(snapshot RuntimeSnapshot, text string) bool {
	switch snapshot.LastHTTPStatus {
	case 408, 500, 502, 503, 504:
		return true
	}
	for _, keyword := range []string{
		"transient upstream error", "timeout", "timed out", "dns failure", "connection reset",
		"temporary network", "network error", "service unavailable", "bad gateway", "gateway timeout",
	} {
		if strings.Contains(text, keyword) {
			return true
		}
	}
	return false
}

func transientReason(status int, text string) string {
	if status != 0 {
		return fmt.Sprintf("http_%d", status)
	}
	for _, candidate := range []struct {
		needle string
		code   string
	}{
		{"dns", "dns_failure"},
		{"connection reset", "connection_reset"},
		{"timeout", "timeout"},
		{"timed out", "timeout"},
		{"service unavailable", "provider_unavailable"},
	} {
		if strings.Contains(text, candidate.needle) {
			return candidate.code
		}
	}
	return "transient_error"
}

func genericReason(statusMessage, errorCode string) string {
	for _, value := range []string{errorCode, statusMessage} {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		value = strings.NewReplacer(" ", "_", "-", "_", "/", "_").Replace(value)
		if len(value) > 64 {
			value = value[:64]
		}
		return value
	}
	return "credential_error"
}
