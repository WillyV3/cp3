# cp3 mod for Claude Code

A [Claude Code mod](https://code.claude.com/docs/en/plugins/mods/overview) that puts the cp3
peer network inside Claude Code. Needs Claude Code 2.1.290 or later and the `cp3` binary
on PATH (0.3.1 or later, for `cp3 whoami` and `cp3 subscribe --deliver claude-socket`).

What you get:

- **A band above the prompt** — `● keeper · 14 peers · ✉2`, live. Replaces the statusline
  segment, and never times out on a slow network: the roster is refreshed in the background.
- **`/cp3`** — a pane with the fleet roster (name, machine, directory, summary, pending mail)
  and a field to message any peer. `/cp3 jim hello` sends without opening the pane.
- **Claude's own `SendMessage` and `ListAgents` reach the fleet.** Claude Code's
  cross-session messaging only spans one machine unless both sides go through Anthropic's
  servers with Remote Control. With this mod, `ListAgents` also lists every cp3 peer, and
  `SendMessage` to a cp3 peer's name goes over cp3 — same machine, other machine, or a
  box that is offline right now (cp3 queues it). A cp3 name wins over a same-named local
  session.
- **Inbound mail is a native message, with no special launch flag.** The mod's subscriber
  posts each peer message into the session's own cross-session inbox socket, so it lands
  mid-turn between tool calls the way a message from another session does. Plain `claude`
  is a peer; `--dangerously-load-development-channels` is not needed.

## Identity

When the session also runs the cp3 MCP server (`cp3 run`), that server owns the peer name and
the inbox and the mod only draws and bridges. Without it, the mod owns both: it runs
`cp3 subscribe --deliver claude-socket`, which claims the directory's identity (`.cp3-peer-name`,
`CLAUDE_PEERS_AGENT`, or the directory name) and holds it for the session.

## Try it

```sh
claude --plugin-dir ~/projects/cp3/mod            # one session, hot-reloads on save
claude plugin validate ~/projects/cp3/mod         # what it hooks and calls, without running it
claude plugin test ~/projects/cp3/mod             # 7 tests, no session or network
```

A mod runs with your permissions. `claude plugin validate` lists everything this one does:
it runs `cp3` (whoami, peers, send, subscribe), reads the tool list, draws, and keeps one
subscriber pid in the plugin store.

## Verified 2026-10-10 on Claude Code 2.1.296 (omarchy)

Register with cwd; band and pane render; a message from `keeper` arrived as a native
message in a session started with plain `claude` and in one started with
`--dangerously-skip-permissions` (delivered, not held); Claude replied through the native
`SendMessage` tool and the ACK arrived over cp3; `ListAgents` listed the fleet; a hot
reload replaced the subscriber without leaving a twin name. The sample mods Anthropic ships
(`claude-code-playground/claude-code/mods`) and the built-in `/plugin-authoring` skill were
the references. The exact events and methods for your build are written to
`.claude-plugin/types/` the first time the mod loads; trust those over any doc page.

## Files

```
mod/
├── .claude-plugin/plugin.json   manifest
├── hooks/hooks.json             points at the module
├── hooks/cp3.mjs                the mod (one file; the cp3 binary is the only network code)
└── tests/cp3.test.ts            claude plugin test
```
