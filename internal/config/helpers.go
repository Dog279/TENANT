package config

import (
	"fmt"
	"strings"
	"time"

	"tenant/internal/model"
)

// ModelConfig converts the persisted settings to the router's HealthConfig. A
// nil receiver or a non-positive threshold is the zero value, which is off.
func (h *HealthGating) ModelConfig() model.HealthConfig {
	if h == nil || h.SlowThresholdMS <= 0 {
		return model.HealthConfig{}
	}
	cfg := model.HealthConfig{
		SlowThreshold: time.Duration(h.SlowThresholdMS) * time.Millisecond,
		MinSamples:    h.MinSamples,
		Alpha:         h.Alpha,
	}
	if h.CooldownMS > 0 {
		cfg.Cooldown = time.Duration(h.CooldownMS) * time.Millisecond
	}
	return cfg
}

// Problems lists configured values the engine will replace with its default,
// so doctor can say so instead of the gate quietly behaving differently.
func (h *HealthGating) Problems() []string {
	if h == nil || h.SlowThresholdMS <= 0 {
		return nil
	}
	var out []string
	if h.MinSamples < 0 {
		out = append(out, fmt.Sprintf("min_samples %d is negative; using 5", h.MinSamples))
	}
	if h.Alpha < 0 || h.Alpha > 1 {
		out = append(out, fmt.Sprintf("alpha %g is outside (0,1]; using 0.3", h.Alpha))
	}
	if h.CooldownMS < 0 {
		out = append(out, fmt.Sprintf("cooldown_ms %d is negative; using 30s", h.CooldownMS))
	}
	return out
}

// LooksLikeURL is a lenient gate for "this string is plausibly an HTTP
// URL the operator typed." Doesn't validate the full URL — just enough
// to catch "I pasted my API key as the endpoint" mistakes. False
// positives are acceptable (a malformed URL still hits the backend and
// fails with a clear network error); false negatives (rejecting a real
// URL) are the danger we minimize.
func LooksLikeURL(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// SafeDisplayEndpoint is the LAST-LINE-OF-DEFENSE renderer for any
// endpoint string that's about to be shown in operator-facing output
// (the /model list, the CLI listing, transcripts, etc.). If the input
// doesn't look like an HTTP URL — meaning something malformed slipped
// past the entry-point validation OR the operator loaded an old
// pre-fix config — we replace it with a redacted marker instead of
// echoing what might be a secret.
//
// Defense-in-depth. The actual fix is preventing the bad value from
// reaching config in the first place (LooksLikeURL in AddModel + the
// load-time guard). This catches anything that did anyway.
func SafeDisplayEndpoint(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(unset)"
	}
	if LooksLikeURL(s) {
		return s
	}
	// The claudecode discovery sentinel is a known non-secret non-URL —
	// redacting it would just confuse the /model list.
	if s == ClaudeCodeAutoEndpoint {
		return s
	}
	return "[redacted — non-URL endpoint, see `tenant doctor`]"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
