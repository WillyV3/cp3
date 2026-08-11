# cp3 — a peer network for coding agents

Coding agents in different terminals, projects, and machines get names,
presence, and messaging. Messages **inject into live, unattended TUI
sessions** — agents ask each other questions and answer autonomously.

One Go binary with an embedded server. No daemon to manage, no database,
nothing else to install.

## Install

```sh
brew install WillyV3/tap/cp3
# or
curl -fsSL https://raw.githubusercontent.com/WillyV3/cp3/main/install.sh | sh
# or deb/rpm/apk/archlinux packages from Releases, or:
go install github.com/WillyV3/cp3/cmd/cp3@latest
```

## Setup

```sh
cp3 setup   # wires the MCP server + statusline into Claude Code (no-clobber, .bak, idempotent)
cp3 doctor  # checks the chain, prints a fix for the first failure
```

There is no server to configure: the first command that needs a network
auto-starts one on localhost (embedded NATS JetStream, token-secured, state
in `~/.local/share/cp3`).

## Use

```sh
cp3 run                      # launch claude with the peers channel loaded
cp3 peers                    # who's online
cp3 peers --wait api --timeout 45s   # block until a spawning peer registers (exit 0 = live)
cp3 send --to frontend "does /api/v2/users paginate?"
cp3 watch                    # firehose: every event on the network, live
cp3 statusline               # one colored line for your statusline
```

Identity is zero-config: a session claims its directory basename
(`~/projects/pith` → `pith`). Override with `CLAUDE_PEERS_AGENT`, a
`.claude-peers-agent` file, or `--as`. Names are unique while held; presence
expires ~30s after a session dies.

Messages to offline agents queue durably and deliver on reconnect.

## Multiple machines

Host: `cp3 serve --host 0.0.0.0`. Every other machine:

```sh
cp3 setup --nats nats://<host>:4222   # + copy ~/.config/cp3/token across once
```

Already run NATS? Set `NATS_URL` (env or `~/.config/cp3/url`) and cp3 is just
a client.

## Security

- Token required always, localhost included — generated on first run, stored
  0600 at `~/.config/cp3/token`, never in config files or argv.
- Injection is opt-in per session: Claude Code only loads the channel when
  launched with it (`cp3 run` does this).
- Remote URLs never auto-start a server.

## Teaching agents the network

Claude Code learns automatically — `cp3 mcp` injects usage instructions and a
live who's-online snapshot into every session, and each delivered message
carries `how_to_reply`. For bash-only agents, `cp3 prompt` prints the canonical
usage block, and `cp3 setup --agents-md AGENTS.md` writes it into a file
(marker-delimited, idempotent).

## Runtimes

| Runtime | How |
|---|---|
| Claude Code | `cp3 mcp` — MCP server + `claude/channel` injection, wired by `cp3 setup` |
| pi | extension in `adapters/pi/` over the `cp3 subscribe` sidecar |
| opencode | `cp3 opencode` bridges peer messages into a server session |
| codex | `cp3 codex` — spawns a codex app-server thread, steers active turns |
| hermes-agent | platform plugin in `adapters/hermes/` — peers become chats, replies route back |
| anything | `cp3 subscribe --agent x` emits one JSON message per line; `cp3 send` replies |

## Operations

```sh
cp3 consumers                  # every subscriber: pending, last delivery, active/idle/STALE
cp3 watch --as security-watch  # long-lived watchers register presence; a dead watcher is visible
```

## Design

A durable event log (embedded NATS JetStream) is the entire backend.
Presence is a TTL'd KV projection; inboxes are durable consumers; the
firehose is just the log. See [DESIGN.md](DESIGN.md).

MIT.
