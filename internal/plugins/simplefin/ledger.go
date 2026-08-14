package simplefin

// ledger.go is the log-only local store: every fact learned from the
// bridge is one appended JSONL line in <dir>/ledger.jsonl — accounts,
// balance points, transactions, sync marks. Replay rebuilds the whole
// in-memory state; for an update the LAST record for a key wins (a
// pending transaction that posts is simply appended again). Delete the
// file and the next sync rebuilds it from the bridge's window. cat-able
// by design, and the exact shape wealthd's brain expects to ingest.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// record is one ledger line, discriminated by T:
//
//	"account" — identity/rename of an account
//	"balance" — a balance observation (asof from the bridge)
//	"txn"     — a transaction (re-appended on change, e.g. pending→posted)
//	"sync"    — a completed bridge sync (drives staleness)
type record struct {
	T string `json:"t"`

	AccountID string `json:"account_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Org       string `json:"org,omitempty"`
	Currency  string `json:"currency,omitempty"`

	BalanceCents   *int64 `json:"balance_cents,omitempty"`
	AvailableCents *int64 `json:"available_cents,omitempty"`
	AsOfUnix       int64  `json:"asof_unix,omitempty"`
	AsOfDay        string `json:"asof,omitempty"` // LA date

	TxnID       string `json:"txn_id,omitempty"`
	PostedUnix  int64  `json:"posted_unix,omitempty"`
	PostedDay   string `json:"posted,omitempty"` // LA date
	AmountCents int64  `json:"amount_cents,omitempty"`
	AmountRaw   string `json:"amount_raw,omitempty"`
	Description string `json:"description,omitempty"`
	Payee       string `json:"payee,omitempty"`
	Memo        string `json:"memo,omitempty"`
	Pending     bool   `json:"pending,omitempty"`

	SyncedAt int64 `json:"synced_at,omitempty"`
}

// AccountState is the replayed latest view of one account.
type AccountState struct {
	ID             string
	Name           string
	Org            string
	Currency       string
	BalanceCents   int64
	AvailableCents int64
	AsOfUnix       int64
}

// TxnState is the replayed latest view of one transaction.
type TxnState struct {
	AccountID   string
	ID          string
	PostedUnix  int64
	AmountCents int64
	Description string
	Payee       string
	Memo        string
	Pending     bool
}

type txnKey struct{ account, id string }

// Ledger owns the JSONL file plus the replayed state. One process, one
// writer; the mutex serializes tool-triggered syncs against reads.
type Ledger struct {
	path string

	mu       sync.Mutex
	accounts map[string]*AccountState
	txns     map[txnKey]*TxnState
	lastSync int64
}

// OpenLedger creates dir if needed and replays an existing log.
func OpenLedger(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("simplefin: ledger dir: %w", err)
	}
	l := &Ledger{
		path:     filepath.Join(dir, "ledger.jsonl"),
		accounts: map[string]*AccountState{},
		txns:     map[txnKey]*TxnState{},
	}
	if err := l.replay(); err != nil {
		return nil, err
	}
	return l, nil
}

// replay folds the log into memory. A torn final line (crash mid-append)
// is skipped — the next sync re-observes anything it carried.
func (l *Ledger) replay() error {
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("simplefin: open ledger: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r record
		if json.Unmarshal([]byte(line), &r) != nil {
			continue // torn/foreign line — skip, never fail the open
		}
		l.fold(r)
	}
	return sc.Err()
}

// fold applies one record to the in-memory state (last write wins).
func (l *Ledger) fold(r record) {
	switch r.T {
	case "account":
		a := l.account(r.AccountID)
		a.Name, a.Org, a.Currency = r.Name, r.Org, r.Currency
	case "balance":
		a := l.account(r.AccountID)
		if r.BalanceCents != nil {
			a.BalanceCents = *r.BalanceCents
		}
		if r.AvailableCents != nil {
			a.AvailableCents = *r.AvailableCents
		}
		a.AsOfUnix = r.AsOfUnix
	case "txn":
		l.txns[txnKey{r.AccountID, r.TxnID}] = &TxnState{
			AccountID: r.AccountID, ID: r.TxnID, PostedUnix: r.PostedUnix,
			AmountCents: r.AmountCents, Description: r.Description,
			Payee: r.Payee, Memo: r.Memo, Pending: r.Pending,
		}
	case "sync":
		if r.SyncedAt > l.lastSync {
			l.lastSync = r.SyncedAt
		}
	}
}

func (l *Ledger) account(id string) *AccountState {
	a := l.accounts[id]
	if a == nil {
		a = &AccountState{ID: id}
		l.accounts[id] = a
	}
	return a
}

// ApplyResult reports what one sync changed.
type ApplyResult struct {
	NewTxns     int
	UpdatedTxns int
	Accounts    int
	ParseErrs   []string
}

// Apply diffs a fresh bridge snapshot against state, appends only the
// changed facts, and folds them in. Unparseable amounts are reported,
// never fatal — one weird transaction must not block the book.
func (l *Ledger) Apply(set *AccountSet, now time.Time) (ApplyResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var res ApplyResult
	var out []record

	for _, acct := range set.Accounts {
		res.Accounts++
		cur := l.accounts[acct.ID]
		if cur == nil || cur.Name != acct.Name || cur.Org != acct.Org.Name || cur.Currency != acct.Currency {
			out = append(out, record{
				T: "account", AccountID: acct.ID, Name: acct.Name,
				Org: acct.Org.Name, Currency: acct.Currency,
			})
		}
		balC, err := ParseCents(acct.Balance)
		if err != nil {
			res.ParseErrs = append(res.ParseErrs, fmt.Sprintf("%s balance: %v", acct.Name, err))
		} else {
			availC := balC
			if acct.AvailableBalance != "" {
				if a, aerr := ParseCents(acct.AvailableBalance); aerr == nil {
					availC = a
				}
			}
			if cur == nil || cur.BalanceCents != balC || cur.AvailableCents != availC || cur.AsOfUnix != acct.BalanceDate {
				bc, ac := balC, availC
				out = append(out, record{
					T: "balance", AccountID: acct.ID,
					BalanceCents: &bc, AvailableCents: &ac,
					AsOfUnix: acct.BalanceDate, AsOfDay: Day(acct.BalanceDate),
				})
			}
		}
		for _, txn := range acct.Transactions {
			amt, err := ParseCents(txn.Amount)
			if err != nil {
				res.ParseErrs = append(res.ParseErrs, fmt.Sprintf("txn %s: %v", txn.ID, err))
				continue
			}
			key := txnKey{acct.ID, txn.ID}
			prev := l.txns[key]
			if prev != nil && prev.PostedUnix == txn.Posted && prev.AmountCents == amt &&
				prev.Description == txn.Description && prev.Pending == txn.Pending {
				continue // already recorded, unchanged
			}
			if prev == nil {
				res.NewTxns++
			} else {
				res.UpdatedTxns++
			}
			out = append(out, record{
				T: "txn", AccountID: acct.ID, TxnID: txn.ID,
				PostedUnix: txn.Posted, PostedDay: Day(txn.Posted),
				AmountCents: amt, AmountRaw: txn.Amount,
				Description: txn.Description, Payee: txn.Payee, Memo: txn.Memo,
				Pending: txn.Pending,
			})
		}
	}
	out = append(out, record{T: "sync", SyncedAt: now.Unix()})

	if err := l.append(out); err != nil {
		return res, err
	}
	for _, r := range out {
		l.fold(r)
	}
	return res, nil
}

// append writes records as JSONL in one file append.
func (l *Ledger) append(recs []record) error {
	var b strings.Builder
	for _, r := range recs {
		j, err := json.Marshal(r)
		if err != nil {
			return err
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("simplefin: open ledger for append: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return fmt.Errorf("simplefin: append ledger: %w", err)
	}
	return nil
}

// LastSync returns the unix time of the last completed sync (0 = never).
func (l *Ledger) LastSync() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastSync
}

// Balances returns every account's latest state, sorted org then name.
func (l *Ledger) Balances() []AccountState {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AccountState, 0, len(l.accounts))
	for _, a := range l.accounts {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Org != out[j].Org {
			return out[i].Org < out[j].Org
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// AccountName resolves an account id to "Org · Name" for display.
func (l *Ledger) AccountName(id string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.accounts[id]
	if a == nil {
		return id
	}
	if a.Org != "" {
		return a.Org + " · " + a.Name
	}
	return a.Name
}

// Transactions returns txns posted in [fromUnix, toUnix] whose
// description/payee/memo contains query (case-insensitive; "" = all),
// newest first, capped at limit.
func (l *Ledger) Transactions(fromUnix, toUnix int64, query string, limit int) []TxnState {
	l.mu.Lock()
	defer l.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(query))
	out := make([]TxnState, 0, 32)
	for _, t := range l.txns {
		if t.PostedUnix < fromUnix || t.PostedUnix > toUnix {
			continue
		}
		if q != "" {
			hay := strings.ToLower(t.Description + " " + t.Payee + " " + t.Memo)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PostedUnix > out[j].PostedUnix })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// SpendSummary aggregates [fromUnix, toUnix]: money in/out per account
// (pending excluded — it double-counts against balances) and the top
// outflow descriptions.
type SpendSummary struct {
	PerAccount []AccountFlow
	TopSpend   []DescTotal
	InCents    int64
	OutCents   int64
}

type AccountFlow struct {
	AccountID string
	InCents   int64
	OutCents  int64
}

type DescTotal struct {
	Description string
	OutCents    int64
	Count       int
}

func (l *Ledger) Summary(fromUnix, toUnix int64) SpendSummary {
	l.mu.Lock()
	defer l.mu.Unlock()
	flows := map[string]*AccountFlow{}
	descs := map[string]*DescTotal{}
	var sum SpendSummary
	for _, t := range l.txns {
		if t.PostedUnix < fromUnix || t.PostedUnix > toUnix || t.Pending {
			continue
		}
		f := flows[t.AccountID]
		if f == nil {
			f = &AccountFlow{AccountID: t.AccountID}
			flows[t.AccountID] = f
		}
		if t.AmountCents >= 0 {
			f.InCents += t.AmountCents
			sum.InCents += t.AmountCents
		} else {
			f.OutCents += -t.AmountCents
			sum.OutCents += -t.AmountCents
			key := strings.ToLower(strings.Join(strings.Fields(t.Description), " "))
			d := descs[key]
			if d == nil {
				d = &DescTotal{Description: t.Description}
				descs[key] = d
			}
			d.OutCents += -t.AmountCents
			d.Count++
		}
	}
	for _, f := range flows {
		sum.PerAccount = append(sum.PerAccount, *f)
	}
	sort.Slice(sum.PerAccount, func(i, j int) bool { return sum.PerAccount[i].AccountID < sum.PerAccount[j].AccountID })
	for _, d := range descs {
		sum.TopSpend = append(sum.TopSpend, *d)
	}
	sort.Slice(sum.TopSpend, func(i, j int) bool { return sum.TopSpend[i].OutCents > sum.TopSpend[j].OutCents })
	if len(sum.TopSpend) > 10 {
		sum.TopSpend = sum.TopSpend[:10]
	}
	return sum
}
