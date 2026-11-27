package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/mldsa"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"tenant/internal/peering"
)

// served is a master listening on loopback: both endpoints on one port, the
// way MasterTLS serves them.
type served struct {
	addr    string
	handler *enrollHandler
	clock   atomic.Int64 // added to the master's clock, in nanoseconds

	mu      sync.Mutex
	refused []Refusal
	alerts  []Refusal
}

func (s *served) audit() (refused, alerts []Refusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.refused), slices.Clone(s.alerts)
}

func (s *served) reasons() []Reason {
	refused, _ := s.audit()
	out := make([]Reason, len(refused))
	for i, r := range refused {
		out[i] = r.Reason
	}
	return out
}

// serve starts the master's listener with cfg, or MasterTLS when cfg is nil,
// and makes its address the one enrollment codes carry. The link endpoint
// here stands in for the real one: it answers with who connected, or with
// why that machine gets no calls.
func (m *master) serve(t *testing.T, cfg *tls.Config) *served {
	t.Helper()
	s := &served{}
	m.store.now = func() time.Time { return time.Now().Add(time.Duration(s.clock.Load())) }
	s.handler = NewEnrollHandler(m.store,
		func(r Refusal) { s.mu.Lock(); s.refused = append(s.refused, r); s.mu.Unlock() },
		func(r Refusal) { s.mu.Lock(); s.alerts = append(s.alerts, r); s.mu.Unlock() },
	).(*enrollHandler)
	mux := http.NewServeMux()
	mux.Handle(EnrollPath, s.handler)
	mux.HandleFunc("/fleet/link", func(w http.ResponseWriter, r *http.Request) {
		mach, err := m.store.Identify(r.TLS)
		if err != nil {
			http.Error(w, "who are you", http.StatusForbidden)
			return
		}
		if err := m.store.Check(mach.ID); err != nil {
			fmt.Fprint(w, strings.TrimPrefix(err.Error(), "fleet: "))
			return
		}
		fmt.Fprint(w, mach.ID)
	})
	if cfg == nil {
		cfg = must(MasterTLS(m.id, nil, m.store))(t)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = cfg
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // refused handshakes are the point
	srv.StartTLS()
	t.Cleanup(srv.Close)
	s.addr = srv.Listener.Addr().String()
	if err := m.store.SetAddresses([]string{s.addr}); err != nil {
		t.Fatal(err)
	}
	return s
}

func ctxFor(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// link asks the stand-in link endpoint who the master thinks id is.
func (m *master) link(t *testing.T, s *served, id Identity) (string, error) {
	t.Helper()
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		TLSClientConfig: must(ChildTLS(id, m.id.Fingerprint))(t), DisableKeepAlives: true}}
	resp, err := client.Get("https://" + s.addr + "/fleet/link")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return string(b), err
}

// post sends a raw request to the enroll endpoint as id. body makes the
// request's body from the connection it is sent over, so that it can carry
// a proof for that connection (enrollBody); nil sends no body.
func (m *master) post(t *testing.T, s *served, id Identity, method string, body func(*tls.ConnectionState) []byte) int {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 20 * time.Second}, "tcp", s.addr, must(childTLS(id, m.id.Fingerprint, enrollServerName))(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	var b []byte
	if body != nil {
		cs := conn.ConnectionState()
		b = body(&cs)
	}
	req := must(http.NewRequest(method, "https://"+enrollServerName+EnrollPath, bytes.NewReader(b)))(t)
	req.Close = true
	// Written while the answer is read: a master that refuses a body too
	// large answers before it has read all of it.
	go func() { _ = req.Write(conn) }()
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// rawBody is a body that is the same on every connection.
func rawBody(s string) func(*tls.ConnectionState) []byte {
	return func(*tls.ConnectionState) []byte { return []byte(s) }
}

// enrollBody is the start of what Enroll posts for code over a connection,
// as the machine whose certificate is signer's: the code's public key and its
// proof for that connection and certificate, then rest, which carries on the
// JSON object and closes it.
func enrollBody(t *testing.T, code string, signer Identity, rest string) func(*tls.ConnectionState) []byte {
	return func(cs *tls.ConnectionState) []byte {
		t.Helper()
		key := must(parseEnrollCode(code))(t).key()
		msg := must(enrollMessage(cs, signer.Fingerprint))(t)
		proof := must(key.Sign(nil, msg, &mldsa.Options{Context: enrollContext}))(t)
		return fmt.Appendf(nil, `{"key":%q,"proof":%q%s`, base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()),
			base64.StdEncoding.EncodeToString(proof), rest)
	}
}

func TestEnrollConfirmAndConnect(t *testing.T) {
	m := newMaster(t)
	s := m.serve(t, nil)
	code := must(m.store.CreateEnrollment(m.id.Fingerprint, "box", 0, ""))(t)

	// The code is one line that a peer invite parser turns away, and that
	// says nothing in the clear.
	if !strings.HasPrefix(code, "tntfleet2_") || strings.ContainsAny(code, " \n") {
		t.Errorf("code = %q", code)
	}
	if _, err := peering.ParseInvite(code); err == nil {
		t.Error("a fleet enrollment code was read as a peer invite")
	}

	child := newIdentity(t)
	got, digits, err := Enroll(ctxFor(t), "  "+code+"\n", child, linuxBox)
	if err != nil {
		t.Fatal(err)
	}
	if !validID(got.ID) || got.Name != "box" || !slices.Equal(got.MasterAddresses, []string{s.addr}) ||
		got.MasterFingerprint != m.id.Fingerprint || got.Sandbox != "host" || got.Ceiling != CeilingFull {
		t.Errorf("Enroll returned %+v", got)
	}
	if len(digits) != 6 || digits != ConfirmCode(m.id.Fingerprint, child.Fingerprint) {
		t.Errorf("digits = %q", digits)
	}
	path := ChildPath(t.TempDir())
	if err := SaveChild(path, got); err != nil {
		t.Fatalf("what Enroll returned does not save as child.json: %v", err)
	}

	// On the master: pending, pinned to the certificate of that connection,
	// with what the machine said about itself.
	mach := must(m.store.Machine(got.ID))(t)
	if mach.State != StatePending || mach.Fingerprint != child.Fingerprint || mach.Reported != linuxBox || mach.Name != "box" {
		t.Errorf("machine = %+v", mach)
	}
	if answer, err := m.link(t, s, child); err != nil || answer != WaitingForConfirmation {
		t.Errorf("a pending machine was told %q, %v; want %q", answer, err, WaitingForConfirmation)
	}

	// A wrong code confirms nothing.
	other := newIdentity(t)
	for _, wrong := range []string{"", "12345", "1234567", "abcdef", digits + "0", ConfirmCode(m.id.Fingerprint, other.Fingerprint),
		ConfirmCode(child.Fingerprint, m.id.Fingerprint)} {
		if wrong == digits {
			continue // one in a million
		}
		if _, err := m.store.Confirm(m.id.Fingerprint, got.ID, wrong); !errors.Is(err, ErrWrongCode) {
			t.Errorf("Confirm with %q = %v, want ErrWrongCode", wrong, err)
		}
	}
	if _, err := m.store.Confirm(other.Fingerprint, got.ID, digits); !errors.Is(err, ErrWrongCode) {
		t.Errorf("Confirm against another master's certificate = %v", err)
	}
	if _, err := m.store.Confirm("", got.ID, digits); err == nil {
		t.Error("Confirm without the master's fingerprint worked")
	}
	if _, err := m.store.Confirm(m.id.Fingerprint, "m0000000000000000", digits); !errors.Is(err, ErrUnknown) {
		t.Errorf("Confirm of an unknown machine = %v", err)
	}
	if now := must(m.store.Machine(got.ID))(t); now.State != StatePending || !now.ConfirmedAt.IsZero() {
		t.Fatalf("wrong codes left the machine %s", now.State)
	}

	// The right one does, typed the way it is read out.
	confirmed, err := m.store.Confirm(m.id.Fingerprint, got.ID, " "+digits[:3]+" "+digits[3:]+" ")
	if err != nil || confirmed.State != StateConfirmed || confirmed.ConfirmedAt.IsZero() {
		t.Fatalf("Confirm = %+v, %v", confirmed, err)
	}
	if answer, err := m.link(t, s, child); err != nil || answer != got.ID {
		t.Errorf("a confirmed machine was told %q, %v", answer, err)
	}
	if refused, alerts := s.audit(); len(refused) != 0 || len(alerts) != 0 {
		t.Errorf("a clean enrollment left refusals %v, alerts %v", refused, alerts)
	}
}

func TestConfirmCode(t *testing.T) {
	a, b, c := strings.Repeat("0a", 48), strings.Repeat("0b", 48), strings.Repeat("0c", 48)
	ab := ConfirmCode(a, b)
	if len(ab) != 6 || strings.Trim(ab, "0123456789") != "" {
		t.Errorf("ConfirmCode = %q, want six digits", ab)
	}
	if ab != ConfirmCode(a, b) {
		t.Error("ConfirmCode is not the same twice")
	}
	if ab == ConfirmCode(a, c) && ab == ConfirmCode(c, b) && ab == ConfirmCode(b, a) {
		t.Error("ConfirmCode does not depend on the fingerprints")
	}
}

func TestEnrollExpiredCode(t *testing.T) {
	m := newMaster(t)
	s := m.serve(t, nil)
	code := must(m.store.CreateEnrollment(m.id.Fingerprint, "box", time.Minute, ""))(t)
	child := newIdentity(t)

	// The master's clock passes the code's end; the child's has not.
	s.clock.Store(int64(2 * time.Minute))
	if _, _, err := Enroll(ctxFor(t), code, child, linuxBox); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("Enroll with an expired code: %v", err)
	}
	refused, alerts := s.audit()
	if len(refused) != 1 || refused[0].Reason != RefusedExpired || refused[0].Name != "box" ||
		refused[0].Fingerprint != child.Fingerprint || refused[0].Source == "" || refused[0].Time.IsZero() {
		t.Errorf("audit = %+v", refused)
	}
	if len(alerts) != 0 {
		t.Errorf("an expired code raised an alert: %+v", alerts)
	}
	if list := must(m.store.Machines())(t); len(list) != 0 {
		t.Errorf("an expired code enrolled %+v", list)
	}

	// A code past its end on the child's own clock is not even sent.
	old := enrollCode{Name: "box", Addresses: []string{s.addr}, Fingerprint: m.id.Fingerprint, Seed: fuzzSeed, Expires: time.Now().Add(-time.Minute).Unix()}
	if _, _, err := Enroll(ctxFor(t), old.encode(), child, linuxBox); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("Enroll with a code that expired here: %v", err)
	}
	if refused, _ := s.audit(); len(refused) != 1 {
		t.Error("an expired code was sent to the master")
	}

	// Long after, the record of the code is dropped.
	s.clock.Store(int64(enrollKeep + time.Hour))
	if err := m.store.SetAddresses([]string{"10.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if d, _, _ := m.store.view(); len(d.Enrollments) != 0 {
		t.Errorf("%d enrollment records kept past their time", len(d.Enrollments))
	}
}

func TestEnrollReplayedCode(t *testing.T) {
	m := newMaster(t)
	s := m.serve(t, nil)
	code := must(m.store.CreateEnrollment(m.id.Fingerprint, "box", 0, ""))(t)
	first, thief := newIdentity(t), newIdentity(t)

	got, _, err := Enroll(ctxFor(t), code, first, linuxBox)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Enroll(ctxFor(t), code, thief, linuxBox); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a second enrollment with the same code: %v", err)
	}
	refused, alerts := s.audit()
	if len(refused) != 1 || len(alerts) != 1 {
		t.Fatalf("refused %+v, alerts %+v; want one of each", refused, alerts)
	}
	for _, r := range []Refusal{refused[0], alerts[0]} {
		if r.Reason != RefusedReplay || r.Machine != got.ID || r.Name != "box" || r.Fingerprint != thief.Fingerprint {
			t.Errorf("replay record = %+v", r)
		}
	}
	// The machine the code did enroll retrying it is refused the same way:
	// spent is spent.
	if _, _, err := Enroll(ctxFor(t), code, first, linuxBox); err == nil {
		t.Error("a spent code worked again for the same machine")
	}
	if _, alerts := s.audit(); len(alerts) != 2 || alerts[1].Fingerprint != first.Fingerprint {
		t.Errorf("alerts = %+v", alerts)
	}
	list := must(m.store.Machines())(t)
	if len(list) != 1 || list[0].Fingerprint != first.Fingerprint || list[0].ID != got.ID {
		t.Errorf("machines = %+v", list)
	}
	if _, err := m.link(t, s, thief); err == nil {
		t.Error("the certificate of the refused attempt got onto the link")
	}
}

func TestEnrollTwoAtOnceWithOneCode(t *testing.T) {
	m := newMaster(t)
	s := m.serve(t, nil)
	code := must(m.store.CreateEnrollment(m.id.Fingerprint, "box", 0, ""))(t)
	const racers = 4
	ids := make([]Identity, racers)
	for i := range ids {
		ids[i] = newIdentity(t)
	}
	ctx := ctxFor(t)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []Child
	)
	start := make(chan struct{})
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if got, _, err := Enroll(ctx, code, id, linuxBox); err == nil {
				mu.Lock()
				winners = append(winners, got)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("%d machines enrolled with one code, want exactly one", len(winners))
	}
	list := must(m.store.Machines())(t)
	if len(list) != 1 || list[0].ID != winners[0].ID {
		t.Fatalf("machines = %+v", list)
	}
	refused, alerts := s.audit()
	if len(refused) != racers-1 || len(alerts) != racers-1 {
		t.Errorf("%d refusals, %d alerts; want %d of each", len(refused), len(alerts), racers-1)
	}
	for _, r := range refused {
		if r.Reason != RefusedReplay || r.Machine != winners[0].ID {
			t.Errorf("refusal = %+v", r)
		}
	}
}

func TestEnrollRateLimit(t *testing.T) {
	m := newMaster(t)
	s := m.serve(t, nil)
	child := newIdentity(t)
	guess := func() error {
		seed, _, err := newEnrollKey()
		if err != nil {
			t.Fatal(err)
		}
		code := enrollCode{Name: "box", Addresses: []string{s.addr}, Fingerprint: m.id.Fingerprint, Seed: seed, Expires: time.Now().Add(time.Hour).Unix()}
		_, _, err = Enroll(ctxFor(t), code.encode(), child, linuxBox)
		return err
	}
	for i := range enrollAttempts {
		if err := guess(); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	good := must(m.store.CreateEnrollment(m.id.Fingerprint, "box", 0, ""))(t)
	// Over the limit: guesses and a good code alike wait the minute out.
	for i := range 3 {
		if err := guess(); err == nil || !strings.Contains(err.Error(), "429") {
			t.Fatalf("guess %d over the limit: %v", i, err)
		}
	}
	if _, _, err := Enroll(ctxFor(t), good, child, linuxBox); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("a good code over the limit: %v", err)
	}
	want := slices.Repeat([]Reason{RefusedUnknownCode}, enrollAttempts)
	want = append(want, RefusedRateLimit) // once, not once per attempt of the flood
	if got := s.reasons(); !slices.Equal(got, want) {
		t.Errorf("audit = %v, want %v", got, want)
	}
	if list := must(m.store.Machines())(t); len(list) != 0 {
		t.Fatalf("enrolled while limited: %+v", list)
	}
	// The limit did not spend the code: a minute later it enrolls.
	s.clock.Add(int64(enrollWindow))
	if _, _, err := Enroll(ctxFor(t), good, child, linuxBox); err != nil {
		t.Errorf("after the window: %v", err)
	}
}

func TestAttemptLimiter(t *testing.T) {
	l := attemptLimiter{max: 2, window: time.Minute}
	t0 := time.Unix(1_800_000_000, 0)
	for i, want := range []struct{ ok, report bool }{{true, false}, {true, false}, {false, true}, {false, false}} {
		if ok, report := l.allow("a", t0); ok != want.ok || report != want.report {
			t.Errorf("attempt %d: ok %v report %v, want %v", i, ok, report, want)
		}
	}
	if ok, _ := l.allow("b", t0); !ok {
		t.Error("one source's attempts counted against another")
	}
	if ok, _ := l.allow("a", t0.Add(time.Minute)); !ok {
		t.Error("the window did not start over")
	}

	// A full table refuses new sources, says so once, and keeps counting
	// the ones it knows; it empties as windows end.
	full := attemptLimiter{max: 2, window: time.Minute}
	for i := range maxEnrollSources {
		if ok, _ := full.allow(fmt.Sprint("source-", i), t0); !ok {
			t.Fatalf("source %d refused before the table was full", i)
		}
	}
	if ok, report := full.allow("one-more", t0); ok || !report {
		t.Errorf("a new source with the table full: ok %v report %v", ok, report)
	}
	if ok, report := full.allow("and-another", t0); ok || report {
		t.Errorf("a second new source: ok %v report %v", ok, report)
	}
	if ok, _ := full.allow("source-0", t0); !ok {
		t.Error("a known source was refused because the table is full")
	}
	if len(full.sources) != maxEnrollSources {
		t.Errorf("the table holds %d sources, want it capped at %d", len(full.sources), maxEnrollSources)
	}
	if ok, _ := full.allow("one-more", t0.Add(time.Minute)); !ok || len(full.sources) != 1 {
		t.Errorf("after the window: ok %v, %d sources", ok, len(full.sources))
	}

	for remote, want := range map[string]string{
		"192.0.2.7:5000":                "192.0.2.7",
		"[::ffff:192.0.2.7]:5000":       "192.0.2.7",
		"[2001:db8:1:2:aaaa::1]:5000":   "2001:db8:1:2::/64",
		"[2001:db8:1:2:bbbb::2]:6000":   "2001:db8:1:2::/64", // the same host's other address
		"[2001:db8:1:3::1]:5000":        "2001:db8:1:3::/64",
		"[fe80::1%eth0]:5000":           "fe80::/64",
		"not an address":                "not an address",
		"name.example:80":               "name.example",
		net.JoinHostPort("::1", "5000"): "::/64",
	} {
		if got := sourceKey(remote); got != want {
			t.Errorf("sourceKey(%q) = %q, want %q", remote, got, want)
		}
	}
}

// What the endpoint does with bodies that are not an enrollment. None of it
// spends a code.
func TestEnrollRequestRefusals(t *testing.T) {
	m := newMaster(t)
	s := m.serve(t, nil)
	s.handler.limit.max = 1000
	code := must(m.store.CreateEnrollment(m.id.Fingerprint, "box", 0, ""))(t)
	child := newIdentity(t)
	valid := func(rest string) func(*tls.ConnectionState) []byte {
		return enrollBody(t, code, child, `,"os":"linux","ceiling":"full"`+rest)
	}
	// A valid request and a second value. (Two whole requests no longer fit
	// in maxEnrollBody: a key and proof are about 10 KiB in base64.)
	twice := func(cs *tls.ConnectionState) []byte {
		return append(valid(`}`)(cs), `{"os":"linux","ceiling":"full"}`...)
	}
	// The code's key, signing for another machine's certificate.
	forOther := enrollBody(t, code, newIdentity(t), `,"os":"linux","ceiling":"full"}`)
	short := base64.StdEncoding.EncodeToString(make([]byte, 64))

	for _, tc := range []struct {
		name   string
		method string
		body   func(*tls.ConnectionState) []byte
		status int
		reason Reason
	}{
		{"a body over the limit", "POST", valid(`,"user":"` + strings.Repeat("a", maxEnrollBody) + `"}`), http.StatusRequestEntityTooLarge, RefusedTooLarge},
		{"sixteen times the limit", "POST", rawBody(strings.Repeat(" ", 16*maxEnrollBody)), http.StatusRequestEntityTooLarge, RefusedTooLarge},
		{"not JSON", "POST", rawBody("enroll me"), http.StatusBadRequest, RefusedBadRequest},
		{"an empty body", "POST", rawBody(""), http.StatusBadRequest, RefusedBadRequest},
		{"two values", "POST", twice, http.StatusBadRequest, RefusedBadRequest},
		{"no key or proof", "POST", rawBody(`{"os":"linux","ceiling":"full"}`), http.StatusBadRequest, RefusedBadRequest},
		{"a key and proof that are not one", "POST", rawBody(`{"key":"` + short + `","proof":"` + short + `","os":"linux","ceiling":"full"}`), http.StatusBadRequest, RefusedBadRequest},
		{"an unknown OS", "POST", enrollBody(t, code, child, `,"os":"templeos","ceiling":"full"}`), http.StatusBadRequest, RefusedBadRequest},
		{"an unknown ceiling", "POST", enrollBody(t, code, child, `,"os":"linux","ceiling":"root"}`), http.StatusBadRequest, RefusedBadRequest},
		{"escape codes in the user", "POST", valid(`,"user":"\u001b[31mroot"}`), http.StatusBadRequest, RefusedBadRequest},
		{"a long version", "POST", valid(`,"version":"` + strings.Repeat("1", maxReportedLen+1) + `"}`), http.StatusBadRequest, RefusedBadRequest},
		{"a proof for another certificate", "POST", forOther, http.StatusForbidden, RefusedBadProof},
		{"a GET", "GET", nil, http.StatusMethodNotAllowed, RefusedMethod},
		{"a PUT", "PUT", valid(`}`), http.StatusMethodNotAllowed, RefusedMethod},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := s.audit()
			if got := m.post(t, s, child, tc.method, tc.body); got != tc.status {
				t.Errorf("status = %d, want %d", got, tc.status)
			}
			after, alerts := s.audit()
			if len(after) != len(before)+1 || after[len(after)-1].Reason != tc.reason || after[len(after)-1].Fingerprint != child.Fingerprint {
				t.Errorf("audit = %+v, want one record with reason %s", after[len(before):], tc.reason)
			}
			if len(alerts) != 0 {
				t.Errorf("alerts = %+v", alerts)
			}
		})
	}
	// A proof read off one connection is no proof on another: the first
	// connection here sends nothing that enrolls, the second its proof.
	var lifted []byte
	if got := m.post(t, s, child, "POST", func(cs *tls.ConnectionState) []byte {
		lifted = valid(`}`)(cs)
		return []byte("{")
	}); got != http.StatusBadRequest {
		t.Errorf("status = %d", got)
	}
	if got := m.post(t, s, child, "POST", rawBody(string(lifted))); got != http.StatusForbidden {
		t.Errorf("a proof from another connection: status %d", got)
	}
	if got := s.reasons(); len(got) < 2 || got[len(got)-1] != RefusedBadProof {
		t.Errorf("audit = %v", got)
	}
	if list := must(m.store.Machines())(t); len(list) != 0 {
		t.Fatalf("a refused request enrolled %+v", list)
	}
	// A key this master does not know is skipped, and the code still works.
	if got := m.post(t, s, child, "POST", valid(`,"user":"svc","hostname":"later"}`)); got != http.StatusOK {
		t.Fatalf("a valid request with one more key: status %d", got)
	}
	mach := must(m.store.ByName("box"))(t)
	if mach.Fingerprint != child.Fingerprint || mach.Reported != (Reported{OS: "linux", User: "svc", Ceiling: "full"}) {
		t.Errorf("machine = %+v", mach)
	}
}

// The handler takes a machine's certificate from the connection. A request
// that did not come over TLS with one has nothing to pin.
func TestEnrollHandlerRefusesWithoutACertificate(t *testing.T) {
	m := newMaster(t)
	code := must(m.store.CreateEnrollment(m.id.Fingerprint, "box", 0, ""))(t)
	var refused []Refusal
	h := NewEnrollHandler(m.store, func(r Refusal) { refused = append(refused, r) }, nil)
	// The code's own key, with a signature of the right size: only the
	// missing certificate is wrong.
	key := must(parseEnrollCode(code))(t).key()
	sig := must(key.Sign(nil, []byte("x"), &mldsa.Options{Context: enrollContext}))(t)
	body := `{"key":"` + base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()) + `","proof":"` + base64.StdEncoding.EncodeToString(sig) +
		`","os":"linux","ceiling":"full","fingerprint":"` + strings.Repeat("ab", 48) + `"}`

	plain := httptest.NewRequest("POST", EnrollPath, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, plain)
	if rec.Code != http.StatusForbidden {
		t.Errorf("plain HTTP: status %d", rec.Code)
	}
	noCert := httptest.NewRequest("POST", EnrollPath, strings.NewReader(body))
	noCert.TLS = &tls.ConnectionState{}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, noCert)
	if rec.Code != http.StatusForbidden {
		t.Errorf("TLS without a client certificate: status %d", rec.Code)
	}
	if len(refused) != 2 || refused[0].Reason != RefusedNoCert || refused[1].Reason != RefusedNoCert {
		t.Errorf("audit = %+v", refused)
	}
	if list := must(m.store.Machines())(t); len(list) != 0 {
		t.Errorf("enrolled without a certificate: %+v", list)
	}
	// With no callbacks at all it still refuses.
	rec = httptest.NewRecorder()
	NewEnrollHandler(m.store, nil, nil).ServeHTTP(rec, httptest.NewRequest("POST", EnrollPath, strings.NewReader(body)))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d", rec.Code)
	}
}

func TestEnrollSwitchedOff(t *testing.T) {
	m := newMaster(t)
	_, pinned := m.add(t, "old", StateConfirmed)
	s := m.serve(t, nil)
	code := must(m.store.CreateEnrollment(m.id.Fingerprint, "box", 0, ""))(t)
	if err := m.store.SetEnrollOpen(false); err != nil {
		t.Fatal(err)
	}
	if m.store.EnrollOpen() {
		t.Fatal("EnrollOpen after switching off")
	}
	child := newIdentity(t)
	// On the one port a new machine does not get past the handshake now, so
	// the handler never runs.
	if _, _, err := Enroll(ctxFor(t), code, child, linuxBox); err == nil {
		t.Fatal("enrolled with enrollment switched off")
	}
	if refused, _ := s.audit(); len(refused) != 0 {
		t.Errorf("the handler was reached: %+v", refused)
	}
	// A machine already pinned reaches it and is told no.
	if got := m.post(t, s, pinned, "POST", enrollBody(t, code, pinned, `,"os":"linux","ceiling":"full"}`)); got != http.StatusServiceUnavailable {
		t.Errorf("status = %d", got)
	}
	if got := s.reasons(); !slices.Equal(got, []Reason{RefusedOff}) {
		t.Errorf("audit = %v", got)
	}
	// The link is not part of the switch.
	if answer, err := m.link(t, s, pinned); err != nil || !validID(answer) {
		t.Errorf("link with enrollment off: %q, %v", answer, err)
	}
	if list := must(m.store.Machines())(t); len(list) != 1 {
		t.Fatalf("machines = %+v", list)
	}

	if err := m.store.SetEnrollOpen(true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Enroll(ctxFor(t), code, child, linuxBox); err != nil {
		t.Errorf("after switching enrollment back on: %v", err)
	}
}

// Served on a port of its own, the enroll endpoint refuses by itself when
// it is switched off or the fleet cannot be read.
func TestEnrollOnItsOwnPort(t *testing.T) {
	m := newMaster(t)
	s := m.serve(t, must(MasterEnrollTLS(m.id))(t))
	s.handler.limit.max = 1000
	child := newIdentity(t)

	code := must(m.store.CreateEnrollment(m.id.Fingerprint, "one", 0, ""))(t)
	if err := m.store.SetEnrollOpen(false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Enroll(ctxFor(t), code, child, linuxBox); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("switched off: %v", err)
	}
	if err := m.store.SetEnrollOpen(true); err != nil {
		t.Fatal(err)
	}

	good := must(os.ReadFile(StorePath(m.dir)))(t)
	writeRaw(t, StorePath(m.dir), []byte("{"))
	if _, _, err := Enroll(ctxFor(t), code, child, linuxBox); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("unreadable fleet: %v", err)
	}
	writeRaw(t, StorePath(m.dir), good)
	if got := s.reasons(); !slices.Equal(got, []Reason{RefusedOff, RefusedUnavailable}) {
		t.Errorf("audit = %v", got)
	}
	if _, _, err := Enroll(ctxFor(t), code, child, linuxBox); err != nil {
		t.Errorf("neither refusal should have spent the code: %v", err)
	}
	// No link is served here: a new machine is nobody to Identify.
	if _, err := m.link(t, s, newIdentity(t)); err == nil {
		t.Error("a stranger was identified")
	}
}

func TestEnrollConflicts(t *testing.T) {
	m := newMaster(t)
	s := m.serve(t, nil)
	s.handler.limit.max = 1000
	enroll := func(code string, id Identity) (Child, error) {
		got, _, err := Enroll(ctxFor(t), code, id, linuxBox)
		return got, err
	}
	last := func() Reason {
		reasons := s.reasons()
		if len(reasons) == 0 {
			return ""
		}
		return reasons[len(reasons)-1]
	}

	// One certificate, one machine.
	child := newIdentity(t)
	first := must(enroll(must(m.store.CreateEnrollment(m.id.Fingerprint, "one", 0, ""))(t), child))(t)
	second := must(m.store.CreateEnrollment(m.id.Fingerprint, "two", 0, ""))(t)
	if _, err := enroll(second, child); err == nil || last() != RefusedConflict {
		t.Errorf("one certificate enrolled twice: %v, audit %s", err, last())
	}
	// That refusal did not spend the code.
	if _, err := enroll(second, newIdentity(t)); err != nil {
		t.Errorf("the code a conflict refused: %v", err)
	}

	// A revoked machine's name is free, and a machine enrolled under it
	// starts clean: another id.
	if err := m.store.Revoke(first.ID); err != nil {
		t.Fatal(err)
	}
	again := must(enroll(must(m.store.CreateEnrollment(m.id.Fingerprint, "one", 0, ""))(t), newIdentity(t)))(t)
	if again.ID == first.ID {
		t.Error("a new machine under an old name got the old id without being asked to")
	}

	// Adopting: the new machine takes the revoked machine's id.
	if _, err := m.store.CreateEnrollment(m.id.Fingerprint, "heir", 0, again.ID); err == nil {
		t.Error("made a code that adopts the id of a machine that is not revoked")
	}
	if _, err := m.store.CreateEnrollment(m.id.Fingerprint, "heir", 0, "m0000000000000000"); !errors.Is(err, ErrUnknown) {
		t.Errorf("adopting an id nobody has: %v", err)
	}
	if _, err := m.store.CreateEnrollment(m.id.Fingerprint, "heir", 0, "../x"); !errors.Is(err, ErrUnknown) {
		t.Errorf("adopting something that is not an id: %v", err)
	}
	heirID := newIdentity(t)
	heir := must(enroll(must(m.store.CreateEnrollment(m.id.Fingerprint, "heir", 0, first.ID))(t), heirID))(t)
	mach := must(m.store.Machine(first.ID))(t)
	if heir.ID != first.ID || mach.Name != "heir" || mach.State != StatePending || mach.Fingerprint != heirID.Fingerprint || !mach.RevokedAt.IsZero() {
		t.Errorf("adopted machine = %+v (child got id %s, want %s)", mach, heir.ID, first.ID)
	}
	// The old certificate of that id is gone with the old record.
	if _, err := m.link(t, s, child); err == nil {
		t.Error("the revoked machine's certificate works again after its id was adopted")
	}

	// A code whose id to adopt is no longer a revoked machine enrolls nothing.
	if err := m.store.Revoke(again.ID); err != nil {
		t.Fatal(err)
	}
	stale := must(m.store.CreateEnrollment(m.id.Fingerprint, "late", 0, again.ID))(t)
	if err := m.store.Remove(again.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := enroll(stale, newIdentity(t)); err == nil || last() != RefusedConflict {
		t.Errorf("adopted an id that was removed: %v, audit %s", err, last())
	}
	if _, alerts := s.audit(); len(alerts) != 0 {
		t.Errorf("alerts = %+v", alerts)
	}

	// A name that got taken behind a code's back (nothing in this package
	// lets that happen; a file put there by hand could) enrolls nothing.
	taken := must(m.store.CreateEnrollment(m.id.Fingerprint, "taken", 0, ""))(t)
	var d fileData
	if err := json.Unmarshal(must(os.ReadFile(StorePath(m.dir)))(t), &d); err != nil {
		t.Fatal(err)
	}
	d.Machines["m00000000000000aa"] = &Machine{Name: "TAKEN", Fingerprint: strings.Repeat("ee", 48), State: StatePaused,
		Reported: linuxBox, EnrolledAt: time.Now().UTC()}
	writeRaw(t, StorePath(m.dir), must(json.Marshal(d))(t))
	if _, err := enroll(taken, newIdentity(t)); err == nil || last() != RefusedConflict {
		t.Errorf("enrolled under a name a machine has: %v, audit %s", err, last())
	}
}

// A master that takes the connection and then says nothing does not hold
// the child for longer than the child is willing to wait.
func TestEnrollGivesUpOnASilentMaster(t *testing.T) {
	m := newMaster(t)
	ln := must(tls.Listen("tcp", "127.0.0.1:0", must(MasterEnrollTLS(m.id))(t)))(t)
	defer ln.Close()
	hung := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, c) // reads the request, never answers
		hung <- c
	}()
	code := enrollCode{Name: "box", Addresses: []string{ln.Addr().String()}, Fingerprint: m.id.Fingerprint, Seed: fuzzSeed, Expires: time.Now().Add(time.Hour).Unix()}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := Enroll(ctx, code.encode(), newIdentity(t), linuxBox); err == nil {
		t.Fatal("enrolled with a master that never answered")
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Errorf("Enroll held on for %s after its context ended", waited)
	}
	select {
	case c := <-hung:
		c.Close()
	case <-time.After(10 * time.Second):
		t.Error("the child did not close its connection")
	}
}

func TestCreateEnrollment(t *testing.T) {
	m := newMaster(t)
	s := m.store
	fp := m.id.Fingerprint
	for name, call := range map[string]func() (string, error){
		"a bad name":           func() (string, error) { return s.CreateEnrollment(fp, "two words", 0, "") },
		"no name":              func() (string, error) { return s.CreateEnrollment(fp, "", 0, "") },
		"no fingerprint":       func() (string, error) { return s.CreateEnrollment("", "box", 0, "") },
		"a bad fingerprint":    func() (string, error) { return s.CreateEnrollment(strings.ToUpper(fp), "box", 0, "") },
		"a negative lifetime":  func() (string, error) { return s.CreateEnrollment(fp, "box", -time.Minute, "") },
		"a lifetime of a week": func() (string, error) { return s.CreateEnrollment(fp, "box", 7*24*time.Hour, "") },
	} {
		if code, err := call(); err == nil || code != "" {
			t.Errorf("%s: a code was made", name)
		}
	}
	if d, _, _ := s.view(); len(d.Enrollments) != 0 {
		t.Fatalf("refused calls left %d records", len(d.Enrollments))
	}

	before := time.Now()
	code := must(s.CreateEnrollment(fp, "box", 0, ""))(t)
	ec := must(parseEnrollCode(code))(t)
	if ec.Name != "box" || ec.Fingerprint != fp || !slices.Equal(ec.Addresses, []string{"127.0.0.1:1"}) || ec.key() == nil {
		t.Errorf("code carries %+v", ec)
	}
	if end := time.Unix(ec.Expires, 0); end.Before(before.Add(DefaultEnrollTTL-time.Second)) || end.After(time.Now().Add(DefaultEnrollTTL+time.Second)) {
		t.Errorf("the code ends %s, want %s from now", end, DefaultEnrollTTL)
	}

	// A new code for the name replaces the one not spent: the lost code is dead.
	newer := must(s.CreateEnrollment(fp, "BOX", time.Hour, ""))(t)
	child := newIdentity(t)
	var why *Refusal
	if _, err := s.enroll(keyHashOf(t, code), child.Fingerprint, linuxBox); !errors.As(err, &why) || why.Reason != RefusedUnknownCode ||
		!strings.Contains(err.Error(), string(RefusedUnknownCode)) {
		t.Errorf("the replaced code: %v", err)
	}
	if got, err := s.enroll(keyHashOf(t, newer), child.Fingerprint, linuxBox); err != nil || got.Name != "BOX" {
		t.Errorf("the newer code: %+v, %v", got, err)
	}
	if _, err := s.CreateEnrollment(fp, "box", 0, ""); err == nil {
		t.Error("made a code for a name a machine has")
	}

	// No address, no code: a child could not dial anything.
	bare := Open(t.TempDir())
	if _, err := bare.CreateEnrollment(fp, "box", 0, ""); err == nil {
		t.Error("made a code with no master address in it")
	}
}

func TestParseEnrollCode(t *testing.T) {
	fp := strings.Repeat("ab", 48)
	later := time.Now().Add(time.Hour).Unix()
	good := enrollCode{Name: "box", Addresses: []string{"10.0.0.1:7443", "[::1]:7443"}, Fingerprint: fp, Seed: fuzzSeed, Expires: later}
	if got, err := parseEnrollCode("\t" + good.encode() + "\r\n"); err != nil || got.Name != "box" || len(got.Addresses) != 2 {
		t.Fatalf("the good code: %+v, %v", got, err)
	}
	// A real peer invite is named for what it is.
	invite := must(peering.Invite{Name: "hub", URL: "https://hub:1", Secret: "s", InstanceID: "i", Expiry: later}.Encode())(t)
	if _, err := parseEnrollCode(invite); err == nil || !strings.Contains(err.Error(), "peer invite") {
		t.Errorf("a peer invite: %v", err)
	}
	with := func(change func(*enrollCode)) string {
		c := good
		c.Addresses = slices.Clone(good.Addresses)
		change(&c)
		return c.encode()
	}
	raw := func(payload string) string { return codePrefix + base64.RawURLEncoding.EncodeToString([]byte(payload)) }
	for name, bad := range map[string]string{
		"empty":               "",
		"no prefix":           strings.TrimPrefix(good.encode(), codePrefix),
		"prefix only":         codePrefix,
		"not base64":          codePrefix + "!!!!",
		"padded base64":       good.encode() + "==",
		"not JSON":            raw("hello"),
		"an array":            raw("[]"),
		"an unknown key":      raw(`{"n":"box","a":["10.0.0.1:1"],"f":"` + fp + `","s":"` + fuzzSeed + `","e":` + fmt.Sprint(later) + `,"u":"http://x"}`),
		"two values":          raw(`{}{}`),
		"no name":             with(func(c *enrollCode) { c.Name = "" }),
		"a bad name":          with(func(c *enrollCode) { c.Name = "../box" }),
		"no addresses":        with(func(c *enrollCode) { c.Addresses = nil }),
		"a URL for address":   with(func(c *enrollCode) { c.Addresses = []string{"https://10.0.0.1:7443"} }),
		"too many addresses":  with(func(c *enrollCode) { c.Addresses = slices.Repeat([]string{"10.0.0.1:1"}, maxAddresses+1) }),
		"no fingerprint":      with(func(c *enrollCode) { c.Fingerprint = "" }),
		"a short fingerprint": with(func(c *enrollCode) { c.Fingerprint = fp[:40] }),
		"a legacy pin":        with(func(c *enrollCode) { c.Fingerprint = fp[:64] }),
		"no seed":             with(func(c *enrollCode) { c.Seed = "" }),
		"a strange seed":      with(func(c *enrollCode) { c.Seed = "a\nb" }),
		"a short seed":        with(func(c *enrollCode) { c.Seed = fuzzSeed[:22] }),
		"a long seed":         with(func(c *enrollCode) { c.Seed = fuzzSeed + "AAAA" }),
		"no expiry":           with(func(c *enrollCode) { c.Expires = 0 }),
		"expired":             with(func(c *enrollCode) { c.Expires = time.Now().Add(-time.Second).Unix() }),
		"too long":            codePrefix + strings.Repeat("A", maxCodeLen),
	} {
		if got, err := parseEnrollCode(bad); err == nil {
			t.Errorf("%s: accepted as %+v", name, got)
		} else if got.Seed != "" || got.Fingerprint != "" {
			t.Errorf("%s: refused, but returned %+v", name, got)
		}
	}
	// A code from a master before ML-DSA-87 is named for what it is.
	legacy := legacyCodePrefix + strings.TrimPrefix(good.encode(), codePrefix)
	if got, err := parseEnrollCode(legacy); err == nil || !strings.Contains(err.Error(), "ML-DSA-87") || got.Seed != "" {
		t.Errorf("a tntfleet1_ code: %+v, %v", got, err)
	}
}

// The child's side of an enrollment that cannot go through.
func TestEnrollDialing(t *testing.T) {
	m := newMaster(t)
	s := m.serve(t, nil)
	child := newIdentity(t)

	// A dead address first: the next one is tried.
	dead := must(net.Listen("tcp", "127.0.0.1:0"))(t)
	deadAddr := dead.Addr().String()
	dead.Close()
	if err := m.store.SetAddresses([]string{deadAddr, s.addr}); err != nil {
		t.Fatal(err)
	}
	code := must(m.store.CreateEnrollment(m.id.Fingerprint, "box", 0, ""))(t)
	if got, _, err := Enroll(ctxFor(t), code, child, linuxBox); err != nil || !slices.Equal(got.MasterAddresses, []string{deadAddr, s.addr}) {
		t.Fatalf("Enroll past a dead address: %+v, %v", got, err)
	}

	// A server that is not the master named in the code: nothing is sent.
	impostor := newIdentity(t)
	forged := enrollCode{Name: "trap", Addresses: []string{s.addr}, Fingerprint: impostor.Fingerprint, Seed: fuzzSeed, Expires: time.Now().Add(time.Hour).Unix()}
	before, _ := s.audit()
	if _, _, err := Enroll(ctxFor(t), forged.encode(), newIdentity(t), linuxBox); err == nil || !strings.Contains(err.Error(), "could not reach the master") {
		t.Errorf("a server with another certificate: %v", err)
	}
	if after, _ := s.audit(); len(after) != len(before) {
		t.Error("the proof was sent to a server that is not the master")
	}

	// What this machine says about itself is checked before anything is dialed.
	next := must(m.store.CreateEnrollment(m.id.Fingerprint, "next", 0, ""))(t)
	for name, self := range map[string]Reported{
		"no sandbox":  {OS: "linux", Ceiling: "full"},
		"bad ceiling": {OS: "linux", Ceiling: "all", Sandbox: "host"},
		"bad OS":      {OS: "", Ceiling: "full", Sandbox: "host"},
	} {
		if _, _, err := Enroll(ctxFor(t), next, newIdentity(t), self); err == nil {
			t.Errorf("%s: enrolled", name)
		}
	}
	if _, _, err := Enroll(ctxFor(t), next, Identity{}, linuxBox); err == nil {
		t.Error("enrolled without an identity")
	}
	if _, _, err := Enroll(ctxFor(t), "tnt1_abc", newIdentity(t), linuxBox); err == nil {
		t.Error("enrolled with a peer invite")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Enroll(cancelled, next, newIdentity(t), linuxBox); err == nil {
		t.Error("enrolled on a cancelled context")
	}
	if after, _ := s.audit(); len(after) != len(before) {
		t.Errorf("a refused local call reached the master: %+v", after[len(before):])
	}
	// None of that spent the code.
	if _, _, err := Enroll(ctxFor(t), next, newIdentity(t), linuxBox); err != nil {
		t.Errorf("the untouched code: %v", err)
	}
}

// A master that answers nonsense is not believed.
func TestEnrollDistrustsTheAnswer(t *testing.T) {
	m := newMaster(t)
	for name, answer := range map[string]string{
		"not JSON":     "welcome",
		"a path as id": `{"id":"../../etc","name":"box"}`,
		"a bad name":   `{"id":"m0123456789abcdef","name":"two words"}`,
		"nothing":      `{}`,
	} {
		mux := http.NewServeMux()
		mux.HandleFunc(EnrollPath, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, answer) })
		srv := httptest.NewUnstartedServer(mux)
		srv.TLS = must(MasterEnrollTLS(m.id))(t)
		srv.StartTLS()
		code := enrollCode{Name: "box", Addresses: []string{srv.Listener.Addr().String()}, Fingerprint: m.id.Fingerprint, Seed: fuzzSeed, Expires: time.Now().Add(time.Hour).Unix()}
		if got, digits, err := Enroll(ctxFor(t), code.encode(), newIdentity(t), linuxBox); err == nil || got.ID != "" || digits != "" {
			t.Errorf("%s: Enroll = %+v, %q, %v", name, got, digits, err)
		}
		srv.Close()
	}
	if got := clip("  refused\x1b[2J\nnow \xff " + strings.Repeat("é", 300)); len(got) > 200 || !utf8.ValidString(got) || strings.ContainsAny(got, "\x1b\n") || !strings.HasPrefix(got, "refused") {
		t.Errorf("clip = %q", got)
	}
}

// TEN-553: a master may confirm a machine as it enrolls, on the strength of
// its code. The hook is told of the pending machine and where it came from
// before the machine is answered; when it says yes the machine is told, and
// has no digits to show. When it says no, the digits are there as before.
func TestEnrollConfirmedByItsCode(t *testing.T) {
	m := newMaster(t)
	s := m.serve(t, nil)
	var seen []string
	s.handler.enrolled = func(mach Machine, remote string) bool {
		seen = append(seen, mach.Name+" "+string(mach.State)+" "+strconv.FormatBool(remote != ""))
		if mach.Name != "yes" {
			return false
		}
		_, err := m.store.Confirm(m.id.Fingerprint, mach.ID, ConfirmCode(m.id.Fingerprint, mach.Fingerprint))
		return err == nil
	}
	for name, wantDigits := range map[string]bool{"yes": false, "no": true} {
		code := must(m.store.CreateEnrollment(m.id.Fingerprint, name, 0, ""))(t)
		child := newIdentity(t)
		got, digits, err := Enroll(ctxFor(t), code, child, linuxBox)
		if err != nil {
			t.Fatal(err)
		}
		if (digits != "") != wantDigits {
			t.Errorf("%s: digits = %q", name, digits)
		}
		want := StateConfirmed
		if wantDigits {
			want = StatePending
		}
		if mach := must(m.store.Machine(got.ID))(t); mach.State != want {
			t.Errorf("%s: the machine is %s, want %s", name, mach.State, want)
		}
	}
	slices.Sort(seen)
	if !slices.Equal(seen, []string{"no pending true", "yes pending true"}) {
		t.Errorf("the hook was told %q", seen)
	}
}
