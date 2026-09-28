package runtrace

import "testing"

func TestBootIDIsStableAndShaped(t *testing.T) {
	a, b := BootID(), BootID()
	if a != b {
		t.Fatalf("BootID changed within one process: %q then %q", a, b)
	}
	if len(a) != 32 {
		t.Fatalf("BootID = %q, want 32 hex characters", a)
	}
}
