-- Provenance tombstones (OPS-003 / TEN-309).
--
-- A privacy deletion or a retention prune can remove an episode or a run that
-- a fact was distilled from. The fact keeps its text but loses that link, and
-- a row here records what the link pointed at, when it went and why. The
-- fact's provenance then says "source removed" instead of silently shrinking.
-- Rows go with their fact.

CREATE TABLE IF NOT EXISTS fact_source_tombstones (
    fact_id     INTEGER NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    source_kind TEXT    NOT NULL,   -- episode | run
    source_id   TEXT    NOT NULL,
    removed_at  INTEGER NOT NULL,   -- unix epoch
    reason      TEXT    NOT NULL,   -- forget | retention
    PRIMARY KEY (fact_id, source_kind, source_id)
);

-- Fact origin (PRD-001 / TEN-297): which job put a fact here (distill,
-- consolidate, import, remember, mcp, edit, restore) and, where a model did
-- the work, which one. A side table like fact_signals, so the hot facts
-- table and its scans are untouched; a fact from before this table simply
-- has no origin row.
CREATE TABLE IF NOT EXISTS fact_origin (
    fact_id     INTEGER PRIMARY KEY REFERENCES facts(id) ON DELETE CASCADE,
    job         TEXT    NOT NULL,
    model       TEXT,
    recorded_at INTEGER NOT NULL
);
