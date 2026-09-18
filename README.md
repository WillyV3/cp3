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
cp3 send --to frontend "does /api/v2/users paginate?"   # reports delivered / queued / NOT DELIVERED
cp3 watch                    # firehose: every event on the network, live
cp3 statusline               # one colored line for your statusline
```

Identity is zero-config: a session claims its directory basename
(`~/projects/pith` → `pith`). Override with `CLAUDE_PEERS_AGENT`, a
`.cp3-peer-name` file, or `--as`. (`.claude-peers-agent` still works — the
old name from before the rename — but new markers should use the current
one.) Names are unique while held; presence expires ~30s after a session
dies.

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

Not every runtime is at the same level. The bar is Claude Code: one command
opens the agent's normal interactive TUI, that TUI is a named peer, messages
from other peers appear in it as turns you can watch, and mail sent while it
was down is delivered when it comes back.

| Runtime | Start | Interactive TUI is the peer | Status |
|---|---|---|---|
| Claude Code | `cp3 run --as name` | yes | the reference. MCP tools (`list_peers`, `send_message`, `set_summary`, `check_messages`) inside the session |
| opencode | `cp3 opencode --as name` | yes | **at parity for messaging.** Verified 2026-09-18 on opencode 1.18.30 (macOS arm64; also 1.17.13) and 1.18.31 (Linux): register, inbound renders in the TUI and runs a turn, reply lands under the right name, exit drops presence and stops the server it started, queued mail replays on restart. Gaps: no MCP tools inside opencode — the agent replies by running `cp3 send` from the hint on each message; no roster summary |
| codex | `cp3 codex --as name` | yes | **at parity for messaging.** Verified 2026-09-18 on codex-cli 0.155.0 (Linux): register with cwd, inbound renders in the TUI and runs a turn, reply lands under the right name, exit stops the app-server it started and frees the name, queued mail replays on restart. cp3 runs its own `codex app-server` on a loopback port and the TUI attaches to it; startup costs one short briefing turn. Codex's default is read-only + ask, so it asks approval before its first `cp3 send`. For an unattended peer pass config overrides after `--` (they go to the app-server and the briefing — codex rejects permission flags on the TUI itself): `cp3 codex --as name -- -c sandbox_mode=\"workspace-write\" -c approval_policy=\"never\"`. Needs a working codex provider (login or `model_provider` in `~/.codex/config.toml`). Gaps: a message arriving mid-turn queues behind it (`--headless` can steer instead, but has no TUI); no MCP tools; no roster summary |
| pi | `CLAUDE_PEERS_AGENT=name pi -e adapters/pi/peer.ts` (or copy it to `~/.pi/agent/extensions/`) | yes | **at parity for messaging.** Verified 2026-09-18 on pi 0.85.1 (Linux): register with cwd, inbound renders in the TUI and runs a turn, reply via a native `peer_send` tool, exit frees the name, queued mail replays on restart. Gaps: no one-word `cp3 pi` launcher yet; only `peer_send` — no `list_peers`/`set_summary` tools |
| hermes-agent | copy `adapters/hermes/` to `~/.hermes/plugins/claude-peers/`, `hermes plugins enable claude-peers-platform`, then `CLAUDE_PEERS_AGENT=name CLAUDE_PEERS_ALLOW_ALL_USERS=true hermes gateway run` | **no** — a gateway, not a TUI | **works, different shape.** Verified 2026-09-18 on hermes-agent 0.19.0 (Linux): register with cwd, each peer is a chat with its own persistent session, the reply routes back automatically (the agent runs no send command), exit frees the name, queued mail replays on restart. There is no TUI to watch — the conversation is in `~/.hermes/logs/agent.log`. Hermes denies unknown senders by default, hence the allow env (`CLAUDE_PEERS_ALLOWED_USERS=a,b` to restrict). First contact from a new peer gets a one-time "no home channel" notice unless `CLAUDE_PEERS_HOME` is set |
| anything | `cp3 subscribe --agent x` + `cp3 send` | — | the raw primitive: one JSON message per line in, `cp3 send` out |

`cp3 opencode` starts `opencode serve` itself if nothing is listening on
`OPENCODE_URL` (default `http://127.0.0.1:4096`), creates a session, and
attaches the TUI to it. `--headless` skips the TUI. `OPENCODE_MODEL=provider/model`
picks the model; unset uses opencode's own default. `OPENCODE_SESSION_ID` joins
a session you already have open instead of creating one.

## Operations

```sh
cp3 consumers                  # every subscriber: pending, last delivery, active/idle/STALE
cp3 consumers --reap           # delete detached, empty, idle inboxes (never one holding mail)
cp3 watch --as security-watch  # long-lived watchers register presence; a dead watcher is visible
```

## Fleet versioning

Machines track **releases**; only the box where cp3 is developed runs an
untagged build. `cp3 doctor` reports the version first and warns on `dev`, so
"what is this machine running" is one command instead of a checksum sweep —
divergence between machines caused every incident worth writing a postmortem
about.

## Design

A durable event log (embedded NATS JetStream) is the entire backend.
Presence is a TTL'd KV projection; inboxes are durable consumers; the
firehose is just the log. See [DESIGN.md](DESIGN.md).

MIT.
