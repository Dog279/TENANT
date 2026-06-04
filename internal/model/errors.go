package model

import "errors"

// Typed errors the agent runtime distinguishes for retry / degrade / surface
// decisions. Wrap underlying transport / parse errors with %w so errors.Is
// continues to work on the chain.
var (
	// ErrEndpointDown means the backend HTTP endpoint is unreachable
	// (connection refused, DNS failure, etc.). Caller may failover.
	ErrEndpointDown = errors.New("model: endpoint down")

	// ErrContextOverflow means the prompt exceeded the model's context
	// window. The memory layer should compact and retry. Surfaced from
	// backends that report it explicitly; preferable to detect upfront
	// via TokenCount + Profile.OperationalContextBudget.
	ErrContextOverflow = errors.New("model: context window exceeded")

	// ErrRateLimited means the backend asked us to slow down. Caller
	// should back off; vLLM is unlikely to emit this locally but the
	// OpenAI-compat wire format reserves 429 for it.
	ErrRateLimited = errors.New("model: rate limited")

	// ErrInsufficientBalance means the upstream provider returned a
	// billing/quota failure dressed up as some other status code. Z.ai
	// in particular returns HTTP 429 with body
	// `{"error":{"code":"1113","message":"Insufficient balance ..."}}`
	// for "out of credits" — which is meaningfully different from rate
	// limiting (backoff won't help; only the operator topping up will).
	// Surfaced as its own sentinel so operator-facing messages can be
	// accurate and so callers can short-circuit retry loops.
	ErrInsufficientBalance = errors.New("model: insufficient balance / quota exhausted (recharge or check plan)")

	// ErrCancelled means the call was cancelled (typically via ctx).
	// Distinct from a network error so the runtime can tell user-aborted
	// from server-aborted.
	ErrCancelled = errors.New("model: cancelled")

	// ErrInvalidRequest means the backend rejected the request shape
	// (4xx other than 429). Usually a programmer error.
	ErrInvalidRequest = errors.New("model: invalid request")

	// ErrInternal wraps unexpected backend failures.
	ErrInternal = errors.New("model: internal error")

	// ErrRoleNotRegistered means the Router has no Profile bound to
	// the requested Role.
	ErrRoleNotRegistered = errors.New("model: role not registered")

	// ErrDuplicateProfile means two profiles share the same ID across
	// embedded defaults and disk overrides.
	ErrDuplicateProfile = errors.New("model: duplicate profile ID")

	// ErrInvalidProfile means a profile failed schema validation at
	// registry load.
	ErrInvalidProfile = errors.New("model: invalid profile")
)
