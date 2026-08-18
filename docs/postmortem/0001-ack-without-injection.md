# Post-mortem 0001: Messages were acked before anyone saw them

Status: resolved (commit 46d9d35, 2026-08-14)

## Executive summary

`Subscribe` ran `h(m); msg.Ack()`, where `h` only writes a JSON-RPC
notification to stdout. Claude Code returns no receipt for a channel
notification, so writing bytes to a pipe was treated as proof a human read
them. Any session that dropped the notification lost the message permanently:
the ack had already advanced the durable consumer, and the only other copy
lived in a memory buffer that died with the process. Two briefings to `rover`
were reported as sent, delivered, and acked, and were never seen by anyone.
The fix separates transport ack from user delivery, because they were always
two different facts.

## Summary

`caretaker` sent `rover` a rebuild briefing twice. Both sends returned success.
`inbox-rover` showed `delivered=2, pending=0` — a perfect delivery record. The
message never appeared on rover's screen, and no layer disagreed with any
other.

## Timeline

- 12:36 — message published to `peers.msg.rover`. rover's MCP process consumes
  it, writes the channel notification, and acks. Consumer advances to 2.
- ~12:5x — that MCP process exits (session reconnect).
- 13:01 — replacement MCP process starts, resumes the durable consumer at
  ack floor 2, and correctly finds nothing to replay. The message is gone.
- 13:04 — user reports "rover still has not received the message". The stream
  shows exactly 2 messages ever published to that subject and exactly 2
  delivered: the transport was flawless.

## Root cause

Transport readiness was treated as application readiness. `msg.Ack()` means
"this process received the bytes"; it was being used to mean "the agent has
this message". The gap between those two statements is invisible by
construction — Claude Code sends nothing back for a channel notification, so
there is no signal to check.

A second, independent defect made the first one fatal instead of merely
unlucky: the undelivered-to-user copy was in-memory, so it could not survive
the process whose death caused the loss.

(A later contributing factor, found the same day: current Claude Code discards
channel notifications entirely unless launched with
`--dangerously-load-development-channels`. Sessions restarted without it are
deaf while looking healthy — see the `✖ NO CHANNEL` guardrail below.)

## Guardrails added

- Messages append to `~/.local/share/cp3/spool/<agent>.jsonl` **before** the
  notification, and clear only when the agent demonstrably has them:
  `check_messages`, or an automatic replay marked `[delivered late]` on
  session start. Startup drains both the claimed and configured names, since a
  reconnect can land on a suffixed twin and orphan its own spool.
- Regression proof runs the worst case: message acked, process SIGKILLed with
  no clean release, JetStream left with `pending=0` and nothing to replay,
  successor session under a *different* name — message still arrives.
- `cp3-mcp` inspects the launching Claude's argv and marks the statusline
  `✖ NO CHANNEL (relaunch: cp3 run)` when injection cannot work at all.

## Lesson

Never let one layer's success stand in for the whole path's success. If the
final hop gives no receipt, the layer before it must keep the data until
something proves the hop happened.
