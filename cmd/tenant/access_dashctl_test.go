package main

import (
	"errors"
	"testing"

	"tenant/internal/dashboard"
)

// The Access adapter phrases the failures it can (D21): a bad phone number
// or email, and a channel this Tenant doesn't have.
func TestDashAccess_PlainErrors(t *testing.T) {
	var pe dashboard.PlainError
	if _, _, err := (dashAccess{}).IMessageAllow("a@b.c"); !errors.As(err, &pe) {
		t.Errorf("iMessage unavailable = %v, want a PlainError", err)
	}
	if err := (dashAccess{}).SetRelayEnabled(true); !errors.As(err, &pe) {
		t.Errorf("Discord unavailable = %v, want a PlainError", err)
	}
	a := dashAccess{im: newIMessageAllowManager(nil, nil)}
	if _, _, err := a.IMessageAllow("not a handle"); !errors.As(err, &pe) {
		t.Errorf("a bad handle = %v, want a PlainError", err)
	}
	if norm, added, err := a.IMessageAllow("a@b.c"); err != nil || !added || norm == "" {
		t.Errorf("a good handle = %q %v %v", norm, added, err)
	}
}
