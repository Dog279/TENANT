package cron

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec string) *Schedule {
	t.Helper()
	s, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", spec, err)
	}
	return s
}

func TestParseRejectsGarbage(t *testing.T) {
	bad := []string{
		"", "   ",
		"* * * *",      // 4 fields
		"* * * * * *",  // 6 fields
		"60 * * * *",   // minute out of range
		"* 24 * * *",   // hour out of range
		"* * 0 * *",    // dom < 1
		"* * 32 * *",   // dom > 31
		"* * * 0 *",    // month < 1
		"* * * 13 *",   // month > 13
		"* * * * 8",    // dow > 7
		"30-5 * * * *", // inverted range
		"*/0 * * * *",  // zero step
		"*/-1 * * * *", // negative step (Atoi fails)
		"abc * * * *",  // garbage
		"@every",       // missing duration
		"@every 0s",    // zero interval
		"@every -5m",   // negative interval
		"@every 30s",   // sub-minute
		"@bogus",       // unknown macro
	}
	for _, spec := range bad {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) = nil error, want rejection", spec)
		}
	}
}

func TestParseAcceptsForms(t *testing.T) {
	good := []string{
		"* * * * *", "0 9 * * *", "*/15 * * * *", "0 9-17/2 * * 1-5",
		"0 0 1,15 * *", "5-30/5 * * * *", "0 0 * * 7", "0 0 * * 0",
		"@hourly", "@daily", "@midnight", "@weekly", "@monthly", "@yearly", "@annually",
		"@every 1m", "@every 30m", "@every 2h",
	}
	for _, spec := range good {
		if _, err := Parse(spec); err != nil {
			t.Errorf("Parse(%q) error: %v", spec, err)
		}
	}
}

func TestNextStrictlyAfterAndMinuteGranular(t *testing.T) {
	s := mustParse(t, "* * * * *") // every minute
	// Input exactly on a minute boundary -> next is the FOLLOWING minute.
	in := time.Date(2026, 6, 7, 10, 30, 0, 0, time.Local)
	got := s.Next(in)
	want := time.Date(2026, 6, 7, 10, 31, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("Next(%v) = %v, want %v", in, got, want)
	}
	// Mid-minute input -> next whole minute.
	in2 := time.Date(2026, 6, 7, 10, 30, 45, 0, time.Local)
	if got := s.Next(in2); !got.Equal(want) {
		t.Errorf("Next(mid-minute) = %v, want %v", got, want)
	}
}

func TestNextDailyAtTime(t *testing.T) {
	s := mustParse(t, "0 9 * * *") // 09:00 daily
	in := time.Date(2026, 6, 7, 10, 0, 0, 0, time.Local)
	got := s.Next(in)
	want := time.Date(2026, 6, 8, 9, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v (next day 9am)", got, want)
	}
	// Before 9am same day -> today 9am.
	in2 := time.Date(2026, 6, 7, 8, 0, 0, 0, time.Local)
	want2 := time.Date(2026, 6, 7, 9, 0, 0, 0, time.Local)
	if got := s.Next(in2); !got.Equal(want2) {
		t.Errorf("Next = %v, want %v (today 9am)", got, want2)
	}
}

// The classic DOM/DOW OR-rule: when BOTH are restricted, EITHER matches.
func TestNextDomDowOrSemantics(t *testing.T) {
	// "0 0 1 * 1" = midnight on the 1st OR any Monday.
	s := mustParse(t, "0 0 1 * 1")
	// 2026-06-07 is a Sunday. Next Monday is 2026-06-08; the 1st already passed.
	in := time.Date(2026, 6, 7, 12, 0, 0, 0, time.Local)
	got := s.Next(in)
	want := time.Date(2026, 6, 8, 0, 0, 0, 0, time.Local) // Monday
	if !got.Equal(want) {
		t.Errorf("OR-rule Next = %v, want Monday %v", got, want)
	}
	if got.Weekday() != time.Monday {
		t.Errorf("expected a Monday, got %v", got.Weekday())
	}

	// When only DOM is restricted (dow=*), AND-rule -> just the 1st.
	s2 := mustParse(t, "0 0 1 * *")
	got2 := s2.Next(in)
	want2 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	if !got2.Equal(want2) {
		t.Errorf("DOM-only Next = %v, want %v", got2, want2)
	}
}

func TestNextWeekdayRange(t *testing.T) {
	// 09:00 Mon-Fri.
	s := mustParse(t, "0 9 * * 1-5")
	// 2026-06-07 is Sunday -> next is Monday 09:00.
	in := time.Date(2026, 6, 7, 10, 0, 0, 0, time.Local)
	got := s.Next(in)
	if got.Weekday() != time.Monday || got.Hour() != 9 {
		t.Errorf("Next = %v, want Monday 09:00", got)
	}
}

func TestNextImpossibleSpecReturnsZero(t *testing.T) {
	// Feb 31 never exists.
	s := mustParse(t, "0 0 31 2 *")
	in := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	if got := s.Next(in); !got.IsZero() {
		t.Errorf("Next(impossible) = %v, want zero (never)", got)
	}
}

func TestNextLeapDay(t *testing.T) {
	// Feb 29 only on leap years. 2027 is not leap; next leap is 2028.
	s := mustParse(t, "0 0 29 2 *")
	in := time.Date(2027, 3, 1, 0, 0, 0, 0, time.Local)
	got := s.Next(in)
	want := time.Date(2028, 2, 29, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("Next leap = %v, want %v", got, want)
	}
}

func TestNextStepAndList(t *testing.T) {
	s := mustParse(t, "*/15 * * * *") // 0,15,30,45
	in := time.Date(2026, 6, 7, 10, 1, 0, 0, time.Local)
	got := s.Next(in)
	want := time.Date(2026, 6, 7, 10, 15, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("step Next = %v, want %v", got, want)
	}

	s2 := mustParse(t, "0 0 1,15 * *") // 1st and 15th
	in2 := time.Date(2026, 6, 7, 0, 0, 0, 0, time.Local)
	got2 := s2.Next(in2)
	want2 := time.Date(2026, 6, 15, 0, 0, 0, 0, time.Local)
	if !got2.Equal(want2) {
		t.Errorf("list Next = %v, want %v", got2, want2)
	}
}

func TestDowSevenIsSunday(t *testing.T) {
	s7 := mustParse(t, "0 0 * * 7")
	s0 := mustParse(t, "0 0 * * 0")
	in := time.Date(2026, 6, 7, 12, 0, 0, 0, time.Local) // Sunday
	g7, g0 := s7.Next(in), s0.Next(in)
	if !g7.Equal(g0) {
		t.Errorf("dow 7 (%v) and dow 0 (%v) should be identical (Sunday)", g7, g0)
	}
	if g7.Weekday() != time.Sunday {
		t.Errorf("expected Sunday, got %v", g7.Weekday())
	}
}

func TestNextInterval(t *testing.T) {
	s := mustParse(t, "@every 30m")
	if !s.IsInterval() {
		t.Fatal("expected interval schedule")
	}
	in := time.Date(2026, 6, 7, 10, 0, 0, 0, time.Local)
	got := s.Next(in)
	want := in.Add(30 * time.Minute)
	if !got.Equal(want) {
		t.Errorf("interval Next = %v, want %v", got, want)
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, spec := range []string{"0 9 * * 1-5", "@every 30m", "@daily"} {
		if got := mustParse(t, spec).String(); got != spec {
			t.Errorf("String() = %q, want %q", got, spec)
		}
	}
}

func TestNextNilSafe(t *testing.T) {
	var s *Schedule
	if got := s.Next(time.Now()); !got.IsZero() {
		t.Errorf("nil Schedule Next = %v, want zero", got)
	}
}
