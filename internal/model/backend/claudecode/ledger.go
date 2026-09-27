package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"tenant/internal/model"
)

// ledger is the rolling-7-day token guard for the subscription. It exists so
// a chatty daemon cannot silently eat the operator's interactive weekly
// allowance: once the local sum crosses TENANT_CLAUDECODE_WEEKLY_BUDGET the
// backend refuses BEFORE spawning (ErrInsufficientBalance → FallbackLLM moves
// to the next provider with a long cooldown).
//
// Accounting is intentionally approximate: it records input + cache-write +
// output tokens as reported by the CLI envelope (cache reads are ~free and
// excluded). The guard is process-locked only — concurrent tenant processes
// each keep the file consistent via read-modify-write, but a racing write can
// lose an entry. Acceptable: this is a budget rail, not billing.
type ledger struct {
	mu     sync.Mutex
	path   string
	budget int64
}

type ledgerEntry struct {
	TS     time.Time `json:"ts"`
	Tokens int64     `json:"tokens"`
}

type ledgerFile struct {
	Entries []ledgerEntry `json:"entries"`
}

const ledgerWindow = 7 * 24 * time.Hour

// openLedger builds the ledger; budget 0 (unset/invalid env) disables the
// pre-call refusal but spend is still tracked for observability.
func openLedger() *ledger {
	var budget int64
	if v := os.Getenv(budgetEnv); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			budget = n
		} else {
			model.BudgetInvalid.Log(context.Background(), "env", budgetEnv, "value", v)
		}
	}
	return &ledger{
		path:   LedgerPath(),
		budget: budget,
	}
}

// LedgerPath is the spend ledger's file: in the OS config dir's tenant folder,
// whatever --config says. cmd/tenant keeps it out of the file tools' reach, so
// a turn cannot reset its own budget rail.
func LedgerPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "tenant", "claudecode-usage.json")
}

// spent returns the token sum inside the rolling window.
func (l *ledger) spent() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	lf := l.load()
	var sum int64
	cutoff := time.Now().Add(-ledgerWindow)
	for _, e := range lf.Entries {
		if e.TS.After(cutoff) {
			sum += e.Tokens
		}
	}
	return sum
}

// overBudget reports whether the guard should refuse new calls.
func (l *ledger) overBudget() bool {
	return l.budget > 0 && l.spent() >= l.budget
}

// add records a call's tokens and prunes entries older than the window.
// Best-effort persistence: a write failure logs and moves on — losing an
// entry loosens the rail slightly, which beats failing the turn.
func (l *ledger) add(tokens int) {
	if tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	lf := l.load()
	cutoff := time.Now().Add(-ledgerWindow)
	pruned := lf.Entries[:0]
	for _, e := range lf.Entries {
		if e.TS.After(cutoff) {
			pruned = append(pruned, e)
		}
	}
	lf.Entries = append(pruned, ledgerEntry{TS: time.Now(), Tokens: int64(tokens)})
	if err := l.save(lf); err != nil {
		model.BudgetUnsaved.Log(context.Background(), "path", l.path, "err", err)
	}
}

// load reads the ledger file; a missing or corrupt file is an empty ledger
// (corruption self-heals on the next save).
func (l *ledger) load() ledgerFile {
	var lf ledgerFile
	b, err := os.ReadFile(l.path)
	if err != nil {
		return lf
	}
	if err := json.Unmarshal(b, &lf); err != nil {
		return ledgerFile{}
	}
	return lf
}

func (l *ledger) save(lf ledgerFile) error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(lf)
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}
