-- T3 per-fact signals (Phase 1 of docs/memory-sme-plan.md).
--
-- A SIDE table, joined LEFT, so a fact with no row behaves EXACTLY as it
-- did before this table existed: importance = 0.5 (neutral), no pin/
-- protect, access_count = 0, valid_to = NULL (currently true). The hot
-- `facts` table, its FTS5 triggers, and the positional scanFact are never
-- touched — this keeps the change strictly additive.
--
-- importance is a Generative-Agents poignancy score (1-10 mapped to 0..1)
-- written at distill time and reinforced as a running average (never a
-- one-way ratchet). access_count + last_accessed are the MemoryOS revealed
-- "heat". pinned/protected gate decay/merge immunity. valid_from/valid_to
-- (Phase 2) and project_id (Phase 3) are reserved now to avoid a later
-- migration; they are unused until those phases land.

CREATE TABLE IF NOT EXISTS fact_signals (
    fact_id       INTEGER PRIMARY KEY REFERENCES facts(id) ON DELETE CASCADE,
    importance    REAL    NOT NULL DEFAULT 0.5,   -- GA poignancy 1-10 → 0..1; 0.5 = neutral
    confirm_count INTEGER NOT NULL DEFAULT 0,     -- times importance has been (re)scored; drives the running average
    access_count  INTEGER NOT NULL DEFAULT 0,     -- MemoryOS N_visit (revealed value)
    last_accessed INTEGER,                         -- unix epoch of last retrieval hit; NULL = never
    pinned        INTEGER NOT NULL DEFAULT 0,      -- Letta core block: always-include + decay-immune + merge-immune
    protected     INTEGER NOT NULL DEFAULT 0,      -- merge-immune (NOT always-included)
    valid_from    INTEGER,                          -- Zep event-time start (Phase 2; NULL = unknown)
    valid_to      INTEGER,                          -- Zep event-time end   (Phase 2; NULL = currently true)
    project_id    TEXT                              -- per-project scoping  (Phase 3; NULL = global)
);

CREATE INDEX IF NOT EXISTS idx_signals_pinned
    ON fact_signals(pinned, importance);

CREATE INDEX IF NOT EXISTS idx_signals_project
    ON fact_signals(project_id);
