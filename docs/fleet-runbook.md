# Running a fleet: a master and its machines (TEN-531)

A **master** is a Tenant that owns other machines. A **child** is the same `tenant` binary
run as `tenant child` on a machine you own: no model, no agent, no dashboard. It dials the
master, holds one link open, and serves that machine's tools to the master's agent. Every
approval is decided on the master.

This is the first of three parts (design: `docs/plans/gap6-fleet-plan.md`). It has the link,
enrollment, the `fleet_*` tools, per-machine approval schemes, the child's ceiling, pause and
audit log. It does not have a machine's memory (TEN-532), `tenant fleet update`, `tenant child
rehome`, the dashboard's Fleet page or doctor checks (TEN-533).

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
  computer's name and its IPv4 addresses. The list can be changed later with `init --address`
  again; a connected machine is told the new list over its link.
- Nothing restarts: a running `serve` (or terminal app) sees the new identity within a few
  seconds, takes the fleet, and says `fleet: listening on …`. One process holds the links;
  another on the same dirs says which (`/fleet`, `tenant fleet list`).

Nothing answers on the fleet port but the pinned link and the enroll endpoint, both TLS 1.3.

## 2. Enroll a machine

On the master:

```
tenant fleet enroll web1            # prints a one-time code (15 minutes; --ttl), and the line to paste
```

On the new machine, with Tenant installed (the install scripts take `TENANT_CHILD=user|system`
and go straight on to this step):

```
printf '%s\n' '<code>' | tenant child install            # Linux, macOS
'<code>' | tenant child install                          # PowerShell
```

Add `--system` from an elevated shell for a service that runs as root or SYSTEM from boot;
without it the child runs as your account from logon. `--ceiling full|no-exec|read-only` sets
what the machine lets its master do (below). The code is read from standard input or
`$TENANT_ENROLL_CODE`, never from an argument.

The machine prints **six digits** and shows as `pending` on the master. Until you confirm it,
the master sends it nothing.

```
tenant fleet confirm web1           # at a terminal: shows what the machine reported, asks for the six digits
```

Confirming takes a person at a terminal (or `/fleet confirm web1 <digits>` typed in the
terminal app). There is no flag for the digits, no tool, no relay command and no dashboard
button. The screen shows what the machine said about itself (OS, account, version, sandbox,
ceiling: its own words, not checked) and the approval scheme it will start with:

- a copy of the master's local scheme, with exec, write, destructive, send and sandbox no
  looser than **ask** whatever the master allows;
- the fleet's rules for new machines laid over it (`tenant fleet permissions --defaults`);
- anything you set here: `tenant fleet confirm web1 --set exec=allow`.

The enroll endpoint takes five attempts a minute from one address. A code that is presented
twice is refused and raises an alert: compare the digits on the machine's own screen.

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
tenant fleet permissions web1 --from defaults|master        # lay the fleet's rules over it again; copy the master's again
tenant fleet permissions --defaults add "os_exec(uptime)" allow   # what every newly confirmed machine starts with
```

A relay (Discord, iMessage) or a cron job reaches no machine, reads included, until you open
it: `tenant fleet relay discord on`; `/cron fleet <job-id> <machine>...` in the terminal app.

What comes back from a machine is framed (`[fleet web1 (m…, linux): os_exec. …]` down to
`[/fleet …]`) and labelled untrusted. Text one machine returned that turns up in a later
outbound call meets the egress check; after a turn has read from one machine, anything but a
plain read on another asks, whatever that machine's scheme says.

A picture a tool makes on a machine (a screenshot, an image file read) stays there: the link
carries text. The result says so.

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
`fleet call`) carries the hash of that record, so the two can be laid side by side.

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

## 6. When a link drops

Nothing is pushed to you. A link that goes silent is given up on after two missed pings (30
seconds each), so within a minute:

- the agent's context says the machine is offline, and `fleet_list` says since when;
- `/fleet` in the process that holds the fleet shows `offline, last seen <time>`, and
  `/fleet show <name>` there adds why the last link ended (`tenant fleet` in another process
  cannot tell online from offline);
- that process's log has `fleet: link ended` with the machine's id and the reason;
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
| `child status`: "it is revoked" | the master revoked it. The child has stopped (exit 0) and will not connect with this key again: uninstall and enroll anew. |
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
