# `/configure github` — Architecture Plan

> Debate + verdict for TEN GitHub integration. Auto-generated 2026-06-11.

## State of Play

- **TEN-162** (remote MCP client: go-sdk + DCR + Streamable-HTTP + gated pseudo-plugin) is **Done**. The `mcpremote` package connects to any hosted MCP server with browser OAuth.
- **`/configure atlassian`** is the proven pattern (`skillctl_atlassian.go`): `skillKind` struct with Fields, Probe, ShowIf conditionals, three auth modes (mcp/oauth/token).
- **GitHub's hosted MCP server** lives at `https://api.githubcopilot.com/mcp/` and exposes ~30 tools across repos, issues, PRs, actions, users, context, dependabot, code scanning, secret scanning, and discussions.

## Key Finding: GitHub Hosted MCP Does NOT Support DCR

Atlassian's MCP server (`https://mcp.atlassian.com/v1/mcp`) supports RFC 7591 Dynamic Client Registration — Tenant can connect with zero pre-setup, no app creation needed. Just click Authorize in the browser.

GitHub's hosted MCP server does **not** support DCR. From the official README:

> "Each MCP host application needs to configure a GitHub App or OAuth App to support remote access via OAuth."

This means browser OAuth requires a pre-registered GitHub App owned by the host. Two sub-paths:
1. **Maintainer-shipped GitHub App** (like gsuite's embedded OAuth pattern) — Tenant ships with a client ID, users click Authorize, done.
2. **User creates their own GitHub App** — more steps, advanced users only.

## The Debate

### Researcher Position: PAT-first via MCP Remote

Connect to `https://api.githubcopilot.com/mcp/` using a Personal Access Token as a `Bearer` header. This reuses the existing `mcpremote` infrastructure with a small auth-header injection. Zero browser flow needed. The hosted MCP server already has ~30 tools covering everything.

- **Effort**: ~30 min AI-assisted (new `skillctl_github.go` mirroring `skillctl_atlassian.go`, but simpler)
- **Completeness**: 7/10 — PAT works, but long-lived token. No fine-grained permission UX.
- **Pros**: Ship today. No GitHub App creation. Reuses TEN-162.
- **Cons**: PAT is a long-lived secret. User must manage token lifecycle (creation, rotation, revocation).

### Strategist Position: Ship PAT Now, Add OAuth GitHub App Later

Same as Researcher's PAT path for v1, but plan the OAuth GitHub App as a follow-up. The GitHub App would be created once by the maintainer (Dylan), its client ID embedded in the binary, and `/configure github` offers "browser sign-in" as the recommended mode — identical UX to `/configure atlassian`.

- **Effort**: v1 PAT = 30 min. v2 OAuth GitHub App = 2 hrs (create app, embed client ID, wire OAuth flow).
- **Completeness**: 9/10 — both paths covered, progressive enhancement.
- **Why not start with OAuth**: Creating and maintaining a GitHub App is maintainer burden (verification, secret rotation, terms of service). PAT is zero-maintainer-cost. Ship fast, add polish after.

### Adversarial Check

- **Q: Why not a native `internal/plugins/github` package?**
  - A: The hosted MCP server already provides all the tools. Writing a native plugin would duplicate effort. Native plugin is an "ocean" (full rewrite of 30+ tools), not a "lake."

- **Q: What about GitHub Enterprise?**
  - A: GHES doesn't support the remote hosted MCP server. GHES users would need the local MCP server (`ghcr.io/github/github-mcp-server`) or a native plugin. This is a v3 concern.

- **Q: What about fine-grained PATs vs classic PATs?**
  - A: Fine-grained PATs are preferred (per-repo permissions, expiration). The probe doesn't need to distinguish — the MCP server handles auth validation. The note text should recommend fine-grained.

## Verdict

**Ship two modes. "pat" is primary and recommended. "mcp" (browser OAuth) is listed but deferred to a follow-up ticket.**

```
RECOMMENDATION: PAT-first MCP Remote. Reuses TEN-162 infrastructure,
ships in 30 min, gives ~30 GitHub tools with zero maintainer overhead.
Option A: PAT via MCP Remote (Completeness: 8/10)
Option B: Native github plugin (Completeness: 3/10 — ocean, defers work)
```

## Architecture: `skillctl_github.go`

### Auth Modes

| Mode | Label | UX | Status |
|------|-------|----|--------|
| `pat` | Personal Access Token — paste a fine-grained PAT (recommended) | Paste token, probe verifies via MCP connect | **v1** |
| `mcp` | Browser OAuth — connect via GitHub App (coming soon) | Opens browser, authorize, done | **v2** (TBD) |

### Fields

```
auth: "How do you want to connect to GitHub?" (required, default: "pat")
  Options: pat, mcp
  Labels:
    - "Personal Access Token — paste a fine-grained PAT (recommended)"
    - "Browser OAuth — connect via GitHub App (coming soon)"

pat: "GitHub Personal Access Token" (secret, required, showIf: auth=pat)
  Note: "Create at github.com/settings/tokens?type=beta — fine-grained recommended.
         Select repos (or all), enable Permissions: Issues (read/write), Pull requests (read/write),
         Actions (read), Contents (read), Metadata (read)."

toolset: "Toolsets to enable" (showIf: auth=pat)
  Options: default, readonly, all, custom
  Labels:
    - "Default — repos, issues, PRs, users, context"
    - "Read-only — safe, no write operations"
    - "All — every available toolset"
    - "Custom — specify toolsets (e.g. repos,issues,actions)"
  Default: "default"

toolsets_custom: "Comma-separated toolsets" (showIf: toolset=custom)
  Note: "Available: repos, issues, pull_requests, users, actions, context, dependabot, code_scanning, secret_scanning, discussions"
```

### Probe Logic

```go
func probeGitHub(ctx context.Context, creds *credentials, settings map[string]string, _ func() error) (string, error) {
    auth := settings["auth"]
    if auth == "" { auth = "pat" }

    switch auth {
    case "pat":
        token := creds.get(skillSecretID("github", "pat"))
        if token == "" {
            return "", errors.New("pat mode needs a Personal Access Token")
        }
        url := githubMCPEndpoint(settings)
        return mcpConnectWithPAT(ctx, url, token)

    case "mcp":
        return "", errors.New("browser OAuth mode requires a GitHub App — coming soon (use 'pat' mode for now)")

    default:
        return "", fmt.Errorf("unknown auth mode %q", auth)
    }
}
```

### MCP Endpoint Builder

```go
func githubMCPEndpoint(settings map[string]string) string {
    switch settings["toolset"] {
    case "readonly":
        return "https://api.githubcopilot.com/mcp/readonly"
    case "all":
        return "https://api.githubcopilot.com/mcp/x/all"
    case "custom":
        ts := settings["toolsets_custom"]
        if ts == "" { ts = "repos,issues" }
        return "https://api.githubcopilot.com/mcp/x/" + ts
    default:
        return "https://api.githubcopilot.com/mcp/"
    }
}
```

### MCP Connection with PAT

`mcpremote.Config` gains a `BearerToken` field. The transport layer injects `Authorization: Bearer <token>` on every request. ~10 LOC change to `internal/plugins/mcpremote`.

### Activator (toolmux.go catalog)

Mirrors the Atlassian activator pattern. Stub has no static specs (MCP provides them dynamically after connection). Tools appear after activation, same as other MCP remotes.

### Files Changed

| File | Change | LOC |
|------|--------|-----|
| `cmd/tenant/skillctl_github.go` | **New** — skillKind + probe + endpoint builder | ~120 |
| `cmd/tenant/skillctl.go` | Register `github` in the catalog | ~5 |
| `cmd/tenant/toolmux.go` | Add GitHub to the stub catalog with activator | ~30 |
| `internal/plugins/mcpremote/mcpremote.go` | Add `BearerToken` to Config, inject into transport | ~15 |
| `cmd/tenant/commands.go` | Wire `/configure github` help text | ~3 |

**Total: ~170 LOC new/changed. Estimated 30 min AI-assisted.**

### /configure UX Flow

```
> /configure github

  GitHub Integration

  How do you want to connect to GitHub?
  > Personal Access Token — paste a fine-grained PAT (recommended)
    Browser OAuth — connect via GitHub App (coming soon)

  Create a fine-grained PAT at github.com/settings/tokens?type=beta
  Select repos (or all). Enable: Issues, Pull requests, Actions, Contents, Metadata.

  GitHub Personal Access Token: ****

  Toolsets to enable:
  > Default — repos, issues, PRs, users, context
    Read-only
    All
    Custom

  ✓ Connected to GitHub as @dtaylor — 28 tools available.
  Run /enable github to activate.
```

## Jira Tickets to Create

### TEN-173: `/configure github` — PAT mode via MCP Remote

**Type**: Task | **Priority**: Medium | **Depends on**: TEN-162 (Done)

Add `/configure github` slash command with PAT auth mode, connecting to GitHub's hosted MCP server at `https://api.githubcopilot.com/mcp/`. Includes skillctl_github.go, catalog registration, stub activator, mcpremote BearerToken field, and toolset selection.

### TEN-174: GitHub OAuth browser sign-in for `/configure github`

**Type**: Story | **Priority**: Low | **Blocked by**: TEN-173

Add browser OAuth mode using a maintainer-owned GitHub App. Embed client ID in binary. Mirrors gsuite embedded-OAuth pattern (`setup_oauth.go`).

## Effort Estimate

| Task | Human-hours | AI-assisted min |
|------|-------------|-----------------|
| TEN-173 (PAT mode) | 2 days | 30 min |
| TEN-174 (OAuth flow) | 4 hrs | 45 min |

## Open Questions

1. **`mcpremote` directly or thin `internal/plugins/github` wrapper?** Recommendation: mcpremote directly. No new plugin package needed.
2. **PAT storage**: credentials file (0600, same as Atlassian API token). Acceptable for v1?
3. **GitHub Enterprise**: GHES doesn't support remote hosted MCP. Separate ticket.
