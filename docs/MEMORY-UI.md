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
