-- Per-project Subject-Matter-Expert documents (Phase 3 of docs/memory-sme-plan.md).
--
-- The SME is the durable nuance carrier: a sectioned, living document
-- synthesized by the ReflectionJob from a project's protected/high-importance
-- facts (+ recent episodes), injected into the system reserve every turn. It is
-- consolidation-by-ADDITION over the fact layer — it never supersedes the
-- source facts (which it cites for provenance + reaffirms on use).
--
-- One row per (project_id, section, version). A re-synthesis writes a NEW
-- version rather than overwriting, so prior versions stay for audit/rollback;
-- the "active" doc is the highest version of each section. project_id is "" for
-- the single-project (global) deployment that Phase 3 ships; the multi-project
-- registry is deferred (design §12.1).
--
-- Sibling table over the shared facts DB: CREATE TABLE IF NOT EXISTS, never
-- touches the facts / fact_signals tables. Strictly additive.

CREATE TABLE IF NOT EXISTS sme_docs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id      TEXT NOT NULL DEFAULT '',   -- "" = global (single-project)
    agent_id        TEXT NOT NULL,
    section         TEXT NOT NULL,               -- e.g. Architecture & Decisions, Conventions & Gotchas, ...
    body            TEXT NOT NULL,
    source_fact_ids TEXT,                         -- JSON array of fact IDs (provenance; reaffirmed on synthesis)
    version         INTEGER NOT NULL DEFAULT 1,
    updated_at      INTEGER NOT NULL,
    token_estimate  INTEGER NOT NULL DEFAULT 0
);

-- Active-doc lookup: latest version per (project, agent, section). UNIQUE so a
-- (project, agent, section, version) tuple can exist only once — UpsertSection's
-- max(version)+1 guarantees this by construction; the constraint turns any
-- hypothetical concurrent double-write (not possible under the serial scheduler
-- today) into a loud insert error on one writer rather than a silent duplicate.
CREATE UNIQUE INDEX IF NOT EXISTS idx_sme_active
    ON sme_docs(project_id, agent_id, section, version);

-- Reserved project registry for the deferred multi-project mode (design §12.1).
-- Unused in Phase 3 (single-project); created now so adding it later needs no
-- migration.
CREATE TABLE IF NOT EXISTS sme_projects (
    id             TEXT PRIMARY KEY,
    label          TEXT,
    root_path      TEXT,
    created_at     INTEGER,
    last_active_at INTEGER
);
