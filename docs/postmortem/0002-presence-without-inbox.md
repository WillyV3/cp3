# Post-mortem 0002: On the roster, but unreachable

Status: resolved (2026-08-18)

## Executive summary

`cp3 register` published a presence record and created no durable consumer, so
an agent could appear in `cp3 peers`, be addressed by name, and never receive
anything. Publishing to `peers.msg.<name>` always succeeds — the log accepts
the write whether or not anything is subscribed — so `send` reported success
while the message sat in the log addressed to nobody. Registration now creates
the inbox with the presence record, making "on the roster" mean "reachable" by
construction.

## Summary

Reported by `jim` on 2026-08-13, then confirmed in source: `cmdRegister` called
`Register()` and nothing else. `bridge.Run` (used by `cp3 subscribe` and the
adapters) *did* subscribe, so the common paths worked and the gap only appeared
for registration-only agents — the ones nobody watches closely.

## Root cause

Two facts that must be atomic were produced by two unrelated calls:
*presence* (a KV write) and *deliverability* (a durable consumer). Nothing
asserted their relationship, so any code path that did one without the other
created an agent-shaped decoy. The decoy is indistinguishable from a healthy
peer in every UI cp3 has.

This is the same shape as 0001 — a layer's local success reported as the
system's success — arriving from the opposite direction: there, delivery was
claimed after the fact; here, reachability was implied before it.

## Guardrails added

- `Register` creates the inbox before writing presence, via a single
  `ensureInbox` definition shared with `Subscribe` so the two can never drift.
- `TestRegisterIsReachable` asserts the invariant directly: register, confirm
  the agent is on the roster, then confirm a send to it is not `no-inbox`.
- `Send` now returns a `DeliveryStatus`, so even if the invariant is somehow
  violated again, the sender is told `NOT DELIVERED` instead of "sent"
  (see 0001's lesson, applied preemptively).

## Lesson

If two pieces of state must agree, create them in one place and assert the
agreement in a test. A convention that they "should" be created together is
not a mechanism.
