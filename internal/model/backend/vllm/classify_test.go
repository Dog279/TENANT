package vllm

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"tenant/internal/model"
)

// mockResponse builds an *http.Response with the given status + body
// — just enough for classifyHTTPError to consume.
func mockResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
}

// TestClassifyHTTPError_BillingShaped429_BecomesInsufficientBalance —
// the load-bearing fix from the Z.ai investigation 2026-05-25. Z.ai
// returns 429 with body containing "Insufficient balance" for an
// out-of-credits situation, which is meaningfully different from a
// real rate limit (no backoff strategy will help — only recharging).
// The classifier MUST split these.
func TestClassifyHTTPError_BillingShaped429_BecomesInsufficientBalance(t *testing.T) {
	// Verbatim Z.ai response shape observed live.
	body := `{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`
	err := classifyHTTPError(mockResponse(429, body))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, model.ErrInsufficientBalance) {
		t.Errorf("billing-shaped 429 should map to ErrInsufficientBalance; got: %v", err)
	}
	if errors.Is(err, model.ErrRateLimited) {
		t.Errorf("billing-shaped 429 should NOT also be classified as ErrRateLimited; got: %v", err)
	}
	// Operator-facing text should keep the upstream message so the
	// operator can act (recharge vs check plan).
	if !strings.Contains(err.Error(), "Insufficient balance") {
		t.Errorf("error text should preserve upstream message; got: %v", err)
	}
}

// TestClassifyHTTPError_GenuineRateLimit_StaysRateLimited — drift
// guard against over-aggressive billing detection. A real 429 with a
// "too many requests" / vague body stays as ErrRateLimited (correct +
// retryable). We only steal 429 from rate-limit when the body clearly
// names billing.
func TestClassifyHTTPError_GenuineRateLimit_StaysRateLimited(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"openai shape", `{"error":{"type":"rate_limit_exceeded","message":"Too many requests"}}`},
		{"plain text", `Too Many Requests`},
		{"empty body", ``},
		{"vague tps", `{"error":"requests per minute exceeded"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := classifyHTTPError(mockResponse(429, c.body))
			if !errors.Is(err, model.ErrRateLimited) {
				t.Errorf("genuine 429 should map to ErrRateLimited; got: %v", err)
			}
			if errors.Is(err, model.ErrInsufficientBalance) {
				t.Errorf("genuine rate limit must NOT classify as insufficient balance; got: %v", err)
			}
		})
	}
}

// TestClassifyHTTPError_BillingPatternCoverage — table-driven coverage
// of every marker isBillingError matches against. Each is a real
// pattern observed in the wild on at least one OpenAI-compat provider.
func TestClassifyHTTPError_BillingPatternCoverage(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"z.ai insufficient balance", `{"error":{"code":"1113","message":"Insufficient balance"}}`},
		{"underscored variant", `{"error":{"code":"insufficient_balance"}}`},
		{"z.ai no resource package", `{"error":{"message":"no resource package available"}}`},
		{"please recharge", `please recharge your account`},
		{"openai quota exhausted", `{"error":{"code":"quota_exhausted"}}`},
		{"openai quota exceeded", `quota exceeded for this month`},
		{"billing keyword", `{"error":{"type":"billing_error"}}`},
		{"out of credits", `you are out of credits`},
		{"openai insufficient_quota", `{"error":{"code":"insufficient_quota"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := classifyHTTPError(mockResponse(429, c.body))
			if !errors.Is(err, model.ErrInsufficientBalance) {
				t.Errorf("body %q should match a billing pattern; got: %v", c.body, err)
			}
		})
	}
}

// TestClassifyHTTPError_OtherStatusesUnaffected — drift guard. The
// billing-detection branch must not steal 400 / 401 / 500 etc. Those
// status codes have their own classification paths.
func TestClassifyHTTPError_OtherStatusesUnaffected(t *testing.T) {
	// 400 with "context" still maps to ErrContextOverflow even if it
	// also happens to contain "billing" (unlikely but defensive).
	err := classifyHTTPError(mockResponse(400, "context window exceeded"))
	if !errors.Is(err, model.ErrContextOverflow) {
		t.Errorf("400+context should stay ErrContextOverflow; got: %v", err)
	}
	// 401 with billing keyword still maps to ErrInvalidRequest — auth
	// failures aren't billing problems even if message mentions it.
	err = classifyHTTPError(mockResponse(401, "billing token revoked"))
	if !errors.Is(err, model.ErrInvalidRequest) {
		t.Errorf("401 should stay ErrInvalidRequest regardless of body; got: %v", err)
	}
	// 500 with billing keyword still maps to ErrInternal.
	err = classifyHTTPError(mockResponse(500, "billing service unavailable"))
	if !errors.Is(err, model.ErrInternal) {
		t.Errorf("500 should stay ErrInternal; got: %v", err)
	}
}
