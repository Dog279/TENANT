package fleet

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for a second Tenant process that
// changes the same fleet.json (see multiproc_test.go), or for the machines of
// a fleet (see link_scale_test.go).
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		run := helperProcess
		if mode == helperChildren {
			run = childrenProcess
		}
		if err := run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var linuxBox = Reported{OS: "linux", User: "root", Version: "1.2.3", Sandbox: "host", Ceiling: "full"}

// master is a master's config dir: its identity and its fleet.json.
type master struct {
	dir   string
	id    Identity
	store *Store
}

func newMaster(t *testing.T) *master {
	t.Helper()
	dir := t.TempDir()
	id, err := MintIdentity(MasterIdentityPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	m := &master{dir: dir, id: id, store: Open(dir)}
	if err := m.store.SetAddresses([]string{"127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	return m
}

func newIdentity(t *testing.T) Identity {
	t.Helper()
	id, err := MintIdentity(ChildIdentityPath(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// secretOf digs the secret out of an enrollment code.
func secretOf(t *testing.T, code string) string {
	t.Helper()
	ec, err := parseEnrollCode(code)
	if err != nil {
		t.Fatal(err)
	}
	return ec.Secret
}

// add enrolls a machine without the network and brings it to state.
func (m *master) add(t *testing.T, name string, state State) (Machine, Identity) {
	t.Helper()
	child := newIdentity(t)
	return m.addIdentity(t, name, state, child), child
}

func (m *master) addIdentity(t *testing.T, name string, state State, child Identity) Machine {
	t.Helper()
	code, err := m.store.CreateEnrollment(m.id.Fingerprint, name, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	mach, err := m.store.enroll(secretOf(t, code), child.Fingerprint, linuxBox)
	if err != nil {
		t.Fatal(err)
	}
	if state == StatePending {
		return mach
	}
	if _, err := m.store.Confirm(m.id.Fingerprint, mach.ID, ConfirmCode(m.id.Fingerprint, child.Fingerprint)); err != nil {
		t.Fatal(err)
	}
	switch state {
	case StatePaused:
		err = m.store.Pause(mach.ID)
	case StateRevoked:
		err = m.store.Revoke(mach.ID)
	}
	if err != nil {
		t.Fatal(err)
	}
	mach, err = m.store.Machine(mach.ID)
	if err != nil {
		t.Fatal(err)
	}
	return mach
}

// must wraps a call that returns a value and an error: must(f())(t) is the
// value, and a failed test if there was an error.
func must[T any](v T, err error) func(testing.TB) T {
	return func(t testing.TB) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// handshake makes one real TLS connection over loopback between a server
// using server and a client using client. It returns the server's view of
// the connection when its handshake succeeded, the server's handshake error,
// and what the client got: nil only if it read the server's greeting, which
// the server sends after its own handshake. (In TLS 1.3 a client finishes its
// handshake before the server has judged its certificate, so only the read
// tells.)
func handshake(t *testing.T, server, client *tls.Config) (state *tls.ConnectionState, serverErr, clientErr error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type result struct {
		state *tls.ConnectionState
		err   error
	}
	done := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- result{err: err}
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		sc := tls.Server(c, server)
		if err := sc.Handshake(); err != nil {
			done <- result{err: err}
			return
		}
		cs := sc.ConnectionState()
		_, _ = sc.Write([]byte("ok"))
		done <- result{state: &cs}
	}()
	cc, clientErr := tls.DialWithDialer(&net.Dialer{Timeout: 20 * time.Second}, "tcp", ln.Addr().String(), client)
	if clientErr == nil {
		_ = cc.SetDeadline(time.Now().Add(20 * time.Second))
		_, clientErr = io.ReadFull(cc, make([]byte, 2))
		_ = cc.Close()
	}
	r := <-done
	return r.state, r.err, clientErr
}

// fleetFiles makes a master's and a child's files the way the package does,
// each saved more than once, and returns the ones that hold a key or decide
// who is trusted. The master's directory is made by the package; the child's
// is there already and open to everyone, as a directory an installer made
// might be.
func fleetFiles(t *testing.T) []string {
	t.Helper()
	masterDir := filepath.Join(t.TempDir(), "master")
	childDir := filepath.Join(t.TempDir(), "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(childDir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := must(MintIdentity(MasterIdentityPath(masterDir)))(t)
	s := Open(masterDir)
	if err := s.SetAddresses([]string{"127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	childID := must(MintIdentity(ChildIdentityPath(childDir)))(t)
	code := must(s.CreateEnrollment(id.Fingerprint, "box", 0, ""))(t)
	mach := must(s.enroll(secretOf(t, code), childID.Fingerprint, linuxBox))(t)
	must(s.Confirm(id.Fingerprint, mach.ID, ConfirmCode(id.Fingerprint, childID.Fingerprint)))(t)

	c := Child{ID: mach.ID, Name: mach.Name, MasterAddresses: []string{"127.0.0.1:1"}, MasterFingerprint: id.Fingerprint,
		Sandbox: "host", Ceiling: CeilingFull}
	if err := SaveChild(ChildPath(childDir), c); err != nil {
		t.Fatal(err)
	}
	if err := UpdateChild(ChildPath(childDir), func(c *Child) error { c.Ceiling = CeilingReadOnly; return nil }); err != nil {
		t.Fatal(err)
	}
	return []string{StorePath(masterDir), MasterIdentityPath(masterDir), ChildPath(childDir), ChildIdentityPath(childDir)}
}

// underAFile is a path nothing can be created at: its parent is a file.
func underAFile(t *testing.T, name string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(file, name)
}
