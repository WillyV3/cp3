# Post-mortem 0003: A retired service was resurrected by a corrected document

Status: resolved (2026-08-05)

## Executive summary

The v1 broker was stopped and disabled on 2026-07-01. On 2026-07-29 a session
hit an error from the retired v1 CLI, "fixed" it by starting the dead service,
and logged that action. A later session read that log line, concluded its own
earlier reading had been mistaken, and wrote a **correction into the fleet's
persistent memory** stating the broker was live and that the fix for a CLI
error was `systemctl --user start claude-peers-broker`. That entry then
instructed every subsequent session for a week. The service was public-facing
attack surface serving zero peers. Documentation was the propagation mechanism.

## Summary

Discovered when a routine roster question prompted a check of fleet services
and `:7899` answered a health probe it should not have been able to answer.
No user ever requested the service; no code change re-enabled it.

## Timeline

- 07-01 — `claude-peers-broker.service` stopped and **disabled**. Verified dead.
- 07-29 08:16 — a session troubleshooting an expired v1 UCAN token starts the
  broker and records it in the autonomous-actions log.
- 07-29 (later) — a second session sees the service running plus that log line,
  treats its own correct earlier reading as the error, and writes the
  "CORRECTION" into `reference_homelab_urls.md` (line 22).
- 07-29 → 08-05 — every session reading that memory file is told the broker is
  required. Nobody re-verifies, because the memory file is the thing you
  consult *instead of* re-verifying.
- 08-05 — inconsistency caught, the entry rewritten with an explicit
  retraction, service stopped and disabled again.

## Root cause

An observation of a *symptom* (the service is running) was promoted to a
*specification* (the service should be running) and written to the one artifact
designed to be trusted without re-derivation. Memory files exist precisely so
sessions don't re-verify; that property makes a wrong entry self-perpetuating
in a way a wrong line of code is not, because code gets executed and tested
while documentation only gets believed.

The v1 CLI binary still on `PATH` was the trigger. Its error message was
actionable enough to invite a fix and gave no hint that the correct action was
"stop using this binary".

## Guardrails added

- `reference_homelab_urls.md` line 22 now carries the correction *and its own
  history*: the retired status, the date, an explicit "my 2026-07-29
  correction was WRONG", and `Do NOT systemctl start claude-peers-broker`.
  Naming the retraction inside the entry is what stops the next session from
  re-deriving the same mistake from the same evidence.
- The same entry states the cp3-vs-v1 distinction, since reasoning from the
  retired v1 repo caused a separate wrong diagnosis on 2026-08-05.
- Open item, deliberately not closed by fiat: the v1 `claude-peers` binaries
  remain on `PATH` on two machines. Removal is the durable fix and is the
  operator's call.

## Lesson

A memory or docs file is executable in the only sense that matters: agents act
on it without re-checking. Corrections written into one must carry evidence and
a date, and a correction that *reverses* an earlier correction must say so out
loud — otherwise the next reader repeats the reasoning that produced the error.
