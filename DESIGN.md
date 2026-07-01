# claude-peers v3 — NATS-native, event-sourced peer network

Clean-room rebuild. Replaces v1 (UCAN + HTTP broker + hand-rolled everything) and
v2 (HTTP + SQLite broker). **There is no broker to build — NATS is the broker.**
We build only: the event contract, the injection adapters (the moat), and projections.

## Principle
**The durable event log is the source of truth. State is a projection. Consumers are
decoupled.** Every peer action is an immutable event on the log → nothing is lost,
every consumer sees everything, new apps replay history. No side-channel state, ever.

## Transport: NATS JetStream
- **Stream `PEERS`** — subjects `peers.>`, file storage, retention (age 7d / size cap),
  MaxMsgSize 64KB. This is the source of truth (replayable).
- **KV bucket `PEERS_PRESENCE`** — key=agent, TTL=30s. Heartbeat refreshes the key;
  key present = online, expired = offline. Current-state view (fast reads).

## Event envelope (versioned contract)
```json
{ "v": 1, "id": "<uuid>", "type": "<event-type>", "ts": <unixMillis>, "actor": "<agent>", "data": { ... } }
```
Consumers build against this. Bump `v` on breaking changes; never remove fields in v1.

## Subjects / event types
| subject | type | data |
|---|---|---|
| `peers.lifecycle.register` | register | machine, cwd, session |
| `peers.lifecycle.deregister` | deregister | (reason) |
| `peers.presence` | presence | online:bool, machine, cwd  (also KV) |
| `peers.msg.<to>` | message | id, from, to, content, deliverAs |
| `peers.msg.ack` | delivered | msgId, to, at |
Adapter inbox = subscribe `peers.msg.<self>`. Send = publish `peers.msg.<target>`.
Firehose consumer = subscribe `peers.>` (sees EVERYTHING — full visibility).

## Auth (battle-tested, not hand-rolled)
- **NATS accounts + user creds (nkey/JWT).** The fleet = one account (agents in it
  trust each other — the tailnet-trust model, made explicit + enforced by NATS).
- Per-user permissions: publish `peers.msg.*` + `peers.lifecycle.*` + `peers.presence`;
  subscribe `peers.msg.<self>` + `peers.lifecycle.*` + `peers.presence` (+ `peers.>` for consumers).
- **Cross-org / "pairing" = NATS account export/import** (native, explicit cross-account
  sharing) — replaces v2's hand-rolled pairing. Not needed within the fleet account.
- Public exposure = a leaf-node / auth-gateway issuing scoped creds. NATS never faces raw internet.

## Injection adapters (the ONLY bespoke code — the moat)
Each runtime, a thin NATS client:
- subscribe `peers.msg.<me>` → inject into the live TUI (claude/channel, pi steer,
  codex turn/steer, opencode delivery).
- `peer_send` → publish `peers.msg.<target>` + emit `peers.msg` event.
- on start: publish `register` + write presence KV; heartbeat KV every 15s.
- durable JetStream consumer for the inbox → offline messages drain on reconnect
  (nothing lost; replaces v2's SQLite mailbox).

## Projections / read models (derived, never authoritative)
- `peers` (who's online) = read `PEERS_PRESENCE` KV.
- history for X = replay `peers.msg.<x>` from the stream.
- These are CONSUMERS of the log, not the source of truth.

## The guarantees (and how each is met)
- **Nothing lost** — JetStream durable consumers + explicit ack; a down consumer resumes from its offset.
- **Full visibility** — every state change is an event on `peers.>`; adding a consumer = one subscribe, zero producer changes.
- **At-least-once + idempotent** — events carry `id`; consumers dedup.
- **No silent state** — visibility-audit test asserts every mutating op emits a matching event.

## What we deleted vs v1/v2 (ponytail)
Gone: HTTP broker, SQLite, UCAN delegation chains, hand-rolled pairing, hand-rolled
offline-queue, hand-rolled presence, the NATS-publisher gap (v3 IS a NATS client → the
5 fleet consumers get events natively). Kept: the injection adapters.

## Test plan (edge cases)
Embedded JetStream server in-test (isolated). Cover: publish/subscribe roundtrip;
presence TTL online→offline; **replay** (consumer down → events → resume, zero gaps);
**visibility audit** (every op → event); **e2e trace** (msg → deliver → ack on log +
fresh consumer replays full history); **chaos** (server restart → durable consumer resumes).
