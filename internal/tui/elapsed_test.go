package tui

import (
	"testing"
	"time"
)

func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{3400 * time.Millisecond, "3.4s"},
		{59 * time.Second, "59.0s"},
		{90 * time.Second, "1:30"},                // m:ss
		{59*time.Minute + 5*time.Second, "59:05"}, // still m:ss just under an hour
		{time.Hour, "1:00:00"},                    // rolls into hours
		{100 * time.Minute, "1:40:00"},            // the old "100:00" → now 1:40:00 (the bug)
		{time.Hour + time.Minute + time.Second, "1:01:01"},
		{25*time.Hour + 30*time.Minute, "25:30:00"}, // hours unbounded for very long tasks
	}
	for _, c := range cases {
		if got := formatElapsed(c.d); got != c.want {
			t.Errorf("formatElapsed(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}
