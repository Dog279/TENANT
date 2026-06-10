# Textarea Migration — textinput → bubbles/textarea

**Branch:** `feat/textarea-input`
**Commit:** `f9cb311` — `feat(tui): swap textinput → textarea for multiline chat input`
**Status:** Build clean (`CGO_ENABLED=0 go build ./...`), all tests pass.

## What changed

Single-line `textinput.Model` replaced with multiline `textarea.Model` for the main chat input.

### Files modified

| File | Change |
|------|--------|
| `internal/tui/tui.go` | Core swap: field, init, resize, submit, interject, View |
| `internal/tui/tui_test.go` | Two tests updated to use `m.ta.SetValue()` |

### Architecture

```
m.ta    textarea.Model   ← primary chat input (multiline, 3 rows)
m.input textinput.Model  ← secret/password entry only (/configure, /setup)
```

Input routing logic (3 sites, all consistent):

```go
if m.secretEntry != nil || m.setupEntry != nil || m.configureSession != nil {
    // use m.input (single-line, supports EchoPassword)
} else {
    // use m.ta (multiline textarea)
}
```

### Key bindings

| Key | Action |
|-----|--------|
| Enter | Send message |
| Shift+Enter | Insert newline |
| Ctrl+Enter | Insert newline |
| Esc | Interrupt turn / cancel picker |

### Bug fixes included

1. **Response-on-same-line:** Added `m.streaming = false` at top of `submit()` so each turn gets a fresh assistant message.
2. **Input overflow into feed:** Textarea width constrained to `chatW` (chat pane width) instead of spanning full terminal.

### Layout change

Body height changed from `h - 4` to `h - 6` to accommodate the 3-row textarea.

### Revert

```bash
git checkout main
# or
git revert f9cb311
```

## Design decisions

- **Why keep `m.input`?** The `textinput` widget supports `EchoPassword` for masking secrets during `/configure`. `textarea` has no equivalent. Keeping both avoids a custom masking solution.
- **Why 3 rows?** Gives enough room for short multiline edits without consuming too much vertical space from the chat viewport. `MaxHeight` defaults to 99 so it grows if needed.
- **Why `chatW` for width?** Prevents the textarea from overflowing into the activity feed pane — the exact bug reported with the old `textinput`.
