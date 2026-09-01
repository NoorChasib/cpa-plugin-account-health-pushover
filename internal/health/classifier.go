package health

import (
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
	result := func(state State, reason ReasonCode, detail string) Observation {
		return Observation{State: state, ReasonCode: reason, Detail: detail, ObservedAt: now}
	}
	if snapshot.Disabled {
		return result(Disabled, ReasonOperatorDisabled, "credential is disabled by the operator")
	}

	status := strings.ToLower(strings.TrimSpace(snapshot.Status))
	currentError := status == "error" || snapshot.Unavailable

	// Current active/available runtime state supersedes stale request evidence.
	if status == "active" && !snapshot.Unavailable {
		return result(Healthy, ReasonNone, "credential is active")
	}

	if currentError {
		switch snapshot.MessageCode {
		case RuntimeMessageInvalidGrant:
			return result(ReauthRequired, ReasonInvalidGrant, "OAuth refresh was rejected and manual sign-in is required")
		case RuntimeMessageInvalidToken:
			return result(ReauthRequired, ReasonInvalidToken, "OAuth token was rejected and manual sign-in is required")
		case RuntimeMessageRefreshRevoked:
			return result(ReauthRequired, ReasonRefreshTokenRevoked, "OAuth refresh token was revoked")
		case RuntimeMessageRefreshRejected:
			return result(ReauthRequired, ReasonRefreshTokenRejected, "OAuth refresh token was rejected")
		case RuntimeMessageUnauthorized:
			if snapshot.NextRetryAfter.IsZero() || !snapshot.NextRetryAfter.After(now) {
				return result(ReauthRequired, ReasonUnauthorizedRefreshFailed, "CPA reports a definitive refresh-path authorization failure")
			}
		}
	}

	if currentError {
		switch snapshot.Evidence.HTTPStatus {
		case 429:
			return result(QuotaLimited, ReasonHTTP429Quota, "credential is authenticated but temporarily quota limited")
		case 401:
			observation := result(Suspect, ReasonUnauthorizedRequest, "request-level authorization failure requires confirmation after CPA refresh")
			observation.Confirmation = ConfirmationUnauthorized
			observation.ConfirmAs = ReauthRequired
			return observation
		case 403:
			observation := result(Suspect, ReasonHTTP403, "provider returned a non-definitive forbidden or entitlement error")
			observation.Confirmation = ConfirmationTransient
			observation.ConfirmAs = CredentialDown
			return observation
		case 408:
			return transientObservation(now, ReasonHTTP408)
		case 500:
			return transientObservation(now, ReasonHTTP500)
		case 502:
			return transientObservation(now, ReasonHTTP502)
		case 503:
			return transientObservation(now, ReasonHTTP503)
		case 504:
			return transientObservation(now, ReasonHTTP504)
		}
	}

	if currentError {
		switch snapshot.MessageCode {
		case RuntimeMessageQuotaExhausted:
			return result(QuotaLimited, ReasonQuotaExhausted, "credential is authenticated but temporarily quota limited")
		case RuntimeMessageUnauthorized:
			observation := result(Suspect, ReasonUnauthorizedRequest, "authorization failure is still inside CPA's retry window")
			observation.Confirmation = ConfirmationUnauthorized
			observation.ConfirmAs = ReauthRequired
			return observation
		case RuntimeMessagePaymentRequired:
			observation := result(Suspect, ReasonPaymentRequired, "provider entitlement failure requires sustained confirmation")
			observation.Confirmation = ConfirmationTransient
			observation.ConfirmAs = CredentialDown
			return observation
		case RuntimeMessageNotFound:
			observation := result(Suspect, ReasonNotFound, "provider resource failure requires sustained confirmation")
			observation.Confirmation = ConfirmationTransient
			observation.ConfirmAs = CredentialDown
			return observation
		case RuntimeMessageTransientError:
			return transientObservation(now, ReasonTransientUpstreamError)
		case RuntimeMessageCloudflareChallenge:
			return transientObservation(now, ReasonCloudflareChallenge)
		case RuntimeMessageRequestFailed:
			return transientObservation(now, ReasonRequestFailed)
		}
	}

	if currentError && snapshot.NextRetryAfter.After(now) {
		// CPA uses this field for quota, unauthorized, forbidden, and transient
		// cooldowns. Without structured status evidence it is deliberately held
		// as non-promotable suspect rather than guessed to be credential failure.
		return result(Suspect, ReasonCooldownActive, "CPA cooldown is active without a classified failure cause")
	}
	if currentError && !snapshot.NextRetryAfter.IsZero() && !snapshot.NextRetryAfter.After(now) && snapshot.Evidence.HTTPStatus == 0 {
		// An elapsed retry timestamp makes the credential eligible for another
		// attempt; it is not proof of successful authentication. Keep the state
		// non-promotable until CPA reports active/available or new typed evidence.
		return result(Suspect, ReasonCooldownRecoveryUnconfirmed, "CPA cooldown elapsed but credential recovery is not yet confirmed")
	}
	if currentError {
		observation := result(Suspect, ReasonUnclassifiedCredentialError, "credential error requires sustained confirmation")
		observation.Confirmation = ConfirmationTransient
		observation.ConfirmAs = CredentialDown
		return observation
	}
	return result(Healthy, ReasonNone, "no current credential failure is reported")
}

func transientObservation(now time.Time, reason ReasonCode) Observation {
	return Observation{
		State:        Suspect,
		ReasonCode:   reason,
		Detail:       "temporary provider or network failure requires confirmation",
		ObservedAt:   now,
		Confirmation: ConfirmationTransient,
		ConfirmAs:    CredentialDown,
	}
}
