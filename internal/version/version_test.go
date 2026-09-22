package version

import "testing"

func TestString(t *testing.T) {
	originalVersion, originalCommit := Version, Commit
	originalBuildTime, originalTarget, originalDirty := BuildTime, Target, Dirty
	t.Cleanup(func() {
		Version, Commit = originalVersion, originalCommit
		BuildTime, Target, Dirty = originalBuildTime, originalTarget, originalDirty
	})

	Version = "1.2.3"
	Commit = "0123456789abcdef"
	BuildTime = "2026-07-17T18:30:00Z"
	Target = "windows_amd64_v1"
	Dirty = "true"

	want := "1.2.3 (commit=01234567, built=2026-07-17T18:30:00Z, target=windows_amd64_v1, dirty=true)"
	if got := String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestShortCommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "full SHA", in: "0123456789abcdef", want: "01234567"},
		{name: "short SHA", in: "0123456", want: "0123456"},
		{name: "unknown", in: "unknown", want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortCommit(tc.in); got != tc.want {
				t.Fatalf("shortCommit(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
