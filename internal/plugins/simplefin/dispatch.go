package simplefin

// dispatch.go exposes the money_* tools. Everything is READ-ONLY (the
// protocol cannot move money), so nothing here is gated. Freshness is
// sync-on-read: a tool call first refreshes from the bridge when the
// ledger is older than staleAfter — no background goroutine to manage,
// and the TUI/serve/one-shot paths all behave identically. A failed
// refresh degrades to the cached ledger with a visible note instead of
// failing the turn.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tenant/internal/model"
)

// staleAfterDefault is how old the ledger may get before a tool call
// re-syncs. Bridges refresh bank data roughly daily; 6h keeps answers
// current without hammering anyone.
const staleAfterDefault = 6 * time.Hour

// syncOverlap re-fetches this far behind the last sync so pending
// transactions that posted (or late-arriving ones) are re-observed.
const syncOverlap = 72 * time.Hour

// firstSyncWindow bounds the initial backfill.
const firstSyncWindow = 90 * 24 * time.Hour

// Dispatcher implements the plugin contract over one Service + Ledger.
type Dispatcher struct {
	svc        *Service
	led        *Ledger
	staleAfter time.Duration
	clock      func() time.Time
}

func NewDispatcher(svc *Service, led *Ledger) *Dispatcher {
	clock := time.Now
	if svc != nil && svc.clock != nil {
		clock = svc.clock
	}
	return &Dispatcher{svc: svc, led: led, staleAfter: staleAfterDefault, clock: clock}
}

func (d *Dispatcher) Tools() []model.ToolSpec {
	obj := func(props string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[]}`)
	}
	return []model.ToolSpec{
		{Name: "money_balances", Description: "Current balance of every linked bank/card/investment account (read-only, via SimpleFIN). Returns per-account balances with as-of dates plus a net total.",
			Parameters: obj(``)},
		{Name: "money_transactions", Description: "List bank/card transactions, newest first (read-only). Filter by look-back window, free-text match on description/payee/memo, and count. Amounts: negative = money out.",
			Parameters: obj(`"days":{"type":"integer","description":"look-back window in days (default 30)"},"query":{"type":"string","description":"case-insensitive substring filter, e.g. a merchant name"},"limit":{"type":"integer","description":"max results (default 20, cap 200)"}`)},
		{Name: "money_spend", Description: "Spending summary over a window (read-only): money in/out per account and the top outflow merchants. Pending transactions excluded.",
			Parameters: obj(`"days":{"type":"integer","description":"look-back window in days (default 30)"}`)},
		{Name: "money_sync", Description: "Force a refresh from the SimpleFIN bridge now (bank data itself updates roughly daily). Tools auto-refresh when stale, so this is rarely needed.",
			Parameters: obj(``)},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if d.svc == nil || d.led == nil {
		return "simplefin is not configured — run `/configure simplefin <setup-token>` (get the token at bridge.simplefin.org)", true, nil
	}
	var args struct {
		Days  int    `json:"days"`
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return fmt.Sprintf("bad arguments: %v", err), true, nil
		}
	}
	switch call.Name {
	case "money_balances":
		note := d.ensureFresh(ctx, false)
		return d.renderBalances(note), false, nil
	case "money_transactions":
		note := d.ensureFresh(ctx, false)
		return d.renderTransactions(args.Days, args.Query, args.Limit, note), false, nil
	case "money_spend":
		note := d.ensureFresh(ctx, false)
		return d.renderSpend(args.Days, note), false, nil
	case "money_sync":
		note := d.ensureFresh(ctx, true)
		if note != "" {
			return note, true, nil
		}
		return "synced. " + d.freshness(), false, nil
	default:
		return "", false, fmt.Errorf("simplefin: unknown tool %q", call.Name)
	}
}

// ensureFresh syncs from the bridge when forced or stale. Returns a
// human-readable warning ("" = all good) — callers surface it alongside
// cached data rather than failing the turn.
func (d *Dispatcher) ensureFresh(ctx context.Context, force bool) string {
	now := d.clock()
	last := d.led.LastSync()
	if !force && last > 0 && now.Sub(time.Unix(last, 0)) < d.staleAfter {
		return ""
	}
	start := now.Add(-firstSyncWindow)
	if last > 0 {
		start = time.Unix(last, 0).Add(-syncOverlap)
	}
	set, err := d.svc.Accounts(ctx, start, now)
	if err != nil {
		if last == 0 {
			return fmt.Sprintf("(sync failed: %v — no cached data yet)", err)
		}
		return fmt.Sprintf("(sync failed: %v — showing cached data as of %s)", err, Day(last))
	}
	res, err := d.led.Apply(set, now)
	if err != nil {
		return fmt.Sprintf("(ledger write failed: %v)", err)
	}
	var notes []string
	if len(set.Errors) > 0 {
		notes = append(notes, "bridge notices: "+strings.Join(set.Errors, "; "))
	}
	if len(res.ParseErrs) > 0 {
		n := res.ParseErrs
		if len(n) > 3 {
			n = n[:3]
		}
		notes = append(notes, "unparsed amounts: "+strings.Join(n, "; "))
	}
	if len(notes) == 0 {
		return ""
	}
	return "(" + strings.Join(notes, " | ") + ")"
}

func (d *Dispatcher) freshness() string {
	last := d.led.LastSync()
	if last == 0 {
		return "ledger: never synced"
	}
	return "ledger as of " + time.Unix(last, 0).In(LA()).Format("2006-01-02 15:04 MST")
}

func (d *Dispatcher) renderBalances(note string) string {
	var b strings.Builder
	if note != "" {
		b.WriteString(note + "\n")
	}
	accounts := d.led.Balances()
	if len(accounts) == 0 {
		b.WriteString("no accounts in the ledger yet — run money_sync (bridge may still be aggregating)")
		return b.String()
	}
	totals := map[string]int64{}
	for _, a := range accounts {
		fmt.Fprintf(&b, "%s · %s: %s (as of %s)\n", orDash(a.Org), a.Name,
			FormatCents(a.BalanceCents, a.Currency), orDash(Day(a.AsOfUnix)))
		cur := a.Currency
		if cur == "" {
			cur = "USD"
		}
		totals[strings.ToUpper(cur)] += a.BalanceCents
	}
	for cur, t := range totals {
		fmt.Fprintf(&b, "net total: %s\n", FormatCents(t, cur))
	}
	b.WriteString(d.freshness())
	return b.String()
}

func (d *Dispatcher) renderTransactions(days int, query string, limit int, note string) string {
	if days <= 0 {
		days = 30
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	now := d.clock()
	from := now.Add(-time.Duration(days) * 24 * time.Hour).Unix()
	txns := d.led.Transactions(from, now.Unix(), query, limit)
	var b strings.Builder
	if note != "" {
		b.WriteString(note + "\n")
	}
	if len(txns) == 0 {
		fmt.Fprintf(&b, "no transactions in the last %d days", days)
		if query != "" {
			fmt.Fprintf(&b, " matching %q", query)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "%d transaction(s), last %d days", len(txns), days)
	if query != "" {
		fmt.Fprintf(&b, ", matching %q", query)
	}
	b.WriteString(" (negative = money out):\n")
	for _, t := range txns {
		pend := ""
		if t.Pending {
			pend = " [pending]"
		}
		desc := t.Description
		if t.Payee != "" && !strings.EqualFold(t.Payee, t.Description) {
			desc += " — " + t.Payee
		}
		fmt.Fprintf(&b, "%s  %9s  %s  (%s)%s\n", Day(t.PostedUnix),
			FormatCents(t.AmountCents, ""), desc, d.led.AccountName(t.AccountID), pend)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (d *Dispatcher) renderSpend(days int, note string) string {
	if days <= 0 {
		days = 30
	}
	now := d.clock()
	from := now.Add(-time.Duration(days) * 24 * time.Hour).Unix()
	sum := d.led.Summary(from, now.Unix())
	var b strings.Builder
	if note != "" {
		b.WriteString(note + "\n")
	}
	fmt.Fprintf(&b, "last %d days: in %s, out %s, net %s\n", days,
		FormatCents(sum.InCents, ""), FormatCents(sum.OutCents, ""),
		FormatCents(sum.InCents-sum.OutCents, ""))
	for _, f := range sum.PerAccount {
		fmt.Fprintf(&b, "  %s: in %s, out %s\n", d.led.AccountName(f.AccountID),
			FormatCents(f.InCents, ""), FormatCents(f.OutCents, ""))
	}
	if len(sum.TopSpend) > 0 {
		b.WriteString("top outflows:\n")
		for _, t := range sum.TopSpend {
			fmt.Fprintf(&b, "  %s — %s (%d txn)\n", t.Description, FormatCents(t.OutCents, ""), t.Count)
		}
	}
	b.WriteString(d.freshness())
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
