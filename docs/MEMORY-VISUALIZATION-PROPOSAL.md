# Temporal Memory Visualization — Proposal

Status: PROPOSAL (not implementation)
Date: 2026-06-01
Companion to: MEMORY-TEMPORAL.md, MEMORY-DESIGN.md, MEMORY-UI.md

---

## 0. What this is

A design proposal for a visual landing page inside the existing Memory view of
the Tenant dashboard. The goal: render the bi-temporal fact store and entity
graph as an interactive visualization, giving the operator an at-a-glance
understanding of what the agent knows, when it learned it, and how facts relate
to each other — before they dive into the curatorial detail of the Facts/Soul
tabs that already exist.

This is NOT a replacement for the existing Memory Curator UI (MEMORY-UI.md).
It is a new "Overview" sub-tab that becomes the default landing view when you
click "Memory" in the sidebar.

---

## 1. Architecture constraints (non-negotiable)

These come from the existing dashboard implementation:

1. No build step. The dashboard is an embedded Go binary (//go:embed assets).
   All JS must be plain script-tag or ES-module — no bundler, no JSX, no npm.
   The existing app.js is 56KB of vanilla JS.
2. No external dependencies that require a CDN at runtime. The dashboard may
   run on localhost with no internet. Any library must be vendored into assets/.
3. Must work with the existing REST API surface in memory_rest.go — no new
   backend endpoints required for the MVP (Phase 1). Phase 2 adds 1 endpoint.
4. Must not regress the existing Memory view. New code lives in a separate
   sub-tab ("Overview") and a new JS module (memory-viz.js).

---

## 2. Technology choice

### Rejected options

| Library | Why rejected |
|---------|-------------|
| vis-timeline + vis-network | 250KB+ combined; requires npm ecosystem; last meaningful update 2023; module system assumes bundler |
| Cytoscape.js | 400KB min; designed for bioinformatics-scale graphs; overkill for <10K nodes; complex API for simple use |
| Sigma.js | Requires graphology dependency chain; 200KB+; WebGL renderer is unnecessary at our scale |
| D3 (full) | 250KB; we'd use 5% of it; D3 modules work via npm only |
| react-flow / svelte-flow | Requires React/Svelte — contradicts constraint 1 |

### Selected: Two lightweight libraries, vendored

1. **D3 submodules (d3-scale, d3-axis, d3-zoom, d3-selection, d3-array)**
   - ~30KB combined gzipped (cherry-picked, not full D3)
   - MIT license
   - Works as individual script tags or ES modules
   - Used for: the timeline axis (horizontal time scale with zoom/pan)
   - Version: v7 latest stable

2. **Force-graph (v1.x, by vasco-santos) or a hand-rolled force layout**
   - Force-graph: ~20KB, Canvas-based, zero dependencies, script-tag compatible
   - Alternative: a 100-line hand-rolled force simulation using Canvas 2D
     (simpler, no dependency, sufficient for <500 entity nodes)
   - Used for: the entity graph view

**Recommendation: vendored D3 submodules for the timeline + hand-rolled Canvas
force graph.** This keeps the dependency surface to ~30KB (D3 subset) and zero
for the graph. The hand-rolled force is trivial at the scale we're targeting
(personal agent memory: tens to low hundreds of entities, thousands of facts).

### Why hand-rolled force graph is viable

The MEMORY-TEMPORAL design caps the entity graph at personal scale:
- Phase 2 entity table: ~100-500 entities (people, projects, concepts, orgs)
- Phase 2 edges table: ~1000-5000 typed relationships
- This is 2 orders of magnitude below where Cytoscape/Sigma add value

A basic force simulation is ~80 lines of Canvas 2D:
- O(n^2) repulsion (fine for n<1000)
- Barnes-Hut optimization not needed
- requestAnimationFrame loop
- Pan/zoom via canvas transform

---

## 3. The visualization: three panels

```
+-- Memory ---------------------------------------------------+
|  [ Overview* ]  [ Facts ]  [ Soul ]        Refresh          |
|                                                             |
|  +-------- TIMELINE (full width, ~200px height) ----------+ |
|  |  ----[====]------[===]---[======]-------[===]----->    | |
|  |       ^          ^        ^             ^              | |
|  |    "prefers     "lives   "project      "removed       | |
|  |     dark mode"   Berlin"  Tenant"      Composio"      | |
|  |                                                        | |
|  |  [ Jan ]  [ Feb ]  [ Mar ]  [ Apr ]  [ May ]  [ Jun ] | |
|  +--------------------------------------------------------+ |
|                                                             |
|  +-- GRAPH (left 60%) --+  +-- DETAIL (right 40%) ------+ |
|  |                       |  |                            | |
|  |     (Ada)----[owns] |  |  4 live facts              | |
|  |        |        |     |  |  2 contradicted            | |
|  |     [Tenant]  [Mia]   |  |  Last confirmed: 2h ago    | |
|  |        |               |  |                            | |
|  |     [Go]               |  |  [View in Facts tab]      | |
|  |                        |  |                            | |
|  +------------------------+  +----------------------------+ |
|                                                             |
|  +-- STATS BAR --------------------------------------------+ |
|  | 142 facts  |  38 entities  |  12 edges  |  3 conflicts | |
|  +----------------------------------------------------------+ |
+-------------------------------------------------------------+
```

### 3a. Timeline panel (top)

**What it shows:** Every live fact as a horizontal bar spanning its
valid_at...invalid_at range. If valid_at is NULL, the bar starts at first_seen.
If invalid_at is NULL, the bar extends to "now" with a faded right edge.

**Interaction:**
- Zoom: mouse wheel scales the time axis
- Pan: drag the timeline left/right
- Hover a bar: tooltip with fact text, confidence, and "Learned: <date>"
- Click a bar: highlights the related entity in the graph panel below
- Color coding:
  - Green: live fact (high confidence >0.8)
  - Yellow: live fact (medium confidence 0.5-0.8)
  - Red/striped: contradicted/superseded fact (still visible in timeline for
    history)
  - Gray: tombstoned (shown faded, toggle to hide)

**Temporal filter:** A time-range scrubber below the axis. Drag to select a
range — only facts whose valid_at overlaps the range are shown. This is the
"what was true on 2026-03-15?" query visualized.

**Data source:** Existing GET /api/memory/facts with no search query. The
response already includes id, text, confidence. For the timeline we need
valid_at/invalid_at/first_seen — these are Phase 1 columns from
MEMORY-TEMPORAL.md. We need one small backend addition:

**New endpoint (Phase 1.5):**
```
GET /api/memory/facts/temporal
```
Returns the same facts but with the temporal fields included:
```json
{
  "facts": [
    {
      "id": 42,
      "text": "User prefers dark mode in all apps.",
      "confidence": 0.92,
      "valid_at": 1740787200,
      "invalid_at": null,
      "first_seen": 1740787200,
      "invalidated_at": null,
      "status": "live"
    }
  ],
  "stats": {
    "total": 142,
    "live": 138,
    "contradicted": 2,
    "tombstoned": 3,
    "entities": 38,
    "edges": 12
  }
}
```

This is one new handler in memory_rest.go — it reads from the same facts table
with the temporal columns. The stats object is a count query that feeds the
bottom stats bar.

### 3b. Graph panel (bottom-left, 60% width)

**What it shows:** Entities as labeled circles (nodes), edges as labeled lines.
Node size proportional to the number of connected edges. Color-coded by entity
type (person=blue, project=green, org=orange, concept=purple).

**Interaction:**
- Drag nodes to rearrange
- Click a node: highlights all connected edges, populates the detail panel
- Hover an edge: shows the fact text and confidence
- Temporal coupling: when the timeline filter is active, edges whose
  valid_at does not overlap the selected range are hidden. The graph
  re-layouts smoothly.

**Data source (Phase 2):** The entities and edges tables from MEMORY-TEMPORAL
Phase 2. Until Phase 2 is implemented, the graph panel shows a placeholder:
"Entity graph requires Phase 2 of the temporal memory system."

**New endpoints (Phase 2):**
```
GET /api/memory/graph/entities
GET /api/memory/graph/edges?as_of=<unix>
```

For the MVP (Phase 1, no entity graph yet), the graph panel instead shows a
**fact cluster view**: facts are positioned by semantic similarity (using the
existing embeddings via a new endpoint that returns fact embeddings projected to
2D via a simple PCA/t-SNE — or more practically, just laid out by their
temporal proximity on the time axis as a scatter plot).

**Practical Phase 1 graph alternative:** Skip the graph entirely for Phase 1.
Show the timeline full-width and the detail panel below it. The graph panel
appears when Phase 2 lands. This is simpler and still delivers the core value.

### 3c. Detail panel (bottom-right, 40% width)

**What it shows:** Context-sensitive detail for whatever is selected.

**When nothing is selected:** Summary stats (same as the bottom stats bar, but
with a mini bar chart showing facts added per week — computable from first_seen
timestamps).

**When a timeline bar is clicked:** The fact's full text, confidence score,
source episodes (from existing /api/memory/facts/{id}/provenance), valid/invalid
dates, and a link to "View in Facts tab" for curation actions.

**When a graph node is clicked (Phase 2):** The entity's name, type, summary,
and a list of all connected facts (live and historical).

---

## 4. Implementation plan

### Phase 1: Timeline landing page (no new deps except D3 subset)

| Step | Work | File | Est. effort |
|------|------|------|-------------|
| 1 | Vendor d3-scale, d3-axis, d3-zoom, d3-selection, d3-array (minified, ~30KB total) into assets/vendor/ | assets/vendor/d3-*.min.js | 30 min |
| 2 | Create memory-viz.js module (~400 lines) | assets/memory-viz.js | 3 hours |
| 3 | Add GET /api/memory/facts/temporal handler | internal/dashboard/memory_rest.go | 1 hour |
| 4 | Add temporal columns to FactView | internal/dashboard/memory.go | 15 min |
| 5 | Wire "Overview" sub-tab into index.html | assets/index.html | 30 min |
| 6 | Add CSS for timeline + detail panels | assets/styles.css | 1 hour |
| 7 | Test with existing facts (valid_at=NULL for pre-temporal data) | manual | 30 min |

Total: ~7 hours

### Phase 2: Entity graph (when MEMORY-TEMPORAL Phase 2 lands)

| Step | Work | File | Est. effort |
|------|------|------|-------------|
| 1 | Add graph rendering to memory-viz.js (~200 lines Canvas 2D force) | assets/memory-viz.js | 3 hours |
| 2 | Add GET /api/memory/graph/entities and /edges handlers | internal/dashboard/memory_rest.go | 2 hours |
| 3 | Wire graph <-> timeline interaction | assets/memory-viz.js | 2 hours |
| 4 | Temporal filter coupling (graph nodes dim when outside time range) | assets/memory-viz.js | 1 hour |

Total: ~8 hours

---

## 5. File layout

```
internal/dashboard/assets/
  vendor/
    d3-scale.min.js          # ~8KB
    d3-axis.min.js           # ~4KB
    d3-zoom.min.js           # ~6KB
    d3-selection.min.js      # ~5KB
    d3-array.min.js          # ~4KB
    d3-interpolate.min.js    # ~3KB (needed by d3-zoom)
  memory-viz.js              # new: ~400-600 lines
  app.js                     # existing, minor edits to wire Overview tab
  index.html                 # existing, add Overview sub-tab HTML
  styles.css                 # existing, add timeline/graph/detail CSS
```

---

## 6. How it fits the existing Memory view

The existing MEMORY-UI.md defines two sub-tabs: "Facts" and "Soul". This proposal
adds "Overview" as the first (default) sub-tab:

```
[ Overview* ]  [ Facts ]  [ Soul ]
```

- "Overview" is the new temporal visualization (this proposal)
- "Facts" is the existing curatorial list (unchanged)
- "Soul" is the existing soul editor (unchanged)

The Overview tab is the landing page — the first thing the operator sees when
they click "Memory" in the sidebar. It answers "what does the agent know?" at a
glance. The Facts tab is for when they want to curate (search, remove, resolve
conflicts). The Soul tab is for identity editing.

Navigation from Overview to Facts: clicking a fact in the timeline or detail
panel can switch to the Facts tab with that fact highlighted (a simple
app.switchTab('facts', {highlight: factId}) call).

---

## 7. Open questions for Ada

1. **Phase 1 scope**: Ship the timeline-only version first (no graph panel at
   all, just timeline + detail + stats), or wait for Phase 2 entities and ship
   everything together? Recommendation: ship Phase 1 standalone — it delivers
   value immediately and the graph is additive.

2. **Pre-temporal data**: Existing facts have NULL for valid_at/invalid_at. The
   timeline shows these as bars starting at their first_seen and extending to
   "now". Is this the right visual treatment, or should they be visually
   distinguished (e.g., dashed outline) to signal "we don't know when this
   became true"?

3. **Stats bar counts**: The stats object requires counting entities/edges which
   don't exist until Phase 2. For Phase 1, should the stats bar show only fact
   counts, or should it hide entity/edge counts entirely?

4. **D3 vendor approach**: Vendored as individual script tags (simpler, each is
   a global) or as a single concatenated bundle (smaller, one script tag)?
   Recommendation: individual script tags — easier to update/debug, and 30KB
   total is negligible.

5. **Color scheme**: The existing dashboard uses a dark theme (per your
   preference). The timeline and graph should match. Confirming: dark background,
   green/yellow/red for fact status, blue/green/orange/purple for entity types?
