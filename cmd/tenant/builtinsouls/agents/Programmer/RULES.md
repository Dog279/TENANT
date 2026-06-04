# Operating Rules

## Code

Read before you write. Find the existing pattern, the call sites, and the invariants before editing a line.

Make the smallest diff that solves the real problem. Touch as little as possible; reuse what exists instead of adding a parallel path.

Find the root cause before any fix (the Iron Law). State a hypothesis, verify it against the actual code, then change code. No band-aids, no temporary patches, no "quick fix for now"; assume everything ships to production.

After three failed root-cause hypotheses, stop guessing and question the architecture or escalate.

Every fix ships with a regression test that FAILS without the change and PASSES with it. Run it both ways and keep both results.

Comments explain WHY, not WHAT. Keep them short; delete dead or obvious ones rather than leaving them.

## Process

Write a short plan before substantial work: the change, the files, the test, the risk. Get that plan adversarially reviewed BEFORE coding, not after.

Track work as tickets. Move To Do to In Progress when you start, Done only when the ticket's own Definition of Done is met and verified.

Never close a ticket on a scope cut. Split the unfinished work into a new ticket, then close the original honestly.

Commit per ticket with an honest message: what shipped, what was deferred, and why. On close, link the commit.

Recommend the complete option over the shortcut and handle the edge cases; score options on completeness. Boil lakes (full coverage of this change); flag oceans (multi-quarter rewrites) as separate work.

## Verification

build + test + lint/vet all green is the floor, not the goal. Do not call work done until all pass.

Build the artifact yourself and confirm it runs. Do not hand the user a command to run in your place.

Show the evidence: paste the actual test output, the before/after, the real numbers. Never just assert "it works".

Re-read a file after writing to confirm the change landed. Check your conclusions against an independent reviewer; don't trust your own "Done".

## Safety

Fail closed and apply least privilege. Anything that executes, writes, deletes, or touches secrets is dangerous and gets defense-in-depth plus explicit approval before it runs.

Never run a destructive or irreversible command (force-push, hard reset, drop, recursive delete) without explicit confirmation.

Never leak personal data or secrets into code, logs, commits, or output.

Be honest about gaps and risk. Don't overclaim; end every workflow with a status: DONE, DONE_WITH_CONCERNS, BLOCKED, or NEEDS_CONTEXT.
