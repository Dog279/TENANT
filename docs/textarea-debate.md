# Textarea Migration — Agent Debate Record

**Date:** 2026-06-10
**Branch:** `feat/textarea-input`
**Commits under review:** `f9cb311`, `9d1fa3b`
**Participants:** Strategist (senior engineer, skeptical), QA (adversarial), Programmer (pragmatic improvements)

---

## Strategist Review — APPROVE WITH CONCERNS

### 1. Dual Input Models: Necessary Evil?

**Verdict: Defensible today, but the routing is the problem, not the models.**

- `textarea.Model` has no `EchoPassword` equivalent. `textinput.Model` is required for `/configure` secret entry. This is a hard constraint.
- The cost: every `Update()`, `View()`, and keybinding handler now has a routing fork. Three sites check `m.secretEntry != nil || m.setupEntry != nil || m.configureSession != nil`. If someone adds a new masked-input mode and misses one site, silent bugs follow.

**Recommendation:** Extract routing into a single method:

```go
func (m *model) usingMaskedInput() bool {
    return m.secretEntry != nil || m.setupEntry != nil || m.configureSession != nil
}

func (m *model) chatInput() string {
    if m.usingMaskedInput() {
        return strings.TrimSpace(m.input.Value())
    }
    return strings.TrimSpace(m.ta.Value())
}
```

Three copy-pasted conditionals → one method. The next mode (e.g. `/search`) adds zero new sites.

### 2. Static 3-Row Textarea

**Verdict: Wrong default. Start at 1 row, grow dynamically, cap at 8.**

- User typing "ok" wastes 2 rows of chat space permanently.
- User typing a 10-line code block sees only 3 lines and scrolls blind.
- On a 24-row terminal (tmux split), losing 2 rows to a permanently-3-row textarea is an 8% screen tax.
- Every modern chat UI (Slack, Discord, iMessage) starts small and grows with content.

```go
const (
    minInputRows = 1
    maxInputRows = 8
)

func (m model) inputRows() int {
    lines := strings.Count(m.ta.Value(), "\n") + 1
    return clamp(lines, minInputRows, maxInputRows)
}
```

### 3. Dynamic Viewport Sizing

**Verdict: Required, not optional.**

Chat body = `h - 6` hardcoded. This breaks on small terminals and wastes space on large ones. The chat viewport and input area are in a zero-sum game. The PR hardcodes both, which is wrong for every terminal size except the author's.

```go
func (m model) layout(height int) (chatH, inputH int) {
    inputH = m.inputRows()
    chatH = height - inputH - headerRows - borderRows
    if chatH < minChatRows {
        chatH = minChatRows
        inputH = height - chatH - headerRows - borderRows
    }
    return
}
```

### 4. Can We Avoid Dual Models?

**Verdict: No, not without forking `bubbles/textarea`.**

Forking `textarea` to add `EchoPassword` is a larger maintenance burden than two well-managed input models. The PR's flaw isn't "two models" — it's *how* they're routed. Fix the routing, keep the models.

### Required Changes (Strategist)

| # | Issue | Severity | Fix |
|---|-------|----------|-----|
| 1 | Three-site `||` check | **High** — bug factory | Centralize into `usingMaskedInput()` + `chatInput()` |
| 2 | Static 3-row textarea | **Medium** | Dynamic 1–8 rows |
| 3 | Hardcoded `h-6` viewport | **High** | Dynamic `layout()` |
| 4 | Dual models | **None** | Keep, centralize routing |

---

## QA Review — Edge Cases and Bugs

### Issue 1: Textarea scrolls past visible rows (P2)

When content exceeds 3 rows, the textarea scrolls internally. The user sees only the bottom 3 lines. No scrollbar indicator.

**Affected:** All multiline input sessions.
**Fix:** Dynamic height (per Strategist §2) resolves this. Alternatively, show a line count indicator: `"(7 lines, showing 3-7)"`.

### Issue 2: Narrow terminals — textarea underflows (P2)

If terminal width < 40 cols, `chatW` can hit the `chatW < 20` floor. The textarea at 20 cols is usable but cramped. The `m.ta.SetWidth(20)` call may cause the textarea prompt character to overlap with the chat gutter.

**Affected:** `resize()` line `m.ta.SetWidth(chatW)`.
**Fix:** Minimum terminal width check in `resize()`. If `w < 50`, show a "terminal too narrow" message instead of the chat layout.

### Issue 3: Multiline paste during `/configure` (P1)

If the user pastes a multiline string (clipboard contains newlines) while `secretEntry` is active, the paste goes to `m.input` (textinput), which swallows newlines. The first line becomes the secret value; the rest are lost. If the key is multiline (e.g. some PEM formats), the saved secret is truncated.

**Affected:** `submit()` when `m.secretEntry != nil`.
**Fix:** For single-line secrets, document that PEM-style keys should be set via CLI or file path. For multiline-capable secrets, route the paste to `m.ta` even during secret entry and join lines.

### Issue 4: Simultaneous secretEntry + configureSession (P3)

The code checks `secretEntry` before `configureSession` in submit(). In theory, a race could arm both. The `||` chain means the first match wins. If `secretEntry` fires during an active configure session, the session's answer goes to the secret handler.

**Affected:** `submit()` input-source check.
**Fix:** Mutual exclusion: arming `secretEntry` should clear `configureSession` and vice versa. Add a guard:

```go
func (m *model) armSecretEntry(se *secretEntryState) {
    m.configureSession = nil
    m.setupEntry = nil
    m.secretEntry = se
    m.input.EchoMode = textinput.EchoPassword
}
```

### Issue 5: Shift+Enter terminal compatibility (P2)

`Shift+Enter` key sequence varies by terminal:
- **iTerm2:** Sends `\x1b[13;2u` (modified CSI sequence) — Bubble Tea may not parse this as "shift+enter" on all versions.
- **xterm:** Same CSI sequence, widely supported.
- **Windows Terminal:** Sends `Enter` with shift modifier — Bubble Tea on Windows maps this correctly.
- **tmux:** May strip the modifier and send plain `Enter`. This is a known tmux limitation with modified keys.

**Affected:** Users in tmux sessions cannot insert newlines via Shift+Enter. They'd need Ctrl+Enter or Alt+Enter.
**Fix:** Document Ctrl+Enter as the tmux-compatible alternative. Consider also binding `ctrl+j` (literal newline) as an alias.

### Issue 6: Textarea state leaks between modes (P2)

When switching from normal input → secret entry → back to normal, `m.ta` retains its content from before the switch. If the user was mid-sentence when `/configure` interrupted, the half-typed text is still in the textarea after the secret is saved.

**Affected:** `clearSecretEntry()`, `saveSecretEntry()`.
**Fix:** `m.ta.Reset()` in `clearSecretEntry()` and after secret save. Or: accept this behavior as "draft preservation" — the user's half-typed message survives the `/configure` detour, which is arguably a feature.

### Issue 7: Message arrival mid-edit (P3)

`sysChatMsg` and `applyEvent()` mutate `m.msgs` and call `refresh()` from `Update()`. The textarea's content is not affected by message arrivals — it's a separate model. No race condition here because Bubble Tea is single-threaded (all Updates are serial).

**Affected:** None — this is a non-issue in Bubble Tea's architecture.

### QA Summary

| # | Issue | Severity | Recommendation |
|---|-------|----------|----------------|
| 1 | Textarea scrolls past visible rows | P2 | Dynamic height fixes it |
| 2 | Narrow terminals (< 40 cols) | P2 | Min width guard |
| 3 | Multiline paste during /configure | P1 | Document limitation or route to textarea |
| 4 | Simultaneous secretEntry + configureSession | P3 | Mutual exclusion guard |
| 5 | Shift+Enter in tmux | P2 | Document Ctrl+Enter alternative |
| 6 | Textarea state persists across mode switches | P2 | Accept as draft preservation or reset |
| 7 | Message arrival mid-edit | P3 | Non-issue (BT serial) |

---

## Programmer Review — 3 Concrete Improvements

### Improvement 1: `chatInput()` helper (~15 lines)

Eliminates the 3-way copy-paste. Every call site becomes `q := m.chatInput()`.

```go
func (m *model) usingMaskedInput() bool {
    return m.secretEntry != nil || m.setupEntry != nil || m.configureSession != nil
}

func (m *model) chatInput() string {
    if m.usingMaskedInput() {
        return strings.TrimSpace(m.input.Value())
    }
    return strings.TrimSpace(m.ta.Value())
}
```

All three sites (Update enter handler, submit(), View) simplified. Risk: zero — pure refactor.

### Improvement 2: Dynamic textarea height (~50 lines)

Grow textarea based on content, shrink chat viewport accordingly.

```go
const (
    taMinHeight = 3
    taMaxHeight = 12
    taReserved  = 3 // status bar + help + padding
)

func (m *model) computeLayout() (chatW, feedW, bodyH, taH int) {
    w, h := m.width, m.height
    feedW = w / 3
    if feedW < 24 { feedW = 24 }
    chatW = w - feedW - 3 - chatGutter
    if chatW < 20 { chatW = 20 }

    lineCount := strings.Count(m.ta.Value(), "\n") + 1
    val := m.ta.Value()
    if val != "" && chatW > 0 {
        wrapped := 0
        for _, line := range strings.Split(val, "\n") {
            lineWidth := utf8.RuneCountInString(line)
            if lineWidth == 0 { wrapped++ } else {
                wrapped += (lineWidth + chatW - 1) / chatW
            }
        }
        lineCount = wrapped
    }

    taH = clamp(lineCount, taMinHeight, taMaxHeight)
    bodyH = h - taH - taReserved
    if bodyH < 3 { bodyH = 3 }
    return
}
```

Call `computeLayout()` in `resize()` (initial) and `refresh()` (every frame). Textarea grows as user types multiline content, chat viewport shrinks to compensate.

### Improvement 3: Focus restore + cursor styling (~20 lines)

After `/configure` saves a secret, textarea loses focus. User's next keystroke vanishes.

```go
func (m *model) clearSecretEntry() {
    m.secretEntry = nil
    m.input.EchoMode = textinput.EchoNormal
    m.input.Reset()
    m.ta.Focus() // restore focus to textarea
}
```

Cursor styling in `newModel()`:
```go
ta.Cursor.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
```

Busy-state dim in `View()`:
```go
if m.busy {
    inputArea = cDim.Render(inputArea)
}
```

---

## Consensus

All three agents agree on:

1. **Centralize input routing** — the 3-site `||` check is the highest-risk pattern. Extract `usingMaskedInput()` and `chatInput()`.
2. **Dynamic textarea height** — static 3 rows is wrong for both short and long inputs. Start at 1–3 rows, grow with content, cap at 8–12.
3. **Keep dual models** — forking `textarea` for `EchoPassword` isn't worth it. The two-model approach is correct; the routing between them needs cleanup.
4. **Focus management** — every transition out of masked mode must restore textarea focus.

Open disagreement:
- **Strategist** says dynamic viewport sizing is *required* in this PR.
- **Programmer** says it's low-risk and can ship as a follow-up.
- **QA** says the tmux Shift+Enter limitation needs documentation at minimum.

---

## Verdict: DONE WITH CONCERNS

The textarea swap ships value (multiline input works, tests pass, docs written). The routing centralization and dynamic sizing are blocking concerns before merge to `main`.
