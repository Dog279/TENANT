package runtrace

import "sync"

// BootID is this process's identity for its whole lifetime: minted once, at
// first use, like a trace ID. Log events, boot records and approval IDs carry
// it so records from two processes (serve and the TUI on one data directory,
// or two runs of one) never collide.
var BootID = sync.OnceValue(NewID)
