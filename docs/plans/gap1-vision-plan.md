# Vision: the model sees images (TEN-493, GAP-1)

Status: implementation plan, 2026-10-09 (rev 4: three GLM adversarial reviews folded in, final score 7/10; see the review log). Workstream TEN-493, child TEN-277. Source: the
2026-10-02 competitive review (`Tracking/competitive-reviews/2026-10-02/`), where browser and computer use scored
**3.3** against 9 and 8, the lowest category.

**Goal:** an image reaches the model without anyone pointing it at a file. You paste a screenshot into the TUI
(Ctrl+V) or drag an image file in, and the model sees it. A web screenshot or an image file the agent opens is
shown to the model. A photo sent over Discord or iMessage is seen. A model that can't see images gets a one-line
note with the saved path instead of an error. Re-score target **≥7**.

Delivered as **one PR** on `feat/gap1-vision`; the parts below are its commits.

## Decisions (operator, 2026-10-09)

| # | Decision | Effect on this plan |
|---|---|---|
| D1 | Paste with **Ctrl+V** (reads the OS clipboard image); dragging an image file in attaches it | Clipboard read through each OS's own CLI; a pasted path that names an image file becomes an attachment |
| D2 | The input shows removable `[image N: name W×H]` placeholders | Deleting the placeholder text drops that image; the text is sent as typed |
| D3 | **Probe each model at launch** and on `/model use` for vision | A background probe per (endpoint, model) with a red square; it passes only if the answer says "red" |
| D4 | An image stays visible for the **last 3 exchanges** | Older images become a one-line note with the saved path (the model can reopen it with `os_read_image`) |
| D5 | **Both tools attach images** when the model can see | `web_screenshot` and `os_read_image` hand their image to the model; their text result is unchanged |
| D6 | **HEIC**: convert with macOS `sips`; on Linux and Windows a text note | No new dependency; iPhone photos work on the Mac host, where iMessage's chat.db mode runs anyway |
| D7 | One PR for the whole of GAP-1 | Commits A to G below, one branch, one CI run when it's ready |

## Ground rules

- **No new Go modules.** Stdlib `image`, `image/png`, `image/jpeg`, `image/gif`, `encoding/base64`,
  `net/http` (`DetectContentType`). Clipboard and HEIC go through OS CLIs (`osascript`, `sips`, `wl-paste`,
  `xclip`, `powershell`) behind one swappable `runCmd` seam, as `cmd/tenant/service.go` does.
- **Live, no relaunch.** The probe reruns on `/model use`. `/model vision <provider> on|off|auto` changes it live
  and saves it.
- **Generalized for any host.** Every OS gets a working path or a clear message naming what to install.
- **Files, not bytes, in memory.** Images are saved once under `<data>/attachments/` and referenced by path. The
  working set, archive and episodes carry the reference, and backends read and inline the file when they send it.
  Nothing base64-encodes an image into a JSONL line or the working set.

## Verified current state (main 3794a03, 2026-10-09)

The live endpoint already sees images: `glm-5.3-flash` on the operator's vLLM server answered a 64×64 red
`image_url` data URI with "Red" in 1.2s. It also handled 3 and 6 images in one request, and an image inside a
`role: tool` message. Nothing in Tenant can send one:

| Stage | Where | Today |
|---|---|---|
| TUI input | `internal/tui/tui.go:1496` key switch, `:1841` falls through to the textarea | `ctrl+v` is unbound, so bubbles' textarea pastes clipboard **text** via `atotto/clipboard` (text only); `KeyMsg.Paste` is never inspected |
| Turn request | `internal/agent/agent.go:514` `TurnRequest{UserQuery string, …}` | No attachments. `:605` rejects an empty `UserQuery` |
| Working set | `internal/memory/working/working.go:26` | `Content string` only (has `Meta map[string]any`) |
| Assembler | `internal/memory/assemble/assemble.go:866` `buildMessages`, loop `:876-883` | Copies Role, Content, ToolCalls, ToolCallID only. Token counts (`:608`, `:642`) see text only |
| Model message | `internal/model/llm.go:85` `Message{Role, Content, ToolCalls, ToolCallID, Name}` | Text only |
| vLLM backend | `internal/model/backend/vllm/vllm.go:116` `wireMsg{Content string}`, `:160` `toWireMessages` | String content; no `image_url` parts |
| Anthropic backend | `internal/model/backend/anthropic/anthropic.go:84` `block`, `:218` `toWireMessages` | `tool_result` content is a string; no `image` block |
| Claude Code backend | `internal/model/backend/claudecode/claudecode.go:238` `buildPrompt` | Flattens to a text transcript on stdin |
| Archive | `internal/memory/archive/archive.go:42` `Event{…, Metadata map[string]any}` | Text; `Metadata` is free for attachment refs |
| Tool results | `internal/agent/tools.go:115` `Dispatch(...) (string, bool, error)` | Text only. The `ReportFlow` context side channel (`internal/agent/infoflow.go:319-355`) is the precedent for a per-call attachment |
| `os_read_image` | `internal/plugins/osys/dispatch.go:111`, `osys.go:407` `ReadImage` | Returns `format=… width=… height=…` text, no pixels |
| `web_screenshot` | `internal/plugins/web/dispatch.go:413`, `web.go:686` | Saves `<data>/screenshots/web-<ns>.png`; `chromedp.FullScreenshot(&buf, 80)` writes **JPEG** bytes under the `.png` name |
| Discord inbound | `cmd/tenant/discordrelay.go:307` `ingestAndRoute`, `discordingest.go:38,47,62` | Downloads to `<data>/relay-inbox/`, tells the model only a text manifest; allowlist lacks gif/webp |
| iMessage inbound | `internal/plugins/imessage/chatdb.go:315` `MessagesSince`, `client.go:127` BlueBubbles query, `cmd/tenant/imessagerelay.go:147` | No attachment join or download; attachment-only messages are dropped |
| Model profile | `internal/model/profile.go:22`, `Capabilities` `:82` | No vision flag anywhere (grep for vision/multimodal/image_url: none) |
| Provider config | `internal/config/config.go:327` `Provider{Kind, Endpoint, Model, ToolFmt, EmbedDim, Auth, Reasoning}` | No vision field |

## Design

### The image type (commit A)

`internal/model/llm.go`:

```go
// Image is one picture attached to a message: a normalized copy saved under
// <data>/attachments, read and inlined by a backend only when it sends it.
type Image struct {
	Path   string `json:"path"`
	MIME   string `json:"mime"`           // image/png | image/jpeg | image/gif | image/webp
	Width  int    `json:"w,omitempty"`    // 0 when unknown (webp)
	Height int    `json:"h,omitempty"`
	Name   string `json:"name,omitempty"` // what the operator or tool called it
	// Labels are the info-flow labels of where the image came from (a tool's
	// ResultLabels, a relay's inbound flow), so egress checks see them like text.
	Labels []InfoFlowLabel `json:"labels,omitempty"`
}

type Message struct {
	// existing fields unchanged
	Images []Image `json:"images,omitempty"`
}
```

`working.Message` gains `Images []model.Image`. `archive.Event` records them as
`Metadata["images"] = []{path, mime, w, h, name, labels}`; no new archive field and no schema version bump.

### Saving and normalizing (commit A): `internal/attach`

One small package, the only place image bytes are handled:

```go
type Store struct{ dir string } // <data>/attachments
func NewStore(dataDir string) *Store
// Add saves a normalized copy of src and returns its reference. name is display-only.
// Callers pass bytes they already read safely (a vetted handle, a download, the
// clipboard), so the store never re-opens a caller's path.
func (s *Store) Add(ctx context.Context, src []byte, name string) (model.Image, error)
// DataURI reads img.Path and returns "data:<mime>;base64,…" (backends call it at send time).
func DataURI(img model.Image) (string, error)
var ErrNotImage, ErrUnsupported, ErrTooLarge error
```

Rules, in order:

1. **Sniff** with `http.DetectContentType` on the first 512 bytes, never the extension. Accept png, jpeg, gif and
   webp. HEIC is recognized by an `ftyp` box whose major or compatible brands include `heic` or `heix` (`mif1`
   alone is any HEIF container and is `ErrUnsupported`).
2. **Size cap:** a source over 20 MB is `ErrTooLarge` before anything else.
3. **Pixel budget (decompression bombs):** `image.DecodeConfig` first; more than **40 megapixels** is
   `ErrTooLarge` before any full decode. `gif.Decode` already stops at the first frame.
4. **HEIC and webp on macOS:** `sips -s format jpeg <in> --out <tmp>.jpg`, then continue with the JPEG. HEIC is
   certain; webp support in `sips` depends on the macOS version (verified during implementation; a `sips` failure on
   webp falls back to the pass-through below). HEIC elsewhere is `ErrUnsupported` ("HEIC can't be shown on this host"). webp elsewhere is passed through
   untouched when it's at most 5 MB (size unknown, counted at the token cap), otherwise `ErrTooLarge`.
5. **Downscale** png/jpeg/gif whose long edge is over **1568 px** or whose area is over **1.15 megapixels**
   (Anthropic's own resize targets, which keep an image near 1600 tokens) with a stdlib area-average scaler (about
   40 lines in the package, no `x/image`). Smaller images are kept byte-for-byte (a small animated GIF stays
   animated; the model sees what the backend shows of it).
6. **Re-encode** only what was scaled: PNG stays PNG when the result is at most 1 MB, otherwise JPEG at quality 85,
   with any alpha flattened onto white first. JPEG stays JPEG at quality 85. A scaled GIF becomes a single-frame
   GIF. The result must be at most **5 MB** (Anthropic's per-image limit), otherwise `ErrTooLarge`.
7. **Save** as `<data>/attachments/YYYY-MM/<sha256[:16]>.<png|jpg|gif|webp>` with `fsperm.WriteFile` (0600 in a
   0700 dir). The same bytes saved twice reuse one file.

`attachments` joins the privacy file classes (`internal/privacy/files.go:14`, `privacy.go:30`,
`cmd/tenant/privacyctl.go:198`) with the **archive's** retention default, since archive lines and notes point at
these files, and the permission check list in `cmd/tenant/doctor.go:433`.

### Through the agent (commit B)

- `TurnRequest` gains `Images []model.Image`. `Turn` accepts an empty `UserQuery` when `Images` isn't empty
  (`agent.go:605`), using `"(image)"` as the text for the archive and episode.
- The user message appended at `agent.go:719` carries `Images`. The archive event records them in `Metadata`.
  This is the path the images take to the model: the agent appends the user message to the working set **before**
  `Assemble` and deliberately doesn't pass `UserQuery` (`agent.go:985`), so `buildActiveBlock` carries only
  retrieved memory and the images ride the working-set copy. No `assemble.Request` field is needed.
- `InterjectWithImages(msg string, images []model.Image)` is new; `Interject(msg string)` stays exactly as it is
  and calls it with no images. Changing `Interject`'s signature would break the two interfaces that declare it
  (`relayRunner`, `cmd/tenant/discordrelay.go:43`; `turnGate`, `cmd/tenant/turngate.go:19`) and their test fakes.
  `turnGate` gains a forwarding `InterjectWithImages`; callers reach it through a type assertion on
  `interface{ InterjectWithImages(string, []model.Image) }`, falling back to text-only `Interject`.
- **Tools** attach through a context side channel, like `ReportFlow`, so `ToolDispatcher` keeps its signature and
  plugins don't import `internal/agent`:

  ```go
  // internal/model
  func WithImageSink(ctx context.Context, sink func(data []byte, name string)) context.Context
  func AttachImage(ctx context.Context, data []byte, name string) // no-op without a sink
  ```

  The tool passes the **bytes it already read** through its vetted handle, never a path, so nothing re-opens a file
  that could have been swapped for a symlink in between. `dispatchBatch` installs a sink per call, normalizes the
  bytes through the agent's `attach.Store`, sets each image's `Labels` to the tool spec's `ResultLabels` (the same
  labels the text result gets, `agent.go` dispatch loop), and puts them on `ToolCallResult.Images`. `os_read_image`
  gains the `ResultLabels` `os_read_file` already has (sensitive, local file, `osys/dispatch.go:110`). The egress gate
  (`internal/agent/infoflow.go`) matches recorded **text** against outbound tool arguments, and pixels give it no
  text, so for each labelled image the sink also records the image's saved path and name as tracked fragments
  (`a.infoFlow.record(labels, path+"\n"+name)`). An outbound call that names the file is then caught like one
  quoting a sensitive file. What the model transcribes from the pixels is not trackable, which is the same limit as
  any model paraphrasing text, and is recorded as a known gap. `web_screenshot` stays unlabelled, like every web
  tool's text.
  `feedToolResult` (`agent.go:1820`) stores them on the tool message.
- Microcompaction (`internal/memory/compress/microcompact.go`) never elides a message with non-empty `Images`; the
  assembler's window (below) already turns old images into notes, and tool messages that carry images have short
  text.
- Full compaction (`compress/archive_compact.go`) summarizes old turns into text. Before summarizing, each image on
  a summarized message is rendered as its `[image: <name> <W×H>, saved at <path>]` note, and those note lines are
  pinned verbatim into the summary, the way artifact URIs already are (`archive_compact.go:379`,
  `assemble.ExtractArtifactURIs`). The path survives compaction, so the model can still reopen the image.
- `turnDedupeKey` (`cmd/tenant/turngate.go:356`) folds in the images' paths, so two image-only requests with the
  same text don't coalesce into one turn.
- Episodes keep text only. The prompt gets `" [N images]"` appended, so recall shows that images were there.

### What the model is sent (commit B)

In `assemble.buildMessages` (`assemble.go:866`), after the working-set copy:

1. **Window (D4):** images stay on messages in the last **3 exchanges**. An exchange starts at a user message, so
   that user message, its tool results and its assistant reply count together. Older images become a note
   appended to that message's text:
   `[image: <name> <W×H>, saved at <path>, no longer shown]`. The note is a pure function of the image, so once an
   image has aged out its message stays byte-stable. Each image that ages out costs **one** prefix-cache miss at
   that point, and nothing after; text-only conversations are untouched (TEN-505).
2. **Cap:** at most **8 images per request**, newest first; the rest become the same note.
3. **Vision gate:** only when the assembled request carries images, the assembler reads the vision cache (no probe,
   no wait). When the profile can't see, or the answer is still unknown, every image becomes
   `[image: <name> <W×H>, saved at <path>; the current model can't see images]`. Requests without images never
   touch the gate.
4. **Tokens:** each image counts as `min(ceil(W×H/750), 1600)` tokens, or 1600 when the size is unknown, in
   `countWorking`, `truncateWorking` and `estimateAssembledContextUpper` (`agent.go:2143`). The current message's
   images are in the working set, so they are counted there.

### The backends (commit C)

| Backend | User/assistant images | Tool-result images | Can't see |
|---|---|---|---|
| `vllm` (OpenAI-compatible) | `content` becomes an array: `{"type":"text"}` plus `{"type":"image_url","image_url":{"url":"data:…"}}` per image | A **user message** right after the tool results: `{"type":"text","text":"[images returned by <tool>]"}` plus the images. Portable: OpenAI and llama.cpp reject images in `role: tool`, even though this GLM server takes them | Strips to notes itself (see Fallbacks) |
| `anthropic` | `{"type":"image","source":{"type":"base64","media_type":…,"data":…}}` blocks | Inside the `tool_result` block, whose `content` becomes a block array (text plus image) | Defaults to seeing (every current Claude model does); `vision: off` for a gateway that proxies a model that can't |
| `claudecode` | Note with the path: Claude Code's own Read tool can open image files, so it can still look | Same | Always the note |
| `echo` | Counts images in its reply ("echo: …, 2 images") for tests | Same | n/a |

The vllm `wireMsg` keeps `Content string` and gains `Parts []contentPart` (`json:"-"`), with a custom
`MarshalJSON`: when `Parts` is empty it encodes exactly what today's struct encodes (`content` omitted when empty,
which every assistant tool-call replay relies on); when it isn't, `content` is the parts array. A golden test pins a
text-only request that includes an assistant tool-call message with empty content. `DataURI` reads the file at send
time; a file that has vanished (privacy retention) sends the note with "(file since removed)" instead.

**Fallbacks:** `FallbackLLM` (`internal/model/fallback.go:91`) can send a request assembled for a vision model to a
link that can't see. So each backend also checks `model.CanSee(profile)` on the request it actually sends, and
strips images to notes when it's false (`model.StripImages(msgs, reason)`). The assembler and the backends use the
same helper.

### Knowing which model can see (commit D)

- `config.Provider` gains `Vision string \`json:"vision,omitempty"\``: `""`/`"auto"` (default), `"on"`, `"off"`.
  It is copied to `model.Profile.Vision` in `vllmGenProfiles`, `anthropicGenProfiles`, `claudecodeGenProfiles`
  (`cmd/tenant/wiring.go:1050, 836, 991`) and the fallback, planner-big and helper profiles. `keys.golden` gains the
  key.
- `model.CanSee(p Profile) bool`: `"on"` → true, `"off"` → false. Otherwise, by backend:
  - `anthropic` → true;
  - `claudecode` and `echo` → false;
  - `vllm` → the vision registry for `(endpoint, chat path, model)`. Nothing ever waits on it: an unknown answer
    strips the images for this request, and the backend starts a background probe if none is running for that model
    (one at a time per model). An inconclusive probe is retried in the background after 5 minutes.
- **Where the answer lives:** a small registry in `internal/model` (`model.SetVision(key, Vision)`,
  `model.VisionOf(key) Vision` with `Unknown`/`Yes`/`No`, keyed by endpoint, chat path and model). The vllm probe
  writes it; `CanSee` (in `internal/model`) and the assembler read it. No package imports a backend, so there is no
  import cycle and no layering break.
- **The probe (D3)** lives in the vllm backend: `func (b *Backend) ProbeVision(ctx context.Context) (Vision, error)`,
  a method on the backend built from the profile, so it uses the same chat path (`vllm.go:463` `chatPath()`, which
  honours `Profile.ChatPath` for Z.ai-style URLs), auth headers, HTTP client and `ForceHTTP1` as real requests.
  - It sends one 64×32 PNG: two squares side by side, colours drawn at random (and different) from red, green,
    blue and yellow, with "Name the two colours from left to right, one word each." and `max_tokens` 4096, so a
    thinking model can finish reasoning.
  - Pass: HTTP 200 and the reply, lowercased, names both colours, the left one first. A blind model guessing passes
    one time in twelve at best. **No:** HTTP 400 or 422 whose error is about image input (the server refuses images),
    or a reply that names two colours from the set that aren't the right pair. A 400 about anything else (for
    example `max_tokens` over the server's limit) is inconclusive. **Inconclusive** (not cached, retried after 5 minutes): network
    errors, timeouts, 401, 403, 404, 429, 5xx, and a 200 whose reply names fewer than two colours (empty, or cut off
    mid-reasoning).
  - It runs in the background at launch for every **self-hosted** (keyless) vllm-backend generation profile,
    deduplicated by endpoint and model, and again on `/model use`, `/model reload` and fallback changes. A hosted,
    keyed provider (billed per request) is probed only when an image is first sent to it, or at launch when its
    `vision` is set to `auto` explicitly; `on`/`off` skip the probe entirely.
  - Results are cached in the process.
  - A `model` catalog event logs each answer: "Model {model} at {endpoint} can see images: {yes|no}".
- `/model` lists `vision ✓` / `vision ✗` / `vision ?` per provider. `/model vision <provider> on|off|auto` sets it
  live and saves it. The dashboard's Model page shows the same mark (read-only).

### TUI paste (commit E)

Placeholders and the image list live on the TUI model: `attachments []model.Image`.

- **Ctrl+V** first tries the clipboard image; on **Windows** so does **Alt+V**, because Windows Terminal keeps
  Ctrl+V for its own paste (Alt+V isn't bound on macOS, where Option+V types "√"). The read runs as a `tea.Cmd`
  (the key handler returns it; the placeholder arrives as a message), so the UI never blocks on the 50 to 300 ms
  CLI call. Through one `var runClipboardCmd` seam, by OS:
  - **macOS:** an `osascript` in a `try` block: `the clipboard as «class PNGf»`, else `«class TIFF»`, written with
    `open for access … with write permission` / `write … to f` to a temp file; it prints `png`, `tiff` or `none`.
    TIFF (what Preview and some apps put on the clipboard) is converted with `sips -s format png`. If raw `write`
    proves unreliable on a macOS version, the fallback is parsing the `«data PNGf<hex>»` text form and
    hex-decoding it (verified during implementation).
  - **Linux, Wayland** (`$WAYLAND_DISPLAY` set): `wl-paste --list-types`; if it includes `image/png`, then
    `wl-paste --type image/png`.
  - **Linux, X11:** `xclip -selection clipboard -t TARGETS -o`; if it includes `image/png`, then
    `xclip -selection clipboard -t image/png -o`.
  - **Neither tool installed:** a one-time system line, "Install wl-clipboard (Wayland) or xclip (X11) to paste
    images."
  - **Windows:** `powershell -NoProfile -STA -Command "Add-Type -AssemblyName System.Windows.Forms; Add-Type
    -AssemblyName System.Drawing; $i = [System.Windows.Forms.Clipboard]::GetImage(); if ($i) {
    $i.Save('<tmp>.png', [System.Drawing.Imaging.ImageFormat]::Png); 'ok' } else { 'none' }"`.

  The mechanism: the key switch's `ctrl+v` case returns before `m.ta.Update`, with a `tea.Cmd` that reads the
  clipboard image. Found: the same Cmd also runs `attach.Store.Add` (decoding and scaling a large image can take a
  second, so it never runs in `Update`) and returns a `clipboardImageMsg` carrying the finished `model.Image`; its
  handler only inserts `[image N: <name> <W×H>]` at the cursor. `/image` and dragged paths read and normalize in a
  `tea.Cmd` the same way. None: the Cmd returns `textarea.Paste()`, bubbles' exported paste command
  (`bubbles@v1.0.0/textarea/textarea.go:1390`, `func Paste() tea.Msg`), whose unexported paste message reaches
  `m.ta.Update` through the default path. That is exactly what Ctrl+V does today (`textarea.go:89`, `:1043`), and
  since it is a paste message, not a keypress, it can't re-enter the `ctrl+v` case. Placeholder text
  doesn't count against the textarea's 8000-character limit: the limit is raised by each inserted placeholder's
  length.
- **Dragged files:** a `tea.KeyMsg` with `Paste: true` whose whole text is one or more image file paths becomes
  attachments plus placeholders instead of the path text.
  - Paths are separated by whitespace or newlines, can be quoted, can use backslash-escaped spaces (macOS
    terminals), or can be `file://` URLs.
  - A path counts when it exists and sniffs as an image.
  - A paste mixing text with paths stays text.
  - Each path's file is read by the TUI and passed to `attach.Store.Add` (the operator's own files; no tool roots).
  - Works wherever the terminal inserts a dropped file's path: macOS Terminal, iTerm2, Ghostty, most Linux
    terminals, and Windows Terminal (to be confirmed in the TEN-504 Windows session).
- **`/image <path>`** attaches a file by path: the fallback for terminals with no drag-and-drop. It reads the file
  itself (the operator's own path, no tool roots involved).
- **On Enter:** the images whose placeholder text is still in the input go with the message, in placeholder order.
  Placeholders stay in the sent text so the model can match "image 2" to what it sees. A message that is only a
  placeholder is still non-empty text, so both Enter branches (`tui.go:1576` busy, `:1582` idle) send it unchanged;
  while a turn runs, Enter interjects with the images (`Interject`).
- After the turn, images that weren't sent (placeholder deleted) are dropped from the list. History recall (up)
  brings back text only.
- The status line shows `📎 N` while images are attached.
- The image text placeholder is plain text in the textarea; no rendering of pixels in the terminal.

### Tools (commit F)

- `os_read_image`: `ReadImage` (`osys.go:407`) also returns the bytes it read through the vetted handle, and the
  dispatcher calls `model.AttachImage(ctx, data, base)`. A webp (which `image.DecodeConfig` can't read) is sniffed
  and reported as `format=webp` with unknown size instead of an error. Text result unchanged. Description gains: "The image
  itself is shown to models that can see images."
- `web_screenshot`: sniff the bytes, save as `.jpg` when JPEG (fixes the JPEG-in-`.png` naming). For the model,
  a full-page capture taller than twice its width is **cropped to the top** (height = 2 × width) before the
  1568 px downscale, so the text stays readable instead of a 1568-tall sliver. Then
  `model.AttachImage(ctx, croppedBytes, "screenshot of <host> (top of page)")`. The text result keeps the full
  capture's path, so the model can open it to see the rest.

### Discord and iMessage (commit G, TEN-277)

- **Discord:**
  - `discordingest.go:38` allows `gif` and `webp` too.
  - In `ingestAndRoute` (`discordrelay.go:307`), each saved file that sniffs as an image also goes through
    `attach.Store.Add` (the bytes just downloaded) into `TurnRequest.Images` (operator messages only, as today),
    labelled with the relay's inbound flow. The text manifest stays.
  - When a turn is already running, `routeTurn` (`discordrelay.go:348`) interjects; it passes the images through
    `InterjectWithImages` when the runner (or the `turnGate` around it) has it.
  - When `Add` fails (too large, HEIC on Linux), the turn still runs with the text manifest plus a note naming the
    file and the reason.
- **iMessage (chat.db, macOS):**
  - `MessagesSince` (`chatdb.go:315`) joins `message_attachment_join` and `attachment` for `filename`, `mime_type`
    and `transfer_name`.
  - `~` expands to the home dir. `InboundMessage` gains `Attachments []string` (paths).
  - Reading them uses the Full Disk Access the iMessage mode already requires. The files are read through the
    same symlink-refusing open the osys plugin uses.
  - `mime_type` is often NULL, so the bytes are sniffed. An attachment whose file isn't on disk yet (still in
    iCloud) gets a per-attachment note ("photo not downloaded to this Mac yet") instead of failing the message.
  - The watcher (`internal/plugins/imessage/watch.go:135`) deduplicates on `(chat, text)`. A second photo-only
    message in the same chat has the same empty text and would be dropped, so the dedup key adds the attachment
    GUIDs.
- **iMessage (BlueBubbles):**
  - The query adds `"attachment"` to `with` (`client.go:127`), and `bbMessage` gains `attachments`
    (`guid`, `mimeType`, `transferName`).
  - Images are downloaded from `/api/v1/attachment/<url.PathEscape(guid)>/download` with the existing password,
    into `<data>/relay-inbox/`.
  - Downloads use the same 100 MB, 10-per-message and 90 s caps as Discord.
- `imessagerelay.go:147` stops dropping attachment-only messages when at least one attachment is an image. Image
  attachments go to `TurnRequest.Images`. The allowlist and per-message gate are unchanged.
- HEIC follows D6 through `attach.Store`. Relay images carry the relay's inbound flow labels, like the message
  text.

### Docs and eval (commit H)

- `docs/QUICKSTART.md`: a "Pasting images" section (Ctrl+V; Alt+V on Windows; drag; `/image`; the Linux tools).
- `docs/orchestration.md` or the model doc: the `vision` key and the probe.
- `CHANGELOG.md`.
- `docs/EVENTS.md` regenerated for the new events.
- **Eval:** a `vision` task (not in the `smoke` subset, so CI's echo baseline is unaffected).
  - Its fixture is generated in the test from stdlib `image/draw` rectangles: the number **4817** in
    seven-segment digits on white, 400×120.
  - The task asks "What number is shown in the image?" and passes on `4817`.
  - It runs with `tenant eval --subset vision` against a real model.

## Acceptance criteria

1. On macOS, with `glm-5.3-flash` as the active provider, copying a screenshot and pressing Ctrl+V in the TUI
   inserts `[image 1: … W×H]`. Sending "What number is in the top right?" gets the number shown in the screenshot.
2. Dragging a `.png` and a `.jpg` from Finder into the TUI inserts two placeholders and no path text. Pasting a
   path that isn't an image inserts the path text unchanged.
3. Ctrl+V with text (no image) on the clipboard pastes the text exactly as today, through bubbles' own
   `textarea.Paste` (a test with the fake clipboard seam asserts it).
4. An image-only message (no text) is sent and answered.
5. `tenant eval --subset vision` against `glm-5.3-flash` passes the 4817 task. Against a vision-off profile, the
   model gets the note and the task fails gracefully, with no error event.
6. With a text-only model active, the probe logs "can see images: no" within 10 s of launch, and an image message
   gets the note `the current model can't see images` instead of an HTTP error. A probe that gets HTTP 429 or a
   timeout logs "inconclusive" and caches nothing.
7. The 4th exchange after an image no longer sends its pixels. The request carries the `no longer shown` note with
   the saved path instead, and the 5th exchange's request is byte-identical to the 4th's up to the 4th's last
   message (one cache miss per image aging out, no more).
8. During a browsing turn, `web_screenshot` puts the screenshot in front of the model: the next request carries an
   `image_url` part, and the screenshot file has a `.jpg` name when its bytes are JPEG. On a page 10 000 px tall
   the attached image is the top of the page at most 2:1 tall, and GLM reads a heading from it.
9. A Discord DM from the operator with a photo, and an iMessage from an allowlisted sender with only a photo, each
   produce a turn whose request carries the image. A HEIC photo on macOS arrives as JPEG.
10. A vllm request without images is byte-identical to today's (golden test on the request JSON).
11. No new module in `go.mod`; `go mod tidy -diff` is clean.
12. A 20 MB PNG that declares 100 000 × 100 000 pixels is refused with `ErrTooLarge` without decoding (peak memory
    stays under 100 MB in the test).
13. `go test -race ./...`, vet, docfacts, the security workflow and the smoke eval all pass. CI passes on all three
    OSes.

## Testing plan

| Layer | What | Count |
|---|---|---|
| Unit, `internal/attach` | Sniffing (png/jpeg/gif/webp/HEIC brands/`mif1`-only/not-image); 40 MP budget refuses a bomb before decode; downscale of a 4000×3000 to 1568 long edge; small GIF kept byte-for-byte; PNG with alpha → JPEG flattened on white; 5 MB cap; dedup by hash; `DataURI`; HEIC on non-macOS → `ErrUnsupported` | +12 |
| Unit, `internal/model` | `CanSee` for each backend and override; `StripImages` note text; image token estimate | +4 |
| Unit, vllm backend | Wire JSON for text-only including an empty-content assistant tool call (golden, unchanged), user images, tool images as a follow-up user message; probe at a custom `ChatPath`; pass on both colours in order; no on wrong colours and on 400; inconclusive on 404, 429, timeout and an empty 200; one probe at a time per model (httptest) | +10 |
| Unit, anthropic backend | Image blocks in user turns and inside `tool_result` | +2 |
| Unit, assembler | 3-exchange window; 8-image cap; vision-off notes; token counts include images | +4 |
| Unit, agent | Image-only turn; `InterjectWithImages`; tool `AttachImage` bytes → tool message `Images` with the tool's labels (`os_read_image` sensitive); microcompaction never elides a message with images; a request without images never consults the vision cache | +5 |
| Unit, TUI | Ctrl+V image (fake `runClipboardCmd`) returns a `tea.Cmd` whose message inserts a placeholder; Ctrl+V with no image returns `textarea.Paste`; Alt+V only on Windows; drag paste of quoted, escaped and `file://` paths; mixed paste stays text; deleted placeholder isn't sent; placeholder-only message sends while busy and idle; `/image` | +8 |
| Unit, tools | `os_read_image` attaches the bytes it read (no second open); `web_screenshot` attaches the top-cropped image and names a JPEG `.jpg` | +3 |
| Unit, relays | Discord image attachment → `TurnRequest.Images`, and into `InterjectWithImages` during a turn; failed `Add` → text note; chat.db join with NULL `mime_type` and a missing file (fixture db); two photo-only iMessages in one chat both delivered; BlueBubbles attachment download with an escaped guid (httptest); attachment-only iMessage handled | +7 |
| Eval | `vision` subset: 4817 | +1 |
| Manual | Criteria 1, 2, 6, 8 and 9 on the Mac with the live GLM endpoint, including a TIFF copied from Preview; Ctrl+V on Linux (X11 or Wayland); Alt+V and file drag on Windows Terminal (with TEN-504's Windows session) | checklist |

## Rollback

Revert the PR. No stored data changes shape:
- Attachments are files under `<data>/attachments/`.
- Archive lines gain an optional `meta.images` key that older builds ignore.
- `vision` is an optional config key that older builds ignore, apart from `keys.golden`.

`"vision": "off"` per provider turns images into notes without a revert.

## Effort (Claude Code time, one session each)

| Commit | Part | Estimate |
|---|---|---|
| A | `model.Image`, `internal/attach` (sniff, scale, encode, HEIC, store), privacy class | 1.5 h |
| B | Agent, working set, archive, assembler window, cap and tokens, tool side channel | 2 h |
| C | vllm, anthropic, claudecode, echo wire changes, `StripImages` | 1.5 h |
| D | `vision` config, `CanSee`, probe (chat path, single-flight, retry), `/model vision`, dashboard mark, events | 2.5 h |
| E | TUI Ctrl+V/Alt+V per OS (macOS TIFF path, Windows PowerShell), drag paths, placeholders, `/image`, interject | 3.5 h |
| F | `os_read_image`, `web_screenshot` | 0.5 h |
| G | Discord (incl. interject), iMessage chat.db, watcher dedup and BlueBubbles attachments | 3.5 h |
| H | Docs, eval task, CHANGELOG, events doc, GLM adversarial review, full local CI | 1.5 h |
| | **Total** | **~17.5 h** |

## Files reference

| File | Change |
|---|---|
| `internal/model/llm.go:85` | `Image` type; `Message.Images`; `WithImageSink`/`AttachImage`; `CanSee`, `StripImages` |
| `internal/model/profile.go:22` | `Vision string` |
| `internal/attach/` (new) | `Store`, `Add`, `DataURI`, scaler, HEIC/webp via `sips` |
| `internal/memory/working/working.go:26` | `Images []model.Image` |
| `internal/memory/assemble/assemble.go:866,608,642` | Copy images, 3-exchange window, 8 cap, vision notes, image tokens |
| `internal/memory/compress/microcompact.go` | Never elide a message with `Images` |
| `internal/memory/compress/archive_compact.go:379` | Pin image notes verbatim into the summary |
| `internal/model/vision.go` (new) | Vision registry (`SetVision`, `VisionOf`), `CanSee`, `StripImages` |
| `internal/agent/agent.go:514,605,719,430,1820,2143` | `TurnRequest.Images`, image-only turns, `InterjectWithImages`, tool images, token estimate |
| `cmd/tenant/turngate.go:19,279,356` | Forwarding `InterjectWithImages`; image paths in `turnDedupeKey` |
| `internal/agent/tools.go:133,154`, `internal/agent/infoflow.go` | `ToolCallResult.Images`; image sink per call; labelled images record path and name as tracked fragments |
| `internal/model/backend/vllm/vllm.go:116,160,463`, `stream.go` | `Parts` plus custom `MarshalJSON`, image parts, tool-image follow-up message, `(*Backend).ProbeVision` |
| `internal/model/backend/anthropic/anthropic.go:84,218` | Image blocks; block-array `tool_result` content |
| `internal/model/backend/claudecode/claudecode.go:238` | Path notes |
| `internal/model/backend/echo/echo.go:65` | Image count in reply |
| `internal/config/config.go:327`, `internal/config/testdata/keys.golden` | `Provider.Vision` |
| `cmd/tenant/wiring.go:836,991,1050`, `fallback.go`, `agentprofile.go` | Copy `Vision` into profiles; start probes |
| `cmd/tenant/modelctl.go` | Probe on `/model use`/reload; `SetVision` |
| `internal/tui/tui.go:1496,1568,1997`, `internal/tui/cmd_models.go` | Ctrl+V/Alt+V, paste paths, placeholders, submit with images, `/image`, `/model vision` |
| `internal/tui/clipboard_image.go` (new) | Per-OS clipboard image read through `runClipboardCmd` |
| `internal/plugins/osys/dispatch.go:110,191`, `osys.go:407` | `os_read_image` `ResultLabels`; `ReadImage` returns the bytes; `AttachImage` |
| `internal/plugins/web/dispatch.go:413` | Sniffed extension; `AttachImage` |
| `cmd/tenant/discordingest.go:38`, `discordrelay.go:307,348` | gif/webp; images into the turn and into interjections |
| `internal/plugins/imessage/chatdb.go:38,315`, `watch.go:135`, `client.go:127,239`, `cmd/tenant/imessagerelay.go:147,233` | Attachments from chat.db and BlueBubbles; dedup key with attachment GUIDs; image-only messages |
| `internal/privacy/files.go:14`, `privacy.go:30`, `cmd/tenant/privacyctl.go:198`, `cmd/tenant/doctor.go:433` | `attachments` class and permission check |
| `internal/dashboard/models.go`, `templates/models.html` | Vision mark per provider |
| `internal/eval/…` | `vision` subset task and generated fixture |
| `docs/QUICKSTART.md`, model docs, `CHANGELOG.md`, `docs/EVENTS.md` | Docs |

## Out of scope

- **Dashboard chat uploads** (`internal/dashboard/ssr_chat.go` is text only). A follow-up once the TUI path is
  proven.
- **Showing images inside the terminal** (sixel, kitty graphics). Placeholders only.
- **Desktop computer use** (mouse and keyboard control). It waits for the Docker sandbox (TEN-453), per the epic.
- **A separate vision model in the router.** A non-vision primary gets notes; use a vision model as the primary.
- **Video, audio and PDF pages as images.** Discord still saves them as files with the text manifest.
- **Decoding webp or HEIC in Go.** That would need `x/image` or cgo.
- **Images in episodic recall.** Episodes say `[N images]`; recall stays text.

## Risks

- **Probe false negatives.** A vision model that answers "crimson" or reasons for too long fails the probe.
  Mitigation: four plain colours, `max_tokens` 4096, and `/model vision <p> on` to override. **False positives**
  (a blind model guessing) are cut to at most 1 in 12 by the random two-colour order.
- **Context cost.** 8 images ≈ 12.8K tokens. Fine at 500K, tight on a 32K model. The 3-exchange window and the cap
  bound it, and the notes keep the paths so the model can reopen an image.
- **Windows Terminal swallows Ctrl+V.** Alt+V and `/image` cover it, and the docs say so.
- **Prefix cache.** Each image aging out of the window changes one older message once (one cache miss); the vision
  gate can't flap because inconclusive probes never flip a cached answer.
- **Privacy:** images leave the host like any text when a hosted provider is active. They carry the info-flow
  labels of their source (`model.Image.Labels`: a tool's `ResultLabels`, a relay's inbound flow), so the egress
  gate sees them as it sees text, and `attachments/` follows the archive's retention.

## Review log

**Rev 2 (2026-10-09), GLM-5.3-Flash adversarial review, two passes (core pipeline; edges), each 5/10 on rev 1.**

Accepted and folded in:
- The vllm wire message keeps byte identity with a custom `MarshalJSON`.
- Probe:
  - two random colours instead of "red";
  - only 400/404/422 count as no;
  - no inline 10 s stalls.
- `attach`:
  - a 40 MP pixel budget before any decode;
  - brand-checked HEIC, with webp converted by `sips` on macOS;
  - GIF and alpha rules.
- Tools pass vetted bytes, not paths (symlink-swap race), and images carry info-flow labels.
- Microcompaction keeps image messages in the window.
- Full-page screenshots are cropped to the top before scaling.
- TUI clipboard:
  - the Windows `Add-Type` preamble;
  - macOS `try` plus TIFF fallback;
  - the read runs as a `tea.Cmd`;
  - Alt+V on Windows only;
  - placeholders exempt from `CharLimit`.
- Relays:
  - failed adds become notes;
  - chat.db NULL `mime_type` and not-yet-downloaded files;
  - BlueBubbles `PathEscape`.
- Retention follows the archive. Anthropic vision defaults to on and can be overridden.

Rejected after checking the code:
- "The active turn's images never reach the model / are sent twice": the agent appends the user message to the
  working set before `Assemble` and doesn't pass `UserQuery` (`agent.go:719`, `:985`). Now stated in the plan.
- "bubbles' textarea doesn't bind Ctrl+V": it does (`textarea.go:89`, `:1043`, `:1390`).
- "A placeholder-only message is dropped while busy": the placeholder is text, so `q != ""` holds.
- "Windows Terminal has no file drag-and-drop": it inserts the path; to be confirmed on Windows, and `/image` covers it
  either way.

**Rev 3 (2026-10-09), GLM re-score of rev 2: 6/10.**

Accepted and folded in:
- `Interject`'s signature stays put; the new method is `InterjectWithImages`, because `relayRunner` and `turnGate`
  declare `Interject(msg string)`.
- The probe:
  - is a backend method, so it uses `Profile.ChatPath` (`vllm.go:463`);
  - treats 404 and an empty or cut-off 200 as inconclusive, and gets `max_tokens` 4096.
- Only a request that carries images touches the vision gate; the assembler never waits.
- `os_read_image` gets `os_read_file`'s sensitive labels.
- Normalization also caps area at 1.15 MP, so the 1600-token estimate holds.
- Microcompaction never elides a message with images.
- Discord interjections carry images.
- The iMessage watcher's `(chat, text)` dedup would drop a second photo-only message; its key adds the attachment
  GUIDs.
- webp via `sips` is verified in implementation.
- The files table no longer says `Content any` or `AddFile`.
- The effort estimate is raised to 17.5 h.

Rejected after checking the code: "`textarea.Paste` isn't exported / re-injecting loops". It is `func Paste() tea.Msg`
(`textarea.go:1390`) and returns a paste message, not a keypress, so it can't re-enter the `ctrl+v` case.

**Rev 4 (2026-10-09), GLM final score of rev 3: 7/10 (gate passed).** All ten remaining findings folded in:
- Labelled images record their path and name as egress-tracked fragments. Transcribed pixels are a recorded known
  gap.
- Two contradictions removed: the Files table now matches the microcompaction rule, and there is no join-and-wait
  left in the probe design.
- The vision answer lives in an `internal/model` registry, so there is no import cycle.
- Hosted keyed providers aren't probed at launch, and a non-image 400 is inconclusive.
- Compaction pins image notes verbatim.
- Image normalization runs in a `tea.Cmd`.
- `os_read_image` reports webp.
- `turnDedupeKey` includes image paths.
- The stale `max_tokens` in Risks is fixed.
