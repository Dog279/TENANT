# Maintainer guide: gsuite OAuth without Cloud Console for end users

## The goal

End users running `tenant` should be able to type:

```
/configure gsuite
```

…pick `oauth`, sign in with their **@gmail.com** (or Workspace) account in a browser, and be done. **Zero Google Cloud Console for them.**

That requires you (the maintainer) to do a **one-time** Google Cloud setup. After that, every binary you build has the OAuth client baked in.

## Why this is necessary

Google requires every app accessing Gmail/Calendar APIs to register an OAuth client with Google Cloud. The OAuth client identifies your app to Google; users authorize *your app* to access *their data*. The user account and the OAuth client are separate:

- **OAuth client** = "what app is asking for permission" (registered with Google by the maintainer)
- **Signing-in account** = the end user's @gmail or Workspace account (chosen at sign-in)

You can't ship an app that accesses Gmail without an OAuth client. You CAN ship one OAuth client that works for any signed-in user.

## One-time setup (you, the maintainer)

### 1. Create the OAuth client in Google Cloud Console

1. Visit https://console.cloud.google.com/apis/credentials
2. Create a new project if you don't have one (any name)
3. **APIs & Services → Enabled APIs & Services → "+ ENABLE APIS AND SERVICES"**
   - Enable **Gmail API**
   - Enable **Google Calendar API**
4. **APIs & Services → OAuth consent screen**
   - User type: **External**
   - App name: "tenant" (or whatever)
   - User support email: your email
   - Developer contact: your email
   - On the **Scopes** step, click "Add or Remove Scopes" and add:
     - `.../auth/gmail.readonly`
     - `.../auth/gmail.send` *(optional, if you want agents to send mail)*
     - `.../auth/calendar.readonly`
     - `.../auth/calendar.events` *(optional, if you want agents to create events)*
     - `.../auth/drive.readonly` *(TEN-72: required for `drive_search`, `drive_read`, `drive_list` tools)*
   - On the **Test users** step, add **yourself + everyone you'll distribute the binary to** (max 100 in Testing mode)
   - Publishing status: leave as "Testing" (verification is slow + expensive for Gmail scopes — see below)
5. **APIs & Services → Credentials → + CREATE CREDENTIALS → OAuth client ID**
   - Application type: **Desktop app**
   - Name: anything
   - Click **Create** → click **DOWNLOAD JSON** on the new entry

You should now have a file like `client_secret_xxxxx.apps.googleusercontent.com.json`.

### 2. Bake the credentials into the binary

The placeholder lives at `internal/plugins/gsuite/embedded_oauth_client.json` (ships as `{}` in the repo). Replace it with your downloaded JSON:

```bash
cp ~/Downloads/client_secret_xxx.apps.googleusercontent.com.json \
   internal/plugins/gsuite/embedded_oauth_client.json
```

Now build:

```bash
go build -o tenant.exe ./cmd/tenant
```

The OAuth client is now compiled into `tenant.exe` via `go:embed`. Any binary built from this state ships with the credentials.

### 3. Verify

```bash
./tenant.exe tui --gsuite
/configure gsuite
# pick oauth
# → expected: "Signing you in with Google. A browser will open in a moment…"
#   browser opens → sign in with any test-user account → authorize → done
# (NO Cloud Console walkthrough should appear)
```

If you still see the Cloud Console walkthrough, the embed didn't take. Confirm the JSON file isn't `{}` and rebuild.

### 4. Distribute

`tenant.exe` is now ready. Users who download it can run `/configure gsuite` and sign in directly — no Cloud Console for them.

## The "Google hasn't verified this app" warning

Until you complete Google's OAuth Verification (slow + expensive for Gmail-restricted scopes, requires CASA assessment $75-$15K), end users will see:

```
This app isn't verified
Google hasn't verified this app. Only proceed if you know and trust the developer.
```

Users click **"Advanced" → "Go to tenant (unsafe)"** to proceed. This is normal for Testing-mode apps. The OAuth flow is still secure — Google issues a real token bound to your client. The warning is purely UX.

To remove the warning: complete OAuth Verification (months-long process, separate effort).

## Two-layer architecture (advanced)

The gsuite plugin resolves OAuth credentials in this order:

1. **Operator-supplied** `oauth_creds_json` setting (per-skill config) — takes priority
2. **Runtime file** at `<cfgDir>/oauth_client.json` (installed via `tenant oauth-setup gsuite <path>`) — overrides compiled-in
3. **Compiled-in via `go:embed`** (`internal/plugins/gsuite/embedded_oauth_client.json`) — the distributed default

This means:

- A vanilla build (placeholder `{}`) falls back to (1) — operators have to supply their own JSON.
- A distribution build (real JSON in step 2 above) gets (3) — users skip all Cloud setup.
- An operator can override the distributed default per-machine with (2) using `tenant oauth-setup gsuite <path>`.

## Should you commit the JSON to git?

**The `client_secret` field for Desktop App OAuth clients is NOT actually secret** per RFC 8252 (BCP for OAuth on native apps). Embedding it in distributed code is the documented pattern.

That said, committing it to a public repo is bad form because:

- It signals "this is a secret" to readers who don't know RFC 8252
- A future Google policy change might tighten the rules
- Anyone can use your client_id to make API requests masquerading as your app (subject to your project's quota)

**Recommendation:**

- Add `internal/plugins/gsuite/embedded_oauth_client.json` to `.gitignore`
- Keep the placeholder committed for vanilla checkouts
- Drop in your real JSON locally before building distribution binaries
- CI builds without the JSON will use the placeholder (and fall back to layer 1/2)

Add this to `.gitignore` if you want that workflow:

```
# Maintainer-only: real OAuth client lives here for distribution builds
internal/plugins/gsuite/embedded_oauth_client.json
```

(But then how do you SHIP a placeholder? Either: commit the placeholder, gitignore real JSON, AND have maintainers swap in the real one. OR: never commit either, document the workflow. Pick what fits your team.)

## Scope migration (existing users after upgrading)

If your users authorized **before** Drive support shipped (TEN-72), their cached OAuth tokens were minted with `gmail.readonly + calendar.readonly` only — no Drive. After upgrading the binary, `drive_*` tool calls will fail with `Insufficient Permission` (HTTP 403) at first use.

The fix: have them re-authorize. Two paths:

```
# Inside the TUI:
/skill clear gsuite oauth_token
/configure gsuite       # → pick "Sign in with your Google account" → re-grant
```

That clears the stale token, runs the OAuth flow again, and the new token includes the Drive scope.

Tenant's `tenant doctor` will FAIL with an actionable hint when the gsuite skill is enabled with `auth=oauth` and no creds are configured — but it does NOT currently detect "token exists but is missing a scope" (would require a tokeninfo round-trip per doctor run). That gap is a follow-up.

## Quotas

The default Gmail API quota per OAuth project is generous (~1 billion quota units/day). For early-stage distribution this is plenty. Monitor at:

https://console.cloud.google.com/apis/dashboard

If you hit limits, request a quota bump — Google approves these quickly for non-abusive usage.
