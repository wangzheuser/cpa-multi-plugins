package main

// model_provider_fault_test.go — pins the issue #80 fix.
//
// The reporter's production table (2026-11-07, scheduler_mode: credits +
// scheduler_priority_reclaim): an upstream model-level fault — HTTP 500 with
// {"code":11134,"msg":"the model provider is temporarily unavailable, please
// retry later or switch to another model"} — matched none of the five
// recordUpstreamFailure routes, so the (credential, model) pair stayed "live"
// and priority reclaim re-served the same dead pair 5m18s apart while healthy
// candidates never rotated in. The fix routes the family into the #35
// cooldown table at the conservative unknown window (60 s, re-probed on
// expiry); a declared reset instant in the envelope wins when parsable.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// issue80FaultPayload is row 1 of the reporter's table (internal_error
// variant); issue80ServiceUnavailablePayload is row 3.
const issue80FaultPayload = `{"code":11134,"msg":"the model provider is temporarily unavailable, please retry later or switch to another model","extError":{"code":"internal_error"}}`

const issue80ServiceUnavailablePayload = `{"code":11134,"msg":"the model provider is temporarily unavailable, please retry later or switch to another model","extError":{"code":"service_unavailable"}}`

// TestIsModelProviderFaultMatrix locks the classifier: the business code is
// the evidence, the status guard keeps 4xx shapes in the 11102/11115 lane,
// and the wording conjunction never fires on the 50001 "service temporarily
// unavailable" frames the #35 matrix already cools as upstream_error.
func TestIsModelProviderFaultMatrix(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		payload string
		want    bool
	}{
		{"production payload on 500", http.StatusInternalServerError, issue80FaultPayload, true},
		{"service_unavailable variant on 500", http.StatusInternalServerError, issue80ServiceUnavailablePayload, true},
		{"family on a 503 gateway error", http.StatusServiceUnavailable, issue80FaultPayload, true},
		{"string code spelling", http.StatusInternalServerError, `{"code":"11134","msg":"the model provider is temporarily unavailable"}`, true},
		{"hyphenated code spelling (#75 lesson)", http.StatusInternalServerError, `{"code":"11-134","msg":"the model provider is temporarily unavailable"}`, true},
		{"envelope embedded in a frame error text (status 0)", 0, "workbuddy upstream error: " + issue80FaultPayload, true},
		{"bare envelope with status 0 (sync-collector shape)", 0, issue80FaultPayload, true},
		{"wording conjunction (code + model provider)", http.StatusInternalServerError, `upstream said 11134 and the model provider is down`, true},
		{"4xx stays in the 11102/11115 lane", http.StatusBadRequest, issue80FaultPayload, false},
		{"429 stays in the 6004 lane", http.StatusTooManyRequests, issue80FaultPayload, false},
		{"500 without the family code (50001 frame)", http.StatusInternalServerError, `{"code":50001,"msg":"service temporarily unavailable"}`, false},
		{"500 with the 11102 code", http.StatusInternalServerError, `{"code":11102,"msg":"model [x] service info not found"}`, false},
		{"6004 payload is not this family", http.StatusInternalServerError, modelRateLimitSamplePayload, false},
		{"bare error event frame", 0, "workbuddy upstream error event", false},
		{"empty payload", http.StatusInternalServerError, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isModelProviderFault(tc.status, tc.payload); got != tc.want {
				t.Fatalf("isModelProviderFault(%d, %.60s…) = %v, want %v", tc.status, tc.payload, got, tc.want)
			}
		})
	}
}

// TestProviderFaultRouterTier: the exact production payload routes into the
// #35 table at the 60 s unknown window — not the 300 s rate-limit tier (a
// fault is not a throttle), and with its own reason so the merged operator
// listing (#77) can name it.
func TestProviderFaultRouterTier(t *testing.T) {
	withFrozenCooldown(t, time.Date(2026, 11, 7, 15, 6, 55, 0, time.UTC))
	recordUpstreamFailure("workbuddy-intl-1610d8fd", nil, "deepseek-v4.1-flash-sg", http.StatusInternalServerError, issue80FaultPayload)

	until := modelCoolingUntil("workbuddy-intl-1610d8fd", "deepseek-v4.1-flash-sg")
	if until.IsZero() {
		t.Fatal("the faulted pair must hold a cooldown window")
	}
	cooldownMu.Lock()
	entry := cooldownTable[modelCooldownKey{AuthID: "workbuddy-intl-1610d8fd", ModelID: "deepseek-v4.1-flash-sg"}]
	cooldownMu.Unlock()
	if entry.Reason != cooldownReasonProviderFault {
		t.Fatalf("reason = %q, want %q", entry.Reason, cooldownReasonProviderFault)
	}
	if got := entry.Until.Sub(cooldownNowFn()); got != modelCooldownUnknown {
		t.Fatalf("window = %v, want the conservative unknown window %v", got, modelCooldownUnknown)
	}
	// The reporter's exact pain: 5m18s between re-selections of the same dead
	// pair — a 60 s window rotates the third request to the next candidate.
	if modelCooldownUnknown >= 5*time.Minute {
		t.Fatal("window must be shorter than the observed re-selection gap")
	}
	// Other models on the account and other credentials are untouched.
	if modelIsCooling("workbuddy-intl-1610d8fd", "deepseek-v4.1") {
		t.Fatal("other models on the same credential must not cool")
	}
	if modelIsCooling("workbuddy-intl-ca64800b", "deepseek-v4.1-flash-sg") {
		t.Fatal("other credentials must not cool")
	}
}

// TestProviderFaultSchedulerRotation: the issue #80 scenario end-to-end —
// priority reclaim serves the first live candidate in host file order, the
// faulted pair is that first candidate, and the pick must rotate to the
// healthy one instead of re-serving the dead pair.
func TestProviderFaultSchedulerRotation(t *testing.T) {
	withFrozenCooldown(t, time.Date(2026, 11, 7, 15, 21, 41, 0, time.UTC))
	resetActiveAuth(t)
	setActiveAuthID("healthy")
	recordUpstreamFailure("dead", nil, "deepseek-v4.1-flash", http.StatusInternalServerError, issue80FaultPayload)

	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Model:    "deepseek-v4.1-flash",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "dead", Provider: providerName, Status: "active"},
			{ID: "healthy", Provider: providerName, Status: "active"},
		},
	}))
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "healthy" {
		t.Fatalf("faulted head candidate must be skipped, got handled=%v auth=%q", resp.Handled, resp.AuthID)
	}

	// The catalog agrees with the pick (handleModelForAuth contract).
	models := []pluginapi.ModelInfo{{ID: "deepseek-v4.1-flash"}, {ID: "kimi-k3"}}
	if got := modelIDsOf(filterCoolingModels("dead", models)); len(got) != 1 || got[0] != "kimi-k3" {
		t.Fatalf("catalog must drop the faulted model for the account, got %v", got)
	}
	if got := modelIDsOf(filterCoolingModels("healthy", models)); len(got) != 2 {
		t.Fatalf("healthy account keeps the model, got %v", got)
	}

	// The fast-fail copy names the family and never carries 429 (hosts
	// re-read messages as credential quota).
	fastErr := modelCooldownError("dead", nil, "deepseek-v4.1-flash")
	if fastErr == nil {
		t.Fatal("active window must fast-fail")
	}
	if !strings.Contains(fastErr.Error(), "11134") {
		t.Fatalf("copy must name code 11134: %v", fastErr)
	}
	if strings.Contains(fastErr.Error(), "429") {
		t.Fatalf("copy must never carry 429: %v", fastErr)
	}
}

// TestProviderFaultDeclaredResetWins: a declared reset instant in the
// envelope beats the fixed 60 s window (the issue's stated preference). No
// such variant has been observed — this pins the path for the day one ships.
func TestProviderFaultDeclaredResetWins(t *testing.T) {
	// Frozen now 02:00 UTC = 10:00 UTC+8; declared reset 10:30 UTC+8.
	withFrozenCooldown(t, time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC))
	payload := `{"code":11134,"msg":"the model provider is temporarily unavailable, will 将在 2026-11-08 10:30:00 UTC+8 重置","extError":{"code":"internal_error"}}`
	recordUpstreamFailure("acct-r", nil, "m", http.StatusInternalServerError, payload)

	cooldownMu.Lock()
	entry := cooldownTable[modelCooldownKey{AuthID: "acct-r", ModelID: "m"}]
	cooldownMu.Unlock()
	wantUntil := time.Date(2026, 11, 8, 2, 30, 0, 0, time.UTC)
	if !entry.Until.Equal(wantUntil) {
		t.Fatalf("declared reset must win: until = %v, want %v", entry.Until, wantUntil)
	}
	if entry.Reason != cooldownReasonProviderFault {
		t.Fatalf("reason = %q, want %q", entry.Reason, cooldownReasonProviderFault)
	}
}

// TestProviderFaultNeverShortens: a later, smaller fault window cannot
// release an earlier, larger hold — and the larger hold keeps its reason.
func TestProviderFaultNeverShortens(t *testing.T) {
	withFrozenCooldown(t, time.Now())
	// An active 300 s rate-limit hold survives a later 60 s fault.
	recordUpstreamFailure("acct-s", nil, "m", http.StatusTooManyRequests, "upstream rate limit exceeded")
	recordUpstreamFailure("acct-s", nil, "m", http.StatusInternalServerError, issue80FaultPayload)
	cooldownMu.Lock()
	entry := cooldownTable[modelCooldownKey{AuthID: "acct-s", ModelID: "m"}]
	cooldownMu.Unlock()
	if entry.Reason != cooldownReasonRateLimit || entry.Until.Sub(cooldownNowFn()) != modelCooldownRateLimit {
		t.Fatalf("larger hold must stay: reason=%q window=%v", entry.Reason, entry.Until.Sub(cooldownNowFn()))
	}
	// The reverse: a declared-reset fault hold survives a later default fault.
	withFrozenCooldown(t, time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC))
	payload := `{"code":11134,"msg":"temporarily unavailable, 将在 2026-11-08 10:30:00 UTC+8 重置"}`
	recordUpstreamFailure("acct-t", nil, "m", http.StatusInternalServerError, payload)
	recordUpstreamFailure("acct-t", nil, "m", http.StatusInternalServerError, issue80FaultPayload)
	cooldownMu.Lock()
	entry2 := cooldownTable[modelCooldownKey{AuthID: "acct-t", ModelID: "m"}]
	cooldownMu.Unlock()
	wantUntil := time.Date(2026, 11, 8, 2, 30, 0, 0, time.UTC)
	if !entry2.Until.Equal(wantUntil) {
		t.Fatalf("declared-reset hold must survive the default window: until = %v, want %v", entry2.Until, wantUntil)
	}
}

// TestTranslateChatUpstreamErrorProviderFault: the raw "upstream 500: {...}"
// pass-through is replaced by copy that names the family, the model scoping
// and the auto-rotation; upstreamStatusError keeps the error plain (no
// statusError wrapper) so no host version cools the whole credential on a
// model-scoped fault — the 6004 policy, applied to its 5xx sibling.
func TestTranslateChatUpstreamErrorProviderFault(t *testing.T) {
	err := translateChatUpstreamErrorFull(http.StatusInternalServerError, issue80FaultPayload, nil, nil)
	if err == nil {
		t.Fatal("translation must exist")
	}
	msg := err.Error()
	if !strings.Contains(msg, "11134") || !strings.Contains(msg, "切换其他模型") {
		t.Fatalf("copy must name the code and upstream's advice: %v", msg)
	}
	if strings.HasPrefix(msg, "upstream 500") {
		t.Fatalf("raw pass-through shape must be replaced: %v", msg)
	}
	if !strings.Contains(msg, "raw:") {
		t.Fatalf("raw payload must ride along for diagnosis: %v", msg)
	}

	plain := translateChatUpstreamErrorFull(http.StatusInternalServerError, issue80FaultPayload, nil, nil)
	if wrapped := upstreamStatusError(http.StatusInternalServerError, issue80FaultPayload, plain); wrapped != plain {
		t.Fatalf("model-scoped fault must stay plain, got %#v", wrapped)
	}
}
