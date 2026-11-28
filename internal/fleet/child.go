package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unicode"

	"tenant/internal/datalock"
	"tenant/internal/secrets"
)

const (
	childFileName  = "child.json"
	childVersion   = 1
	maxChildBytes  = 64 << 10
	maxChildTools  = 512
	maxToolLen     = 128
	maxSandboxLen  = 32
	maxSecretPaths = 256
	maxPathLen     = 1024
)

// Ceiling is the limit a machine sets on itself, in its own child.json,
// which its master cannot lift.
type Ceiling string

const (
	// CeilingFull: the machine does whatever its master approves.
	CeilingFull Ceiling = "full"
	// CeilingNoExec: the machine refuses every action that runs a command.
	CeilingNoExec Ceiling = "no-exec"
	// CeilingReadOnly: the machine refuses every action that needs approval.
	CeilingReadOnly Ceiling = "read-only"
)

func (c Ceiling) valid() bool {
	switch c {
	case CeilingFull, CeilingNoExec, CeilingReadOnly:
		return true
	}
	return false
}

// Child is child.json: all a child knows about its master and about what it
// lets that master do. The fields from ID to Ceiling are required. Whether
// the machine is paused is not in this file (PausedPath): a pause is a state
// a command sets, and this file is what the operator wrote.
type Child struct {
	// ID and Name are the ones the master gave this machine at enrollment.
	ID   string `json:"id"`
	Name string `json:"name"`
	// MasterAddresses are the host:port addresses to dial, tried in turn.
	MasterAddresses []string `json:"master_addresses"`
	// MasterFingerprint pins the master's certificate. It is the master's
	// identity; the addresses are only where to look for it.
	MasterFingerprint string `json:"master_fingerprint"`
	// Sandbox is the backend this machine runs commands through.
	Sandbox string  `json:"sandbox"`
	Ceiling Ceiling `json:"ceiling"`
	// Tools, when the key is there, lists the only tools this machine serves.
	// Left out, the ceiling alone decides. An empty list is an error, not
	// "none" and not "all".
	Tools []string `json:"tools,omitempty"`
	// SecretPaths are the operator's additions to what this machine's file
	// tools never read or write, whoever asks: absolute paths of files or
	// directories, the last element of which may use "*".
	SecretPaths []string `json:"secret_paths,omitempty"`
	// Updates is whose word an update over the link is taken on (decision
	// A71): "" or UpdatesRelease, the release's own signature, verified on
	// this machine against the release identity built into its binary; or
	// UpdatesMaster, the SHA-256 its master names, so that an owner can ship
	// a build of their own. Chosen on the machine, never over the link. The
	// default is written as nothing, so that an older binary put back by a
	// rollback still reads the file.
	Updates UpdateRule `json:"updates,omitempty"`
}

// UpdateRule is Child.Updates.
type UpdateRule string

const (
	UpdatesRelease UpdateRule = "release"
	UpdatesMaster  UpdateRule = "master"
)

// Rule is the rule in force: UpdatesRelease unless the file says master.
func (c Child) Rule() UpdateRule {
	if c.Updates == UpdatesMaster {
		return UpdatesMaster
	}
	return UpdatesRelease
}

// childFile is child.json as written: Child and the file's version.
type childFile struct {
	Version int `json:"version"`
	Child
}

// Serves reports whether this machine serves a tool at all. A Child that was
// not loaded from a valid file serves nothing.
func (c Child) Serves(tool string) bool {
	return c.Ceiling.valid() && (c.Tools == nil || slices.Contains(c.Tools, tool))
}

var errInvalidSandbox = fmt.Errorf("fleet: the sandbox backend is a name of up to %d lowercase letters, digits, '-' or '_'", maxSandboxLen)

func validSandbox(s string) bool {
	if len(s) == 0 || len(s) > maxSandboxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func (c Child) validate() error {
	if !validID(c.ID) {
		return errors.New("fleet: child.json: id is not a machine id")
	}
	if !validName(c.Name) {
		return errInvalidName(c.Name)
	}
	if len(c.MasterAddresses) == 0 {
		return errors.New("fleet: child.json: master_addresses is empty")
	}
	if err := validAddresses(c.MasterAddresses); err != nil {
		return err
	}
	if _, err := parsePin(c.MasterFingerprint); err != nil {
		return fmt.Errorf("child.json: master_fingerprint: %w", err)
	}
	if !validSandbox(c.Sandbox) {
		return errInvalidSandbox
	}
	if !c.Ceiling.valid() {
		return errors.New("fleet: child.json: ceiling is not full, no-exec or read-only")
	}
	switch c.Updates {
	case "", UpdatesRelease, UpdatesMaster:
	default:
		return errors.New("fleet: child.json: updates is release or master")
	}
	if c.SecretPaths != nil && (len(c.SecretPaths) == 0 || len(c.SecretPaths) > maxSecretPaths) {
		return fmt.Errorf("fleet: child.json: secret_paths lists between 1 and %d paths; leave it out to add none", maxSecretPaths)
	}
	for i, p := range c.SecretPaths {
		// A relative path would name a different file from every working
		// directory, and so protect none.
		if len(p) > maxPathLen || !printable(p) || !filepath.IsAbs(p) || slices.Contains(c.SecretPaths[:i], p) {
			return fmt.Errorf("fleet: child.json: secret_paths entry %d is not an absolute path, or is there twice", i)
		}
	}
	if c.Tools == nil {
		return nil
	}
	if len(c.Tools) == 0 || len(c.Tools) > maxChildTools {
		return fmt.Errorf("fleet: child.json: tools lists between 1 and %d tools; leave it out to serve every tool", maxChildTools)
	}
	for i, tool := range c.Tools {
		if len(tool) == 0 || len(tool) > maxToolLen || !printable(tool) || strings.ContainsFunc(tool, unicode.IsSpace) ||
			slices.Contains(c.Tools[:i], tool) {
			return fmt.Errorf("fleet: child.json: tools entry %d is not a tool name, or is there twice", i)
		}
	}
	return nil
}

// parseChild reads child.json strictly: a key it does not know, a missing or
// malformed value, or a second value in the file is an error. A misspelt
// ceiling must stop the child, not leave it wide open.
func parseChild(b []byte) (Child, error) {
	if len(b) > maxChildBytes {
		return Child{}, fmt.Errorf("fleet: child.json is larger than %d bytes", maxChildBytes)
	}
	var f childFile
	if err := decodeStrict(b, &f); err != nil {
		return Child{}, fmt.Errorf("fleet: child.json: %w", err)
	}
	if f.Version != childVersion {
		return Child{}, fmt.Errorf("fleet: child.json is version %d and this Tenant reads version %d", f.Version, childVersion)
	}
	if err := f.Child.validate(); err != nil {
		return Child{}, err
	}
	return f.Child, nil
}

// ChildPath is child.json in dir.
func ChildPath(dir string) string { return filepath.Join(dir, childFileName) }

// SystemDir is where a child installed with --system keeps child.json and
// its identity: a place the service account reads and the installing user's
// profile is not part of. The installer creates it.
func SystemDir() string { return systemDir(runtime.GOOS, os.Getenv("ProgramData")) }

func systemDir(goos, programData string) string {
	switch goos {
	case "windows":
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return strings.TrimRight(programData, `\/`) + `\tenant\child`
	case "darwin":
		return "/Library/Application Support/tenant/child"
	}
	return "/etc/tenant/child"
}

// LoadChild reads and checks child.json. It does not change the file's
// protection, so a file another account owns and this one may only read
// still loads.
func LoadChild(path string) (Child, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Child{}, fmt.Errorf("fleet: child.json: %w", err)
	}
	if fi.Size() > maxChildBytes {
		return Child{}, fmt.Errorf("fleet: child.json is larger than %d bytes", maxChildBytes)
	}
	b, err := secrets.Peek(path)
	if err != nil {
		return Child{}, fmt.Errorf("fleet: child.json: %w", err)
	}
	return parseChild(b)
}

// SaveChild checks c and writes it as child.json, owner-only, replacing the
// file whole.
func SaveChild(path string, c Child) error {
	if err := c.validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(childFile{Version: childVersion, Child: c}, "", "  ")
	if err != nil {
		return fmt.Errorf("fleet: encode child.json: %w", err)
	}
	if err := writePrivate(path, append(b, '\n')); err != nil {
		return fmt.Errorf("fleet: save child.json: %w", err)
	}
	return nil
}

// UpdateChild changes child.json in place: under the file's lock it reads
// the file as it is now, lets fn change that, and saves it. A running child
// that stores a new address list this way keeps a ceiling the operator
// edited a moment before, where saving a copy from memory would undo it.
func UpdateChild(path string, fn func(*Child) error) error {
	lock, err := datalock.File(path, true)
	if err != nil {
		return fmt.Errorf("fleet: lock child.json: %w", err)
	}
	defer lock.Release()
	c, err := LoadChild(path)
	if err != nil {
		return err
	}
	if err := fn(&c); err != nil {
		return err
	}
	return SaveChild(path, c)
}

// ChildFile is a child's view of its own child.json, kept in step with the
// disk. The ceiling is the operator's word on that machine, so it is taken
// from the file as it is at the moment of each check, not from what the
// process read when it started.
type ChildFile struct {
	path string

	mu     sync.Mutex
	loaded bool
	stamp  stamp
	cur    Child
	err    error
}

// OpenChild returns the view of the child.json at path. It reads nothing
// yet; Current does.
func OpenChild(path string) *ChildFile { return &ChildFile{path: path} }

// Current is child.json as it is on disk now. It costs one stat while the
// file is unchanged and re-reads it when the stat differs. While the file is
// missing or does not pass LoadChild it returns the error and a Child that
// serves nothing: a child that cannot read its ceiling refuses.
func (f *ChildFile) Current() (Child, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fi, err := os.Lstat(f.path)
	if err != nil {
		f.loaded = false
		return Child{}, fmt.Errorf("fleet: child.json: %w", err)
	}
	// The stat comes before the read, so a file replaced between the two is
	// read again next time rather than missed.
	// ponytail: time and size, as peers.json is watched. A rewrite of equal
	// size inside one timestamp tick is not seen; hash the file if child.json
	// ever gets a writer that could do that.
	if st := (stamp{mod: fi.ModTime(), size: fi.Size(), exists: true}); !f.loaded || !f.stamp.same(st) {
		f.cur, f.err = LoadChild(f.path)
		f.stamp, f.loaded = st, true
	}
	c := f.cur
	c.MasterAddresses, c.Tools, c.SecretPaths = slices.Clone(c.MasterAddresses), slices.Clone(c.Tools), slices.Clone(c.SecretPaths)
	return c, f.err
}
