# The dashboard: an operator's guide

The dashboard is your agent's control panel in a web browser. From it you see what your agent is doing, answer its requests, and change what it may do on its own.

This guide is for the person who runs Tenant day to day. You don't need a terminal for anything here except finding the sign-in key the first time and the setup steps in sections 2 and 3. [serve-runbook.md](serve-runbook.md) covers the technical side: running Tenant as a background service, and the JSON API.

## 1. Turn it on

- When Tenant runs as a background service (`tenant serve`), the dashboard is always on.
- In the terminal app it is on unless you turned it off; `/dashboard on` turns it back on.
- The address is **http://127.0.0.1:9770** on the computer running Tenant. Use exactly that address: `http://localhost:9770` is refused with "unrecognized dashboard host" (section 3 explains how to reach it from elsewhere).

## 2. Sign in

Open the address. The first time, you see a **Sign in** page asking for your sign-in key. Paste the key and press **Sign in**. You land on the page you were opening.

The key is a long line of letters and digits in a file named `dashboard-auth-token`, in Tenant's settings folder on the computer running Tenant. Tenant creates it the first time it starts. Show it like this:

- **macOS:** `cat ~/Library/Application\ Support/tenant/dashboard-auth-token`
- **Linux:** `cat ~/.config/tenant/dashboard-auth-token`

If Tenant was started with `--config <folder>`, the file is in that folder instead. Tenant says where on every start: `tenant serve` in a terminal prints the key itself, and otherwise it writes "the auth token is in <path>" to the terminal app's feed and its system log (**Logs** on the dashboard, and `logs/system/` in its data folder).

### On a Windows service

A Windows service runs as a service account, and the key is in that account's settings folder. You need Tenant built with the Windows secrets fix (September 2026, "Windows service installs share their secrets with the machine's administrators"). With it, an administrator can read the folder. Older builds lock administrators out of the key and of config.json: upgrade first.

1. Find the account the service runs as, and whether it was given its own folder. Open PowerShell **as administrator** (right-click, Run as administrator) and run:

   ```powershell
   Get-CimInstance Win32_Service | Where-Object Name -like '*tenant*' | Select-Object Name, StartName, PathName
   ```

   `StartName` is the account. If `PathName` has `--config "<folder>"`, the key is in that folder.

2. Show the key, in the same administrator window:

   - Service running as **LocalSystem** (SYSTEM):

     ```powershell
     Get-Content -LiteralPath "C:\Windows\System32\config\systemprofile\AppData\Roaming\tenant\dashboard-auth-token"
     ```

   - Service running as **NT AUTHORITY\LocalService**:

     ```powershell
     Get-Content -LiteralPath "C:\Windows\ServiceProfiles\LocalService\AppData\Roaming\tenant\dashboard-auth-token"
     ```

   - Service given its own folder: `Get-Content -LiteralPath "<folder>\dashboard-auth-token"`.

3. If Windows says access is denied, check the folder: `icacls "<the folder above>"` must list `BUILTIN\Administrators`, and `tenant doctor` run as administrator reports "Windows DACL for this user, Administrators and service accounts". If it doesn't, you are on an older build: upgrade.

Paste the key into the browser's sign-in form. Never paste it into the address bar: a key in an address is refused, because the browser keeps addresses in its history.

### Your own key instead

If you'd rather use a key you already know, set `dashboard.auth` in config.json (in the same folder) to 32 or more random characters from a password manager, then restart Tenant. `tenant doctor` warns when it is shorter than that. After five wrong keys from one address, Tenant makes that address wait before it may try again (the wait doubles each time, up to a minute). On Windows, edit config.json from an administrator window.

### Signing out, and changing the key

- **Log out** (at the bottom of the menu) ends that browser's sign-in. The browser keeps no copy of the key: it holds a sign-in that lasts 7 days, after which it asks for the key again.
- Restarting Tenant signs out every browser.
- If the key itself may have got out (you pasted it somewhere, or a lost laptop had it saved), change it: stop Tenant, delete `dashboard-auth-token` (or change `dashboard.auth`), and start Tenant. It makes a new key. On a Windows service: delete the file from an administrator window, then restart the Tenant service (open Services, find Tenant, click Restart). Every browser then has to sign in again with the new key.
- A tab left open after a restart or a key change goes to the sign-in form by itself the next time it checks for news.

### Other web pages on this computer

Browsers don't keep a sign-in apart by port: every other web service on this computer's address (127.0.0.1, or your Tailscale name) receives the dashboard's sign-in when this browser opens it, and could use it until it ends. So in the browser you use for the dashboard, don't open links to other services on 127.0.0.1 or your Tailscale name, above all ones your agent suggests. The dashboard itself never links there: a research source on this computer, your network or your tailnet shows as plain text. If you did open one, press **Log out** (or restart Tenant) to end that sign-in.

### Monitoring

`GET http://127.0.0.1:9770/healthz` answers without the key, so an uptime monitor can use it. Only GET is open: `HEAD /healthz` needs the key like everything else.

## 3. Where it can be reached

- **This computer only**, by default: http://127.0.0.1:9770, or http://localhost:9770 if you prefer the name.
- **From your phone:** use Tailscale. In the terminal app run `/tailscale serve`, or set `"tailscale": {"serve": true}` in config.json so a background service publishes it on every start. Tenant trusts the Tailscale name on its own.
- **From another computer on your network, with no VPN:** in the terminal app type `/dashboard serve lan`
  (or start with `--dashboard-lan`, or set `"dashboard": {"lan": {"enabled": true}}` in config.json).
  That starts a second listener, HTTPS only, on every interface at port 9771; give an address to
  choose one (`/dashboard serve lan 192.168.1.5:9771`). `/dashboard serve off` closes it. It is off
  until you turn it on, and http://127.0.0.1:9770 keeps working beside it. See "On your local
  network" below.
- **Behind a web proxy on the same computer:** add the name the proxy uses to `dashboard.allowed_hosts` in config.json.
- **On a network address** (for example `--dashboard-addr 0.0.0.0:9770`): Tenant refuses to start unless the dashboard has a key and a certificate (`dashboard.tls_cert` and `dashboard.tls_key`).
- **Never** forward the port to the internet.

One thing works only on the computer running Tenant, in a browser signed in at http://127.0.0.1:9770 (or localhost): **confirming a fleet machine** on the **Fleet** page, the step that lets your agent work on another computer. By your Tailscale name, behind a proxy, from another computer or with the key in a script, the page shows the machine and where to confirm it, and the six digits are refused ("Nothing changed: a machine is confirmed in a browser on the computer running Tenant…"). The terminal on that computer can confirm it too. A browser on another computer that reaches the port through an SSH tunnel counts as being there, as a terminal over that SSH would. On a master with no screen you can let a signed-in browser elsewhere confirm machines, with the sign-in key typed again each time: run `tenant fleet remote-confirm on` in a terminal on that computer (off by default; [fleet-runbook.md](fleet-runbook.md), section 2).

### On your local network

`/dashboard serve lan` answers with the address to open and a certificate fingerprint:

```
dashboard: also serving the local network at https://192.168.1.5:9771 (listening on 0.0.0.0:9771)
  certificate: self-signed, SHA-256 fingerprint 7C:32:6C:…
```

- **The certificate.** Tenant makes it once and keeps it in the config dir
  (`dashboard-lan-cert.pem`, `dashboard-lan-key.pem`, owner-only), good for two years, then
  replaced. No authority signed it, so the browser warns the first time: open the certificate in
  that warning and compare its SHA-256 fingerprint with the one Tenant printed (`/dashboard` prints
  it again). If they differ, stop: something is between you and Tenant. To use a certificate of
  your own, set `dashboard.tls_cert` and `dashboard.tls_key`.
- **Signing in** is the same key as on this computer (section 2).
- **Who may connect:** this computer, and private or link-local addresses (10.x, 172.16-31.x,
  192.168.x, fc00::/7, fe80::/10). To let another network in, such as a VPN's range, list it:
  `"dashboard": {"lan": {"allow": ["100.64.0.0/10"]}}`. Every other address is refused before
  sign-in, and the refusal is in the Security log.
- **The name you type** must be the address you are connecting to, or this computer's name (as
  the system reports it, its first label, or that label with `.local`). Another name needs to be
  in `dashboard.allowed_hosts`.
- **One address, not every interface:** `/dashboard serve lan 192.168.1.5:9771`. On macOS and
  Windows the firewall may ask about the first listener that is not loopback; allow it for the
  networks you mean to serve.
- **What it cannot do from there:** confirm a fleet machine (above), or turn this listener on or
  off. That is done at the keyboard, by the flag or in config.json, never from a page.

If a page shows one of these instead of the dashboard:

| It says | It means | What to do |
|---|---|---|
| unrecognized dashboard host | The address you typed isn't one Tenant trusts. | Use http://127.0.0.1:9770 (or http://localhost:9770), your Tailscale address, or add the name to `dashboard.allowed_hosts`. |
| the dashboard is served to the local network only | You reached the LAN listener from an address that is not private. | Use a computer on the local network, or list your network in `dashboard.lan.allow`. |
| cross-origin request rejected | The request came from another site, another tab's page, or another port on this computer. | Use the dashboard's own buttons. If it keeps happening, something else on this computer is sending requests to it. |
| CSRF validation failed | A browser extension or privacy setting removed the header that shows the request came from the dashboard. | Turn that extension off for the dashboard's address. |
| Nothing changed: that needs a second click to confirm. | A change that asks first was sent without its second step. | Open the step again and press its **Yes** button. |

## 4. When something needs your OK

Some things your agent wants to do wait for you first: running a command, changing files, sending a message and so on (section 5 says which). Doing nothing never says yes.

**Where it appears.** Within a second:

- on **Approvals & safety**, under "Waiting for your OK";
- above the message box on **Chat**, when you are there;
- on every page, in the line under the page title: "1 action is waiting for your OK", with an **Answer it** link. While your Chat message is the one waiting, that line reads "Your agent is answering a message · It's paused until you answer a request. Answer it";
- on **Overview**, in the "Waiting for your OK" count.

Each request says what your agent wants to do, in which area, which of its tools asked, how long it has waited, and the full text of what it wants to run or send. Special characters that could hide text show as markers like ⟨U+202E⟩. A long request says so ("This request is 41 lines long. Read all of it before you allow it."): scroll through it before you answer.

**The buttons.**

- **Allow once:** this one action goes ahead. Your agent carries on.
- **Deny:** refused. Your agent is told no and carries on without it.
- **Allow for the rest of this session** (only on some requests, behind a second step): the same kind of action goes ahead without asking until Tenant restarts, or until you press **Ask me again** on its row under "What your agent may do on its own". It isn't offered where the grant would be hidden or would let your agent get around your settings: commands that don't run in the sandbox, leaving the sandbox, requests from iMessage, and requests a tool rule made ask.
- A request from **another computer that wants to pair** asks a second time: allow it only if the person pairing told you the same PIN that the request shows.
- A request for something on **a fleet machine** starts with "On the machine" and its name, and its second line gives the machine's id and "not this computer". Its text starts with the same two. It never offers **Allow for the rest of this session**: that grant is this computer's, and a machine's own settings are on its **Fleet** page.

There is no "always" button here. To stop being asked about a whole area, change its setting (section 5): that is a deliberate step with a warning.

**If nobody answers**, the request is turned down:

- when the job that asked ends: a scheduled job after 15 minutes (`cron.per_run_minutes`); your Chat message when you press Stop;
- an alert investigation after 15 minutes;
- a Discord card after 10 minutes (`relay.approval_timeout`);
- every one still waiting, when Tenant stops.

**"This was already answered (here, in another tab, or in the terminal) or it timed out. This tap didn't change anything."** Someone got there first: the terminal app, another tab, or the timeout. The first answer is the one that counts.

**Requests from a Discord conversation** are answered on Discord, with the buttons on the card in the conversation. They aren't in the list here; when Discord is running the page says so: "Requests raised in a Discord conversation are answered with the buttons on the Discord card." Requests from an **iMessage** conversation are in the list, marked "from iMessage", and you answer them here.

**"Live updates paused. Reload the page to see new requests."** The page lost its connection to Tenant (it restarted, or the network dropped). Reload the page.

## 5. What your agent may do on its own

**Approvals & safety**, under "What your agent may do on its own", has one row per area. Each row says, in words, what your agent can do now.

| Area | What it covers | Recommended |
|---|---|---|
| Run commands | run commands and programs on the computer running Tenant | Ask |
| Change files | create and change files | Ask |
| Irreversible actions | things that can't be undone | Ask |
| Act on websites | click, type and submit forms on websites | Ask |
| Send messages | email, texts and posts in your name | Ask |
| Share private data | send private information it has read to an outside service | Ask |
| Connect remote services | connect new remote services and trust their tools | Ask |
| Leave the sandbox | run a command outside the safety box after the box stopped it | Ask |

- **Ask:** it asks you first, every time (section 4). One tap.
- **Allow:** it goes ahead without asking. Choosing Allow opens a second step that explains it before anything changes.
- **Deny:** it can't, and doesn't ask. One tap. Two settings are stronger than Deny, for their own tools only: a tool rule on **Tools** that lets a tool go ahead (**Don't ask**), and a remote service set to **Don't ask** on **Remote services**. Both ask first when you set them. Discord's lock (**Access**, Discord may not run commands) holds even over those.

**New.** An area an update added, which you haven't chosen for yet, is marked **New**. It asks you first until you choose. A notice on every page links here, and **Keep them at Ask** chooses Ask for all of them at once.

**Why "Share private data" can ask when "Run commands" is Allow.** They are separate questions. "Run commands: Allow" lets a command run. "Share private data" asks about private information leaving the computer, whatever sends it.

**Before you set "Run commands" to Allow**, read its warning. It names the account commands run as. As SYSTEM on Windows that account has full control of the computer. Unless commands run in the sandbox, a command can also change Tenant's own settings, and your agent could then answer its own requests on this page. Keep "Run commands" at Ask unless your commands run in the sandbox. "Leave the sandbox" always carries the same warning.

**Scheduled jobs that run commands** follow their own switch on **Scheduled jobs**, not these rows. Everything else follows these rows: Chat, alerts, research, and scheduled jobs that don't run commands.

**Approvals & safety or Access?** **Access** sets who may message your agent on Discord and iMessage, and what those conversations may make it do. **Approvals & safety** sets what it does for you and for its own jobs.

## 6. Page by page

The menu groups the pages. On a phone, **Menu** at the top opens it; **Log out** is at its bottom. "Asks first" means the button opens a second step that says what will happen, with a **Yes** button to go ahead and **Cancel** to back out.

| Page | What it's for | Asks first |
|---|---|---|
| **Overview** | How your agent is (an alert, or a model it can't reach), what's waiting for you, what it may do, and the services it's connected to. Its model and quality score show once they have data. | Nothing. |
| **Chat** | Talk to your agent. Its requests for your OK appear above the message box. | Nothing. **Stop** is immediate. |
| **Autopilot** | A goal your agent is working toward on its own, started from the terminal app. A background service has no Autopilot, and says so. | Nothing: **Stop after this step** is immediate; the step in progress finishes. |
| **Logs** | Everything your agent and Tenant do, newest first: look into any event, with filters and saved views, and press **○ live** to see new events as they are written. Saving a view, a log's properties and clearing a log answer on the page, with a short note at the top. An old link to the Activity feed opens here. | **Clear** a log. **Reveal** shows a hidden value and is recorded. The Security log can never be cleared. |
| **Memory** | What your agent knows about you and remembers. | Nothing: a removed fact goes to **Recently removed**, where you can restore it. |
| **Skills** | Things your agent figured out, waiting for you to accept or dismiss, and skills to trust. | **Dismiss** or **Remove** a skill, **Trust** one, and **Choose On** or **Choose Trusted** (accepting new skills without you). |
| **Quality** | How well your agent answers a set of test questions over time. | Nothing. A check takes a few minutes. |
| **Research** | Ask a question your agent researches for a few minutes and writes up. The report is saved to your agent's notes. A source on this computer or your own network shows as plain text, not a link (section 2). | **Stop** a run (no report is written) and **Delete** one. |
| **Tools** | Which tools your agent has, **All on** or **All off** for the tools that change things, and tool rules. | **All on**, a rule that lets a tool go ahead (**Don't ask**), and removing a rule that made a tool ask or blocked it. |
| **Helpers** | Specialists your agent hands work to, each with its own model and instructions. A helper on a hosted model says "Sends its work to <provider>". | **Remove**, **Reset** and **Clear instructions**. |
| **Remote services** | Outside services your agent can use. Connecting one that needs a sign-in opens a sign-in page on the computer running Tenant (section 7). An address that can't work is refused on the page; a connection that fails later is in **Logs** (`log:mcp*`). | **Disconnect**, and **Don't ask** (its tools go ahead without asking, even in an area set to Deny). **Ask first** and **Block** are one tap. |
| **Scheduled jobs** | Jobs your agent runs on a timetable. | **Delete** a job, and turning on **Scheduled jobs may make changes**. |
| **Fleet** | The other computers your agent can work on (its fleet): which are online, what each reports about itself, its memory, and what your agent may do on each. A machine's own page confirms it (on the computer running Tenant only, section 3), renames or revokes it, changes its settings and rules, and compares them with a fresh copy of this Tenant's. | **Revoke**, **Allow** for an area, a rule that lets a tool or a command go ahead (**Don't ask**), removing a rule that made one ask or blocked it, **Copy again** and **Apply fleet rules**. Confirming has no second step: typing the machine's six digits is the step. |
| **Approvals & safety** | What's waiting for your OK (section 4), and what your agent may do on its own (section 5). | **Allow** for an area, **Allow for the rest of this session**, and a pairing request's **Allow once**. |
| **Access** | Who may message your agent on iMessage and Discord, and what those conversations may make it do. | **Add someone**, **Remove everyone**, removing the last person, turning on answering (**Turn on**, **Start**), changing the Discord account that approves requests, letting Discord run commands, and **Allow** for an area. |
| **Model** | Which model answers, and models to add: one on a server on your network (see below), or a cloud model. | **Use this model** and **Remove**. |
| **Integrations** | Connect your agent to outside tools (Google, Atlassian, GitHub and others). | **Disconnect**, and **Allow changes** on a GitHub repository. |
| **Provider keys** | The API keys Tenant uses. A key you paste is never shown again, only replaced or removed. A new key for the model in use applies when you press **Reload API keys** on **Model**; other changes apply after Tenant restarts. | **Remove**. |

**Busy.** While your agent is answering a Chat message, the line under the page title says "Your agent is answering a message", with **Stop**. Scheduled jobs, Discord and iMessage conversations and alert checks run alongside and never hold up Chat; **Logs** shows what they are doing. A second message you send while it is answering tells you it wasn't sent and stays in the box. Nothing is lost.

**A model on your network.** On **Model**, "Add a model from a server on your network" takes the address you would give any OpenAI-compatible app: `http://` or `https://`, the server's name or IP address, and its port, with or without `/v1` at the end. vLLM, llama.cpp's server, Ollama, LM Studio and the like all work.

- **Model**: leave it blank and your agent uses the one model the server offers, and follows the server when it switches to another. A server that offers several models (Ollama, usually) says which, and you type the one you want.
- **More options**: a name (blank names it after the model) and an API key, only if the server asks for one.
- **Add server** checks the server first, which takes a few seconds, longer while it loads a model. It finds what kind of server it is and sends one test message that offers the model a tool, to see how the model writes tool calls. The answer says what it found.
- If the model answered without using the tool, or the test didn't finish in time, the model is still added, with a warning: your agent may not be able to use its tools with it. Turn on tool calling in the server (vLLM: `--enable-auto-tool-choice` with a `--tool-call-parser`; llama.cpp: `--jinja`), remove the model and add it again. If the server refuses the test message outright (an embedding model that can't chat, or a model the server won't give tools), the model isn't added, and the answer quotes the server's reason: your agent offers tools with every message.
- Nothing changes until you press **Use this model** or give it to a helper. **Logs** (`log:model`) keeps what each check found and why one failed.

## 7. Signing in to a remote service or an integration

Connecting a remote service that needs a sign-in, or testing an integration such as Google or Atlassian, opens a sign-in page in a browser **on the computer running Tenant**. The sign-in comes back to that computer, so it has to be finished there.

When Tenant runs as a Windows service, that browser never appears: a service has no screen. To finish the sign-in:

1. Press **Connect** (Remote services) or **Test connection** (Integrations) as usual.
2. Sign in to the computer running Tenant itself (at its screen, or with Remote Desktop), and open the dashboard there: http://127.0.0.1:9770.
3. Open **Logs** and type `open this URL` in **Find**. Open the line "sign-in: open this URL to authorize Tenant, if no browser opened:": the link follows it. A remote service also writes "Open this URL in a browser to sign in to <service>" in its own log (**Logs**, filter `log:mcp*`).
4. Copy the link into a browser **on that same computer**, and approve the sign-in. A phone or another computer won't work: the sign-in returns to that computer.
5. Back on **Remote services** or **Integrations**, reload the page.

The link is only good for that one sign-in. If it expired, press **Connect** or **Test connection** again for a new one.

## 8. When something goes wrong

- **The sign-in page says the key didn't match.** Copy the key again (section 2). If the key file changed, Tenant was restarted with a new key.
- **A tab went to the sign-in form by itself.** Tenant restarted, the key changed, the 7-day sign-in ended, or another Tenant on this computer signed this browser in (they share the browser's sign-in). Sign in again.
- **"config.json is unreadable by this account" (Tenant won't start).** The settings folder's permissions are wrong. On Windows, from an administrator window, check the folder with `icacls` and run `tenant doctor` (section 2).
- **Nothing appears under "Waiting for your OK", but your agent is stuck.** Look at the Discord card: requests from Discord conversations are answered there. **Logs** shows what your agent and its jobs are doing.
- **A remote service's sign-in never finishes.** See section 7.
- **"Live updates paused".** Reload the page.

## 9. What is recorded

The Security log (**Logs**, then Security) records, with the time and the address it came from:

- every sign-in, sign-out and failed sign-in, and every request the dashboard refused;
- every request for your OK, who answered it (you on the dashboard, the terminal, Discord), and what they answered; ones that expired or were turned down when Tenant stopped;
- every change to what your agent may do, and where it was made;
- every reveal of a hidden value, export and clear of a log, and change to a log's settings;
- every key stored or removed, by name only: the key itself is never recorded.

The Security log can't be cleared or switched off.
