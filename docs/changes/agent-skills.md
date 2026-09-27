# Change: Agent Skills packages, and skills that are trusted before they are used (EXT-001)

**Branch:** `feat/ten-298-agent-skills` · **Status:** implemented and tested; not yet
deployed.
**Ticket:** EXT-001 / TEN-298.

## What changed
- **Tenant reads and writes the open Agent Skills format.** `internal/skillpkg`
  handles a `SKILL.md` package: YAML frontmatter and a Markdown body.
  - It validates the specification's rules: the name, 1–64 lowercase letters,
    digits and single hyphens, matching the directory; the description, 1–1024
    characters; `compatibility`, at most 500; `metadata`, strings only; and the
    experimental `allowed-tools`.
  - It lists the package's bundled files, and renders what it reads back.
- **Import:** `/skills import <dir|SKILL.md> [--trust]` and `tenant skills import`.
  The instructions become the skill's recipe. Tenant installs no bundled
  `scripts/`, `references/` or `assets/`, and says which it skipped.
  - A package replaces only a skill it imported earlier, never one the operator,
    the agent or a seed made.
  - The same package again changes nothing. A changed one comes back untrusted,
    its prior version in the history.
- **Export:** `/skills export <name> <dir>` and `tenant skills export`. They write
  a standard package that opens anywhere:
  - the recipe as the body;
  - the required tools as `allowed-tools`;
  - Tenant's version and origin in `metadata`, as ADR 0001 says.

  A name the format disallows is exported under its slug. Trust is never
  written: it is the decision of the operator who imports the package.
- **Trust, provenance, version and requirements are recorded** (`skills.db`):
  - `trust`: `trusted` or `untrusted`;
  - `origin`: operator, agent, induction, seed or import;
  - the declared `version`;
  - the required tools: an import's `allowed-tools`;
  - for an import, the package: source, `SKILL.md` digest, license,
    compatibility, metadata, the files not installed, and when.

  Old databases migrate in place, and their skills stay trusted, as they were.
- **Only eligible skills reach the agent.** A skill is offered only when it is
  live, enabled and trusted, and, for the main agents of the TUI, serve and
  orchestrate, when every tool it requires is enabled there. `allowed-tools`
  names from other agents map to Tenant's (`Bash` is `os_exec`, `Read` is
  `os_read_file`, …). A name Tenant does not know is never available.
- **Untrusted skills cannot expand what the agent does:**
  - **`skill_save` is untrusted.** What the agent saves, including an edit to a
    trusted skill, is untrusted and not offered until the operator trusts it.
    A skill is injected into every later turn's operating rules, and the agent
    may be writing what a web page, an email, or a relayed Discord or iMessage
    message told it to. Cron, the relays and the iMessage responder can all
    call `skill_save`, so this closes a way for content to persist into the
    interactive agent. The tool's result tells the agent the skill waits for
    review.
  - **Imports are untrusted** unless the operator passes `--trust`.
  - **Widening loses trust.** A write that widens a skill's required tools,
    without an explicit trust decision, makes it untrusted.
  - **Requirements approve nothing.** A package's `allowed-tools` decide only
    where the skill is offered: nothing in the approval path reads a skill, and
    a test holds it so.
- **Review surfaces:**
  - `/skills show <name>` and `tenant skills show` print what the operator needs
    to decide: trust, origin, version, required tools, the package's provenance,
    why it is not offered, and the instructions.
  - `/skills trust|untrust` and `tenant skills trust|untrust` decide. Each
    decision is a history entry, so `/skills history` shows who trusted what,
    and when.
  - The list marks untrusted skills and who wrote them.
  - The dashboard's Skills page has a "Waiting for your review" group, with the
    instructions, a Trust button and a Stop trusting button.
  - `tenant skills list` is new.
- **History and revert still work,** and now carry trust. Every snapshot records
  the trust and required tools it had, and a revert restores them. So reverting
  to the version the operator trusted brings back that trust, and reverting to
  an untrusted one does not trust it.
- **Seeds are recorded as seeds** (origin and history `seed`), no longer as skills
  the operator wrote.

## Why
Tenant's skill model had status, an enabled flag and history, but no version,
tool requirements, provenance or trust, and it could not read or write the
standard format. Every live skill went straight into the system prompt's
operating rules. `skill_save` wrote live skills with no review, from any
surface, recorded as the operator's own. ADR 0001 names Agent Skills as one
of Tenant's two extension boundaries, with explicit trust.

## Behavior changes
- **`skill_save` no longer takes effect at once.** The operator trusts a saved
  skill first: `/skills trust <name>`, `tenant skills trust <name>`, or the
  dashboard.
- **An agent edit of a trusted skill untrusts it** until the operator reviews
  the change (`/skills diff`, then `/skills trust`).
- **Some skills are no longer offered.** A skill whose required tools are off is
  not offered to the TUI, serve or orchestrate main agent. Skills made before
  this change require nothing.
- **Skills that sub-agents save** under their own ids are untrusted too. They
  were invisible to the operator's `/skills` before, and still are.

## Still open
- Where tools are checked: sub-agents, cron runners and the relays are offered
  trusted skills without the tool check; their registries are narrower views of
  the main one.
- Bundled files: a package's `scripts/`, `references/` and `assets/` are listed,
  not installed. Nothing in Tenant runs a skill's code.
- The `allowed-tools` mapping covers the common names of the agents packages
  are written for; others are reported and never available.

## Evidence
- **`internal/skillpkg`:**
  - parsing, including CRLF, a BOM, a list-form `allowed-tools`, and unknown keys;
  - malformed frontmatter refused;
  - every specification rule, at and past its limits;
  - slugs;
  - render round-trips;
  - reading a directory or its `SKILL.md`, with hidden files skipped;
  - writing, which refuses to overwrite.
- **`internal/memory/skills`:**
  - the new fields round-trip, and an edit keeps them;
  - untrusted skills are never retrieved;
  - required tools filter retrieval;
  - widening drops trust, narrowing keeps it, and an explicit decision wins;
  - trust decisions are recorded in history, and a revert restores trust and
    requirements;
  - eligibility reasons;
  - a pre-TEN-298 database migrates with its skills trusted, and its old history
    still reverts.
- **`cmd/tenant`:**
  - imports start untrusted with full provenance;
  - the re-import rules;
  - export round-trips, and trust does not travel;
  - `skill_save` is untrusted, and an agent edit untrusts;
  - retrieval checks tools, through the aliases;
  - seeds are seeds;
  - no approval file reads a skill, and importing and trusting a skill that
    lists `os_exec` leaves `os_exec` asking.
- **`internal/tui`:** the list markers, import, export, trust and untrust, and
  show.
- **`internal/dashboard`:** the review group, whose hostile recipe renders
  escaped, and the trust form, where anything but `trusted=true` distrusts.
- **By hand:** the built binary ran `tenant skills import`, `list`, `show`,
  `trust` and `export` against scratch directories. The exported `SKILL.md` is a
  standard package.
- **Mutation testing:** 18 mutants of the invariants above, all caught.
- **Suite:** `go test ./...` (70 packages), `go vet`, smoke eval 5/5.
