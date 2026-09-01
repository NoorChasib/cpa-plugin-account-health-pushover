package health

import (
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

func runtimeSnapshot(now time.Time, status, message string, unavailable bool, retry time.Time, httpStatus int) RuntimeSnapshot {
	entry := protocol.HostAuthFileEntry{
		AuthIndex:      "one",
		Provider:       "claude",
		Type:           "claude",
		AccountType:    "oauth",
		Status:         status,
		StatusMessage:  message,
		Unavailable:    unavailable,
		NextRetryAfter: retry,
	}
	return FromHostEntry(entry, FailureEvidence{HTTPStatus: httpStatus, ObservedAt: now}, now)
}

func TestClassifierUsesProductionAvailableEvidence(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		snapshot    RuntimeSnapshot
		want        State
		reason      ReasonCode
		confirm     ConfirmationClass
		confirmAs   State
		forbidQuota bool
	}{
		{
			name:     "healthy enabled auth",
			snapshot: runtimeSnapshot(now, "active", "", false, time.Time{}, 0),
			want:     Healthy,
		},
		{
			name:     "429 arbitrary provider body",
			snapshot: runtimeSnapshot(now, "error", `{"opaque":"provider prose without known words"}`, true, now.Add(time.Hour), 429),
			want:     QuotaLimited,
			reason:   ReasonHTTP429Quota,
		},
		{
			name:        "401 body containing quota words is not quota",
			snapshot:    runtimeSnapshot(now, "error", "quota exhausted unauthorized provider prose", true, now.Add(time.Minute), 401),
			want:        Suspect,
			reason:      ReasonUnauthorizedRequest,
			confirm:     ConfirmationUnauthorized,
			confirmAs:   ReauthRequired,
			forbidQuota: true,
		},
		{
			name:     "active runtime overrides stale 401",
			snapshot: runtimeSnapshot(now, "active", "unauthorized", false, time.Time{}, 401),
			want:     Healthy,
		},
		{
			name:     "canonical refresh unauthorized is definitive",
			snapshot: runtimeSnapshot(now, "error", "unauthorized", true, time.Time{}, 0),
			want:     ReauthRequired,
			reason:   ReasonUnauthorizedRefreshFailed,
		},
		{
			name:      "request unauthorized inside retry window needs confirmation",
			snapshot:  runtimeSnapshot(now, "error", "unauthorized", true, now.Add(time.Minute), 401),
			want:      Suspect,
			reason:    ReasonUnauthorizedRequest,
			confirm:   ConfirmationUnauthorized,
			confirmAs: ReauthRequired,
		},
		{
			name:      "structured 503 is transient",
			snapshot:  runtimeSnapshot(now, "error", `{"raw":"do not use"}`, true, now.Add(time.Minute), 503),
			want:      Suspect,
			reason:    ReasonHTTP503,
			confirm:   ConfirmationTransient,
			confirmAs: CredentialDown,
		},
		{
			name:     "unknown future cooldown is nonpromotable",
			snapshot: runtimeSnapshot(now, "error", `{"raw":"unknown"}`, true, now.Add(time.Minute), 0),
			want:     Suspect,
			reason:   ReasonCooldownActive,
		},
		{
			name:     "expired cooldown requires confirmed recovery",
			snapshot: runtimeSnapshot(now, "error", "", true, now.Add(-time.Minute), 0),
			want:     Suspect,
			reason:   ReasonCooldownRecoveryUnconfirmed,
		},
		{
			name:      "ambiguous 403",
			snapshot:  runtimeSnapshot(now, "error", "payment_required", true, now.Add(time.Minute), 403),
			want:      Suspect,
			reason:    ReasonHTTP403,
			confirm:   ConfirmationTransient,
			confirmAs: CredentialDown,
		},
		{
			name:     "canonical quota state",
			snapshot: runtimeSnapshot(now, "error", "quota exhausted", true, now.Add(time.Hour), 0),
			want:     QuotaLimited,
			reason:   ReasonQuotaExhausted,
		},
		{
			name:     "invalid grant",
			snapshot: runtimeSnapshot(now, "error", "invalid_grant", true, time.Time{}, 0),
			want:     ReauthRequired,
			reason:   ReasonInvalidGrant,
		},
		{
			name: "operator disabled",
			snapshot: func() RuntimeSnapshot {
				s := runtimeSnapshot(now, "disabled", "", false, time.Time{}, 0)
				s.Disabled = true
				return s
			}(),
			want:   Disabled,
			reason: ReasonOperatorDisabled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ClaudeClassifier().Classify(test.snapshot)
			if got.State != test.want || got.ReasonCode != test.reason || got.Confirmation != test.confirm || got.ConfirmAs != test.confirmAs {
				t.Fatalf("observation=%+v, want state=%q reason=%q confirmation=%q target=%q", got, test.want, test.reason, test.confirm, test.confirmAs)
			}
			if test.forbidQuota && got.State == QuotaLimited {
				t.Fatal("401 was incorrectly classified as quota")
			}
			if strings.Contains(got.Detail, "provider prose") || strings.Contains(string(got.ReasonCode), "provider prose") {
				t.Fatalf("raw provider text escaped classifier: %+v", got)
			}
		})
	}
}

func TestCanonicalRuntimeMessageRejectsFreeFormBodies(t *testing.T) {
	for input, want := range map[string]RuntimeMessageCode{
		"unauthorized":             RuntimeMessageUnauthorized,
		" INVALID_GRANT ":          RuntimeMessageInvalidGrant,
		"invalid token":            RuntimeMessageInvalidToken,
		"quota exhausted":          RuntimeMessageQuotaExhausted,
		"transient upstream error": RuntimeMessageTransientError,
		"request failed":           RuntimeMessageRequestFailed,
	} {
		if got := CanonicalRuntimeMessage(input); got != want {
			t.Fatalf("CanonicalRuntimeMessage(%q)=%q want %q", input, got, want)
		}
	}
	for _, input := range []string{
		`{"error":"unauthorized"}`,
		"prefix unauthorized",
		"unauthorized suffix",
		"weekly limit exhausted",
		"5-hour limit reached",
		"arbitrary provider body",
	} {
		if got := CanonicalRuntimeMessage(input); got != RuntimeMessageUnknown {
			t.Fatalf("free-form body %q mapped to %q", input, got)
		}
	}
}

func TestNormalizeReasonCodeRejectsLegacyRawText(t *testing.T) {
	if got := NormalizeReasonCode(`{"secret":"provider body"}`); got != ReasonUnclassifiedCredentialError {
		t.Fatalf("legacy raw reason normalized to %q", got)
	}
	if got := NormalizeReasonCode(string(ReasonHTTP503)); got != ReasonHTTP503 {
		t.Fatalf("known reason normalized to %q", got)
	}
}
