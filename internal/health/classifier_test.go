package health

import (
	"testing"
	"time"
)

func TestClassifierRequiredCases(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		snapshot RuntimeSnapshot
		want     State
		reason   string
	}{
		{
			name:     "healthy enabled auth",
			snapshot: RuntimeSnapshot{Provider: "claude", Status: "active", ObservedAt: now},
			want:     Healthy,
		},
		{
			name:     "normal quota 429",
			snapshot: RuntimeSnapshot{Provider: "codex", Status: "error", Unavailable: true, LastHTTPStatus: 429, ObservedAt: now},
			want:     QuotaLimited,
		},
		{
			name:     "quota future recovery",
			snapshot: RuntimeSnapshot{Provider: "claude", Unavailable: true, QuotaExceeded: true, NextRecoverAt: now.Add(time.Hour), ObservedAt: now},
			want:     QuotaLimited,
		},
		{
			name:     "five hour limit",
			snapshot: RuntimeSnapshot{Provider: "claude", Unavailable: true, StatusMessage: "5-hour limit reached", ObservedAt: now},
			want:     QuotaLimited,
			reason:   "five_hour_limit",
		},
		{
			name:     "weekly limit",
			snapshot: RuntimeSnapshot{Provider: "codex", Unavailable: true, StatusMessage: "weekly limit exhausted", ObservedAt: now},
			want:     QuotaLimited,
			reason:   "weekly_limit",
		},
		{
			name:     "fable model scoped limit",
			snapshot: RuntimeSnapshot{Provider: "claude", Unavailable: true, StatusMessage: "Fable model-scoped quota exhausted", ObservedAt: now},
			want:     QuotaLimited,
			reason:   "model_quota",
		},
		{
			name:     "invalid grant",
			snapshot: RuntimeSnapshot{Provider: "claude", Status: "error", Unavailable: true, StatusMessage: "invalid_grant", ObservedAt: now},
			want:     ReauthRequired,
			reason:   "invalid_grant",
		},
		{
			name:     "status error unauthorized unavailable",
			snapshot: RuntimeSnapshot{Provider: "codex", Status: "error", Unavailable: true, StatusMessage: "unauthorized", ObservedAt: now},
			want:     ReauthRequired,
			reason:   "unauthorized",
		},
		{
			name:     "single 401 followed by refresh success",
			snapshot: RuntimeSnapshot{Provider: "claude", Status: "active", Unavailable: false, LastHTTPStatus: 401, LastErrorCode: "unauthorized", ObservedAt: now},
			want:     Healthy,
		},
		{
			name:     "one 503",
			snapshot: RuntimeSnapshot{Provider: "codex", Status: "error", Unavailable: true, LastHTTPStatus: 503, ObservedAt: now},
			want:     Suspect,
			reason:   "http_503",
		},
		{
			name:     "ambiguous 403",
			snapshot: RuntimeSnapshot{Provider: "claude", Status: "error", Unavailable: true, LastHTTPStatus: 403, StatusMessage: "payment_required", ObservedAt: now},
			want:     Suspect,
			reason:   "ambiguous_403",
		},
		{
			name:     "explicit invalid token 403",
			snapshot: RuntimeSnapshot{Provider: "claude", Status: "error", Unavailable: true, LastHTTPStatus: 403, LastErrorCode: "invalid_token", ObservedAt: now},
			want:     ReauthRequired,
			reason:   "invalid_token",
		},
		{
			name:     "operator disabled",
			snapshot: RuntimeSnapshot{Provider: "codex", Disabled: true, Status: "disabled", ObservedAt: now},
			want:     Disabled,
			reason:   "operator_disabled",
		},
		{
			name:     "stale historic error",
			snapshot: RuntimeSnapshot{Provider: "claude", Status: "active", LastErrorCode: "invalid_grant", LastHTTPStatus: 401, ObservedAt: now},
			want:     Healthy,
		},
		{
			name:     "expired unavailability without error",
			snapshot: RuntimeSnapshot{Provider: "codex", Status: "active", Unavailable: true, NextRetryAfter: now.Add(-time.Minute), ObservedAt: now},
			want:     Healthy,
		},
		{
			name:     "network timeout",
			snapshot: RuntimeSnapshot{Provider: "claude", Status: "error", Unavailable: true, LastErrorMessage: "network timeout", ObservedAt: now},
			want:     Suspect,
			reason:   "timeout",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classifier := Classifiers()[test.snapshot.Provider]
			got := classifier.Classify(test.snapshot)
			if got.State != test.want {
				t.Fatalf("state = %q, want %q; observation=%+v", got.State, test.want, got)
			}
			if test.reason != "" && got.ReasonCode != test.reason {
				t.Fatalf("reason = %q, want %q", got.ReasonCode, test.reason)
			}
		})
	}
}

func TestQuotaEvidenceWinsOverUnavailableAndUnauthorizedText(t *testing.T) {
	now := time.Now()
	got := ClaudeClassifier().Classify(RuntimeSnapshot{
		Provider:       "claude",
		Status:         "error",
		StatusMessage:  "unauthorized after quota exhausted",
		Unavailable:    true,
		QuotaExceeded:  true,
		NextRecoverAt:  now.Add(time.Hour),
		LastHTTPStatus: 401,
		ObservedAt:     now,
	})
	if got.State != QuotaLimited {
		t.Fatalf("state = %q, want quota_limited", got.State)
	}
}
