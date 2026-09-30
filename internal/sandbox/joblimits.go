package sandbox

// joblimits.go maps a Policy's Limits onto a Windows Job Object. It is pure Go,
// with no build tags, no syscalls and no golang.org/x/sys types, so the mapping
// is unit-tested on every host; restricted_windows.go applies it.

// Win32 JOB_OBJECT_LIMIT_* bit values (winnt.h). They live in this pure file so
// jobLimitsFrom is testable off-Windows; restricted_windows.go carries a
// compile-time assertion that each still equals its golang.org/x/sys/windows
// counterpart, so the two can never silently drift.
const (
	jobLimitProcessTime   uint32 = 0x00000002 // JOB_OBJECT_LIMIT_PROCESS_TIME
	jobLimitActiveProcess uint32 = 0x00000008 // JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	jobLimitJobMemory     uint32 = 0x00000200 // JOB_OBJECT_LIMIT_JOB_MEMORY
	jobLimitKillOnClose   uint32 = 0x00002000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
)

// jobLimits is the OS-agnostic projection of a Policy's Limits onto a Windows
// Job Object's JOBOBJECT_EXTENDED_LIMIT_INFORMATION. It is pure Go (no syscalls,
// no golang.org/x/sys types) so the mapping is unit-tested on any host;
// restricted_windows.go copies these fields into the real windows struct.
type jobLimits struct {
	Flags                   uint32 // OR of the jobLimit* bits above
	ActiveProcessLimit      uint32 // JOB_OBJECT_LIMIT_ACTIVE_PROCESS count; 0 ⇒ unused
	JobMemoryLimit          uint64 // JOB_OBJECT_LIMIT_JOB_MEMORY bytes;     0 ⇒ unused
	PerProcessUserTime100ns int64  // JOB_OBJECT_LIMIT_PROCESS_TIME in 100ns ticks; 0 ⇒ unused
}

// jobLimitsFrom maps a Limits onto the Job Object fields. KILL_ON_JOB_CLOSE is
// ALWAYS set so closing (or killing) the job tears the whole process tree down;
// the other three bits are set only when the matching Limits field is non-zero.
func jobLimitsFrom(l Limits) jobLimits {
	jl := jobLimits{Flags: jobLimitKillOnClose}
	if l.MaxPIDs > 0 {
		jl.Flags |= jobLimitActiveProcess
		jl.ActiveProcessLimit = uint32(l.MaxPIDs)
	}
	if l.MemoryBytes > 0 {
		jl.Flags |= jobLimitJobMemory
		jl.JobMemoryLimit = uint64(l.MemoryBytes)
	}
	if l.CPU > 0 {
		jl.Flags |= jobLimitProcessTime
		// A Job Object's PerProcessUserTimeLimit is counted in 100-nanosecond ticks.
		jl.PerProcessUserTime100ns = l.CPU.Nanoseconds() / 100
	}
	return jl
}
