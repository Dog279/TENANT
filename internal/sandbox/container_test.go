package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDockerNetwork_FailsClosed(t *testing.T) {
	cases := map[NetworkMode]string{
		NetInherit:   "none", // an entry naming a backend resolves to inherit → none
		NetAllowlist: "none", // no proxy yet → none
		NetNone:      "none",
		NetFull:      "bridge",
	}
	for mode, want := range cases {
		if got := dockerNetwork(mode); got != want {
			t.Errorf("dockerNetwork(%v) = %q, want %q", mode, got, want)
		}
	}
}

func TestEnvNamesFor_DropsHostAndDangerousNames(t *testing.T) {
	e := EnvPolicy{
		Pass: []string{
			"TENANT_API_KEY",                                   // kept
			"PATH", "HOME", "TMP", "SystemRoot", "USERPROFILE", // host-only, dropped
			"DOCKER_HOST",            // dropped: the schema forbids DOCKER_*
			"NVIDIA_VISIBLE_DEVICES", // dropped: would request a GPU
			"BAD NAME",               // dropped: not a valid name
			"TENANT_API_KEY",         // duplicate, dropped
		},
		Add: []string{"TENANT_ACTIVITY_ID=abc", "TENANT_TRACE=xyz"},
	}
	got := envNamesFor(e)
	want := map[string]bool{"TENANT_API_KEY": true, "TENANT_ACTIVITY_ID": true, "TENANT_TRACE": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want keys %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected env name %q crossed into the container", n)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	type tc struct {
		a, b string
		want int
		ok   bool
	}
	for _, c := range []tc{
		{"29.8.0", "29.4.3", 1, true},
		{"29.4.3", "29.4.3", 0, true},
		{"29.4.2", "29.4.3", -1, true},
		{"1.4.0-rc.3", "1.2.8", 1, true},
		{"v1.2.8", "1.2.8", 0, true},
		{"4.44.3", "4.76.0", -1, true},
		{"", "1.0.0", 0, false},
		{"unknown", "1.0.0", 0, false},
	} {
		got, ok := compareVersions(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("compareVersions(%q,%q) = (%d,%v), want (%d,%v)", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

// checkFloors is table-tested in container_engine_test.go (W1-1 made the
// floors refuse rather than warn).

func TestWindowsProgramInvoked(t *testing.T) {
	// The INVOKED program (first token) is what matters, not a substring.
	for _, cmd := range []string{"cmd.exe /c ver", "powershell -Command x", "app.exe --flag", "x.bat"} {
		if _, ok := windowsProgramInvoked(cmd); !ok {
			t.Errorf("expected %q to be flagged as a Windows program", cmd)
		}
	}
	// A .exe mentioned as an ARGUMENT must not be flagged — this was the false
	// positive that refused legitimate builds.
	for _, cmd := range []string{"ls -la", "echo hi", "python3 script.py", "go build -o app.exe .", "gcc -o out.exe x.c"} {
		if line, ok := windowsProgramInvoked(cmd); ok {
			t.Errorf("did not expect %q to be flagged (got %q)", cmd, line)
		}
	}
}

// specFor's deny-root handling: a read/write root overlapping a deny root (either
// direction) is refused, but the work dir may sit INSIDE a deny root (cron's
// carve-out) — only a work dir that is or contains a deny root fails closed.
func TestSpecForDenyRoots(t *testing.T) {
	b := Container(ContainerOptions{})
	base := t.TempDir()
	deny := filepath.Join(base, "data")
	workUnderDeny := filepath.Join(deny, "cron-work")
	outside := filepath.Join(base, "proj")
	for _, d := range []string{workUnderDeny, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Carve-out: work dir inside the deny root is allowed (the cron case).
	if _, err := b.specFor(Command{Shell: "echo hi"}, Policy{WorkDir: workUnderDeny, DenyRoots: []string{deny}}); err != nil {
		t.Errorf("work dir inside a deny root should be allowed (carve-out), got: %v", err)
	}
	// Work dir that IS a deny root fails closed.
	if _, err := b.specFor(Command{Shell: "echo hi"}, Policy{WorkDir: deny, DenyRoots: []string{deny}}); err == nil {
		t.Error("work dir equal to a deny root should fail closed")
	}
	// A read root that CONTAINS a deny root is refused (would expose it) — the
	// security hole: read_roots:[~] must not mount ~/.ssh.
	if _, err := b.specFor(Command{Shell: "echo hi"}, Policy{WorkDir: outside, ReadRoots: []string{base}, DenyRoots: []string{deny}}); err == nil {
		t.Error("a read root containing a deny root should be refused")
	}
	// A read root INSIDE a deny root is refused too (denied content).
	if _, err := b.specFor(Command{Shell: "echo hi"}, Policy{WorkDir: outside, ReadRoots: []string{workUnderDeny}, DenyRoots: []string{deny}}); err == nil {
		t.Error("a read root inside a deny root should be refused")
	}
	// The same place named differently on the two sides (a symlink, a Windows 8.3
	// short name) must still match: container_test_windows.go exercises 8.3, and
	// the macOS /var symlink is the same class.
}

func TestContainerDest(t *testing.T) {
	cases := []struct {
		host, goos, want string
	}{
		{`C:\Users\x\work`, "windows", "/mnt/c/Users/x/work"},
		{`D:\data`, "windows", "/mnt/d/data"},
		{"/home/u/work", "linux", "/home/u/work"},
		{"/Users/u/work", "darwin", "/Users/u/work"},
	}
	for _, c := range cases {
		if got := containerDest(c.host, c.goos); got != c.want {
			t.Errorf("containerDest(%q,%q) = %q, want %q", c.host, c.goos, got, c.want)
		}
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 10}
	b.Write([]byte("hello"))
	b.Write([]byte("world!!!")) // total 13 > 10
	if !b.Truncated() {
		t.Error("expected truncated")
	}
	s := b.String()
	if !strings.HasPrefix(s, "helloworld") || !strings.Contains(s, "truncated") {
		t.Errorf("unexpected capped output %q", s)
	}
	if b.total != 13 {
		t.Errorf("total = %d, want 13", b.total)
	}

	// No cap: everything is kept, nothing marked truncated.
	u := &cappedBuffer{max: 0}
	u.Write([]byte(strings.Repeat("x", 100)))
	if u.Truncated() || len(u.String()) != 100 {
		t.Errorf("uncapped buffer altered output: truncated=%v len=%d", u.Truncated(), len(u.String()))
	}
}

func TestAttributeContainer(t *testing.T) {
	rofs := Result{ExitCode: 1, Output: "sh: cannot create /x: Read-only file system"}
	if v, ok := attributeContainer(rofs, containerSpec{network: "none"}); !ok || v.Kind != "filesystem" {
		t.Errorf("expected filesystem violation, got %+v ok=%v", v, ok)
	}
	oom := Result{ExitCode: 137, Output: "Killed"}
	if v, ok := attributeContainer(oom, containerSpec{network: "none"}); !ok || v.Kind != "limit" {
		t.Errorf("expected limit violation, got %+v ok=%v", v, ok)
	}
	netFail := Result{ExitCode: 6, Output: "curl: (6) Could not resolve host: example.com"}
	if v, ok := attributeContainer(netFail, containerSpec{network: "none"}); !ok || v.Kind != "network" {
		t.Errorf("expected network violation under none, got %+v ok=%v", v, ok)
	}
	// The same network failure with a bridge is NOT attributed to the sandbox.
	if _, ok := attributeContainer(netFail, containerSpec{network: "bridge"}); ok {
		t.Error("did not expect a network violation when the bridge is up")
	}
	// An ordinary non-zero exit is not attributed (fails closed, not escalated).
	if _, ok := attributeContainer(Result{ExitCode: 2, Output: "grep: no match"}, containerSpec{network: "none"}); ok {
		t.Error("did not expect a violation for an ordinary non-zero exit")
	}
}

// W1-6: ENOENT is a filesystem refusal only when the path exists on the host
// and is not mounted — then the container hid it — and the violation carries
// the HOST path, mapped back through the mount table, for the one-path grant.
func TestAttributeContainer_ENOENT(t *testing.T) {
	work := "/home/u/work"
	secret := "/home/u/secret.txt"
	if runtime.GOOS == "windows" {
		work, secret = `C:\Users\u\work`, `C:\Users\u\secret.txt`
	}
	spec := containerSpec{
		network: "none",
		work:    mount{host: work, ctr: containerDest(work, runtime.GOOS)},
		hostExists: func(p string) bool {
			return p == secret || strings.HasPrefix(p, work)
		},
	}
	out := "cat: can't open '" + containerDest(secret, runtime.GOOS) + "': No such file or directory"
	v, ok := attributeContainer(Result{ExitCode: 1, Output: out}, spec)
	if !ok || v.Kind != "filesystem" || v.Path != secret {
		t.Fatalf("expected a filesystem refusal naming %s, got %+v ok=%v", secret, v, ok)
	}
	// A file that is genuinely missing on the host is the command's own error.
	missing := "cat: can't open '" + containerDest(work, runtime.GOOS) + "/nope': No such file or directory"
	if _, ok := attributeContainer(Result{ExitCode: 1, Output: missing}, spec); ok {
		t.Error("a path missing on the host must not be attributed")
	}
	// Inside a mount and existing on the host: the container could see it, so
	// ENOENT is not the boundary's doing.
	inside := work + string(filepath.Separator) + "present"
	spec.hostExists = func(p string) bool { return p == inside }
	insideOut := "cat: can't open '" + containerDest(inside, runtime.GOOS) + "': No such file or directory"
	if _, ok := attributeContainer(Result{ExitCode: 1, Output: insideOut}, spec); ok {
		t.Error("a mounted path must not be attributed as hidden")
	}
	// Without a hostExists seam (unknown), ENOENT is never attributed.
	if _, ok := attributeContainer(Result{ExitCode: 1, Output: out}, containerSpec{network: "none"}); ok {
		t.Error("ENOENT must not be attributed when host existence is unknown")
	}
	// EROFS carries the host path too.
	v, ok = attributeContainer(Result{ExitCode: 1, Output: "sh: 1: cannot create " + containerDest(secret, runtime.GOOS) + ": Read-only file system"}, spec)
	if !ok || v.Kind != "filesystem" || v.Path != secret {
		t.Fatalf("EROFS: expected Path %s, got %+v", secret, v)
	}
}

func TestValidImageRef(t *testing.T) {
	for _, ok := range []string{"tenant-sandbox:latest", "tenant-sandbox@sha256:" + strings.Repeat("a", 64), "debian:trixie-slim"} {
		if err := validImageRef(ok); err != nil {
			t.Errorf("validImageRef(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"--privileged", "has space", "with\"quote", "-flag"} {
		if err := validImageRef(bad); err == nil {
			t.Errorf("validImageRef(%q) = nil, want error", bad)
		}
	}
}

// The refusal witness is the deterministic source for attribution (W1-6): it
// scans the whole stream, line by line, before the output cap, so the verdict
// does not depend on how the bytes were chunked or where a line fell.
func TestRefusalWitness_Streams(t *testing.T) {
	line := "sh: 1: cannot create /mnt/c/w/secret.txt: Read-only file system\n"
	want := strings.TrimSuffix(line, "\n")
	// The same logical content, chunked three ways (the signature itself split
	// across writes in the third), must agree.
	chunkings := [][]string{
		{"=== probe ===\n", line, "done\n"},
		{"=== probe ===\n" + line[:20], line[20:45], line[45:] + "done\n"},
		{"=== pr", "obe ===\n" + line[:len(line)-16], line[len(line)-16:] + "done\n"},
	}
	for i, chunks := range chunkings {
		w := &refusalWitness{}
		for _, c := range chunks {
			w.Write([]byte(c))
		}
		w.flush()
		if !w.erofs || w.erofsLine != want {
			t.Errorf("chunking %d: erofs=%v line=%q", i, w.erofs, w.erofsLine)
		}
		if w.enoent || w.limit || w.netCut || w.notFound {
			t.Errorf("chunking %d: spurious flags enoent=%v limit=%v net=%v notfound=%v", i, w.enoent, w.limit, w.netCut, w.notFound)
		}
	}
	// A trailing line with no newline is scanned on flush, not before.
	w := &refusalWitness{}
	w.Write([]byte("curl: (6) Could not resolve host: example.com"))
	if w.netCut {
		t.Error("an unterminated line must not be scanned before flush")
	}
	w.flush()
	if !w.netCut {
		t.Error("the trailing line must be scanned on flush")
	}
	// A newline-free flood stays bounded, and a refusal after it is still seen.
	w = &refusalWitness{}
	w.Write([]byte(strings.Repeat("x", 3*witnessLineCap)))
	if len(w.line) > witnessLineCap {
		t.Errorf("flood: the current line grew to %d, cap %d", len(w.line), witnessLineCap)
	}
	w.Write([]byte("cat: /mnt/c/w/x: No such file or directory"))
	w.flush()
	if !w.enoent {
		t.Error("flood: the refusal after the flood must still be seen")
	}
}

// The latent flap: a refusal line that lands past the output cap is dropped
// from the capped output, which is all attribution used to read, so the label
// depended on where the line fell. With the witness the capped Output may lack
// the line and the run is still attributed, with the host path, while the same
// truncated Output WITHOUT a witness is not: the witness is what fixes it.
func TestAttributeContainer_WitnessBeatsTruncation(t *testing.T) {
	secret := "/home/u/secret.txt"
	if runtime.GOOS == "windows" {
		secret = `C:\Users\u\secret.txt`
	}
	refusal := "sh: 1: cannot create " + containerDest(secret, runtime.GOOS) + ": Read-only file system\n"

	// A tiny display cap and the witness, fed as io.MultiWriter(out, wit) feeds
	// them in Run: a flood of stdout fills the cap, then the refusal arrives
	// past it.
	out := &cappedBuffer{max: 64}
	wit := &refusalWitness{}
	write := func(s string) { out.Write([]byte(s)); wit.Write([]byte(s)) }
	for i := 0; i < 8; i++ {
		write("=== section: benign enumeration ===\n")
	}
	write(refusal)
	wit.flush()

	res := Result{ExitCode: 2, Output: out.String()}
	if !out.Truncated() || strings.Contains(strings.ToLower(res.Output), "read-only file system") {
		t.Fatalf("test setup: the refusal line must have fallen past the cap, got %q", res.Output)
	}

	// Without a witness the truncated output cannot be attributed: the
	// clean-on-replay half of the flap.
	if v, ok := attributeContainer(res, containerSpec{network: "none"}); ok {
		t.Fatalf("without a witness, the truncated output must not attribute, got %+v", v)
	}
	// With the witness it is attributed, with the host path, every time.
	for i := 0; i < 3; i++ {
		v, ok := attributeContainer(res, containerSpec{network: "none", witness: wit})
		if !ok || v.Kind != "filesystem" || v.Path != secret {
			t.Fatalf("run %d: with the witness, expected a filesystem refusal naming %s, got %+v ok=%v", i, secret, v, ok)
		}
	}
}

// W2-2: a folder that is itself a git directory has its hooks and config at
// the top of what a command could write, so it is refused as the work dir
// and as a write root. Read-only it is harmless, and a checkout is fine.
func TestSpecFor_RefusesABareRepoAsAWritableFolder(t *testing.T) {
	b := &containerBackend{image: "img"}
	bare, checkout, plain := t.TempDir(), t.TempDir(), t.TempDir()
	for _, d := range []string{filepath.Join(bare, "objects"), filepath.Join(bare, "refs"), filepath.Join(bare, "hooks"),
		filepath.Join(checkout, ".git", "objects"), filepath.Join(checkout, ".git", "refs")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(bare, "HEAD"), filepath.Join(checkout, ".git", "HEAD")} {
		if err := os.WriteFile(f, []byte("ref: refs/heads/main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := Command{Shell: "echo hi"}
	if _, err := b.specFor(cmd, Policy{WorkDir: bare}); err == nil || !strings.Contains(err.Error(), "bare git repository") {
		t.Errorf("a bare repo as the work dir: err = %v, want a refusal that says why", err)
	}
	if _, err := b.specFor(cmd, Policy{WorkDir: plain, WriteRoots: []string{bare}}); err == nil || !strings.Contains(err.Error(), "bare git repository") {
		t.Errorf("a bare repo as a write root: err = %v, want a refusal", err)
	}
	if _, err := b.specFor(cmd, Policy{WorkDir: plain, ReadRoots: []string{bare}}); err != nil {
		t.Errorf("a bare repo granted read-only was refused: %v", err)
	}
	if _, err := b.specFor(cmd, Policy{WorkDir: checkout}); err != nil {
		t.Errorf("a checkout as the work dir was refused: %v", err)
	}
	// A folder that only has a file named HEAD is not a repository.
	if err := os.WriteFile(filepath.Join(plain, "HEAD"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.specFor(cmd, Policy{WorkDir: plain}); err != nil {
		t.Errorf("a folder with only a HEAD file was refused: %v", err)
	}
}

// D6: the image is built from trixie, pinned by digest, and gives the sandbox
// user the folder its HOME tmpfs mounts on.
func TestSandboxImageIsPinnedTrixie(t *testing.T) {
	ref := BaseImageRef()
	name, digest, ok := strings.Cut(ref, "@sha256:")
	if !ok || name != "debian:trixie-slim" || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		t.Errorf("base image = %q, want debian:trixie-slim pinned by a sha256 digest", ref)
	}
	if !strings.HasPrefix(imageDockerfile, "FROM "+ref+"\n") {
		t.Errorf("the Dockerfile does not start from the pinned base:\n%s", imageDockerfile)
	}
	for _, want := range []string{"install -d -o 65532 -g 65532 -m 0700 " + sandboxHome, "USER 65532:65532", "chmod a-s"} {
		if !strings.Contains(imageDockerfile, want) {
			t.Errorf("the Dockerfile lacks %q", want)
		}
	}
}

// A folder named by more than one grant is mounted once, by its widest
// grant. Two mounts on one destination are refused by a real engine.
func TestSpecFor_OneMountPerDestination(t *testing.T) {
	b := &containerBackend{image: "img"}
	work, shared, ro := t.TempDir(), t.TempDir(), t.TempDir()
	spec, err := b.specFor(Command{Shell: "echo hi"}, Policy{
		WorkDir:    work,
		ReadRoots:  []string{work, shared, ro, ro},
		WriteRoots: []string{shared, work},
	})
	if err != nil {
		t.Fatalf("specFor: %v", err)
	}
	seen := map[string]bool{spec.work.ctr: true}
	for _, m := range spec.mounts {
		if seen[m.ctr] {
			t.Errorf("destination %s is mounted twice: %+v", m.ctr, spec.mounts)
		}
		seen[m.ctr] = true
	}
	if len(spec.mounts) != 2 {
		t.Fatalf("mounts = %+v, want the read-only folder and the shared one", spec.mounts)
	}
	for _, m := range spec.mounts {
		switch m.host {
		case canonForCompare(shared):
			if m.ro {
				t.Errorf("a folder granted read-only and writable must be writable: %+v", m)
			}
		case canonForCompare(ro):
			if !m.ro {
				t.Errorf("the read-only folder became writable: %+v", m)
			}
		}
	}
	// What an approval of a write into a read-only grant builds.
	granted, err := grantedPolicy(Policy{WorkDir: work, ReadRoots: []string{ro}},
		Result{ExitCode: 1, Violations: []Violation{{Kind: "filesystem", Detail: "read-only", Path: ro}}})
	if err != nil {
		t.Fatal(err)
	}
	spec, err = b.specFor(Command{Shell: "echo hi"}, granted)
	if err != nil {
		t.Fatalf("specFor of the granted policy: %v", err)
	}
	if len(spec.mounts) != 1 || spec.mounts[0].ro {
		t.Errorf("the granted folder must be one writable mount: %+v", spec.mounts)
	}
}
