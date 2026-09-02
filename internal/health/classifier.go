package health

import (
	"strings"
	"time"
)

type classifier struct{}

func ClaudeClassifier() ProviderClassifier { return classifier{} }
func CodexClassifier() ProviderClassifier  { return classifier{} }

// XAIClassifier covers CPA's "xai" provider (Grok Build OAuth). At the audited
// CPA revision xAI shares the generic refresh manager, status fields, and
// next_retry_after cooldown semantics with Claude and Codex, so the same
// conservative classification applies.
func XAIClassifier() ProviderClassifier { return classifier{} }

func Classifiers() map[string]ProviderClassifier {
	return map[string]ProviderClassifier{
		"claude": ClaudeClassifier(),
		"codex":  CodexClassifier(),
		"xai":    XAIClassifier(),
	}
}

func (classifier) Classify(snapshot RuntimeSnapshot) Observation {
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

	if !currentError {
		return result(Healthy, ReasonNone, "no current credential failure is reported")
	}

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

	if observation, ok := failureStatusObservation(now, snapshot.Evidence.HTTPStatus); ok {
		return observation
	}

	if snapshot.MessageCode == RuntimeMessageQuotaExhausted {
		return result(QuotaLimited, ReasonQuotaExhausted, "credential is authenticated but temporarily quota limited")
	}

	// CPA can retain a model-scoped error after a different model succeeds. If
	// the auth remains available and there is no auth-level retry or structured
	// request evidence, that residual status cannot establish account-wide
	// credential failure and must never start a promotion clock.
	if !snapshot.Unavailable && snapshot.NextRetryAfter.IsZero() {
		return result(Suspect, ReasonAvailableResidualError, "credential remains available despite a residual runtime error")
	}

	switch snapshot.MessageCode {
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

	if snapshot.NextRetryAfter.After(now) {
		// CPA uses this field for quota, unauthorized, forbidden, and transient
		// cooldowns. Without structured status evidence it is deliberately held
		// as non-promotable suspect rather than guessed to be credential failure.
		return result(Suspect, ReasonCooldownActive, "CPA cooldown is active without a classified failure cause")
	}
	if !snapshot.NextRetryAfter.IsZero() && snapshot.Evidence.HTTPStatus == 0 {
		// An elapsed retry timestamp makes the credential eligible for another
		// attempt; it is not proof of successful authentication. Keep the state
		// non-promotable until CPA reports active/available or new typed evidence.
		return result(Suspect, ReasonCooldownRecoveryUnconfirmed, "CPA cooldown elapsed but credential recovery is not yet confirmed")
	}
	observation := result(Suspect, ReasonUnclassifiedCredentialError, "credential error requires sustained confirmation")
	observation.Confirmation = ConfirmationTransient
	observation.ConfirmAs = CredentialDown
	return observation
}

type failureStatusRule struct {
	state        State
	reason       ReasonCode
	detail       string
	confirmation ConfirmationClass
	confirmAs    State
}

func failureStatus(statusCode int) (failureStatusRule, bool) {
	switch statusCode {
	case 429:
		return failureStatusRule{
			state:  QuotaLimited,
			reason: ReasonHTTP429Quota,
			detail: "credential is authenticated but temporarily quota limited",
		}, true
	case 401:
		return failureStatusRule{
			state:        Suspect,
			reason:       ReasonUnauthorizedRequest,
			detail:       "request-level authorization failure requires confirmation after CPA refresh",
			confirmation: ConfirmationUnauthorized,
			confirmAs:    ReauthRequired,
		}, true
	case 403:
		return failureStatusRule{
			state:        Suspect,
			reason:       ReasonHTTP403,
			detail:       "provider returned a non-definitive forbidden or entitlement error",
			confirmation: ConfirmationTransient,
			confirmAs:    CredentialDown,
		}, true
	case 408:
		return transientFailureStatus(ReasonHTTP408), true
	case 500:
		return transientFailureStatus(ReasonHTTP500), true
	case 502:
		return transientFailureStatus(ReasonHTTP502), true
	case 503:
		return transientFailureStatus(ReasonHTTP503), true
	case 504:
		return transientFailureStatus(ReasonHTTP504), true
	default:
		return failureStatusRule{}, false
	}
}

func transientFailureStatus(reason ReasonCode) failureStatusRule {
	return failureStatusRule{
		state:        Suspect,
		reason:       reason,
		detail:       "temporary provider or network failure requires confirmation",
		confirmation: ConfirmationTransient,
		confirmAs:    CredentialDown,
	}
}

func IsClassifiableFailureStatus(statusCode int) bool {
	_, ok := failureStatus(statusCode)
	return ok
}

func failureStatusObservation(now time.Time, statusCode int) (Observation, bool) {
	rule, ok := failureStatus(statusCode)
	if !ok {
		return Observation{}, false
	}
	return rule.observation(now), true
}

func (rule failureStatusRule) observation(now time.Time) Observation {
	return Observation{
		State:        rule.state,
		ReasonCode:   rule.reason,
		Detail:       rule.detail,
		ObservedAt:   now,
		Confirmation: rule.confirmation,
		ConfirmAs:    rule.confirmAs,
	}
}

func transientObservation(now time.Time, reason ReasonCode) Observation {
	return transientFailureStatus(reason).observation(now)
}
