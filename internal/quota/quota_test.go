package quota

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/protocol"
)

type fakeDoer struct {
	request  protocol.HostHTTPRequest
	response protocol.HostHTTPResponse
	err      error
}

func (f *fakeDoer) HTTPDo(_ context.Context, request protocol.HostHTTPRequest) (protocol.HostHTTPResponse, error) {
	f.request = request
	return f.response, f.err
}

var now = time.Date(2026, time.September, 4, 22, 0, 0, 0, time.UTC)

func TestClaudeReadsSevenDayWindowOnly(t *testing.T) {
	body := `{"five_hour":{"utilization":99.0,"resets_at":"2026-09-04T23:30:00+00:00"},"seven_day":{"utilization":14.0,"resets_at":"2026-09-09T10:00:00.438310+00:00","limit_dollars":null},"seven_day_opus":{"utilization":100.0},"limits":[]}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "claude", []byte(`{"access_token":"tok-secret","refresh_token":"r"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Percent != 14 || observation.Provider != "claude" || !observation.ObservedAt.Equal(now) {
		t.Fatalf("observation=%+v", observation)
	}
	if want := time.Date(2026, time.September, 9, 10, 0, 0, 438310000, time.UTC); !observation.ResetAt.Equal(want) {
		t.Fatalf("reset=%s want %s", observation.ResetAt, want)
	}
	if doer.request.URL != claudeUsageURL || doer.request.Headers["Authorization"][0] != "Bearer tok-secret" || doer.request.Headers["anthropic-beta"][0] != claudeOAuthBeta {
		t.Fatalf("request=%+v", doer.request)
	}
}

func TestCodexSelectsExactWeeklyWindowAndUnixReset(t *testing.T) {
	body := `{"plan_type":"pro","rate_limit":{"allowed":true,"primary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":380992,"reset_at":1788913971},"secondary_window":{"used_percent":90,"limit_window_seconds":18000,"reset_at":1788500000}},"additional_rate_limits":[{"limit_name":"code_review","rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800}}}]}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok","account_id":"acct-1"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Percent != 25 || !observation.ResetAt.Equal(time.Unix(1788913971, 0).UTC()) {
		t.Fatalf("observation=%+v", observation)
	}
	if doer.request.Headers["Chatgpt-Account-Id"][0] != "acct-1" || !strings.HasPrefix(doer.request.Headers["User-Agent"][0], "codex_cli_rs/") {
		t.Fatalf("request headers=%v", doer.request.Headers)
	}
}

func TestCodexResolvesAccountIDFromIDTokenAndRelativeReset(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-jwt"}}`))
	auth := `{"tokens":{"access_token":"tok","id_token":"h.` + claims + `.s"}}`
	body := `{"rate_limits":{"primary":{"usedPercent":97.5,"windowMinutes":10080,"resetAfterSeconds":3600.4}}}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "codex", []byte(auth), now)
	if err != nil {
		t.Fatal(err)
	}
	if doer.request.Headers["Chatgpt-Account-Id"][0] != "acct-jwt" {
		t.Fatalf("account id not resolved from id_token: %v", doer.request.Headers)
	}
	if observation.Percent != 97.5 || !observation.ResetAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("observation=%+v", observation)
	}
}

func TestCodexWithoutAccountIDFailsBeforeHTTP(t *testing.T) {
	doer := &fakeDoer{}
	_, err := Fetch(context.Background(), doer, "codex", []byte(`{"access_token":"tok"}`), now)
	if !errors.Is(err, ErrNoAccountID) || doer.request.URL != "" {
		t.Fatalf("err=%v request=%+v", err, doer.request)
	}
}

func TestXAIReadsUnifiedCreditsAndPeriodEnd(t *testing.T) {
	body := `{"config":{"creditUsagePercent":28.0,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-09-01T19:33:46.436099+00:00","end":"2026-09-08T19:33:46.436099+00:00"},"isUnifiedBillingUser":true,"productUsage":[{"product":"BUILD","usagePercent":28.0}]}}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "xai", []byte(`{"access_token":"tok","sub":"user-123","id_token":"x"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Percent != 28 || !observation.ResetAt.Equal(time.Date(2026, time.September, 8, 19, 33, 46, 436099000, time.UTC)) {
		t.Fatalf("observation=%+v", observation)
	}
	if doer.request.URL != xaiBillingURL || doer.request.Headers["X-XAI-Token-Auth"][0] != xaiTokenAuthHeader || doer.request.Headers["x-userid"][0] != "user-123" {
		t.Fatalf("request=%+v", doer.request)
	}
}

func TestXAIOmittedPercentIsZeroAndFallsBackToBillingPeriodEnd(t *testing.T) {
	// proto3 JSON omits zero-valued scalars: an absent creditUsagePercent in a
	// current period is reported by the CLI as 0% used. We require the field to
	// exist so an unrelated response shape cannot be mistaken for 0%.
	body := `{"config":{"credit_usage_percent":0,"billingPeriodEnd":"2026-09-08T00:00:00Z"}}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "xai", []byte(`{"access_token":"tok"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Percent != 0 || !observation.ResetAt.Equal(time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("observation=%+v", observation)
	}
	if _, ok := doer.request.Headers["x-userid"]; ok {
		t.Fatalf("x-userid must be omitted when sub is absent: %v", doer.request.Headers)
	}
}

func TestErrorsAreStaticAndNeverEchoBodiesOrTokens(t *testing.T) {
	secret := "SECRET-TOKEN-VALUE"
	cases := []struct {
		name     string
		provider string
		auth     string
		response protocol.HostHTTPResponse
		err      error
		want     error
	}{
		{"unsupported", "gemini", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{}, nil, ErrUnsupportedProvider},
		{"no token", "claude", `{"refresh_token":"x"}`, protocol.HostHTTPResponse{}, nil, ErrNoAccessToken},
		{"http 401", "claude", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{StatusCode: 401, Body: []byte(`{"error":"bad ` + secret + `"}`)}, nil, HTTPStatusError{StatusCode: 401}},
		{"invalid json", "claude", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`not json ` + secret)}, nil, ErrInvalidResponse},
		{"trailing json", "claude", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"seven_day":{"utilization":1}} {}`)}, nil, ErrInvalidResponse},
		{"missing window", "claude", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"five_hour":{"utilization":1}}`)}, nil, ErrNoWeeklyWindow},
		{"non weekly codex", "codex", `{"access_token":"` + secret + `","account_id":"a"}`, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000}}}`)}, nil, ErrNoWeeklyWindow},
		{"xai no config", "xai", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(`{"error":"` + secret + `"}`)}, nil, ErrNoWeeklyWindow},
		{"transport", "claude", `{"access_token":"` + secret + `"}`, protocol.HostHTTPResponse{}, errors.New("dial failed"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &fakeDoer{response: tc.response, err: tc.err}
			_, err := Fetch(context.Background(), doer, tc.provider, []byte(tc.auth), now)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) && err.Error() != tc.want.Error() {
				t.Fatalf("err=%v want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked secret: %v", err)
			}
		})
	}
}

func TestPercentIsClamped(t *testing.T) {
	body := `{"seven_day":{"utilization":143.2,"resets_at":"bad"}}`
	doer := &fakeDoer{response: protocol.HostHTTPResponse{StatusCode: 200, Body: []byte(body)}}
	observation, err := Fetch(context.Background(), doer, "claude", []byte(`{"access_token":"tok"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Percent != 100 || !observation.ResetAt.IsZero() {
		t.Fatalf("observation=%+v", observation)
	}
}
