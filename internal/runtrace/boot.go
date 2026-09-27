package runtrace

import "sync"

var (
	bootOnce sync.Once
	bootID   string
)

// BootID is this process's identity for its whole lifetime: minted once, at
// first use, like a trace ID. Log events, boot records and approval IDs carry
// it so records from two processes (serve and the TUI on one data directory,
// or two runs of one) never collide.
func BootID() string {
	bootOnce.Do(func() { bootID = NewID() })
	return bootID
}
