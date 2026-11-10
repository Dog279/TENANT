package main

// sandboxctl.go: `tenant sandbox` — the operator's view of, and test bench for,
// the execution-backend seam (internal/sandbox; SEC-009 / TEN-345).
//
//	tenant sandbox probe [--json]
//	tenant sandbox selftest [--json]
//	tenant sandbox run --surface S [--backend host|restricted|container] [--timeout 60s] [--workdir DIR] -- COMMAND...
//
// probe resolves every surface's exec profile exactly as the spawn sites do and
// prints its backend, isolation, network mode and roots, plus what that backend
// actually enforces on this host (Probe) and whether this OS has a restricted
// backend at all, even before any surface uses it. It exits 1 when a surface's
// backend is unavailable (its spawns would fail closed).
//
// selftest runs the escape suite (internal/sandbox/escapecore) against the host
// backend (the unconfined baseline, expected NOT to contain) and the restricted
// backend (expected to contain). It exits 1 when the restricted backend fails
// to contain a case in a dimension its Probe claims to fully enforce, when any
// legitimate twin fails on either backend, or when this OS has no restricted
// backend. This is how an operator validates a backend on the live box: macOS
// today, the container backend when it lands.
//
// run executes one command under a surface's resolved profile (optionally with a
// forced backend) the way that surface's spawn site would, and reports output,
// exit code, timeout and violations. It is the manual rollout tool for trying a
// cron job's command under a profile before enabling it, and exits 1 unless the
// command ran cleanly.
//
// Like the seam itself this is operator-only: it reads trusted inputs, and no
// agent tool reaches it.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"tenant/internal/sandbox"
	"tenant/internal/sandbox/escapecore"
)

// sandboxSurfaces are the spawn surfaces an exec profile is resolved for.
var sandboxSurfaces = execSurfaces

const sandboxRunUsage = "usage: tenant sandbox run --surface <local|discord|imessage|cron-exec|cron-shell> " +
	"[--backend host|restricted|container] [--timeout 60s] [--workdir DIR] -- <command...>"

const sandboxUsage = `usage: tenant sandbox <probe|selftest|run>

  probe [--json]      each surface's exec profile: backend, isolation, network mode, roots,
                      and what that backend enforces on this host; plus whether this OS has
                      a restricted backend. Exits 1 if a surface's backend is unavailable.
  selftest [--json] [--backend restricted|container]
                      run the escape suite against the host (baseline) and the confining
                      backend (default restricted; container is the Docker backend, the
                      Windows test bed). Exits 1 if it fails a claim it makes, a legitimate
                      twin fails, or the backend is unavailable.
  run --surface S [--backend host|restricted|container] [--timeout 60s] [--workdir DIR] -- COMMAND...
                      run one command under surface S's profile the way its spawn site
                      would; prints output, exit code, timeout and violations. Exits 1
                      unless the command ran cleanly.
  redteam [--model M] [--endpoint URL] [--backend restricted|host] [--turns N] [--json]
                      put a local Ollama model in the adversary seat and let it drive the
                      real sandbox boundary inside a disposable fake environment with
                      canary secrets. Reports what the boundary contained; exits 1 if the
                      model escapes a boundary the backend claims to enforce.
  image build|verify [--cli docker] [--tag tenant-sandbox:latest]
                      build the container backend's image from the embedded, digest-pinned
                      Dockerfile (no registry beyond the base), or verify it is present and
                      print its id and base.

  probe and run take --config DIR / --data DIR / --agent ID to select the instance.`

// cmdSandbox dispatches `tenant sandbox probe|selftest|run`.
func cmdSandbox(ctx context.Context, args []string) error {
	return sandboxMain(ctx, args, os.Stdout)
}

// sandboxMain is cmdSandbox with an injectable stdout, for tests.
func sandboxMain(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(sandboxUsage)
	}
	switch args[0] {
	case "probe":
		return sandboxProbe(ctx, args[1:], out)
	case "selftest":
		return sandboxSelftest(ctx, args[1:], out)
	case "run":
		return sandboxRun(ctx, args[1:], out)
	case "redteam":
		return sandboxRedteam(ctx, args[1:], out)
	case "image":
		return sandboxImage(ctx, args[1:], out)
	case "help", "-h", "--help":
		fmt.Fprintln(out, sandboxUsage)
		return nil
	default:
		return fmt.Errorf("unknown sandbox command %q\n%s", args[0], sandboxUsage)
	}
}

// bindSandboxDirs registers the instance-selecting common flags (--config,
// --data, --agent: the same names, defaults and resolveDirs resolution as
// bindCommon) without bindCommon's inference flags, whose --backend would
// collide with `run --backend`.
func bindSandboxDirs(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.agent, "agent", "main", "agent id")
	fs.StringVar(&c.dataDir, "data", "", "data dir (default OS data dir)")
	fs.StringVar(&c.cfgDir, "config", "", "config dir (default OS config dir)")
	c.fs = fs
	return c
}

// sbxHelpOK turns `-h` into a clean exit: the flag package already printed usage.
func sbxHelpOK(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------- probe ---

// sbxCaps is sandbox.Capabilities with its enums spelled out.
type sbxCaps struct {
	Backend    string `json:"backend"`
	Isolation  string `json:"isolation"`
	Filesystem string `json:"filesystem"`
	Network    string `json:"network"`
	Limits     string `json:"limits"`
	Env        string `json:"env"`
	Detail     string `json:"detail"`
}

func viewCaps(c sandbox.Capabilities) sbxCaps {
	return sbxCaps{
		Backend:    c.Backend,
		Isolation:  c.Isolation.String(),
		Filesystem: c.Filesystem.String(),
		Network:    c.Network.String(),
		Limits:     c.Limits.String(),
		Env:        c.Env.String(),
		Detail:     c.Detail,
	}
}

// surfaceProbe is one surface's resolved profile and what its backend enforces.
type surfaceProbe struct {
	Surface    string   `json:"surface"`
	Backend    string   `json:"backend"`             // profile backend name: host | restricted | ...
	Isolation  string   `json:"isolation,omitempty"` // "" when the backend is unavailable
	Network    string   `json:"network"`             // policy network mode
	Mode       string   `json:"mode"`                // enforce | dry-run (what a refusal does)
	Allow      []string `json:"allow"`               // hosts/CIDRs when network=allowlist
	WorkDir    string   `json:"work_dir"`            // "" = set per run by the spawn site
	ReadRoots  []string `json:"read_roots"`          // empty = whole host (deny roots still mask)
	WriteRoots []string `json:"write_roots"`         // empty = work dir only
	DenyRoots  []string `json:"deny_roots"`
	Caps       *sbxCaps `json:"capabilities,omitempty"` // nil when the backend is unavailable
	Error      string   `json:"error,omitempty"`        // why the backend is unavailable
	Fallback   string   `json:"fallback,omitempty"`     // why the built-in default runs this surface on the host
	Exceptions []string `json:"exceptions"`             // exec_exceptions: folders its commands may write to
}

type restrictedProbe struct {
	Available bool     `json:"available"`
	Error     string   `json:"error,omitempty"`
	Caps      *sbxCaps `json:"capabilities,omitempty"`
	UsedBy    []string `json:"used_by"`
}

type probeReport struct {
	OS         string          `json:"os"`
	Arch       string          `json:"arch"`
	Agent      string          `json:"agent"`
	ConfigDir  string          `json:"config_dir"`
	DataDir    string          `json:"data_dir"`
	Surfaces   []surfaceProbe  `json:"surfaces"`
	Host       sbxCaps         `json:"host"`
	Restricted restrictedProbe `json:"restricted"`
	// ProfileNotes reports how the operator's exec_profiles loaded (overrides
	// applied, invalid entries skipped, or the settings file unreadable).
	ProfileNotes []string `json:"profile_notes"`
}

// errSurfaceUnavailable marks a probe that found a surface whose configured
// backend cannot run on this host, so that surface's spawns would fail closed.
var errSurfaceUnavailable = errors.New("backend unavailable")

func sandboxProbe(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sandbox probe", flag.ContinueOnError)
	c := bindSandboxDirs(fs)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return sbxHelpOK(err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("sandbox probe: unexpected argument %q", fs.Arg(0))
	}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	notes := loadSandboxProfiles(c)
	rep := buildProbeReport(ctx, c)
	rep.ProfileNotes = notes
	if *asJSON {
		if err := sbxWriteJSON(out, rep); err != nil {
			return err
		}
	} else {
		renderProbe(out, rep)
	}
	var bad []string
	for _, s := range rep.Surfaces {
		if s.failsClosed() {
			bad = append(bad, s.Surface)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("sandbox probe: %w for %s (those spawns would fail closed)", errSurfaceUnavailable, strings.Join(bad, ", "))
	}
	return nil
}

func buildProbeReport(ctx context.Context, c *commonFlags) probeReport {
	r := probeReport{
		OS: runtime.GOOS, Arch: runtime.GOARCH, Agent: c.agent,
		ConfigDir: c.cfgDir, DataDir: c.dataDir,
		Host:       viewCaps(sandbox.Host().Probe(ctx)),
		Restricted: restrictedProbe{UsedBy: []string{}},
	}
	for _, name := range sandboxSurfaces {
		s := probeSurface(ctx, name, c.cfgDir, c.dataDir)
		if s.Backend == "restricted" {
			r.Restricted.UsedBy = append(r.Restricted.UsedBy, name)
		}
		r.Surfaces = append(r.Surfaces, s)
	}
	if b, err := sandbox.Restricted(); err != nil {
		r.Restricted.Error = err.Error()
	} else {
		caps := viewCaps(b.Probe(ctx))
		r.Restricted.Available = true
		r.Restricted.Caps = &caps
	}
	return r
}

// probeSurface resolves a surface's exec profile as its spawn site does and
// asks its backend what it enforces on this host. `tenant sandbox probe`,
// `tenant security audit`, `tenant permissions effective` and doctor all read
// it, so none of them can say a surface is confined when it is not.
func probeSurface(ctx context.Context, name, cfgDir, dataDir string) surfaceProbe {
	p := resolveExecProfile(name, cfgDir, dataDir, "")
	s := surfaceProbe{
		Surface:    name,
		Backend:    p.Backend,
		Network:    p.Policy.Network.String(),
		Mode:       refusalMode(p.Policy.Enforce),
		Allow:      nonNilStrings(p.Policy.Allow),
		WorkDir:    p.Policy.WorkDir,
		ReadRoots:  nonNilStrings(p.Policy.ReadRoots),
		WriteRoots: nonNilStrings(p.Policy.WriteRoots),
		DenyRoots:  nonNilStrings(p.Policy.DenyRoots),
		Fallback:   p.Fallback,
		Exceptions: nonNilStrings(defaultExecProfiles.exceptions(name)),
	}
	if b, err := sandbox.For(p.Backend); err != nil {
		s.Error = err.Error()
	} else {
		caps := viewCaps(b.Probe(ctx))
		s.Isolation = b.Isolation().String()
		s.Caps = &caps
	}
	return s
}

// failsClosed reports whether the surface's commands would not run at all:
// its backend is unavailable and it enforces. A dry run runs them on the host.
func (s surfaceProbe) failsClosed() bool { return s.Error != "" && s.Mode == refusalMode(true) }

// unconfined reports whether the surface's commands run on the host with no
// boundary: its backend confines nothing, or it is unavailable in a dry run.
func (s surfaceProbe) unconfined() bool {
	return (s.Caps != nil && s.Caps.Isolation == sandbox.IsolationNone.String()) || (s.Error != "" && !s.failsClosed())
}

// probeExecSurfaces probes every exec surface, in display order.
func probeExecSurfaces(ctx context.Context, cfgDir, dataDir string) []surfaceProbe {
	out := make([]surfaceProbe, 0, len(execSurfaces))
	for _, s := range execSurfaces {
		out = append(out, probeSurface(ctx, s, cfgDir, dataDir))
	}
	return out
}

// execProfileLines says what the surfaces' commands run under, a line per
// group of surfaces that probe alike ("local, discord: restricted, …").
func execProfileLines(ps []surfaceProbe) []string {
	var order []string
	groups := map[string][]string{}
	for _, p := range ps {
		st := execProfileState(p)
		if _, ok := groups[st]; !ok {
			order = append(order, st)
		}
		groups[st] = append(groups[st], p.Surface)
	}
	out := make([]string, 0, len(order))
	for _, st := range order {
		out = append(out, strings.Join(groups[st], ", ")+": "+st)
	}
	return out
}

// execProfileState is a surface's probe in a line: its backend and mode, what
// the backend enforces here, and why it confines nothing when it doesn't.
func execProfileState(s surfaceProbe) string {
	ln := s.Backend
	switch {
	case s.failsClosed():
		return ln + " UNAVAILABLE, so its commands do not run: " + s.Error
	case s.Error != "":
		return ln + " unavailable; in dry run its commands run on the host, unconfined: " + s.Error
	case s.unconfined():
		if s.Fallback != "" {
			return ln + ", unconfined (restricted can't run here: " + s.Fallback + ")"
		}
		return ln + ", unconfined"
	}
	mode := "enforcing"
	if s.Mode != refusalMode(true) {
		mode = "dry run (what it refuses re-runs on the host, logged)"
	}
	return fmt.Sprintf("%s, %s, network %s; enforces filesystem=%s network=%s limits=%s env=%s",
		ln, mode, s.Network, s.Caps.Filesystem, s.Caps.Network, s.Caps.Limits, s.Caps.Env)
}

func renderProbe(w io.Writer, r probeReport) {
	fmt.Fprintf(w, "Execution sandbox on %s/%s (agent %s)\n", r.OS, r.Arch, r.Agent)
	fmt.Fprintf(w, "  config  %s\n  data    %s\n", r.ConfigDir, r.DataDir)
	if len(r.ProfileNotes) == 0 {
		fmt.Fprintln(w, "  profiles  built-in defaults (no exec_profiles in settings)")
	}
	for _, n := range r.ProfileNotes {
		fmt.Fprintln(w, "  profiles  "+n)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "SURFACE PROFILES")
	tw := sbxTable(w)
	fmt.Fprintln(tw, "  SURFACE\tBACKEND\tMODE\tISOLATION\tNET POLICY\tFS\tNET\tLIMITS\tENV")
	for _, s := range r.Surfaces {
		if s.Caps == nil {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t-\t%s\tUNAVAILABLE\t-\t-\t-\n", s.Surface, s.Backend, s.Mode, s.Network)
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Surface, s.Backend, s.Mode, s.Isolation, s.Network,
			s.Caps.Filesystem, s.Caps.Network, s.Caps.Limits, s.Caps.Env)
	}
	tw.Flush()
	fmt.Fprintln(w, "  FS/NET/LIMITS/ENV = what the surface's backend enforces on this host (enforced | partial | none).")
	var fellBack []string
	fallback := ""
	for _, s := range r.Surfaces {
		switch {
		case s.failsClosed():
			fmt.Fprintf(w, "  ! %s: %s (its spawns would fail closed)\n", s.Surface, s.Error)
		case s.Error != "":
			fmt.Fprintf(w, "  ! %s: %s (dry run: its spawns run on the host, unconfined)\n", s.Surface, s.Error)
		case s.Fallback != "":
			fellBack, fallback = append(fellBack, s.Surface), s.Fallback // one reason: builtinExec's
		}
		if len(s.Exceptions) > 0 {
			fmt.Fprintf(w, "  %s exceptions (exec_exceptions): %s\n", s.Surface, strings.Join(s.Exceptions, ", "))
		}
	}
	if len(fellBack) > 0 {
		fmt.Fprintf(w, "  ! %s: on the host, unconfined, because %s\n", strings.Join(fellBack, ", "), fallback)
	}

	fmt.Fprintln(w, "\nROOTS")
	for _, g := range groupByRoots(r.Surfaces) {
		fmt.Fprintf(w, "  %s  [%s]\n", strings.Join(g.names, ", "), g.s.Backend)
		row := func(label string, vals []string, empty string) { sbxRows(w, "    ", 6, label, vals, empty) }
		work := g.s.WorkDir
		if work == "" {
			work = "(set per run by the spawn site)"
		}
		row("work", []string{work}, "")
		if note, fatal := workDirDenyNote(g.s.WorkDir, g.s.DenyRoots); note != "" {
			label := "note"
			if fatal {
				label = "warn"
			}
			row(label, []string{note}, "")
		}
		row("read", g.s.ReadRoots, "(whole host, minus deny roots)")
		row("write", g.s.WriteRoots, "(work dir only)")
		row("deny", g.s.DenyRoots, "(none)")
		if g.s.Network == sandbox.NetAllowlist.String() {
			row("allow", g.s.Allow, "(nothing)")
		}
		if g.s.Caps != nil && g.s.Caps.Filesystem == sandbox.EnforceNone.String() {
			fmt.Fprintf(w, "    note   the %s backend does not enforce filesystem roots; they bind only under a confining backend\n", g.s.Backend)
		}
	}

	fmt.Fprintln(w, "\nBACKENDS ON THIS HOST")
	tw = sbxTable(w)
	fmt.Fprintln(tw, "  NAME\tSTATUS\tISOLATION\tFS\tNET\tLIMITS\tENV")
	h := r.Host
	fmt.Fprintf(tw, "  host\tavailable\t%s\t%s\t%s\t%s\t%s\n", h.Isolation, h.Filesystem, h.Network, h.Limits, h.Env)
	if rc := r.Restricted.Caps; rc != nil {
		fmt.Fprintf(tw, "  %s\tavailable\t%s\t%s\t%s\t%s\t%s\n", implName("restricted", rc.Backend),
			rc.Isolation, rc.Filesystem, rc.Network, rc.Limits, rc.Env)
	} else {
		fmt.Fprintln(tw, "  restricted\tUNAVAILABLE\t-\t-\t-\t-\t-")
	}
	tw.Flush()
	const hang = "              " // details hang under a 12-wide name column
	sbxParagraph(w, "  host        ", h.Detail)
	if rc := r.Restricted.Caps; rc != nil {
		sbxParagraph(w, "  restricted  ", rc.Detail)
		used := "no surface yet"
		if len(r.Restricted.UsedBy) > 0 {
			used = strings.Join(r.Restricted.UsedBy, ", ")
		}
		fmt.Fprintf(w, "%sused by: %s\n", hang, used)
	} else {
		sbxParagraph(w, "  restricted  ", r.Restricted.Error)
	}
}

// implName shows a backend's implementation when it differs from its profile
// name, e.g. "restricted (seatbelt)".
func implName(name, impl string) string {
	if impl == "" || impl == name {
		return name
	}
	return name + " (" + impl + ")"
}

// rootGroup is the surfaces that share one backend and one set of roots, so the
// ROOTS section prints each distinct profile once.
type rootGroup struct {
	names []string
	s     surfaceProbe
}

func groupByRoots(surfaces []surfaceProbe) []rootGroup {
	var groups []rootGroup
	index := map[string]int{}
	for _, s := range surfaces {
		key := strings.Join([]string{
			s.Backend, s.Network, s.WorkDir,
			strings.Join(s.ReadRoots, "\x00"), strings.Join(s.WriteRoots, "\x00"),
			strings.Join(s.DenyRoots, "\x00"), strings.Join(s.Allow, "\x00"),
		}, "\x01")
		if i, ok := index[key]; ok {
			groups[i].names = append(groups[i].names, s.Surface)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, rootGroup{names: []string{s.Surface}, s: s})
	}
	return groups
}

// sbxRows prints a labelled list, one value per line with the label (padded to
// width) on the first, or the empty text when there are no values.
func sbxRows(w io.Writer, indent string, width int, label string, vals []string, empty string) {
	if len(vals) == 0 {
		vals = []string{empty}
	}
	for i, v := range vals {
		if i > 0 {
			label = ""
		}
		fmt.Fprintf(w, "%s%-*s %s\n", indent, width, label, v)
	}
}

// -------------------------------------------------------------- image ---

// sandboxImage builds or verifies the container backend's image.
func sandboxImage(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: tenant sandbox image <build|verify> [--cli docker] [--tag tenant-sandbox:latest]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("sandbox image", flag.ContinueOnError)
	cli := fs.String("cli", "docker", "the docker-compatible CLI to use")
	tag := fs.String("tag", sandbox.DefaultImageTag(), "the image tag to build or verify")
	if err := fs.Parse(args[1:]); err != nil {
		return sbxHelpOK(err)
	}
	switch sub {
	case "build":
		fmt.Fprintf(out, "building %s from %s (this contacts a registry for the base only)\n", *tag, sandbox.BaseImageRef())
		id, err := sandbox.BuildImage(ctx, *cli, *tag, func(line string) { fmt.Fprintln(out, "  "+line) })
		if err != nil {
			return fmt.Errorf("sandbox image build: %w", err)
		}
		fmt.Fprintf(out, "built %s\n  id:   %s\n  base: %s\n", *tag, id, sandbox.BaseImageRef())
		return nil
	case "verify":
		id, err := sandbox.VerifyImage(ctx, *cli, *tag)
		if err != nil {
			return fmt.Errorf("sandbox image verify: %w", err)
		}
		fmt.Fprintf(out, "%s is present\n  id:   %s\n  base: %s\n", *tag, id, sandbox.BaseImageRef())
		return nil
	default:
		return fmt.Errorf("sandbox image: unknown subcommand %q (want build or verify)", sub)
	}
}

// ------------------------------------------------------------- selftest ---

// selftestRun is one backend's pass through the escape suite.
type selftestRun struct {
	Backend string                  // Backend.Name()
	Caps    sandbox.Capabilities    // Backend.Probe(): what it claims to enforce
	Cases   []escapecore.CaseResult // in suite order
	Err     string                  // the suite could not finish (infrastructure failure)
}

// Per-case verdicts. Host verdicts describe the unconfined baseline; restricted
// verdicts judge the case against the backend's own Probe claim.
const (
	verdictEscaped   = "escaped"     // host: the adversary succeeded, as expected
	verdictContained = "contained"   // host: the adversary failed even unconfined
	verdictPass      = "pass"        // restricted: contained, and the host shows it would escape
	verdictFail      = "FAIL"        // restricted: a claimed dimension did not hold, or the twin broke
	verdictUnproven  = "UNPROVEN"    // restricted: contained, but the host baseline was too
	verdictUnclaimed = "not claimed" // restricted: not contained in a dimension it does not claim
	verdictSkipped   = "skipped"     // the adversarial half was not evaluated
)

// caseVerdict is the selftest's judgement of one case on one backend.
type caseVerdict struct {
	Claim   string // restricted only: the Probe's enforcement for the case's kind
	Verdict string
	Reason  string
}

// selftestDecision is the pure outcome of a selftest.
type selftestDecision struct {
	Pass       bool
	Problems   []string      // each one fails the selftest
	Notes      []string      // informational; never fail it
	Host       []caseVerdict // parallel to the host run's Cases
	Restricted []caseVerdict // parallel to the restricted run's Cases; nil when unavailable
}

// decideSelftest judges a selftest. It is pure, so every rule is table-tested.
// The selftest FAILS when:
//   - this OS has no restricted backend (nothing can be contained);
//   - either backend could not finish the suite;
//   - any legitimate twin fails on either backend (a boundary that breaks
//     legitimate work is not a pass);
//   - the restricted backend does not contain a case whose dimension its Probe
//     reports as EnforceFull (a dimension reported partial/none is shown as
//     "not claimed" and does not fail);
//   - a restricted containment in an EnforceFull dimension is UNPROVEN because
//     the unconfined host baseline did not escape either (the adversary is
//     broken on this host, e.g. a /bin/sh without /dev/tcp, so the run cannot
//     show the backend blocked it);
//   - no case at all was evaluated against the restricted backend.
func decideSelftest(host selftestRun, restricted *selftestRun, unavailable string) selftestDecision {
	var d selftestDecision
	problem := func(format string, a ...any) { d.Problems = append(d.Problems, fmt.Sprintf(format, a...)) }
	skipNoted := map[string]bool{}
	noteSkip := func(c escapecore.CaseResult) {
		if !skipNoted[c.Name] {
			skipNoted[c.Name] = true
			d.Notes = append(d.Notes, fmt.Sprintf("%s skipped: %s", c.Name, c.SkipReason))
		}
	}

	if host.Err != "" {
		problem("host baseline could not finish the escape suite: %s", host.Err)
	}
	baseline := make(map[string]escapecore.CaseResult, len(host.Cases))
	for _, c := range host.Cases {
		baseline[c.Name] = c
		v := caseVerdict{Verdict: verdictEscaped}
		switch {
		case c.Skipped:
			v.Verdict, v.Reason = verdictSkipped, c.SkipReason
			noteSkip(c)
		case c.AdversarialContained:
			v.Verdict, v.Reason = verdictContained, "the adversary fails even unconfined"
			d.Notes = append(d.Notes, fmt.Sprintf(
				"host/%s: the adversary failed even unconfined (exit %d%s); containing it elsewhere proves nothing",
				c.Name, c.AdversarialExit, sbxExcerpt(c.AdversarialOutput)))
		}
		if !c.LegitOK {
			problem("host/%s: legitimate twin failed (exit %d%s)", c.Name, c.LegitExit, sbxExcerpt(c.LegitOutput))
		}
		d.Host = append(d.Host, v)
	}

	if restricted == nil {
		problem("this OS has no restricted backend (%s), so nothing can be contained", unavailable)
		return d
	}
	if restricted.Err != "" {
		problem("restricted backend could not finish the escape suite: %s", restricted.Err)
	}
	evaluated := 0
	for _, c := range restricted.Cases {
		claim := claimFor(c.Kind, restricted.Caps)
		full := claim == sandbox.EnforceFull
		base, hasBase := baseline[c.Name]
		baselineContained := hasBase && !base.Skipped && base.AdversarialContained
		v := caseVerdict{Claim: claim.String()}
		switch {
		case c.Skipped:
			v.Verdict, v.Reason = verdictSkipped, c.SkipReason
			noteSkip(c)
		case !c.AdversarialContained && full:
			v.Verdict = verdictFail
			v.Reason = fmt.Sprintf("claims %s=%s but did not contain it", c.Kind, claim)
			problem("restricted/%s: Probe claims %s=%s but the adversary was NOT contained (exit %d%s)",
				c.Name, c.Kind, claim, c.AdversarialExit, sbxExcerpt(c.AdversarialOutput))
		case !c.AdversarialContained:
			v.Verdict = verdictUnclaimed
			v.Reason = fmt.Sprintf("not contained; the backend claims only %s=%s", c.Kind, claim)
		case baselineContained:
			// The host note above already records the broken adversary; only a
			// claimed dimension turns it into a failure.
			v.Verdict, v.Reason = verdictUnproven, "the adversary also fails on the unconfined host"
			if full {
				problem("restricted/%s: containment unproven: the adversary also fails on the unconfined host "+
					"(exit %d%s), so this run cannot show the backend blocked it; the case needs a probe that works on this host",
					c.Name, base.AdversarialExit, sbxExcerpt(base.AdversarialOutput))
			}
		default:
			v.Verdict = verdictPass
		}
		if !c.Skipped {
			evaluated++
		}
		if !c.LegitOK {
			problem("restricted/%s: legitimate twin failed (exit %d%s); the boundary breaks legitimate work",
				c.Name, c.LegitExit, sbxExcerpt(c.LegitOutput))
			v.Verdict = verdictFail
			v.Reason = strings.TrimPrefix(v.Reason+"; legitimate twin failed", "; ")
		}
		d.Restricted = append(d.Restricted, v)
	}
	if evaluated == 0 && restricted.Err == "" {
		problem("no escape case was evaluated against the restricted backend")
	}
	d.Pass = len(d.Problems) == 0
	return d
}

// claimFor is the backend's own Probe claim for the policy dimension a case
// exercises. An unrecognized dimension is held to full enforcement (fail closed:
// a new kind of case must be contained until the selftest learns what it maps to).
func claimFor(kind string, caps sandbox.Capabilities) sandbox.Enforcement {
	switch kind {
	case escapecore.KindFilesystem:
		return caps.Filesystem
	case escapecore.KindNetwork:
		return caps.Network
	case "limit", "limits":
		return caps.Limits
	case "env":
		return caps.Env
	default:
		return sandbox.EnforceFull
	}
}

// selftestReport is the selftest's output (the --json document).
type selftestReport struct {
	OS                    string           `json:"os"`
	Arch                  string           `json:"arch"`
	Pass                  bool             `json:"pass"`
	Problems              []string         `json:"problems"`
	Notes                 []string         `json:"notes"`
	Host                  selftestRunView  `json:"host"`
	Restricted            *selftestRunView `json:"restricted"` // null when this OS has none
	RestrictedUnavailable string           `json:"restricted_unavailable,omitempty"`
}

type selftestRunView struct {
	Backend      string             `json:"backend"`
	Capabilities sbxCaps            `json:"capabilities"`
	Error        string             `json:"error,omitempty"`
	Cases        []selftestCaseView `json:"cases"`
}

type selftestCaseView struct {
	escapecore.CaseResult
	Claim   string `json:"claim,omitempty"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}

func sandboxSelftest(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sandbox selftest", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	backend := fs.String("backend", "restricted", "the confining backend to test: restricted|container")
	if err := fs.Parse(args); err != nil {
		return sbxHelpOK(err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("sandbox selftest: unexpected argument %q", fs.Arg(0))
	}

	host := evaluateBackend(ctx, sandbox.Host())

	b, rerr := confinedBackendFor(ctx, *backend)

	var restricted *selftestRun
	unavailable := ""
	if rerr != nil {
		unavailable = rerr.Error()
	} else {
		r := evaluateBackend(ctx, b)
		restricted = &r
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sandbox selftest: interrupted: %w", err)
	}

	d := decideSelftest(host, restricted, unavailable)
	rep := buildSelftestReport(host, restricted, unavailable, d)
	if *asJSON {
		if err := sbxWriteJSON(out, rep); err != nil {
			return err
		}
	} else {
		renderSelftest(out, rep)
	}
	if !d.Pass {
		return fmt.Errorf("sandbox selftest failed: %d problem(s)", len(d.Problems))
	}
	return nil
}

// confinedBackendFor resolves the confining backend selftest evaluates. The
// default "restricted" is the OS-native backend (macOS Seatbelt); "container"
// is the Docker backend, reported unavailable (never a hard error) when its
// engine or image is not ready, so the selftest says why rather than failing
// every case. Any other name resolves through sandbox.For.
func confinedBackendFor(ctx context.Context, name string) (sandbox.Backend, error) {
	switch name {
	case "", "restricted":
		return sandbox.Restricted()
	case "container":
		b, err := sandbox.For("container")
		if err != nil {
			return nil, err
		}
		if caps := b.Probe(ctx); caps.Filesystem != sandbox.EnforceFull {
			return nil, fmt.Errorf("the container backend is not ready: %s", caps.Detail)
		}
		// Probe runs nothing. Available also starts one container, so an engine
		// that answers but cannot start one is reported here, once, with its
		// reason, and not as every case timing out.
		if a, ok := b.(sandbox.Available); ok {
			if err := a.Available(ctx); err != nil {
				return nil, fmt.Errorf("the container backend is not ready: %w", err)
			}
		}
		return b, nil
	default:
		return sandbox.For(name)
	}
}

func evaluateBackend(ctx context.Context, b sandbox.Backend) selftestRun {
	r := selftestRun{Backend: b.Name(), Caps: b.Probe(ctx)}
	cases, err := escapecore.Evaluate(ctx, b)
	r.Cases = cases
	if err != nil {
		r.Err = err.Error()
	}
	return r
}

func buildSelftestReport(host selftestRun, restricted *selftestRun, unavailable string, d selftestDecision) selftestReport {
	rep := selftestReport{
		OS: runtime.GOOS, Arch: runtime.GOARCH, Pass: d.Pass,
		Problems:              nonNilStrings(d.Problems),
		Notes:                 nonNilStrings(d.Notes),
		Host:                  viewRun(host, d.Host),
		RestrictedUnavailable: unavailable,
	}
	if restricted != nil {
		v := viewRun(*restricted, d.Restricted)
		rep.Restricted = &v
	}
	return rep
}

func viewRun(r selftestRun, verdicts []caseVerdict) selftestRunView {
	v := selftestRunView{Backend: r.Backend, Capabilities: viewCaps(r.Caps), Error: r.Err, Cases: []selftestCaseView{}}
	for i, c := range r.Cases {
		cv := selftestCaseView{CaseResult: c}
		if i < len(verdicts) {
			cv.Claim, cv.Verdict, cv.Reason = verdicts[i].Claim, verdicts[i].Verdict, verdicts[i].Reason
		}
		v.Cases = append(v.Cases, cv)
	}
	return v
}

func renderSelftest(w io.Writer, rep selftestReport) {
	fmt.Fprintf(w, "Sandbox selftest: escape suite on %s/%s\n", rep.OS, rep.Arch)
	sbxParagraph(w, "", "Every case runs under the suite's own policy (work dir read/write, a secret dir outside "+
		"the roots denied, network none, 10s wall). The host runs first as the unconfined baseline: a restricted "+
		"containment counts only when the same adversary escapes on the host.")

	fmt.Fprintf(w, "\nhost: baseline, expected NOT to contain (isolation %s)\n", rep.Host.Capabilities.Isolation)
	tw := sbxTable(w)
	fmt.Fprintln(tw, "  CASE\tKIND\tADVERSARY\tLEGIT TWIN")
	for _, c := range rep.Host.Cases {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", c.Name, c.Kind, c.Verdict, legitCell(c.CaseResult))
	}
	tw.Flush()
	if rep.Host.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", rep.Host.Error)
	}

	if r := rep.Restricted; r == nil {
		fmt.Fprintf(w, "\nrestricted: UNAVAILABLE on %s: %s\n", rep.OS, rep.RestrictedUnavailable)
	} else {
		c := r.Capabilities
		fmt.Fprintf(w, "\nrestricted: %s, isolation %s\n", c.Backend, c.Isolation)
		fmt.Fprintf(w, "  claims  filesystem=%s  network=%s  limits=%s  env=%s\n", c.Filesystem, c.Network, c.Limits, c.Env)
		tw = sbxTable(w)
		fmt.Fprintln(tw, "  CASE\tKIND\tCLAIM\tADVERSARY\tLEGIT TWIN\tVERDICT")
		for _, cv := range r.Cases {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", cv.Name, cv.Kind, cv.Claim, adversaryCell(cv.CaseResult),
				legitCell(cv.CaseResult), cv.Verdict)
		}
		tw.Flush()
		if r.Error != "" {
			fmt.Fprintf(w, "  error: %s\n", r.Error)
		}
	}

	if len(rep.Notes) > 0 {
		fmt.Fprintln(w, "\nnotes:")
		for _, n := range rep.Notes {
			sbxParagraph(w, "  - ", n)
		}
	}
	fmt.Fprintln(w)
	if !rep.Pass || rep.Restricted == nil {
		fmt.Fprintf(w, "FAIL: %d problem(s):\n", len(rep.Problems))
		for i, p := range rep.Problems {
			sbxParagraph(w, fmt.Sprintf("  %d. ", i+1), p)
		}
		return
	}
	sbxParagraph(w, "", "PASS: "+passSummary(*rep.Restricted))
}

// passSummary words a passing selftest honestly: a backend that contains only
// what it claims is not described as a full boundary.
func passSummary(r selftestRunView) string {
	var contained, evaluated int
	var unclaimed []string
	for _, c := range r.Cases {
		if c.Skipped {
			continue
		}
		evaluated++
		if c.AdversarialContained {
			contained++
		}
		if c.Verdict == verdictUnclaimed {
			unclaimed = append(unclaimed, c.Kind+"="+c.Claim)
		}
	}
	name := r.Capabilities.Backend
	if len(unclaimed) == 0 {
		return fmt.Sprintf("restricted (%s) contained %d/%d evaluated case(s); every dimension it claims to enforce held "+
			"and every legitimate twin succeeded on both backends.", name, contained, evaluated)
	}
	slices.Sort(unclaimed)
	return fmt.Sprintf("every dimension restricted (%s) claims to enforce held and every legitimate twin succeeded, "+
		"but it contained only %d/%d evaluated case(s): the rest are in dimensions it does not claim to fully "+
		"enforce (%s), so it is NOT a full boundary on this host.",
		name, contained, evaluated, strings.Join(slices.Compact(unclaimed), ", "))
}

func adversaryCell(c escapecore.CaseResult) string {
	switch {
	case c.Skipped:
		return verdictSkipped
	case c.AdversarialContained:
		return "contained"
	default:
		return "NOT contained"
	}
}

func legitCell(c escapecore.CaseResult) string {
	if c.LegitOK {
		return "ok"
	}
	return fmt.Sprintf("FAILED (exit %d)", c.LegitExit)
}

// ------------------------------------------------------------------ run ---

// sandboxRunOpts is a parsed `tenant sandbox run` invocation.
type sandboxRunOpts struct {
	dirs    *commonFlags
	surface string
	backend string        // "" = the surface profile's own backend
	timeout time.Duration // wall-clock limit (os_exec's default is 60s)
	workDir string        // "" = the profile's work dir, else a fresh temp dir
	command string        // everything after --, joined by spaces, run through the platform shell
}

// parseSandboxRunArgs parses `run` arguments. Flags come before a mandatory --;
// everything after it is the command, so the command's own flags are never
// mistaken for ours. The command runs through the platform shell (sh -c /
// PowerShell -Command) exactly as os_exec and cron-shell commands do; pass it
// as one quoted argument to keep inner quoting.
func parseSandboxRunArgs(args []string) (sandboxRunOpts, error) {
	flagArgs, cmdArgs, hasSep := args, []string(nil), false
	if i := slices.Index(args, "--"); i >= 0 {
		flagArgs, cmdArgs, hasSep = args[:i], args[i+1:], true
	}
	fs := flag.NewFlagSet("sandbox run", flag.ContinueOnError)
	o := sandboxRunOpts{dirs: bindSandboxDirs(fs)}
	fs.StringVar(&o.surface, "surface", "", "surface whose exec profile to run under: "+strings.Join(sandboxSurfaces, "|"))
	fs.StringVar(&o.backend, "backend", "", "force a backend instead of the profile's: host|restricted|container")
	fs.DurationVar(&o.timeout, "timeout", 60*time.Second, "wall-clock limit for the command")
	fs.StringVar(&o.workDir, "workdir", "", "run in this dir (default: the profile's work dir, else a fresh temp dir)")
	if err := fs.Parse(flagArgs); err != nil {
		return sandboxRunOpts{}, err
	}
	if !hasSep {
		return sandboxRunOpts{}, fmt.Errorf("sandbox run: put the command after --\n%s", sandboxRunUsage)
	}
	if fs.NArg() > 0 {
		return sandboxRunOpts{}, fmt.Errorf("sandbox run: unexpected argument %q before -- (the command goes after it)", fs.Arg(0))
	}
	if o.surface == "" {
		return sandboxRunOpts{}, fmt.Errorf("sandbox run: --surface is required (%s)", strings.Join(sandboxSurfaces, ", "))
	}
	if !slices.Contains(sandboxSurfaces, o.surface) {
		return sandboxRunOpts{}, fmt.Errorf("sandbox run: unknown surface %q (want one of %s)", o.surface, strings.Join(sandboxSurfaces, ", "))
	}
	switch o.backend {
	case "", "host", "restricted", "container":
	default:
		return sandboxRunOpts{}, fmt.Errorf("sandbox run: --backend must be host, restricted or container, got %q", o.backend)
	}
	if o.timeout <= 0 {
		return sandboxRunOpts{}, fmt.Errorf("sandbox run: --timeout must be positive, got %s", o.timeout)
	}
	o.command = strings.TrimSpace(strings.Join(cmdArgs, " "))
	if o.command == "" {
		return sandboxRunOpts{}, fmt.Errorf("sandbox run: no command after --\n%s", sandboxRunUsage)
	}
	return o, nil
}

func sandboxRun(ctx context.Context, args []string, out io.Writer) error {
	o, err := parseSandboxRunArgs(args)
	if err != nil {
		return sbxHelpOK(err)
	}
	if err := o.dirs.resolveDirs(); err != nil {
		return err
	}
	for _, n := range loadSandboxProfiles(o.dirs) {
		fmt.Fprintln(out, "note: "+n)
	}
	if o.workDir != "" {
		if o.workDir, err = filepath.Abs(expandPath(o.workDir)); err != nil {
			return fmt.Errorf("sandbox run: --workdir: %w", err)
		}
	}

	p := resolveExecProfile(o.surface, o.dirs.cfgDir, o.dirs.dataDir, o.workDir)
	chosen := "profile"
	if o.backend != "" {
		p, chosen = withBackend(p, o.backend), "forced by --backend"
	}
	be, err := sandbox.For(p.Backend)
	if err == nil {
		// The same readiness gate a spawn site applies (profileBackend): a
		// backend that is known but cannot run anything here says why now.
		if a, ok := be.(sandbox.Available); ok {
			err = a.Available(ctx)
		}
	}
	if err != nil {
		return fmt.Errorf("sandbox run: %s backend %q unavailable: %w", o.surface, p.Backend, err)
	}
	workNote := "profile"
	switch {
	case p.Policy.WorkDir == "":
		dir, err := os.MkdirTemp("", "tenant-sandbox-run-")
		if err != nil {
			return fmt.Errorf("sandbox run: temp work dir: %w", err)
		}
		defer os.RemoveAll(dir)
		p.Policy.WorkDir, workNote = dir, "fresh temp dir, removed afterwards"
	case o.workDir != "":
		workNote = "--workdir"
	}
	pol, applied := sandboxRunPolicy(o.surface, p.Policy, o.timeout)

	caps := viewCaps(be.Probe(ctx))
	name := implName(be.Name(), caps.Backend)
	row := func(label string, vals []string, empty string) { sbxRows(out, "", 11, label, vals, empty) }
	kv := func(label, format string, a ...any) { row(label, []string{fmt.Sprintf(format, a...)}, "") }
	kv("surface", "%s", o.surface)
	kv("backend", "%s, isolation %s [%s]", name, be.Isolation(), chosen)
	kv("enforces", "filesystem=%s  network=%s  limits=%s  env=%s", caps.Filesystem, caps.Network, caps.Limits, caps.Env)
	kv("work", "%s (%s)", pol.WorkDir, workNote)
	if note, fatal := workDirDenyNote(pol.WorkDir, pol.DenyRoots); note != "" {
		label := "note"
		if fatal {
			label = "warning"
		}
		kv(label, "%s", note)
	}
	row("read", pol.ReadRoots, "(whole host, minus deny roots)")
	row("write", pol.WriteRoots, "(work dir only)")
	row("deny", pol.DenyRoots, "(none)")
	kv("network", "%s", pol.Network)
	kv("env", "%s", describeEnv(pol.Env))
	kv("wall", "%s", pol.Limits.Wall)
	row("mirrors", applied, "(nothing beyond the profile)")
	kv("command", "%s", o.command)

	cmd := sandbox.Command{Shell: o.command, Surface: o.surface, Origin: "sandbox-run"}
	if o.surface == surfaceCronShell {
		cmd = sandbox.Command{Args: cronShellArgv(o.command), Surface: o.surface, Origin: "sandbox-run"} // as cron runs it: cmd /C on Windows
	}
	res, err := be.Run(ctx, cmd, pol)
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("sandbox run: interrupted: %w", cerr)
	}
	if err != nil {
		return fmt.Errorf("sandbox run: backend %s could not start the command: %w", be.Name(), err)
	}

	fmt.Fprintln(out, "--- output ---")
	if res.Output != "" {
		fmt.Fprint(out, res.Output)
		if !strings.HasSuffix(res.Output, "\n") {
			fmt.Fprintln(out)
		}
	}
	fmt.Fprintln(out, "--- end output ---")
	kv("exit code", "%d", res.ExitCode)
	kv("timed out", "%v", res.TimedOut)
	violations := make([]string, 0, len(res.Violations))
	for _, v := range res.Violations {
		violations = append(violations, v.Kind+": "+v.Detail)
	}
	row("violations", violations, "none")
	return sandboxRunOutcome(res)
}

// sandboxRunPolicy layers onto a resolved profile's Policy what the surface's
// production spawn site layers on, so a trial run behaves like the real one:
// every site caps wall time (sandboxRunner sets Wall from os_exec's timeout;
// here --timeout), and cron-shell additionally caps output (runSandboxShell). A
// trial always enforces, so it shows what the boundary refuses even for a
// dry-run profile, whose real runs would re-run those on the host. The
// environment is the profile's allowlist, as at every site. It returns the
// policy plus one line per adjustment.
func sandboxRunPolicy(surface string, pol sandbox.Policy, timeout time.Duration) (sandbox.Policy, []string) {
	pol.Enforce = true
	pol.Limits.Wall = timeout
	var applied []string
	if surface == "cron-shell" && pol.Limits.MaxOutputBytes == 0 {
		pol.Limits.MaxOutputBytes = cronShellOutputCap
		applied = append(applied, fmt.Sprintf("cron-shell output cap of %d bytes", cronShellOutputCap))
	}
	return pol, applied
}

// sandboxRunOutcome maps a finished run to the command's exit status: clean only
// when it exited 0, in time, with nothing blocked.
func sandboxRunOutcome(res sandbox.Result) error {
	switch {
	case res.TimedOut:
		return errors.New("sandbox run: the command timed out")
	case res.ExitCode != 0:
		return fmt.Errorf("sandbox run: the command exited %d", res.ExitCode)
	case len(res.Violations) > 0:
		return fmt.Errorf("sandbox run: the sandbox recorded %d violation(s)", len(res.Violations))
	}
	return nil
}

// workDirDenyNote explains how the Policy carve-out treats a work dir that
// overlaps a deny root. A work dir strictly INSIDE a deny root stays usable (the
// rest of the root stays denied) — that is how cron's work dir lives under the
// data dir. A work dir that IS a deny root fails closed. fatal reports the latter.
func workDirDenyNote(dir string, deny []string) (note string, fatal bool) {
	root := deniedRootOf(dir, deny)
	if root == "" {
		return "", false
	}
	if canonicalPath(root) == canonicalPath(dir) {
		return "the work dir IS deny root " + root + ": a confining backend fails closed (the carve-out never reopens a deny root)", true
	}
	return "the work dir is inside deny root " + root + "; the carve-out keeps it usable while the rest of that root stays denied", false
}

// deniedRootOf returns the deny root that contains (or is) dir, or "". Paths are
// compared symlink-resolved, as the backends match them.
func deniedRootOf(dir string, deny []string) string {
	if dir == "" {
		return ""
	}
	d := canonicalPath(dir)
	for _, root := range deny {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(canonicalPath(root), d)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return root
		}
	}
	return ""
}

// canonicalPath is p absolute and symlink-resolved. A path that does not exist
// yet resolves through its deepest existing ancestor, so it compares like an
// existing one under a symlinked dir (macOS /var -> /private/var).
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.Clean(abs)
	tail := ""
	for cur := abs; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

func describeEnv(e sandbox.EnvPolicy) string {
	if !envScoped(e) {
		return "inherit (the full parent environment)"
	}
	return fmt.Sprintf("allowlist: %d variable(s) forwarded, %d set", len(e.Pass), len(e.Set))
}

// -------------------------------------------------------------- helpers ---

func sbxTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

func sbxWriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// sbxParagraph prints text word-wrapped to ~100 columns: the first line after
// prefix, continuation lines indented to match it.
func sbxParagraph(w io.Writer, prefix, text string) {
	indent := strings.Repeat(" ", utf8.RuneCountInString(prefix))
	for i, line := range sbxWrap(text, 100-len(indent)) {
		if i == 0 {
			fmt.Fprintln(w, prefix+line)
		} else {
			fmt.Fprintln(w, indent+line)
		}
	}
}

// sbxWrap breaks s into lines of at most width runes at spaces.
func sbxWrap(s string, width int) []string {
	var lines []string
	cur := ""
	for _, word := range strings.Fields(s) {
		if cur != "" && utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(word) > width {
			lines = append(lines, cur)
			cur = ""
		}
		if cur != "" {
			cur += " "
		}
		cur += word
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// sbxExcerpt is a short single-line excerpt of command output for a problem
// line (": <first line>"), or "" when there is none.
func sbxExcerpt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " ..."
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "..."
	}
	return ": " + s
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// loadSandboxProfiles loads the operator's exec_profiles from settings so probe
// and run show and use the same profiles the running hub does. It returns one
// line per problem (unreadable settings, invalid entries skipped).
func loadSandboxProfiles(c *commonFlags) []string {
	n, skipped, err := loadExecProfilesFromSettings(c.cfgDir, c.dataDir, c.agent)
	if err != nil {
		return []string{"exec_profiles not loaded (" + err.Error() + "); showing built-in defaults"}
	}
	notes := make([]string, 0, len(skipped)+1)
	if n > 0 {
		notes = append(notes, fmt.Sprintf("%d exec profile override(s) loaded from settings", n))
	}
	for _, sk := range skipped {
		notes = append(notes, "skipped invalid entry: "+sk)
	}
	return notes
}

// refusalMode names what a refusal does on a surface: enforce escalates to the
// surface's approver (unattended surfaces deny); dry-run re-runs on the host and
// only logs the refusal.
func refusalMode(enforce bool) string {
	if enforce {
		return "enforce"
	}
	return "dry-run"
}
