package fleet

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The four parsers of bytes that come from outside the process. Each target
// checks more than "does not panic": whatever a parser accepts must be
// something the rest of the package can act on safely, and whatever it
// refuses must come back empty.

const (
	fuzzPrint = "abababababababababababababababababababababababababababababababab"
	fuzzLater = 4102444800 // 2100-01-01: a code in the seed corpus that does not expire
)

// FuzzParseEnrollCode: the code an operator pastes into a new machine. The
// input is tried as a whole code and as the payload of one, so the fuzzer
// reaches the fields behind the base64.
func FuzzParseEnrollCode(f *testing.F) {
	good := enrollCode{Name: "box", Addresses: []string{"10.0.0.1:7443", "[::1]:7443"}, Fingerprint: fuzzPrint, Secret: "c2VjcmV0LXNlY3JldA", Expires: fuzzLater}
	payload, _ := json.Marshal(good)
	for _, seed := range []string{
		good.encode(),
		string(payload),
		"",
		codePrefix,
		peerInvitePrefix + "eyJuIjoiaHViIn0",
		codePrefix + "!!!",
		codePrefix + strings.Repeat("A", 9000),
		`{"n":"box","a":["10.0.0.1:7443"],"f":"` + fuzzPrint + `","s":"c2VjcmV0","e":1}`,
		`{"n":"../../etc","a":["10.0.0.1:7443"],"f":"` + fuzzPrint + `","s":"c2VjcmV0","e":4102444800}`,
		`{"n":"box","a":["http://evil/"],"f":"` + fuzzPrint + `","s":"c2VjcmV0","e":4102444800}`,
		`{"n":"box","a":[],"f":"` + fuzzPrint + `","s":"c2VjcmV0","e":4102444800}`,
		`{"n":"box","a":["10.0.0.1:7443"],"f":"","s":"c2VjcmV0","e":4102444800}`,
		`{"n":"box","a":["10.0.0.1:7443"],"f":"` + strings.ToUpper(fuzzPrint) + `","s":"c2VjcmV0","e":4102444800}`,
		`{"n":"box","a":["10.0.0.1:7443"],"f":"` + fuzzPrint + `","s":"a\nb","e":4102444800}`,
		`{"n":"box","a":["10.0.0.1:7443"],"f":"` + fuzzPrint + `","s":"c2VjcmV0","e":-1}`,
		`{"n":"box","a":["10.0.0.1:7443"],"f":"` + fuzzPrint + `","s":"c2VjcmV0","e":4102444800,"x":1}`,
		`{"n":"box","a":["10.0.0.1:7443"],"f":"` + fuzzPrint + `","s":"c2VjcmV0","e":4102444800}{}`,
		`[]`, `null`, `{"e":1e400}`, `{"a":[null]}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, code := range []string{in, codePrefix + base64.RawURLEncoding.EncodeToString([]byte(in))} {
			c, err := parseEnrollCode(code)
			if err != nil {
				if !reflect.DeepEqual(c, enrollCode{}) {
					t.Fatalf("refused %q and returned %+v", code, c)
				}
				continue
			}
			if !validName(c.Name) || !validSecret(c.Secret) || c.Expires <= 0 ||
				len(c.Addresses) == 0 || len(c.Addresses) > maxAddresses || validAddresses(c.Addresses) != nil {
				t.Fatalf("accepted %+v", c)
			}
			if _, err := parseFingerprint(c.Fingerprint); err != nil {
				t.Fatalf("accepted the fingerprint %q", c.Fingerprint)
			}
			again, err := parseEnrollCode(c.encode())
			if err != nil || !reflect.DeepEqual(again, c) {
				t.Fatalf("%+v does not survive being encoded again: %+v, %v", c, again, err)
			}
		}
	})
}

// FuzzParseStore: fleet.json as found on disk.
// fuzzRecipient is the public half of a recovery identity.
const fuzzRecipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"

func FuzzParseStore(f *testing.F) {
	machine := `{"name":"box","fingerprint":"` + fuzzPrint + `","state":"confirmed","reported":{"os":"linux","user":"root","version":"1.2.3","sandbox":"host","ceiling":"full"},"enrolled_at":"2026-11-18T18:00:00Z","confirmed_at":"2026-11-18T18:05:00Z"}`
	other := strings.Replace(strings.Replace(machine, fuzzPrint, strings.Repeat("cd", 32), 1), `"box"`, `"other"`, 1)
	good := `{"version":1,"addresses":["10.0.0.1:7443"],"machines":{"m0123456789abcdef":` + machine + `,"m00000000000000ff":` + other +
		`},"enrollments":[{"name":"next","secret_sha256":"` + fuzzPrint + `","expires":4102444800,"adopt":"m0123456789abcdef"}]}`
	for _, seed := range []string{
		good,
		`{"version":1,"machines":{}}`,
		`{"version":1,"enroll_off":true,"machines":null}`,
		"", "{", "null", "[]", `{"version":2,"machines":{}}`, strings.Repeat("\x00", 64),
		`{"version":1,"machines":{"m0123456789abcdef":null}}`,
		`{"version":1,"machines":{"../../x":` + machine + `}}`,
		`{"version":1,"machines":{"m0123456789abcdef":` + machine + `,"m0123456789abcdee":` + machine + `}}`,
		`{"version":1,"machines":{"m0123456789abcdef":` + strings.Replace(machine, `"confirmed"`, `"root"`, 1) + `}}`,
		`{"version":1,"machines":{"m0123456789abcdef":` + strings.Replace(machine, `"root"`, `"\u001b[2J"`, 1) + `}}`,
		`{"version":1,"machines":{"m0123456789abcdef":` + strings.Replace(machine, `"2026-11-18T18:00:00Z"`, `"9999-12-31T23:59:59-23:59"`, 1) + `}}`,
		`{"version":1,"machines":{},"token":"x"}`,
		`{"version":1,"machines":{},"enrollments":[null]}`,
		`{"version":1,"machines":{},"enrollments":[{"name":"a","secret_sha256":"x","expires":9223372036854775807}]}`,
		`{"version":1,"addresses":["a:1","a:1","b:99999"],"machines":{}}`,
		good + good,
		// The recovery key (TEN-532): its public half and when it was proven.
		`{"version":1,"machines":{},"recovery":"` + fuzzRecipient + `","recovery_proven":"2026-11-19T10:00:00Z"}`,
		`{"version":1,"machines":{},"recovery":"` + fuzzRecipient + `"}`,
		`{"version":1,"machines":{},"recovery_proven":"2026-11-19T10:00:00Z"}`,
		`{"version":1,"machines":{},"recovery":"` + strings.ToUpper(fuzzRecipient) + `"}`,
		`{"version":1,"machines":{},"recovery":"age1","recovery_identity":"AGE-SECRET-KEY-1"}`,
		`{"version":1,"machines":{},"recovery":"` + fuzzRecipient + `","recovery":"` + fuzzRecipient + `"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		d, err := parseStore(in)
		if err != nil {
			if d != nil {
				t.Fatal("refused and returned a fleet")
			}
			return
		}
		prints, names := map[string]bool{}, map[string]bool{}
		for id, m := range d.Machines {
			if !validID(id) || m == nil || m.ID != id || !validName(m.Name) || !m.State.valid() || m.Reported.validate() != nil {
				t.Fatalf("accepted machine %q: %+v", id, m)
			}
			if _, err := parseFingerprint(m.Fingerprint); err != nil || prints[m.Fingerprint] {
				t.Fatalf("accepted machine %q with fingerprint %q", id, m.Fingerprint)
			}
			prints[m.Fingerprint] = true
			if key := strings.ToLower(m.Name); m.State != StateRevoked {
				if names[key] {
					t.Fatalf("accepted two live machines named %q", m.Name)
				}
				names[key] = true
			}
		}
		if validAddresses(d.Addresses) != nil {
			t.Fatalf("accepted addresses %q", d.Addresses)
		}
		if validRecovery(d.Recovery, d.RecoveryProven) != nil {
			t.Fatalf("accepted recovery key %q proven %v", d.Recovery, d.RecoveryProven)
		}
		// What was read can be acted on and written back: this is what
		// every change to the file does with it.
		d.reserved("box", time.Unix(1_800_000_000, 0))
		d.enrollment(fuzzPrint)
		d.named("box", "")
		d.prune(time.Unix(1_800_000_000, 0))
		first, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			t.Fatalf("an accepted fleet cannot be saved: %v", err)
		}
		again, err := parseStore(first)
		if err != nil {
			t.Fatalf("a saved fleet cannot be read: %v\n%s", err, first)
		}
		if second, err := json.MarshalIndent(again, "", "  "); err != nil || !bytes.Equal(first, second) {
			t.Fatalf("saving twice gives two files: %v\n%s\n%s", err, first, second)
		}
	})
}

// FuzzParseChild: child.json, which an operator edits by hand.
func FuzzParseChild(f *testing.F) {
	good := `{"version":1,"id":"m0123456789abcdef","name":"box","master_addresses":["10.0.0.1:7443"],"master_fingerprint":"` + fuzzPrint + `","sandbox":"host","ceiling":"no-exec","tools":["os_read_file","os_list_dir"]}`
	for _, seed := range []string{
		good,
		strings.Replace(good, `,"tools":["os_read_file","os_list_dir"]`, ``, 1),
		strings.Replace(good, `"no-exec"`, `"full"`, 1),
		strings.Replace(good, `"no-exec"`, `"read-only"`, 1),
		strings.Replace(good, `"ceiling"`, `"cieling"`, 1),
		strings.Replace(good, `"no-exec"`, `"NO-EXEC"`, 1),
		strings.Replace(good, `["os_read_file","os_list_dir"]`, `[]`, 1),
		strings.Replace(good, `["os_read_file","os_list_dir"]`, `null`, 1),
		strings.Replace(good, `["os_read_file","os_list_dir"]`, `["a","a"]`, 1),
		strings.Replace(good, fuzzPrint, ``, 1),
		strings.Replace(good, `10.0.0.1:7443`, `http://10.0.0.1`, 1),
		strings.Replace(good, `"version":1`, `"version":1,"version":2`, 1),
		strings.Replace(good, `"sandbox":"host"`, `"sandbox":"host","ceiling":"full"`, 1),
		good + good, "", "{", "null", "[]", `{"version":1}`, "\xef\xbb\xbf" + good,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		c, err := parseChild(in)
		if err != nil {
			if !reflect.DeepEqual(c, Child{}) || c.Serves("os_read_file") {
				t.Fatalf("refused and returned %+v", c)
			}
			return
		}
		if c.validate() != nil || !c.Ceiling.valid() || len(c.MasterAddresses) == 0 {
			t.Fatalf("accepted %+v", c)
		}
		if _, err := parseFingerprint(c.MasterFingerprint); err != nil {
			t.Fatalf("accepted the master fingerprint %q", c.MasterFingerprint)
		}
		// A tool list is the whole of what is served.
		if c.Tools != nil && (len(c.Tools) == 0 || c.Serves("\x00not-a-tool") || !c.Serves(c.Tools[0])) {
			t.Fatalf("accepted the tool list %q", c.Tools)
		}
		out, err := json.Marshal(childFile{Version: childVersion, Child: c})
		if err != nil {
			t.Fatal(err)
		}
		if again, err := parseChild(out); err != nil || !reflect.DeepEqual(again, c) {
			t.Fatalf("%+v does not survive a save: %+v, %v", c, again, err)
		}
	})
}

// FuzzParseEnrollRequest: the body a machine nobody knows yet posts to the
// enroll endpoint.
func FuzzParseEnrollRequest(f *testing.F) {
	for _, seed := range []string{
		`{"secret":"c2VjcmV0LXNlY3JldA","os":"linux","user":"root","version":"1.2.3","sandbox":"host","ceiling":"full"}`,
		`{"secret":"c2VjcmV0","os":"windows","ceiling":"read-only"}`,
		`{"secret":"c2VjcmV0","os":"darwin","ceiling":"no-exec","hostname":"later"}`,
		`{"secret":"c2VjcmV0","os":"linux","ceiling":"full","user":"\u001b[31m"}`,
		`{"secret":"c2VjcmV0","os":"linux","ceiling":"full","user":"\u202egnp.exe"}`,
		`{"secret":"c2VjcmV0","os":"linux","ceiling":"full","Reported":{"os":"plan9"}}`,
		`{"secret":"c2VjcmV0","os":"linux","ceiling":"full","fingerprint":"` + fuzzPrint + `","id":"m0123456789abcdef","state":"confirmed"}`,
		`{"secret":"a b","os":"linux","ceiling":"full"}`,
		`{"secret":"","os":"linux","ceiling":"full"}`,
		`{"secret":["c2VjcmV0"],"os":"linux","ceiling":"full"}`,
		`{"secret":"c2VjcmV0","os":"linux","ceiling":"full"}{"secret":"x"}`,
		`{"secret":"c2VjcmV0","os":"LINUX","ceiling":"full"}`,
		"", "{", "null", "[]", `"secret"`, strings.Repeat("[", 5000),
		`{"secret":"` + strings.Repeat("A", 4000) + `","os":"linux","ceiling":"full"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		req, err := parseEnrollRequest(in)
		if err != nil {
			if req != (enrollRequest{}) {
				t.Fatalf("refused and returned %+v", req)
			}
			return
		}
		if !validSecret(req.Secret) || len(req.Secret) > maxSecretLen || req.Reported.validate() != nil {
			t.Fatalf("accepted %+v", req)
		}
		for _, text := range []string{req.OS, req.User, req.Version, req.Sandbox, req.Ceiling} {
			if len(text) > maxReportedLen || !printable(text) {
				t.Fatalf("accepted the reported value %q", text)
			}
		}
		// What was accepted is what gets stored: it must be storable.
		d := emptyData()
		d.Machines["m0123456789abcdef"] = &Machine{Name: "box", Fingerprint: fuzzPrint, State: StatePending, Reported: req.Reported}
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if back, err := parseStore(b); err != nil || back.Machines["m0123456789abcdef"].Reported != req.Reported {
			t.Fatalf("an accepted report does not survive fleet.json: %v", err)
		}
	})
}
