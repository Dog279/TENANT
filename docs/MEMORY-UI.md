# Memory Curator — UI spec (TEN-90)

Operator-facing design artifacts for the dashboard **Memory** view. The job is
**curate**: see what the agent knows, understand why, prune what's wrong, resolve
contradictions. NOT free-authoring. Copy is operator language only — never
"supersede / winner / loser / tombstone / provenance" in the UI.

---

## 1. Wireframe (ASCII)

```
┌─ Memory ───────────────────────────────────────────────────────────────┐
│  [ Facts ]  [ Soul ]                       Recently removed (3)  ↻ Refresh│  ← sub-tabs + actions
├──────────────────────────────────────────────────────────────────────────┤
│  FACTS (landing tab)                                                       │
│  ┌──────────────────────────────────────────────────────────────────┐    │
│  │ 🔎 Search facts…                       [ Resolve a conflict ]      │    │  ← search + resolve-mode
│  └──────────────────────────────────────────────────────────────────┘    │
│  ┌── fact row ──────────────────────────────────────────────────────┐     │
│  │ Prefers dark mode in all apps.                          conf 0.92 │     │
│  │ Why does the agent know this?            Remove                    │     │
│  └────────────────────────────────────────────────────────────────────┘   │
│  ┌── fact row (provenance expanded) ─────────────────────────────────┐     │
│  │ Lives in Berlin, Germany.                               conf 0.78 │     │
│  │ Why does the agent know this?            Remove                    │     │
│  │   ┌─ source turn ───────────────────────────────────────────┐     │     │
│  │   │ ⚡ 2026-05-21 14:03                                       │     │     │
│  │   │ You: where should I…   Agent: based on Berlin…           │     │     │
│  │   └──────────────────────────────────────────────────────────┘    │     │
│  └────────────────────────────────────────────────────────────────────┘   │
│                         [ Load more ]                                       │  ← cursor pager
└──────────────────────────────────────────────────────────────────────────┘

Resolve mode (after "Resolve a conflict"):  pick two rows → side-by-side card
┌─ These two look like they conflict — keep which? ──────────────────────┐
│  ( ) Lives in Berlin.            ( ) Lives in Munich.                   │
│      conf 0.78                       conf 0.66                          │
│                          [ Cancel ]  [ Keep selected ]                 │
└────────────────────────────────────────────────────────────────────────┘

SOUL tab
┌──────────────────────────────────────────────────────────────────────────┐
│  Persona            (read-only block; not operator-authored here)          │
│  ┌──────────────────────────────────────────────────────────────────┐    │
│  │ You are tenant, a calm operations copilot…                        │    │
│  └──────────────────────────────────────────────────────────────────┘    │
│                                                                            │
│  What the agent knows about you            [ + Add ]                       │
│  ┌──────────────────────────────────────────────────────────────────┐    │
│  │ Name is Ada.                              Edit   Remove          │    │
│  │ [ inline editor when Edit clicked: textarea + Save / Cancel ]      │    │
│  └──────────────────────────────────────────────────────────────────┘    │
│                                                                            │
│  Standing instructions                     [ + Add ]                       │
│  ┌──────────────────────────────────────────────────────────────────┐    │
│  │ Always answer in metric units.              Edit   Remove          │    │
│  └──────────────────────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────────────────────┘

Recently removed (toggled section / overlay panel)
┌─ Recently removed ─────────────────────────────────────────────────────┐
│  Lives in Munich.                                    Restore            │
│  Old project name was Apollo.                        Restore            │
└──────────────────────────────────────────────────────────────────────────┘

Confirm dialog (NEW primitive)            Toast w/ Undo (NEW primitive)
┌─ Remove this fact? ──────────────┐      ┌────────────────────────────────┐
│ This removes it from what the    │      │ Removed. ·  Undo               │
│ agent knows. You can restore it  │      └────────────────────────────────┘
│ from Recently removed.           │
│           [ Cancel ] [ Remove ]  │
└──────────────────────────────────┘

DASHBOARD addition: a "Working memory" stat card showing the live count.
```

---

## 2. Surface × state matrix

Each panel × {loading, empty, error, partial} → what renders, in the existing
`.banner` / `.muted` vocabulary. (`.banner.err` = red error box; `.banner.notice`
= amber advisory; `.muted` = dim inline text.)

| Surface | loading | empty | error | partial / special |
|---|---|---|---|---|
| **Facts list** | `.muted` "Loading facts…" | `.muted` "No facts yet. The agent adds facts as it learns from conversations." | `.banner.err` "Couldn't load facts: <msg>. Refresh to retry." | results + `[Load more]` while `next_cursor`; spinner→"Loading…" on the button |
| **Facts search (q)** | button/inline "Searching…" | `.muted` "No facts match \"<q>\". Clear the search to see all." | `.banner.err` same as list | — |
| **Provenance (inline)** | `.muted` "Loading source…" | `.muted` "No source recorded for this fact." | `.banner.err` (inline, scoped to the row) "Couldn't load the source: <msg>." | per-episode `missing:true` → card body "Source unavailable — pre-distillation or imported." (never a dead link) |
| **Resolve / conflict pick** | n/a (client-side) | n/a (operator selects) | `.banner.err` "Couldn't resolve: <msg>." | needs exactly 2 picks → "Keep selected" disabled until 2 chosen; identical pick guard |
| **Recently removed** | `.muted` "Loading…" | `.muted` "Nothing here. Removed facts show up so you can restore them." | `.banner.err` "Couldn't load recently removed: <msg>." | list + per-row `Restore` |
| **Soul — persona** | `.muted` "Loading…" | `.muted` "No persona set." | `.banner.err` (top of Soul) "Couldn't load the soul: <msg>." | read-only block |
| **Soul — user facts / instructions** | (same load as persona) | `.muted` "Nothing yet. Add what the agent should always know." / "No standing instructions yet." | shared `.banner.err`; per-row save error → inline `.banner.err` under the row | inline editor states: dirty (Save enabled), saving ("Saving…", inputs disabled), saved (toast "Saved"), error (inline banner, keep editor open) |
| **Working-memory stat (Dashboard)** | value "…" | value "0" | value "—" (silent; it's a peek, never blocks the dash) | — |
| **User-profile peek (Dashboard, optional)** | `.muted` "Loading…" | `.muted` "No profile yet." | `.muted` "Profile unavailable." (non-blocking) | `[Re-synthesize]` → button "Working…" → toast |

---

## 3. Microcopy deck (exact strings)

**Sub-tabs / chrome**
- Tabs: `Facts`, `Soul`
- `Recently removed` (with count when >0: `Recently removed (N)`)
- Refresh action: `Refresh`

**Facts**
- Search placeholder: `Search facts…`
- Resolve entry button: `Resolve a conflict`
- Per row: provenance link `Why does the agent know this?`; delete `Remove`; confidence shown as `conf 0.92` (2 dp).
- Load more: `Load more` → busy `Loading…`
- Empty (no facts): `No facts yet. The agent adds facts as it learns from conversations.`
- Empty (search): `No facts match "<q>". Clear the search to see all.`
- Load error: `Couldn't load facts: <msg>. Refresh to retry.`

**Provenance**
- Trigger: `Why does the agent know this?` (when open: `Hide source`)
- Loading: `Loading source…`
- None: `No source recorded for this fact.`
- Missing episode: `Source unavailable — pre-distillation or imported.`
- Error: `Couldn't load the source: <msg>.`

**Remove → confirm → undo**
- Confirm title: `Remove this fact?`
- Confirm body: `This removes it from what the agent knows. You can restore it from Recently removed.`
- Confirm buttons: `Cancel` / `Remove`
- Undo toast: `Removed.` + action `Undo`
- Undo failure: toast `Couldn't undo — find it under Recently removed.`
- Remove failure: toast `Couldn't remove that fact: <msg>.`

**Resolve a conflict**
- Mode hint: `Pick two facts that conflict, then choose which to keep.`
- Card title: `These two look like they conflict — keep which?`
- Buttons: `Cancel` / `Keep selected`
- Same-fact guard: `Pick two different facts.`
- Success toast: `Kept the one you chose.`
- Error toast: `Couldn't resolve that: <msg>.`

**Recently removed**
- Heading: `Recently removed`
- Sub: `Facts you removed recently. Restore any to add it back.`
- Per row: `Restore`
- Empty: `Nothing here. Removed facts show up so you can restore them.`
- Restore success toast: `Restored.`
- Restore error toast: `Couldn't restore that fact: <msg>.`

**Soul**
- Persona heading: `Persona`
- Persona sub: `How the agent presents itself. Set in configuration.`
- User-facts heading: `What the agent knows about you`
- User-facts empty: `Nothing yet. Add what the agent should always know.`
- Instructions heading: `Standing instructions`
- Instructions empty: `No standing instructions yet.`
- Add button: `+ Add`
- Add placeholder (fact): `Something the agent should always know…`
- Add placeholder (instruction): `An instruction the agent should always follow…`
- Row actions: `Edit` / `Remove`
- Editor buttons: `Save` / `Cancel`
- Saving: `Saving…`
- Saved toast: `Saved.`
- Remove-confirm title: `Remove this item?`
- Remove-confirm body: `The agent will no longer use it. This can't be undone here.`
- Save/remove error (inline): `Couldn't save that change: <msg>.`
- Load error: `Couldn't load the soul: <msg>.`

**Dashboard**
- Stat label: `Working memory`
- Profile peek heading: `User profile`
- Profile resync button: `Re-synthesize` → busy `Working…`
- Profile resync toast: `Re-synthesizing the profile…`
- Profile empty: `No profile yet.`
- Profile unavailable: `Profile unavailable.`

---

## 4. Fact page (TEN-297)

One fact, fully explained, at `/memory/facts/{id}`. Every row of the Facts list
links to it (`Everything about it`) when the server's memory can explain facts.
A plain GET page; its states are query params (`?turns=1`, `?compare=A,B`,
`?edit=1`). Remove / Restore and No longer true / It's true again answer in
place with a toast (`POST /memory/facts/removal` and `/memory/facts/valid`,
the fact's id as the `fact` field); each pair is one form, so a double tap
acts once. Edit and "Make this current again" go to the new version's page.
The same data is JSON at `/api/memory/facts/{id}` for scripts.

```
┌─ Memory · Fact ──────────────────────────────────────────────────────────┐
│ User lives in Munich.                        #7 · current · 0.90 (now 0.88)│
│ [Remove] [Edit] [No longer true] [It contradicts another fact]           │
├──────────────────────────────────────────────────────────────────────────┤
│ Where it came from                                                        │
│ How it got here: learned from a conversation · glm-5 · 2026-09-26 12:00   │
│ Learned 2026-09-01 10:00 · last confirmed 2026-09-20 08:12                │
│   You: I moved to Munich   Agent: noted                  2026-09-01 10:00 │
│   Conversation #12 is no longer in memory.                                │
│ A source was deleted: conversation #10, by a forget on 2026-09-02.        │
│ Show the full conversations from the archive                              │
├──────────────────────────────────────────────────────────────────────────┤
│ Signals    Importance: 0.70 · Pinned: yes · Protected: no                 │
│ Used: 4 retrieval(s), last 2026-09-25 18:40                               │
│ Cited by the expert notes: About the user                                 │
├──────────────────────────────────────────────────────────────────────────┤
│ Versions and conflicts                                                    │
│ Replaced #5 (your edit): User lives in Berlin.                            │
│   User lives in Berlin.      #5 · 2026-09-01 · replaced · learned from …  │
│     Compare   Make this current again                                     │
│   User lives in Munich.      #7 · 2026-09-26 · current · your edit        │
│ #5 → #7   User lives in [-Berlin-] [+Munich+]                             │
└──────────────────────────────────────────────────────────────────────────┘
```

**Microcopy**
- List link: `Everything about it`
- Status words: `current`, `replaced`, `removed` (never superseded / tombstoned)
- Actions on a current fact: `Edit`, `No longer true` (or `It's true again`),
  `It contradicts another fact` (opens the conflict picker with this fact),
  `Remove`. On a removed fact: `Restore`.
- Editor: `Save as a new version` / `Cancel`. Empty: `A fact can't be empty.`
  Saved: `Saved as a new version; the earlier one is kept below.`
- Validity: `No longer true since <date>.` Notices: `Marked no longer true.` /
  `Marked true again.` Errors: `Couldn't mark that fact no longer true: <msg>` /
  `Couldn't mark that fact true again: <msg>`
- Origin: `How it got here: <how> · <model> · <date>`, where how is one of
  `learned from a conversation`, `merged during memory upkeep`, `your edit`,
  `your restore of an earlier version`, `imported`, `saved by the agent`,
  `saved by an MCP client`, `an eval run`. None recorded: `not recorded
  (learned before Tenant kept this)`.
- Sources: `No source conversations are recorded for this fact.` Missing:
  `Conversation #N is no longer in memory.` Deleted: `A source was deleted:
  conversation #N, by a forget on <date>.` (or `run record <id>`, `by retention`)
- Archive: `Show the full conversations from the archive`. A turn it can't
  rebuild says why: `the conversation is no longer in memory`, `the archive has
  no record of it (pruned, or older than the archive)`, `the archive isn't
  available here`.
- Versions: `Replaced by #N (<how>): <text>`, `Replaced #N (<how>): <text>`,
  `Compare`, `Make this current again`. Restored: `Restored as a new version.`
- Use: `Used: N retrieval(s)`, `, last <date>`; `Cited by the expert notes:
  <sections>`
- Errors: `Couldn't load that fact: <msg>.`, `Couldn't save that: <msg>`,
  `Couldn't restore that version: <msg>`, `Couldn't rebuild the conversations:
  <msg>.` Without an inspector: `This server's memory can't explain individual
  facts.`
