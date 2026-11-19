//go:build race

package shadow

// raceEnabled says the race detector is on, which makes a timing or memory
// budget meaningless.
const raceEnabled = true
