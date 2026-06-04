package eval

import "embed"

// EmbeddedTasks is the shipped task catalog. Tasks live under
// internal/eval/tasks/ to satisfy //go:embed's same-tree constraint
// while keeping the data tree visible inside the eval package.
//
//go:embed tasks/smoke/*.yaml tasks/fitness/*.yaml tasks/adversarial/*.yaml
var EmbeddedTasks embed.FS
