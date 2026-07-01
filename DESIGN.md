# cp3 design

**The durable event log is the source of truth. State is a projection.
Consumers are decoupled.** Every peer action is an immutable event on the log:
nothing is lost, every consumer sees everything, new tools replay history.

## Transport: embedded NATS JetStream

- **Stream `PEERS`** — subjects `peers.>`, file storage, 7d retention,
  64KB max message. The replayable source of truth.
- **KV bucket `PEERS_PRESENCE`** — key = agent name, TTL 30s. Heartbeat
  refreshes the key; present = online, expired = gone. No offline state to
  manage.

`cp3 serve` runs the server in-process. Clients auto-start it on localhost
when nothing is listening; remote URLs never auto-start.

## Event envelope

```json
{ "v": 1, "id": "<hex>", "type": "<event-type>", "ts": <unixMillis>, "actor": "<agent>", "data": { ... } }
```

`v` bumps on breaking changes; fields are never removed within a version.
Events carry ids and are published with NATS msg-id dedup — delivery is
at-least-once, consumers can dedup on `id`.

## Subjects

| subject | type | data |
|---|---|---|
| `peers.lifecycle.register` | register | agent, machine, cwd, session |
| `peers.lifecycle.deregister` | deregister | agent |
| `peers.presence` | presence | summary changes |
| `peers.msg.<to>` | message | id, from, to, content, deliverAs |

An agent's inbox is a durable consumer filtered to `peers.msg.<self>` —
offline messages drain on reconnect. The firehose (`cp3 watch`) subscribes
`peers.>` and sees everything.

## Auth

A shared token, generated on first `serve` (0600 file), required for every
connection including localhost. Resolution order: `NATS_TOKEN`,
`NATS_TOKEN_FILE`, `~/.config/cp3/token` — never argv, never config JSON.

One token = one trust domain (everyone on the network can message everyone).
For per-user credentials or cross-org sharing, NATS accounts + nkey/JWT slot
in without code changes — `NATS_CREDS` is already plumbed through.

## Adapters

The only bespoke code. Each runtime is a thin client: subscribe the inbox,
inject into the live TUI (Claude `claude/channel`, pi steer, opencode
delivery), publish to send. Anything that can read a subprocess can join via
`cp3 subscribe` (one JSON message per line).

## Projections

`cp3 peers` reads the presence KV. History is a replay of `peers.msg.<x>`.
`cp3 consumers` reports every subscriber's lag so an abandoned consumer is
visible instead of silently rotting. All derived, none authoritative.

## Tests

Everything runs against an embedded in-process server: register/list,
delivery, offline drain, presence lifecycle, publish dedup, firehose
visibility, consumer listing, token auth (env + file), chaos
(server restart → durable consumer resumes). `go test -race ./...`.
