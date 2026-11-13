# Referee rulings (2026-11-13)

The method sends any cell where the four judges are more than 2 points apart to a referee. Two cells qualified this run, so the referee ran on those two and on nothing else.

Spot-checks were read-only. Nothing was run from the Hermes clone. Live GitHub figures were taken at about 22:00 UTC on 2026-11-13.

## Cell 1: capability 11, Install, setup UX and cross-platform, Tenant

**Scores and reasons**

- A, 6 (was 5): install scripts, service installer and self-update exist and are CI-tested; one point only because the repo is private.
- B, 5 (was 4): the same mechanics; the private repo "caps the gain".
- C, 5 (was 4): the mechanics are CI-tested, but "a newcomer still cannot get it".
- D, 3 (no prior score): the mechanics are strong in code, but the private repo and the 404 one-liners dominate what a user meets.

**Evidence that decides it**

- `gh repo view Dog279/TENANT`: `PRIVATE`, 0 stars.
- Unauthenticated `curl` of `github.com/Dog279/TENANT/releases/latest/download/install.sh`: 404. The repo page: 404.
- The public install page (`findtime.net/tenant/docs/setup/install/`, HTTP 200) prints that one-liner twice. It also says the project is open source and that the checksums carry a cosign signature.
- `gh release list`: `v0.1.0` is "Latest" and has 13 assets, none a script. `install.sh` and `install.ps1` are only on the pre-release `v0.2.0-rc.2`. So making the repo public would not fix the one-liner on its own. A non-pre-release is also needed.
- `release.yml:65-73`: the job that installs a published release and runs `tenant service` and `tenant update` is skipped while the repo is private. It has never run.
- In Tenant's favour: `packaging.yml` passed on `8b9bbf7` (push, 2026-11-13) and installs a snapshot through both scripts on three OSes. `cmd/tenant/service_{darwin,linux,windows}.go`, `update.go` and `internal/selfupdate/` exist with tests. `v0.2.0-rc.2` carries six archives, SBOMs and checksums.

**Where the disagreement comes from**

Anchoring in A, B and C, plus a different reading of the rubric. It is not D's lack of spot-checks: D assumed the 404, and the live check confirms it. D lacked no fact that would raise the score.

On 2026-10-02, A and B justified this cell on mechanics alone ("no installer, brew tap, systemd unit"). They charged the private repo to row 13 only. Their anchors of 5 and 4 never carried the delivery charge. This run they added one point for the new mechanics. A's 6 is close to what the mechanics would earn if anyone could download them. B and C give 5, which the rubric defines as "it works", next to their own text saying a newcomer cannot get it.

The rubric says to score what a user can get and run today. It names install as one of two rows that carry the delivery problem. That puts this cell below the "works" line. D read that rule correctly.

D's 3 is one step too low. The row has three parts. Install delivery fails for every outsider. Setup UX (wizard, doctor, no-key trial) and cross-platform support (six targets, a service installer on three OSes) are real and passed CI at the reviewed commit. The rule says the delivery problem counts against the row. It does not say it cancels the rest. D also listed this cell among those it was least sure of.

**Ruling: 4**

**What would move it next run:** a public repo and a non-pre-release that carries the scripts, so the documented one-liner answers 200 and the release smoke job runs; that should lift the cell to 6 or 7.

## Cell 2: capability 12, Engineering quality and reliability, Hermes

**Scores and reasons**

- A, 4 (was 4): greener CI and a stable release pipeline are offset by open bugs up 40% and a release built from the wrong commit.
- B, 4 (was 4): the stable channel is a structural gain; the bug rise offsets it.
- C, 5 (was 5): evidence moved both ways; the CI sample is too short to move the score.
- D, 7 (no prior score): about 58k test functions and a healthier CI sample, with the backlog and the wrong-commit release noted as caveats.

**Evidence that decides it**

- CI on `main`, last 30 push runs of `ci.yaml` (`gh api`, one afternoon): 20 success, 2 failure, 7 cancelled, 1 queued. That is 20 of 22 decided runs green. It is twice the dossier's sample and agrees with it. It is still one day.
- Backlog (`gh api` search): 19,105 open issues, 34,238 open PRs, 11,148 open `type/bug` (7,953 on 2026-10-02). Of issues opened since 2026-10-02, 6,447 are open and 1,234 closed. Of bugs, 4,477 opened since then are still open; 2,115 were closed in the same window.
- v0.22.2 release notes: "v0.22.1 was built from the wrong commit." v0.22.1 was a full stable release for about eight hours. Twelve stable releases shipped in five weeks.
- "[Audit regression]" issues: 13 open, 5 closed, none filed since 2026-10-01. #127094 is still open at P2.
- Clone at `491f9845`: 57,785 `def test_` functions in 7,971 test files; 47 workflow files, including `stable-release.yml` and install and upgrade E2E jobs. The dossier's counts hold.

**Where the disagreement comes from**

Mostly a different reading of the rubric by D, and partly anchoring in A and B. It is not missing spot-checks: D cites the same facts as the others.

D scores inputs: test volume, workflows, a CI sample. Its row is compressed at the top (Tenant 8, OpenClaw 7, Hermes 7). A 7 says Hermes has only modest gaps. The outcomes do not support that. Open bugs grew 40% in six weeks, new bugs arrive about twice as fast as bugs are closed, and a stable release went out from the wrong commit inside the new pipeline. The dossier's own verdict is that per-release stability needs the user's own soak testing.

A and B's 4 is stale. Their stated reason on 2026-10-02 was "main CI often red", at a time when source installs followed `main`. Both points have reversed: CI is mostly green on two samples, and installs follow tested stable tags by default. Those two changes bear directly on what a user runs. The backlog is worse, but it was already large and already in the 4.

The rubric defines 5 as "it works but with clear gaps against the leaders". That is the plain description of the evidence. It also keeps Hermes a step below OpenClaw on this row, where all three anchored judges put it and where OpenClaw's shrinking backlog supports it. This is C's number, and C's reasoning is the one the evidence bears out. It is not a midpoint.

**Ruling: 5**

**What would move it next run:** open `type/bug` flat or falling with no further mis-built stable release would make it a 6; another 40% rise or a second release slip would return it to 4.

## Rulings

| Cell | A | B | C | D | Referee ruling |
|---|---|---|---|---|---|
| 11 Install, setup UX and cross-platform: Tenant | 6 | 5 | 5 | 3 | **4** |
| 12 Engineering quality and reliability: Hermes | 4 | 4 | 5 | 7 | **5** |
