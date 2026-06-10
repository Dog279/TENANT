package main

// Embed the IANA timezone database so cron jobs with an explicit timezone (and
// the global cron timezone) resolve via time.LoadLocation even on systems
// without a system zoneinfo — notably Windows, which ships none. Additive:
// binary-size only (~450KB); where the OS has zoneinfo (macOS/Linux) it still
// wins, so existing time behavior is unchanged.
import _ "time/tzdata"
