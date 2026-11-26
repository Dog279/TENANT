# Running a fleet: a master and its machines (TEN-531)

A **master** is a Tenant that owns other machines. A **child** is the same `tenant` binary
run as `tenant child` on a machine you own: no model, no agent, no dashboard. It dials the
master, holds one link open, and serves that machine's tools to the master's agent. Every
approval is decided on the master.

Design: `docs/plans/gap6-fleet-plan.md`. The link, enrollment, the `fleet_*` tools,
per-machine approval schemes and the child's ceiling, pause and audit log came first (TEN-531);
a machine's memory second (TEN-532); updates over the link, the dashboard's Fleet page, doctor's
checks and re-homing a machine after the master is lost last (TEN-533).

Peering (`tenant peer`) stays what it is: two Tenants with two operators. A machine is a peer
or a child, never both.

## What you are setting up

Whoever controls the master controls every child, with the rights each child was installed
with. The master's data and config dirs, its dashboard login and its relays are the keys to
the fleet. Everything a child returns (command output, file contents, what it says about
itself) is untrusted text to the master's agent.

## 1. Make a Tenant a master

Run the master as `tenant serve` (as its service: `tenant service install`), so its machines
stay connected. A terminal app alone holds the fleet only while it is open.

```
tenant fleet init --listen 0.0.0.0:9777 --address master.example.net:9777
```

- `--listen` is where the one fleet port binds (`fleet.listen` in `config.json`; the default
  is `0.0.0.0:9777`). Children dial in, so it is not a loopback address on a real fleet. On a
  Windows master, expect Windows Firewall to ask about the first listener on an address that
  is not loopback; allow it for the networks your machines are on. (Not yet tried on a real
  master: the tests listen on loopback only.)
- `--address` (repeatable) is what the machines dial. Without it `init` offers this
  computer's name and its IPv4 addresses. Change the list one address at a time with
  `tenant fleet address list | add HOST:PORT | remove HOST:PORT` (or replace it whole with
  `init --address` again); a connected machine is told the new list over its link. When every
  IP address on the list is private, link-local or loopback, `init`, `enroll`, `address` and the
  dashboard's Add machine say so: a machine at another site cannot dial them. A host name is
  not judged.
- Nothing restarts: a running `serve` (or terminal app) sees the new identity within a few
  seconds, takes the fleet, and says `fleet: listening on …`. One process holds the links;
  another on the same dirs says which (`/fleet`, `tenant fleet list`).

The same init runs inside a running Tenant, from either of its own screens:

- **The terminal app:** `/fleet init [--listen ADDR] [--address HOST:PORT]...`. The app steps
  aside while the recovery identity is shown on the terminal and typed back (the same step as
  the command's), then comes back and says what `init` says. The identity is never in the
  app's chat, its transcript or a log. Like every `/fleet` change it is typed at the keyboard.
- **The dashboard:** the Fleet page of a Tenant with no fleet has **Set up the fleet**, with
  the fleet port's address and the addresses machines dial filled in as `init` would choose
  them. It takes a signed-in browser (never the API key) and makes everything but the
  recovery key: that identity is not shown in a browser. Until `tenant fleet init` or
  `/fleet init` at a terminal makes it, the page says no machine's memory is kept.

Nothing answers on the fleet port but the pinned link and the enroll endpoint, both TLS 1.3.

### What the master records

The master's fleet has a log of its own, `fleet`: `tenant logs query log:fleet`, `/logs
log:fleet` in the terminal app, or the dashboard's Logs page. It starts the moment this Tenant
becomes a master, and a Tenant with no fleet has none.

| In the fleet log | Events |
|---|---|
| this Tenant became a master; the addresses machines dial; the relay switch | `fleet/1001`, `5001`, `5002` |
| the port open and closed (one activity, with its uptime), held by another process, not started | `fleet/1`, `2`, `3001` |
| a code issued, a machine confirmed, renamed, revoked; a machine enrolled and waiting | `fleet/1002`, `1003` |
| an enrollment or a confirmation refused; a spent code presented again | `fleet/2101`, `2102`, `3101` |
| a machine connected, disconnected and why | `fleet/1101`, `1102` |
| each call on a machine, with the hash of the machine's own audit record | `fleet/1201` |
| each step of an update, and one that did not go through | `fleet/1301`, `2301` |
| the machines' memory: open or unavailable, copies, forgets, and what went wrong | `fleet/3`, `2003`, `1401` to `1407`, `2401` to `2414` |

The Security log keeps the audit record of each operator change and each refusal
(`security/4321` to `4324`): who made it and from where. No line in either holds an enrollment
code or the recovery identity. A change made by a `tenant fleet` command while nothing runs
is still written to the fleet log before the command returns. `docs/EVENTS.md` has every
event's meaning, cause and what to do.

A child machine's own process has no log book: it keeps its audit log and writes to its own
log and the system's.

## 1a. Where the machines reach the master: three routes (TEN-541)

A machine always dials the master; the master never dials a machine. So only the master needs
to be reachable, on its one fleet port, at an address on the list a code carries.

- **On one LAN.** The master's LAN address (`192.168.1.5:9777`) is enough. This is what `init`
  guesses. The warning about private addresses is expected here and can be ignored.
- **Across sites, over an overlay network (recommended).** Put the master and the machines on
  WireGuard or Tailscale, make the master listen only on its overlay address, and give that
  address to the machines:

  ```
  tenant fleet init --listen 100.101.102.103:9777
  tenant fleet address add 100.101.102.103:9777
  tenant fleet address remove 192.168.1.5:9777
  ```

  Nothing is exposed to the internet, and nothing new is trusted: the overlay is only a route.
- **Across sites, with a public address or a forwarded port.** This is fine too: both ends are
  pinned by certificate (the code carries the master's fingerprint, and the master accepts only
  certificates it pinned at enrollment), so the address is only a route and whoever else holds
  it learns nothing and can do nothing with it. Say plainly what then faces the internet: the
  TLS 1.3 handshake on the fleet port, and the enroll endpoint's check of a code (five attempts a
  minute per address, a body cap, refusals in the Security log). Keep in mind that the fleet port's
  default listen address is every interface (`0.0.0.0:9777`): on a master with a public
  address, set `--listen` to the one interface you mean.

There is no discovery and no request to join: a machine joins by its one-time code and nothing
else. A machine that found a master on the network would know nothing about it, so whatever
answered first could capture it, and the six digits cannot fix that alone, because they only
prove both ends once the code has already pinned the master. A sound request to join needs a
password-authenticated key exchange or digits committed before they are revealed: new protocol,
not this version.

## 2. Enroll a machine

On the master, at the command line, in the terminal app (`/fleet enroll web1`), or on the
dashboard's Fleet page (**Add machine**: a name, and whether it runs as you or from boot):

```
tenant fleet enroll web1            # prints a one-time code (15 minutes; --ttl), and the lines to paste
```

It prints the code alone on standard output, then (on standard error) when it expires, the
addresses it carries, and one line for each kind of machine. For a machine with nothing on it,
the line fetches the install script from the latest release, checks the download against the
release's checksums, and goes straight on to `tenant child install`:

```
# Linux, macOS: the child runs as you, from logon
curl -fsSL https://github.com/OWNER/REPO/releases/latest/download/install.sh | TENANT_CHILD=user TENANT_ENROLL_CODE='<code>' sh
# Linux, macOS: a service that runs as root from boot (sudo asks for your password)
curl -fsSL .../install.sh | TENANT_CHILD=system TENANT_ENROLL_CODE='<code>' sudo --preserve-env=TENANT_CHILD,TENANT_ENROLL_CODE sh
# Windows PowerShell: as you; for SYSTEM from boot, 'system' in a PowerShell run as administrator
$env:TENANT_CHILD='user'; $env:TENANT_ENROLL_CODE='<code>'; irm https://github.com/OWNER/REPO/releases/latest/download/install.ps1 | iex
```

For a machine that has Tenant already:

```
printf '%s\n' '<code>' | tenant child install            # Linux, macOS
'<code>' | tenant child install                          # PowerShell
```

Add `--system` (in a root shell, or a PowerShell run as administrator) for a service that runs
as root or SYSTEM from boot; without it the child runs as your account from logon.
`--ceiling full|no-exec|read-only` sets what the machine lets its master do (below). The code is
read from standard input or `$TENANT_ENROLL_CODE`, never from an argument: no line puts it on a
command line the process list shows. A build of Tenant that names no release repository cannot
print the install-script lines; it says so and prints the others.

The dashboard's Add machine shows the code in the answer to that one request, with a Copy
button per line, and nowhere else: not in the address bar, not after a reload, not in any log.
The Security log records that a code was made, for which name and by which dashboard session.
The API key cannot make a code.

**That is all.** A machine that presents a code that is valid, unused and not expired is
confirmed as it enrolls (TEN-553). It gets its approval scheme (below), the operator is told
where they look (the terminal app's feed, serve's log) with what the machine said about itself
and the address it came from, and the Security log has the record, with `enroll-code` as its
surface and Tenant as its actor. `tenant fleet revoke web1` undoes it.

The code is the whole check, so treat it as a password for its fifteen minutes: paste it on
the one machine it is for, and nowhere a second person or a log can read it. Whoever uses it
first joins under that name. If a code is presented a **second** time, the second machine is
refused, an alert is raised, and the machine that code enrolled is **paused** until you have
looked: resume it if it is yours (`tenant fleet resume web1`), revoke it if it is not. You can
pause a machine yourself the same way (`tenant fleet pause web1`): no call goes to a paused
machine and its link is refused, while its certificate, scheme and memory stay.

### Asking for six digits as well (`fleet.confirm`)

For a fleet where the code may pass through hands you do not trust, turn the second step on:

```
tenant fleet digits            # says whether it is on
tenant fleet digits on         # every machine that enrolls from now on waits for its six digits
tenant fleet digits off        # back to the code alone; asks you to type "off" at a terminal
```

Turning it on needs nobody. Turning it off again takes a person at a terminal, so a script or
the agent cannot take the second step away with this command. It is `fleet.confirm` in
`config.json` (`"digits"`; empty or `"auto"` is the default, and any other value is read as
`digits`); the running Tenant reads it at each enrollment, so no restart is needed. With it on, the machine prints **six digits** and shows as
`pending` on the master, and until you confirm it the master sends it nothing:

```
tenant fleet confirm web1           # at a terminal: shows what the machine reported, asks for the six digits
```

The digits come from the two certificates of the connection the machine enrolled over, so they
show that the machine which used the code is the one whose screen you are reading. Confirming
takes a person: at a terminal, with `/fleet confirm web1 <digits>` typed in the
terminal app, or on the dashboard's Fleet page (the machine's own page, **Confirm this
machine**) in a browser signed in on the master itself, at its 127.0.0.1 address. There is no
flag for the digits, no tool and no relay command, and a request that carries only the API key
is refused. The screen shows what the machine said about itself (OS, account, version, sandbox,
ceiling: its own words, not checked) and the approval scheme it will start with:

- a copy of the master's local scheme: what the master allows unasked, the machine does too
  (to start every machine tighter, set a fleet default: `tenant fleet permissions --defaults`);
- the fleet's default categories and rules for new machines laid over it (`tenant fleet
  permissions --defaults`), and the rules of each group it is already in (section 3a);
- anything you set here: `tenant fleet confirm web1 --set exec=allow`.

Each line of the screen says where its value came from when it is not the master's own:
`set by you`, `fleet default`, `fleet rule` or `group <name>`.

The enroll endpoint takes five attempts a minute from one address. A code that is presented
twice is refused and raises an alert: compare the digits on the machine's own screen.

### Confirming from another computer (TEN-545)

This applies only with `fleet.confirm: digits`; by default there is nothing to confirm. A browser that reaches the dashboard over the LAN, by a tailnet name or through a proxy is not
"on the master": its Fleet page shows the pending machine and says where to confirm it. On a
master with no screen (Windows Server Core, a headless box) that means a console for every
machine. You can let a signed-in browser elsewhere confirm instead. It is off until you turn
it on, at the master's terminal:

```
tenant fleet remote-confirm          # says whether it is on
tenant fleet remote-confirm on       # asks you to type "on"; also /fleet remote-confirm on, in the terminal app
tenant fleet remote-confirm off
```

Turning it on takes a person at a terminal: the command asks, and refuses when its input is
not a terminal, so a script or the agent cannot turn it on with this command. Turning it off
needs nobody. It is `fleet.remote_confirm` in `config.json`; the running Tenant reads it at
each confirmation, so no restart is needed. There is no switch for it on the dashboard. While
it is on:

- the pending machine's page offers the form to a signed-in browser elsewhere, with one more
  field: **the dashboard's sign-in key, again**. A wrong key changes nothing, counts against
  that address as a wrong sign-in does, and is a Security record;
- a request that carries only the API key is still refused, and the wait after wrong digits is
  the same;
- the Security log records the confirmation and every refusal with the surface
  `dashboard-remote` and the session and address that asked. The fleet log has the
  confirmation and the refusals the fleet itself makes (wrong digits, a machine that is not
  waiting); a wrong or missing key is refused by the dashboard and is in the Security log
  only. Behind `tailscale serve` or a proxy that address is always this computer's own, so the
  record also quotes what the proxy said of who it passed on (the tailnet login), marked as
  not checked.

What you give up:

- At the master, the digits show that the person typing them can see both screens. From
  elsewhere they show only the machine. Read them from the machine's own screen yourself, or
  have someone you trust at it read them to you.
- **Whoever holds the sign-in key can confirm a machine from anywhere the dashboard is
  reached.** The key again stops a session that leaked or a browser left signed in. It does
  not stop the key's holder, and the API key that scripts use is the same key: a script that
  signs in with it looks like a browser. Do not turn this on while a script or a log shipper
  on another machine holds the key.

Leave it off on a master you can sit at.

## 3. Work on it

Once a confirmed machine exists the master's agent has the `fleet_*` tools and one line per
machine in its context (name, OS, online or not). `host` is always a machine's name.

| Tool | On the machine |
|---|---|
| `fleet_list` | the machines: OS, account, online since |
| `fleet_exec`, `fleet_process` | run a command; its background commands, and `result` for a call whose link dropped |
| `fleet_read`, `fleet_list_dir` | read a file, list a folder (no card; the machine refuses its secret files) |
| `fleet_write`, `fleet_edit`, `fleet_copy` | change a file; copy one text file between the master and the machine |
| `fleet_tools`, `fleet_call` | what else the machine serves (browser, SQL, wiki, local MCP servers), and a call to it |

Every shell command on a machine asks, on the master, until you answer **always**: that
saves a rule for that machine alone. It does not carry to another machine or to the master.
A card for a machine names it: the detail starts `[host <id> "<name>"]`, and the dashboard's
headline says "on your fleet machine <name>".

```
tenant fleet permissions web1                               # the machine's scheme
tenant fleet permissions web1 add "os_exec(systemctl status *)" allow
tenant fleet permissions web1 --from defaults|master        # lay the fleet's and its groups' rules over it again; copy the master's again
tenant fleet permissions --defaults add "os_exec(uptime)" allow   # what every newly confirmed machine starts with
tenant fleet permissions --defaults set web deny            # a category every newly confirmed machine starts with
tenant fleet permissions --group web set exec ask           # the same change to each member of a group (--all: every confirmed machine)
```

A relay (Discord, iMessage) or a cron job reaches no machine, reads included, until you open
it: `tenant fleet relay discord on`; `/cron fleet <job-id> <machine>...` in the terminal app.

What comes back from a machine is framed (`[fleet web1 (m…, linux): os_exec. …]` down to
`[/fleet …]`) and labelled untrusted. Text one machine returned that turns up in a later
outbound call meets the egress check; after a turn has read from one machine, anything but a
plain read on another asks, whatever that machine's scheme says.

A picture a tool makes on a machine (a screenshot, an image file read) stays there: the link
carries text. The result says so.

## 3a. Groups and default categories (TEN-546)

A machine's scheme is its own. Nothing the fleet holds is read when a call is decided: a call
on web1 is decided by web1's stored scheme and nothing else. Two things are **laid over** a
scheme on purpose, at a step you take, and each laying has one Security record per machine:

- the **defaults for new machines**: categories you set (`fleet_permissions`; a category you
  did not set comes from the master's copy) and rules (`fleet_rules`);
- **groups**: a name, its machines (by id; a machine may be in any number of groups, in any
  state) and rules. A group has no categories.

**The layering order.** A machine's *fresh copy* (what it gets when it is confirmed, or copied
again with `--from master`) is built in this order, each layer winning over the one before it
where they disagree:

1. a copy of the master's local scheme, as it is;
2. the fleet's default categories: each one you set replaces the copy's mode;
3. the fleet's rules;
4. the rules of each group the machine is in, groups in ascending name order (compared without
   case): a later group wins over an earlier one, and any group wins over a fleet rule.

A rule laid over a scheme replaces every rule of the machine that it answers for (the same
pattern, a narrower command under its prefix, a tool its glob covers) and leaves the machine's
categories alone.

**Which layer wins after that.** Once laid, the scheme is the machine's: an edit made to it
(a category, a rule, an "always" on its card) stands, over every layer, until a layer is laid
over it again. Changing a group (its rules or its machines) or a default **moves no machine**;
the answer says so and names the step that does.

```
tenant fleet group create web                       # names: letters, digits, . _ -; not "all", not a machine id
tenant fleet group add web web1 web2                # machines by name or id
tenant fleet group rules web add "os_exec(systemctl restart nginx)" allow
tenant fleet group                                  # the groups; show <group> for one
tenant fleet group apply web                        # lay the fleet's rules, then each member's groups' rules, over each member
tenant fleet group remove web web2
tenant fleet group delete web                       # its members keep what it laid
tenant fleet permissions --defaults set exec ask    # unset <category>: back to the master's copy
```

**Apply** (`tenant fleet group apply`, and `--from defaults` for one machine or with
`--group`/`--all`) lays the fleet's rules and then the rules of all of that machine's groups,
in name order, over the scheme it has; its categories stay. A member that is pending, revoked
or whose scheme cannot be read is skipped and named in the answer, and the command exits 1.
A laid rule replaces every rule of the machine's own that it answers for, **a block included**:
a group rule `os_exec(git *)` allow removes the machine's `os_exec(git status)` deny. The
answer and the machine's Security record say which rules were replaced and what they were.

**A pending machine in a group** starts with the group's rules when it is confirmed, and the
confirm screen shows them (`group <name>`). The confirmation lays what that screen showed: if a
default, a fleet rule or one of its groups is edited between the screen and the digits,
`tenant fleet confirm` and the dashboard's form confirm nothing and say to read it again. The
terminal app's `/fleet confirm <name> <digits>` is one line with no screen before it in the same
step, so it is not held to one: read `/fleet confirm <name>` just before. The record of a
confirmation names the defaults and the groups it laid, on every surface.

**Bulk.** `tenant fleet permissions --group <group> | --all` takes the same edits as one
machine (`set`, `add`, `rm`, `--from defaults|master`) and makes them to each machine in turn,
by name, through the one-machine path: one line and one Security record per machine changed,
none for the batch, never stopping at a failure, exit 1 if any machine was not changed. `--all`
is every confirmed machine. `tenant fleet update --group <group>` updates the members one after
the other, as `--all` does.

**Revoking.** `--delete-scheme` takes the machine out of every group; `--keep-scheme` keeps its
memberships, for a machine that adopts the id.

**When a key can't be read.** `fleet_permissions` and `fleet_groups` are read strictly, like
`fleet_rules`: an unknown category, a mode that is not ask/allow/deny, a key twice (group names
differing only in case are the same name), null, trailing text, a member that is not a machine
id, or over 256 groups, 4096 machines or 512 rules in a group makes the whole key unreadable.
Then no machine can be confirmed or copied again (and, for `fleet_groups`, no group applied
or changed), the operator is told once, and the text is kept as written for you to fix. Machines that have a
scheme keep deciding by it.

The terminal app has the same verbs: `/fleet group …`, and `/permissions fleet --defaults
set|unset|add|rm`. The command line and the terminal app take a loosening (allow) without a
second step, as for one machine.

### On the dashboard's Fleet page

The Fleet page does all of this with the same operations and the same records, with
`dashboard` as their surface:

- **The fleet's defaults:** each safety category as Ask, Allow, Deny or **From this Tenant**
  (unset; the page says what this Tenant's own setting gives now), and the fleet's
  rules with the editor a machine's page has.
- **Groups:** make one, open it to add or take out machines and rules, **Apply** it to its
  members, delete it. The list can be narrowed to one group.
- **Many machines at once:** tick machines in the list (or choose everyone shown, which the
  master works out from the group you narrowed to, never from what the browser sent), then set
  a category, add or remove a rule, apply the fleet's rules, copy again, or update. The answer
  says "Done on 5 of 7." and names each machine that was not changed, with the reason.
- **A machine's page** uses the same controls and words as this Tenant's own Approvals & safety
  and Tools pages, and lists the groups the machine is in.

What asks first: anything that lets your agent do more on machines that exist (Allow, an allow
rule, removing a rule that asked or blocked), anything that replaces settings (Apply, Copy
again), an update, a default of Allow or From this Tenant, an allow rule in the fleet's
defaults, and deleting a group. For many machines the step is a page that says what will
happen, to **how many machines**, lists them by name, and lists those left out and why; only
then does a second button do it. A rule added to a group asks nothing: it moves no machine
until the group is applied, and Apply asks. Tightening (Ask, Deny, a block rule) and Stop go
through at once.

These changes take a signed-in browser. The API key gets no more than it had: it can still
update one machine (`tenant fleet update` uses it), and it makes none of the changes in this
section.

## 4. On the machine

```
tenant child status      # connected / waiting for confirmation (with the digits) / paused / rejected / offline; exit 3 unless connected
tenant child pause       # drops the link, stops running calls and background commands; survives restarts
tenant child resume
tenant child audit       # checks the chain of the audit log of the master's calls
tenant child uninstall   # removes the service and the machine's key; the audit log stays
```

`child.json` (beside the key, owner-only) holds the **ceiling**. It is read from the disk on
every call, so an edit takes effect at once, with no restart:

- `full`: the master decides.
- `no-exec`: the machine refuses everything that runs a command, without asking the master.
- `read-only`: the machine refuses every action that would need an approval.

`"tools": ["os_read_file", …]` limits what the machine serves at all; `"secret_paths"` adds to
the files its file tools refuse. A refusal the machine makes by itself ends the result with
`[fleet: this machine refused, without asking its master: os_exec (ceiling read-only)]`; the
operator on the master is not asked.

`no-exec` and `read-only` are limits the master cannot lift. `full` with the host sandbox is
advisory: a command runs as the account that owns `child.json`.

Every call is in the machine's audit log (`<child dir>/audit/audit.jsonl`, hash-chained, and a
line in the system log) before it runs. The master's own record of the call (`fleet` log,
event `fleet/1201`) carries the hash of that record, so the two can be laid side by side.

## 5. Revoking a machine

```
tenant fleet revoke web1                    # at a terminal: asks whether to keep or delete its scheme
tenant fleet revoke web1 --keep-scheme      # the record and the scheme stay under its id
tenant fleet revoke web1 --delete-scheme    # both go
```

From then on the master refuses the machine's certificate. The process that holds the fleet
sends the machine a "revoked" notice and closes its link within a second; the machine's child
stops and does not start again. A machine revoked while it was offline is refused at its next
handshake: its `tenant child status` says the master rejected it, and it keeps trying, slowly,
until it is uninstalled. A kept scheme can be given to a machine enrolled later:
`tenant fleet enroll <name> --adopt <id>`. A new machine under an old name starts clean
otherwise.

## 5a. Updating a machine (TEN-533)

```
tenant fleet update web1                       # the latest release
tenant fleet update web1 --version v1.4.0      # that release
tenant fleet update --all                      # every confirmed machine, one after the other
tenant fleet update web1 --version v1.3.0 --allow-older
tenant fleet update old1 --arch arm64          # a machine too old to say its processor
```

The command asks the running Tenant that holds the fleet (through its dashboard, with the
dashboard's token; `--hub ADDR` when the dashboard is not where `config.json` says), which has
the links. That Tenant fetches the release for the machine's system and processor into
`<data>/fleet-updates/`, verifies it as `tenant update` does, and sends it with its SHA-256.
Each step is printed:

```
web1: fetching v1.4.0 for linux/amd64
web1: fetched tenant_1.4.0_linux_x86_64.tar.gz (41.2 MB): verified checksum only (...)
web1: sent 41.2 MB; the machine checked its SHA-256 and is applying it. The calls it is running finish first, and new ones wait until it is back. Its background commands end at the restart
web1: the new version reached the master in its trial run; the machine is putting it in place
web1: back on the new version 1.4.0
```

or `web1: rolled back to "1.3.2": "<why, in the machine's words>"`, `web1: refused: …` (offline,
not confirmed, an older version without `--allow-older`), or `web1: failed` with the reason.
The exit is 0 only when every machine runs the version asked for.

**Calls during an update (TEN-552).** A machine can be updated while the agent works on it.
From the moment the machine says it is applying the update:

- The calls it is running finish first. The machine gives them up to 2 minutes before it drops
  its link; a call still running then ends with the restart, and its outcome is not known.
- New calls to it are not sent. The master keeps them back until the machine is linked again
  and says how the update went (done, or rolled back), and then makes them: on the new version,
  or on the old one after a rollback. A call waits at most 7 minutes this way; after that it is
  made, and fails as offline if the machine is still away. Each call that waited is in the fleet
  log (fleet/1206) with how long.
- Background commands and the machine's browser still end at the restart.

Both halves need the new Tenant: the master's to keep calls back, the machine's to let its
running calls finish. A machine on an older Tenant still drops its link a second after it has
the update, so the first update to this version can cut off a call that is running.

**Whose word the machine takes.** By default a machine installs only a release
it verified itself: with the archive the master sends the release's `checksums.txt` and its
signature bundle, on the first message of the transfer, before any of the archive. The machine,
with no network, checks the archive's name against its own system, processor and the version
named; verifies the signature over the checksums with `cosign verify-blob --offline` against
the release workflow of the repository its own binary was built from (never a name the master
or the environment gives it); and checks that the verified checksums list the archive with the
SHA-256 the master named. The whole archive is then checked against that hash. Anything that
does not hold is refused before the archive is sent, nothing is kept, and the master shows the
machine's reason. So a master, or a master's disk, that changed an archive cannot install it.
This needs `cosign` on the machine's PATH, a release that carries a signature bundle, and a
release build of Tenant on the machine; a development build names no release, and refuses every
update on the default rule (its `tenant child status` says so).

**The project key (TEN-542).** A release binary of the main repository also carries the
project's Ed25519 public key, and the main releases carry `checksums.txt.sig`, a signature over
the checksums with that key (SECURITY.md, "Release signatures"). The master sends it with the
checksums and the bundle. A machine whose own Tenant carries the key verifies that signature
with nothing installed, and then it alone decides: if it verifies the release is taken (cosign
is not needed); if it is malformed or does not verify the update is refused at once, without
trying cosign. When the master sent no project signature (its Tenant is older, or the release
carries none) the machine falls back to cosign as above, and says so in its reason when cosign
is not there either. A machine whose Tenant carries no key (a fork's release) ignores the
project signature and needs cosign. `tenant fleet update` says which check the machine passed:

```
web1: the machine verified the release's signature itself, with the project key its own Tenant carries, and takes the archive only if it hashes to what that release lists
web1: the machine verified the release's signature itself, the release workflow's signature, against the release its own Tenant was built from, and takes the archive only if it hashes to what that release lists
```

The other checks keep their order: the archive's name is the machine's for the version named,
and the verified checksums list it with the SHA-256 the master named. A master whose own binary
carries the key checks the project signature itself when it fetches the release, as `tenant
update` does, and sends nothing when it does not verify. Nothing changes for a machine set to `updates master`.

To run a build of your own on a machine, set it to take its master's hash instead:

```
tenant child install --updates master        # at enrollment
tenant child updates master                  # later, on the machine, with its child stopped
tenant child updates release                 # back to the default
tenant child updates                         # which one is in force (also in `tenant child status`)
```

The rule is in `child.json` (`"updates": "master"`; the default is written as nothing). It is
set only on the machine, never over the link; `tenant child rehome` keeps it; a change is a
record in the machine's audit log. `tenant fleet update` says for each machine which rule it
applied. A machine from before this setting takes its master's hash. Downgrades are refused on
the master unless `--allow-older`, as before; a signed older release is still a genuine release,
and a machine on the default rule takes it.

On the machine: the archive goes to `<dir>-data/update/<sha256>.part` (a dropped link resumes
there), and nothing else is touched until it is whole and hashes right. The binary is unpacked
beside the installed one (`.tenant-child-update/`) and run once with `version`. Then the child
drops its link and its background commands, and runs the new binary as `child run --trial`;
when that reaches the master (`child-update.json` step `proven`) it stops it, swaps the
binaries (the old one stays as `tenant.prev`), and restarts into the new one: `exec` in place
on Linux and macOS; on Windows the old process stays as a keeper of the new one, so the
service manager and the logon task see no change. The new version marks the update done at its
first hello; if it does not reach the master by the deadline in `child-update.json` it puts
the old binary back itself. A trial that exits or does not reach the master within a minute is
deleted, and the machine reconnects on the old version. `tenant child status` shows the last
update, and an update in the middle.

### A rollout: many machines, a few at a time (TEN-546)

On the dashboard's Fleet page, tick machines (or narrow the list to a group and choose everyone
shown), pick **Update**, a version (empty is the latest release) and how many update at once (1
to 8). The page first says how many machines it will update and names them, with those it
leaves out and why (pending, revoked, offline: nothing is queued). After the second button the
running Tenant that holds the fleet starts that many machines, in the list's order, and starts
the next each time one ends:

- a machine that ends **done** is updated; one that already runs the version asked for counts
  as done, and nothing is sent to it;
- a machine that is **refused** (offline, not confirmed, or the version asked for is older
  than the one it runs and "allow an older version" is not ticked) is skipped with its reason,
  and the rollout goes on;
- the first machine that **rolls back or fails stops the rollout**: no further machine is
  started, those already updating finish, and the rest show as not started.

The Fleet page shows the rollout while it runs and afterwards (reload it): each machine as
waiting, its live stage, done, skipped and why, rolled back, failed or not started. **Stop**
starts no more machines; those updating finish. So does the fleet port going down, or another
process taking the fleet: the rollout starts nothing over links it no longer holds. One rollout
runs at a time. It is kept in the
running Tenant's memory: a restart of the master forgets it, though each machine still finishes
or rolls back by itself. `tenant fleet update --all` and `--group <group>` stay as they were:
one machine after the other, going on past a failure.

## 5b. Losing the master (TEN-533)

A master restored from an **encrypted** `tenant backup` has its certificate, `fleet.json` and
the fleet key back: its machines reconnect by themselves. A master restored from a **plain**
backup (`tenant backup` warns when it makes one on a fleet master), or rebuilt from nothing, is a
stranger to its machines: `tenant child status` on each says "the master rejected this
machine", or "offline". Each machine is re-homed, on the machine:

```
# on the new master
tenant fleet init                        # at a terminal: a new fleet key and recovery identity
tenant fleet enroll web1                 # prints a one-time code

# on the machine: stop its child first (its service, or the `tenant child run` holding it)
TENANT_ENROLL_CODE='<code>' tenant child rehome        # or: printf '%s\n' '<code>' | tenant child rehome
#   add --system (from an elevated shell) for the machine's own child
# start its child again. With fleet.confirm: digits on the new master, then there:
tenant fleet confirm web1                # the six digits rehome printed
```

`rehome` is enrollment with the key the machine already has. It changes only which master the
machine trusts (the id, name, addresses and certificate fingerprint in `child.json`, replaced
in one step); its key, ceiling, tool list, sandbox, secret list, pause, audit log and memory
copies stay. It refuses, before the code is read and with nothing changed, while a child
process runs on the directory, on the machine's own child from a terminal that is not elevated,
and for an account that may not write the child's files; a code that does not enroll leaves the
machine its old master's. The new master confirms the machine by its code, or (with
`fleet.confirm: digits`) holds it as pending until you type its digits, as for any new
machine. From then on the old master's certificate is refused, whatever
address it answers at. The audit log gets a `rehome` record: the account, and the old and new
masters' fingerprints. Nothing on the link, no tool and no master-side command makes a machine
rehome.

The machine's memory copies were sealed to the old fleet's key and recovery key, so the new
master shows the machine's memory as **needs recovery key** and reads none of it. Bring the old
fleet's recovery identity (the one the old master's `fleet init` showed you):

```
tenant fleet recover web1 --was m0123456789abcdef    # the id rehome printed as "it was …"
```

It reads the identity at the terminal without showing it, or from standard input; never from an
argument. The running Tenant that holds the fleet (through its dashboard, as `fleet update`;
`--hub ADDR` when it is not where `config.json` says) fetches the machine's newest copy under
the old key, reads it with the identity, makes it the machine's memory and sends it back as a
generation under the new key. The old copies stay on the machine. The identity crosses only a
connection nobody on the way can read: the command sends it over plain http only to this
computer (a loopback address, or a name that resolves to loopback alone), otherwise only to an
`https://` `--hub`, and refuses before it reads the identity; the running Tenant takes it only
from a loopback peer or over TLS. On another computer, run the command on the master itself. `--was` is needed because the
new master gave the machine a new id and the copies name the old one; without the right id, or
with another fleet's identity, nothing is read. The same command reads a machine's memory after
the master's fleet key file was replaced (then without `--was`).

What the recovery reads cannot be checked the way a copy of this master's is. Every snapshot a
master makes ends with an authenticator only its fleet key's secret makes, and a copy under the
master's own key without it is refused unread. The old fleet's secret is gone, so a copy under
the old key vouches for nothing but the recovery key's public half, which the old master kept in
`fleet.json`: anyone who had that file could have made it. So whatever the recovered copy says
the master recorded (its inventory, notes made with `fleet_note`) is recalled from then on as
the machine's own words, inside its frame, under "Records from a copy of its memory that this
master did not make", and the new master takes the machine's inventory again. Look at those
records before relying on any of them.

### Forget on the machines

`tenant privacy forget` leaves a request beside the machines' memory, sealed to the fleet key
and the recovery key, since it names what is to be forgotten. The Tenant that holds the fleet
deletes it from each machine's memory and from what was kept aside, sends the machine a new
copy, and tells it to delete its older ones; a machine that is offline is told when it is back.
A memory rebuilt from a copy older than the forget has it applied again, for as long as the
request is kept. The request is kept until every machine still in the fleet has had it, has
dropped its older copies and keeps no copy under another fleet key; then it is deleted, and
`<data>/fleet/forgets.age` with it when it was the last. A machine enrolled after the forget
does not hold it back, and one revoked since no longer does.

That has a price. Once the request is gone, a copy older than the forget that comes back (the
machine restored from a backup of its own disk) is neither purged nor looked into: newer copies
push it out in time, but if this master loses its own memory of the machine while that copy is
the machine's newest, the memory is rebuilt from it and holds what was forgotten. Forget again
then. Copies a machine keeps under another fleet key are never deleted by a forget: delete them
on the machine (the `memory` folder in its child directory); until they are gone the request is
kept, and a copy under another key that turns up after the request was deleted brings nothing
back unless `tenant fleet recover` reads it.

## 6. When a link drops

Nothing is pushed to you. A link that goes silent is given up on after two missed pings (30
seconds each), so within a minute:

- the agent's context says the machine is offline, and `fleet_list` says since when;
- `/fleet` in the process that holds the fleet shows `offline, last seen <time>`, and
  `/fleet show <name>` there adds why the last link ended (`tenant fleet` in another process
  cannot tell online from offline);
- the `fleet` log has "Machine <name> disconnected" (`fleet/1102`) with the reason;
- `tenant child status` on the machine stops saying "connected to the master" and says
  "offline since …, trying again".

A call made while the machine is offline fails at once; nothing is queued. A call in flight
when the link drops returns "OUTCOME UNKNOWN" (below). The machine dials again by itself and is
online as soon as a link is made; there is no command to bring it back.

## 7. When things go wrong

| You see | It means |
|---|---|
| `fleet: <name> is offline: offline since …` | no link. Nothing was sent, nothing is queued. A dead link is noticed within a minute (two missed 30 s pings). |
| `fleet: OUTCOME UNKNOWN … call <id>` | the link dropped mid-call. The call was not sent again. When the machine is back: `fleet_process {"host":…,"action":"result","id":"<id>"}`. The machine keeps the last 32 results (2 MiB at most) for ten minutes; a pause or a restart of its child drops them. The id of a call is also in the machine's audit log. |
| `tenant serve (process N) holds the fleet, on …` | this process does not hold the links; work from that one. |
| `child status`: "the master rejected this machine" | its certificate is not pinned on the master (revoked, or the master was rebuilt), or the master cannot read `fleet.json` just now. The child keeps trying, slowly. |
| `child status`: "it is revoked" | the master revoked it. The child has stopped (exit 0) and will not connect with this key again: uninstall and enroll anew, or `tenant child rehome` with a code from the master it should trust now. |
| memory "needs recovery key" (`fleet show`, `/fleet`, the Fleet page, doctor) | the machine's copies, or this master's own snapshot, were made under another fleet key. Nothing is overwritten. `tenant fleet recover <name>` with that fleet's recovery identity (section 5b). |
| doctor: "the recovery key was never proven" | `tenant fleet init` at a terminal asks for the recovery identity you kept and records the proof once a test decryption opens with it. |
| every call to one machine refused: "has no approval scheme", or "its approval scheme can't be read" | its entry in the settings file (`fleet_schemes`) is missing or did not load. `tenant fleet permissions <name> --from master` writes it again. |

The master was killed or restarted: its machines reconnect by themselves (the wait doubles
from one second to a ceiling of one minute). A command that was running on a machine runs on;
ask for its result as above. A machine's child that restarts ends its background commands.

## 8. Running it for a test

The suite's own fleets are real processes on loopback ports (`cmd/tenant/fleet_e2e_*_test.go`):

```
go test ./cmd/tenant -run 'TestFleetEndToEnd$' -v            # a master, two machines, the acceptance list
TENANT_FLEET_E2E=long    go test ./cmd/tenant -run TestFleetAnUnreachableMaster -v
TENANT_FLEET_E2E=browser go test ./cmd/tenant -run TestFleetBrowserOnAChild -v     # starts one real browser
TENANT_FLEET_E2E=spike   go test ./cmd/tenant -run TestFleetIdleSpike -v -timeout 45m   # 100 child processes
```

On Windows with WSL, `TestFleetLinuxMasterWindowsChild` runs a Linux master in WSL with a
Linux and a Windows machine: build the test binary for Linux, copy it into WSL, and set
`TENANT_FLEET_E2E_LINUX` to its path as WSL names it (a path under `/tmp`).

By hand, never on the default port of a machine you share: give `fleet init` a loopback
`--listen` first (`127.0.0.1:0` picks a port), use `--config` and `--data` temp dirs for the
master and `--dir` for each child, and start the child with `tenant child run --dir <dir>`
instead of installing a unit.
