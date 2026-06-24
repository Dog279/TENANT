package crm

import (
	"fmt"
	"strings"
	"time"
)

// Config opens a Service over the operator's crm-tool binary.
type Config struct {
	Path    string        // absolute/relative path to the crm-tool binary (required)
	Timeout time.Duration // 0 ⇒ 30s
}

// Open builds a Service backed by the real binary at cfg.Path. The path is
// configured by the operator (flag/env) — never hardcoded. Open does NOT
// require the binary to exist yet (the operator may install it later); the
// binary's presence and arg/output contract are verified live when a tool
// runs, not at open time.
func Open(cfg Config) (*Service, error) {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return nil, fmt.Errorf("crm: binary path is required")
	}
	to := cfg.Timeout
	if to <= 0 {
		to = defaultTimeout
	}
	s := &Service{path: path, timeout: to}
	s.run = s.realRunner
	return s, nil
}
