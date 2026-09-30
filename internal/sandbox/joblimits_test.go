package sandbox

import (
	"testing"
	"time"
)

func TestJobLimitsFrom(t *testing.T) {
	// The pure bit values must match winnt.h (restricted_windows.go additionally
	// asserts they equal golang.org/x/sys/windows' constants at compile time).
	if jobLimitProcessTime != 0x2 || jobLimitActiveProcess != 0x8 ||
		jobLimitJobMemory != 0x200 || jobLimitKillOnClose != 0x2000 {
		t.Fatalf("JOB_OBJECT_LIMIT_* bit values drifted from winnt.h")
	}

	// Zero limits: kill-on-close only, all value fields unset.
	jl := jobLimitsFrom(Limits{})
	if jl.Flags != jobLimitKillOnClose {
		t.Fatalf("zero limits flags = %#x, want kill-on-close %#x", jl.Flags, jobLimitKillOnClose)
	}
	if jl.ActiveProcessLimit != 0 || jl.JobMemoryLimit != 0 || jl.PerProcessUserTime100ns != 0 {
		t.Fatalf("zero limits must leave value fields unset: %+v", jl)
	}

	// Populated limits: every bit set and every field mapped.
	jl = jobLimitsFrom(Limits{MemoryBytes: 1 << 30, MaxPIDs: 8, CPU: 2 * time.Second})
	if jl.Flags&jobLimitKillOnClose == 0 {
		t.Errorf("kill-on-close must always be set: %#x", jl.Flags)
	}
	if jl.Flags&jobLimitJobMemory == 0 || jl.JobMemoryLimit != 1<<30 {
		t.Errorf("memory mapping wrong: %+v", jl)
	}
	if jl.Flags&jobLimitActiveProcess == 0 || jl.ActiveProcessLimit != 8 {
		t.Errorf("pid mapping wrong: %+v", jl)
	}
	wantTicks := int64(2*time.Second) / 100 // 100ns ticks
	if jl.Flags&jobLimitProcessTime == 0 || jl.PerProcessUserTime100ns != wantTicks {
		t.Errorf("cpu mapping wrong: got %d want %d", jl.PerProcessUserTime100ns, wantTicks)
	}
}
