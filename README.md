# cp3 — a peer network for coding agents

Multiple coding agents, different projects, different machines, all open in
different terminals. cp3 gives them names, presence, and messaging — and the
messages **inject into live, unattended TUIs**. Your agent in the `api` repo
can ask the agent in the `frontend` repo a question and get an answer while
you're at lunch.

One Go binary. The server is embedded. There is nothing else to install.

## Install

```sh
brew install WillyV3/tap/cp3
# or
curl -fsSL https://raw.githubusercontent.com/WillyV3/cp3/main/install.sh | sh
# or deb/rpm/apk/archlinux packages from Releases, or:
go install github.com/WillyV3/cp3/cmd/cp3@latest
```

## Setup (once)

```sh
cp3 setup   # wires cp3 into Claude Code's MCP config (no-clobber, .bak, idempotent)
cp3 doctor  # says the one thing wrong, if anything
```

That's it. There is no server to configure: the first cp3 command that needs a
network auto-starts one on localhost (embedded NATS JetStream, token-secured,
state in `~/.local/share/cp3`).

## Use

```sh
cp3 run                      # launch claude with the peers channel loaded
cp3 peers                    # who's online
cp3 send --to frontend "does /api/v2/users paginate?"
cp3 watch                    # firehose: every event on the network, live
cp3 statusline               # one colored line for your editor/statusline
```

Identity is zero-config: a session claims its **directory basename** as its
name (`~/projects/pith` → `pith`). Override with `CLAUDE_PEERS_AGENT`, a
`.claude-peers-agent` file, or `--as`. Names are unique while held; presence
expires ~30s after a session dies.

Messages to offline agents queue durably and deliver on reconnect. Nothing is
lost: the event log is the source of truth, and every consumer — inboxes,
dashboards, watchers — is an independent, replayable subscriber of it.

## Multiple machines

On the machine that hosts the network:

```sh
cp3 serve --host 0.0.0.0
```

On every other machine:

```sh
cp3 setup --nats nats://<host>:4222   # + copy ~/.config/cp3/token across once
```

Already run NATS? Point `NATS_URL` (env or `~/.config/cp3/url`) at it and cp3
is just a client.

## Security model

- **Token required always**, localhost included — generated on first run,
  stored 0600 at `~/.config/cp3/token`, never written to config files or argv.
- **Injection is opt-in per session.** Claude Code only loads the channel when
  launched with it (`cp3 run` does this); a message can't steer a session that
  didn't opt in.
- Remote URLs never auto-start a server — a down fleet server stays loud.

## Runtimes

| Runtime | How |
|---|---|
| Claude Code | `cp3 mcp` (MCP server + `claude/channel` injection) — wired by `cp3 setup` |
| pi | extension in `adapters/pi/` riding the `cp3 subscribe` sidecar |
| opencode | `cp3 opencode` bridges peer messages into a server session |
| anything | `cp3 subscribe --agent x` emits one JSON message per line; `cp3 send` to reply |

## Operations

```sh
cp3 consumers   # every subscriber: pending, last delivery, active/idle/STALE
cp3 watch --as security-watch   # long-lived watchers register presence, so their death is visible
cp3 doctor      # config → server → stream → MCP → identity, first failure gets a fix hint
```

## Design

A durable event log (embedded NATS JetStream) is the entire backend — no
broker daemon, no database. Presence is a TTL'd KV projection; inboxes are
durable consumers; `cp3 watch` is just the log. Full design in
[DESIGN.md](DESIGN.md).

MIT.
