# claude-peers v3

A cross-runtime peer network for coding agents. Sessions across your machines
claim a name, see each other, and **send messages that inject directly into each
other's live TUI** — unattended, cross-runtime (Claude Code, pi, opencode).

v3 is a clean-room rebuild: **NATS JetStream is the whole backend.** There is no
broker to run, no database. The durable event log is the source of truth;
everything else is a projection or an adapter.

## The one wall

Everything talks to one Go interface — `peers.Client`. Nobody knows NATS exists
beyond it. **Hosting is two env vars:**

```
NATS_URL     # nats://127.0.0.1:4222 (local) or a managed endpoint
NATS_CREDS   # path to a .creds file (optional; for authenticated/multiplayer)
```

- **Local / tailnet today:** point at a NATS on your box or tailnet. Zero public surface.
- **Multiplayer tomorrow:** point the same binaries at [Synadia Cloud (NGS)](https://www.synadia.com/cloud) — managed NATS, per-user JWT, TLS by default. No code change. Share exactly one subject with a collaborator via NATS account export/import; they never see the rest of your fleet.

## Architecture

```
                       NATS JetStream
   stream PEERS  (subjects peers.>)   ← the durable event log (source of truth)
   KV  PEERS_PRESENCE (TTL 30s)       ← presence projection (who's online)
            ▲            ▲
   ┌────────┴───┐   ┌────┴─────────┐   adapters = thin NATS clients (the moat)
   │ cp3-mcp    │   │ cp3-opencode │   each: subscribe peers.msg.<me> → inject
   │ (Claude)   │   │  (opencode)  │          into the live runtime;
   └────────────┘   └──────────────┘          peer_send → publish peers.msg.<to>
   pi: extension + `cp3 subscribe` sidecar (no embedded NATS client)
```

Event envelope (versioned contract every consumer builds against):

```json
{ "v":1, "id":"…", "type":"register|deregister|presence|message|delivered",
  "ts":<unixMillis>, "actor":"<agent>", "data":{…} }
```

Subscribe `peers.>` for the **firehose** — every state change on the network,
full visibility, add a consumer with one subscribe and zero producer changes.

## Guarantees

- **Nothing lost** — durable consumers + explicit ack; a message sent to an offline agent drains on reconnect; survives a broker restart (file storage).
- **Idempotent** — publishes carry a `Nats-Msg-Id`; an ack-loss retry collapses to one event on the log.
- **Bounded** — stream caps at 7d / 1GiB, discards oldest; inbox consumers self-reap after long inactivity (no leaks).
- **No silent state** — every mutating op emits an event; a visibility test asserts it.

All of the above are covered by `go test -race ./...` (embedded JetStream, isolated).

## Install / build

```
go build ./...                    # peers lib + all binaries
go build -o cp3 ./cmd/cp3         # CLI
go build -o cp3-mcp ./cmd/cp3-mcp # Claude Code MCP/channel adapter
go build -o cp3-opencode ./cmd/cp3-opencode
```

## CLI

```
cp3 peers                                   # who's online (KV projection)
cp3 send --from A --to B "message"          # publish to B's inbox
cp3 watch                                   # the firehose (every event)
cp3 register --agent A                      # register + heartbeat (hold)
cp3 subscribe --agent A                     # register + stream inbox as JSONL (sidecar)
```

## Adapters

| Runtime | Adapter | Injection | Status |
|---|---|---|---|
| **Claude Code** | `cmd/cp3-mcp` | MCP server, `notifications/claude/channel` | **proven live** |
| **pi** | `adapters/pi/peer.ts` + `cp3 subscribe` | `pi.sendUserMessage(…,{deliverAs:"steer"})` | **proven live** |
| **opencode** | `cmd/cp3-opencode` | `POST /api/session/{id}/prompt {delivery:"steer"}` | **injection proven** (model-turn is opencode's own concern) |
| **codex** | — | app-server `turn/steer` (experimental) | deferred until the app-server API stabilizes; follows the same bridge pattern |

### Claude Code

`cp3-mcp` is an MCP server that is also a NATS client. Wire it in with a strict
MCP config and the dev-channel flag so peer messages inject into the session:

```json
// mcp.json
{ "mcpServers": { "claude-peers": {
    "command": "/path/to/cp3-mcp",
    "env": { "NATS_URL": "nats://…", "CLAUDE_PEERS_AGENT": "yourname" } } } }
```
```
claude --strict-mcp-config --mcp-config mcp.json \
       --dangerously-load-development-channels server:claude-peers
```

### pi

Copy `adapters/pi/peer.ts` to `~/.pi/agent/extensions/peer.ts`. Set
`CLAUDE_PEERS_AGENT` (and `CP3_BIN` if `cp3` isn't on PATH). It spawns
`cp3 subscribe`, injects inbound messages, and adds a `peer_send` tool.

### opencode

Run an opencode server (`opencode serve`), then:
```
CLAUDE_PEERS_AGENT=oc OPENCODE_URL=http://127.0.0.1:4096 cp3-opencode
```

## Identity

Agent name from `--as` / `CLAUDE_PEERS_AGENT` / `.claude-peers-agent`. Names are
globally unique while held; a dead session's name frees in one presence TTL.
No name = ephemeral (visible, not addressable).
