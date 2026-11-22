//go:build race

package main

// raceEnabled says the race detector is on. It makes real processes several
// times slower, and a test that times one against another needs to know.
const raceEnabled = true
