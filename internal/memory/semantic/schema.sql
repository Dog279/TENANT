-- T3 Semantic memory schema.
--
-- One row per atomic distilled fact. Facts are produced by the
-- distillation job (a separate package) that summarizes runs of
-- episodes into one-sentence claims. The store itself is unopinionated:
-- it accepts whatever facts the distiller produces and lets the user
-- supersede / tombstone / reaffirm them over time.
--
-- Confidence decays at retrieval time based on (now - last_confirmed),
-- so facts that aren't being re-validated by recent episodes naturally
-- fall in ranking. Hard truths the user keeps confirming stay sharp.

CREATE TABLE IF NOT EXISTS facts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id        TEXT NOT NULL,
    visibility      TEXT NOT NULL DEFAULT 'private',  -- private | shared | public
    fact            TEXT NOT NULL,                     -- one-sentence atomic claim
    source_episodes TEXT,                              -- JSON array of episode IDs
    confidence      REAL NOT NULL DEFAULT 1.0,         -- 0..1 base confidence
    first_seen      INTEGER NOT NULL,                  -- unix epoch
    last_confirmed  INTEGER NOT NULL,                  -- unix epoch
    superseded_by   INTEGER,                           -- FK to facts.id, NULL = current
    embedder_id     TEXT NOT NULL,
    embedding       BLOB NOT NULL,                     -- little-endian float32[]
    tombstoned      INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (superseded_by) REFERENCES facts(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_facts_agent_vis
    ON facts(agent_id, visibility, tombstoned, superseded_by);

CREATE INDEX IF NOT EXISTS idx_facts_last_confirmed
    ON facts(last_confirmed);

CREATE VIRTUAL TABLE IF NOT EXISTS facts_fts USING fts5(
    fact,
    content='facts', content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS facts_ai AFTER INSERT ON facts BEGIN
    INSERT INTO facts_fts(rowid, fact) VALUES (new.id, new.fact);
END;

CREATE TRIGGER IF NOT EXISTS facts_ad AFTER DELETE ON facts BEGIN
    INSERT INTO facts_fts(facts_fts, rowid, fact) VALUES ('delete', old.id, old.fact);
END;

CREATE TRIGGER IF NOT EXISTS facts_au AFTER UPDATE OF fact ON facts BEGIN
    INSERT INTO facts_fts(facts_fts, rowid, fact) VALUES ('delete', old.id, old.fact);
    INSERT INTO facts_fts(rowid, fact) VALUES (new.id, new.fact);
END;
