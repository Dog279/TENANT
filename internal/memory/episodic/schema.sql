-- T2 Episodic memory schema.
--
-- One row per (prompt, response) turn-pair, with optional tool calls,
-- outcome flag, and user feedback. Vectors are stored as BLOBs of
-- little-endian float32 — sqlite-vec would be faster at scale but
-- requires CGO; we use brute-force cosine in Go for v1 (acceptable
-- up to ~100K episodes on personal hardware).

CREATE TABLE IF NOT EXISTS episodes (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id        TEXT NOT NULL,
    visibility      TEXT NOT NULL DEFAULT 'private',  -- private | shared | public
    session_id      TEXT,
    ts              INTEGER NOT NULL,                  -- unix epoch
    prompt          TEXT NOT NULL,
    response        TEXT NOT NULL,
    tool_calls      TEXT,                              -- JSON: [{id,name,arguments}]
    outcome         TEXT,                              -- success | error | unknown
    user_feedback   TEXT,                              -- ack | undo | (null)
    tags            TEXT,                              -- JSON string array
    embedder_id     TEXT NOT NULL,
    embedding       BLOB NOT NULL,                     -- little-endian float32[]
    tombstoned      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_episodes_agent_vis
    ON episodes(agent_id, visibility, tombstoned);

CREATE INDEX IF NOT EXISTS idx_episodes_ts
    ON episodes(ts);

-- FTS5 mirror for keyword search. External-content table means FTS
-- doesn't duplicate the prompt/response text — it indexes the live
-- main table. Triggers below keep FTS in sync on insert / delete /
-- update.
CREATE VIRTUAL TABLE IF NOT EXISTS episodes_fts USING fts5(
    prompt, response, tags,
    content='episodes', content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS episodes_ai AFTER INSERT ON episodes BEGIN
    INSERT INTO episodes_fts(rowid, prompt, response, tags)
        VALUES (new.id, new.prompt, new.response, coalesce(new.tags, ''));
END;

CREATE TRIGGER IF NOT EXISTS episodes_ad AFTER DELETE ON episodes BEGIN
    INSERT INTO episodes_fts(episodes_fts, rowid, prompt, response, tags)
        VALUES ('delete', old.id, old.prompt, old.response, coalesce(old.tags, ''));
END;

CREATE TRIGGER IF NOT EXISTS episodes_au AFTER UPDATE OF prompt, response, tags ON episodes BEGIN
    INSERT INTO episodes_fts(episodes_fts, rowid, prompt, response, tags)
        VALUES ('delete', old.id, old.prompt, old.response, coalesce(old.tags, ''));
    INSERT INTO episodes_fts(rowid, prompt, response, tags)
        VALUES (new.id, new.prompt, new.response, coalesce(new.tags, ''));
END;
