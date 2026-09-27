// Package archtest holds the module's architecture tests (TEN-314): which
// packages may import which, checked against the source on every go test run.
// The rules and the checker live in rules.go, as plain code nothing imports;
// the tests load the import graph and hold the tree to them. The rules are
// documented, as a generated table, in docs/ARCHITECTURE.md §2. coverage.go
// measures how much of each provider's logging carries a catalogued event ID
// (logging v2), and holds catalogued packages to a ratchet.
package archtest
