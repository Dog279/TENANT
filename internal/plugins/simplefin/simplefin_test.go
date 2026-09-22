package simplefin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
)

func TestParseCents(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1", 100, false},
		{"1.5", 150, false},
		{"1.50", 150, false},
		{"-4.50", -450, false},
		{"+2.05", 205, false},
		{"0", 0, false},
		{"-0.01", -1, false},
		{"1234.56", 123456, false},
		{".99", 99, false},
		{"5.", 500, false},
		{"1.500", 150, false}, // padded zeros OK
		{"1.505", 0, true},    // sub-cent precision
		{"", 0, true},
		{"abc", 0, true},
		{"1.2.3", 0, true},
	}
	for _, c := range cases {
		got, err := ParseCents(c.in)
		if c.wantErr != (err != nil) {
			t.Errorf("ParseCents(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("ParseCents(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFormatCents(t *testing.T) {
	if got := FormatCents(-450, "USD"); got != "-$4.50" {
		t.Errorf("FormatCents = %q", got)
	}
	if got := FormatCents(123456, ""); got != "$1234.56" {
		t.Errorf("FormatCents = %q", got)
	}
	if got := FormatCents(-5, "EUR"); got != "-0.05 EUR" {
		t.Errorf("FormatCents = %q", got)
	}
}

func TestLooksLikeSetupToken(t *testing.T) {
	tok := base64.StdEncoding.EncodeToString([]byte("https://bridge.example/claim/DEMO"))
	if !LooksLikeSetupToken(tok) {
		t.Error("valid setup token not recognized")
	}
	if LooksLikeSetupToken("https://user:pass@bridge.example/simplefin") {
		t.Error("access URL misidentified as setup token")
	}
	if LooksLikeSetupToken("not base64 at all!!") {
		t.Error("garbage misidentified as setup token")
	}
}

// fakeBridge is an httptest SimpleFIN bridge: one claimable token, one
// /accounts payload, records the last basic-auth pair it saw.
func fakeBridge(t *testing.T, accountsJSON func() string) (*httptest.Server, *string, *string) {
	t.Helper()
	var lastUser, lastPass string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/claim/DEMO":
			fmt.Fprintf(w, "http://user1:pass1@%s/simplefin", srv.Listener.Addr().String())
		case r.Method == http.MethodGet && r.URL.Path == "/simplefin/accounts":
			u, p, _ := r.BasicAuth()
			lastUser, lastPass = u, p
			if r.URL.Query().Get("start-date") == "" || r.URL.Query().Get("end-date") == "" {
				http.Error(w, "missing date range", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, accountsJSON())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &lastUser, &lastPass
}

const fixtureAccounts = `{
  "errors": [],
  "accounts": [
    {
      "org": {"name": "First Bank", "domain": "first.example"},
      "id": "acct-1", "name": "Checking", "currency": "USD",
      "balance": "1500.25", "available-balance": "1450.25", "balance-date": 1755000000,
      "transactions": [
        {"id": "t1", "posted": 1754900000, "amount": "-4.50", "description": "COFFEE SHOP", "payee": "Blue Bottle"},
        {"id": "t2", "posted": 1754800000, "amount": "2000.00", "description": "PAYROLL ACME"},
        {"id": "t3", "posted": 1754950000, "amount": "-60.00", "description": "GAS STATION", "pending": true}
      ]
    },
    {
      "org": {"name": "Broker Co"},
      "id": "acct-2", "name": "Brokerage", "currency": "USD",
      "balance": "10000.00", "balance-date": 1755000000,
      "transactions": []
    }
  ]
}`

func TestClaimOpenAccounts(t *testing.T) {
	srv, user, pass := fakeBridge(t, func() string { return fixtureAccounts })

	tok := base64.StdEncoding.EncodeToString([]byte(srv.URL + "/claim/DEMO"))
	access, err := Claim(context.Background(), tok, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !strings.Contains(access, "user1:pass1@") {
		t.Fatalf("access URL missing embedded creds: %q", access)
	}

	svc, err := Open(Config{AccessURL: access})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	set, err := svc.Accounts(context.Background(), time.Unix(1754000000, 0), time.Unix(1755100000, 0))
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if *user != "user1" || *pass != "pass1" {
		t.Errorf("basic auth = %q/%q, want user1/pass1", *user, *pass)
	}
	if len(set.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(set.Accounts))
	}
	a := set.Accounts[0]
	if a.Org.Name != "First Bank" || a.BalanceDate != 1755000000 || a.AvailableBalance != "1450.25" {
		t.Errorf("hyphenated fields mis-decoded: %+v", a)
	}
	if len(a.Transactions) != 3 || !a.Transactions[2].Pending {
		t.Errorf("transactions mis-decoded: %+v", a.Transactions)
	}
}

func TestOpenRejectsBadURLs(t *testing.T) {
	if _, err := Open(Config{AccessURL: "http://user:p@bridge.example/simplefin"}); err == nil {
		t.Error("plain http to a real host must be rejected")
	}
	if _, err := Open(Config{AccessURL: "https://bridge.example/simplefin"}); err == nil {
		t.Error("URL without embedded credentials must be rejected")
	}
}

func decodeFixture(t *testing.T, s string) *AccountSet {
	t.Helper()
	var set AccountSet
	if err := json.Unmarshal([]byte(s), &set); err != nil {
		t.Fatal(err)
	}
	return &set
}

func TestLedgerApplyDedupeReplay(t *testing.T) {
	dir := t.TempDir()
	led, err := OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	set := decodeFixture(t, fixtureAccounts)
	now := time.Unix(1755010000, 0)

	res, err := led.Apply(set, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.NewTxns != 3 || res.UpdatedTxns != 0 {
		t.Fatalf("first apply: %+v", res)
	}

	// Same snapshot again → nothing new.
	res, err = led.Apply(set, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.NewTxns != 0 || res.UpdatedTxns != 0 {
		t.Fatalf("re-apply must be a no-op: %+v", res)
	}

	// Pending gas txn posts (flips pending, same id) → one update.
	set2 := decodeFixture(t, fixtureAccounts)
	set2.Accounts[0].Transactions[2].Pending = false
	res, err = led.Apply(set2, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.NewTxns != 0 || res.UpdatedTxns != 1 {
		t.Fatalf("pending flip: %+v", res)
	}

	// Replay from disk → identical state.
	led2, err := OpenLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if led2.LastSync() != now.Add(2*time.Hour).Unix() {
		t.Errorf("LastSync after replay = %d", led2.LastSync())
	}
	bals := led2.Balances()
	byID := map[string]AccountState{}
	for _, b := range bals {
		byID[b.ID] = b
	}
	if len(bals) != 2 || byID["acct-1"].BalanceCents != 150025 || byID["acct-1"].AvailableCents != 145025 {
		t.Fatalf("balances after replay: %+v", bals)
	}
	txns := led2.Transactions(0, 1<<62, "", 0)
	if len(txns) != 3 {
		t.Fatalf("txns after replay = %d, want 3", len(txns))
	}
	for _, tx := range txns {
		if tx.ID == "t3" && tx.Pending {
			t.Error("pending flip lost on replay (last record must win)")
		}
	}

	// Query filter.
	if got := led2.Transactions(0, 1<<62, "coffee", 0); len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("query filter: %+v", got)
	}

	// Summary: in 2000.00, out 4.50+60.00 (gas now posted).
	sum := led2.Summary(0, 1<<62)
	if sum.InCents != 200000 || sum.OutCents != 6450 {
		t.Fatalf("summary in/out = %d/%d", sum.InCents, sum.OutCents)
	}
}

func TestDispatcherEndToEnd(t *testing.T) {
	srv, _, _ := fakeBridge(t, func() string { return fixtureAccounts })
	access := fmt.Sprintf("http://user1:pass1@%s/simplefin", strings.TrimPrefix(srv.URL, "http://"))
	clock := func() time.Time { return time.Unix(1755010000, 0) }
	svc, err := Open(Config{AccessURL: access, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	led, err := OpenLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(svc, led)

	call := func(name, args string) string {
		t.Helper()
		out, isErr, err := d.Dispatch(context.Background(), model.ToolCall{Name: name, Arguments: json.RawMessage(args)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if isErr {
			t.Fatalf("%s reported error: %s", name, out)
		}
		return out
	}

	// First read auto-syncs (ledger empty → stale).
	out := call("money_balances", `{}`)
	if !strings.Contains(out, "First Bank · Checking: $1500.25") || !strings.Contains(out, "net total: $11500.25") {
		t.Errorf("balances output:\n%s", out)
	}

	out = call("money_transactions", `{"days": 60, "query": "coffee"}`)
	if !strings.Contains(out, "COFFEE SHOP") || !strings.Contains(out, "-$4.50") || strings.Contains(out, "PAYROLL") {
		t.Errorf("transactions output:\n%s", out)
	}

	out = call("money_spend", `{"days": 60}`)
	if !strings.Contains(out, "in $2000.00") || !strings.Contains(out, "out $4.50") {
		t.Errorf("spend output (pending gas must be excluded):\n%s", out)
	}

	out = call("money_sync", `{}`)
	if !strings.Contains(out, "synced") {
		t.Errorf("sync output:\n%s", out)
	}
}

func TestDispatcherUnconfigured(t *testing.T) {
	d := NewDispatcher(nil, nil)
	if got := len(d.Tools()); got != 4 {
		t.Fatalf("Tools() on nil service = %d specs", got)
	}
	out, isErr, err := d.Dispatch(context.Background(), model.ToolCall{Name: "money_balances"})
	if err != nil || !isErr || !strings.Contains(out, "not configured") {
		t.Fatalf("unconfigured dispatch = %q, %v, %v", out, isErr, err)
	}
}
