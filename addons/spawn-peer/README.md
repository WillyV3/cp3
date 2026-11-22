# spawn-peer — a tmux workspace per peer

An optional add-on. cp3 itself does not need it and does not install it.

`spawn-peer api ~/projects/api` opens a tmux session named `api` with the agent
on the left and an editor on the right, starts the agent as the cp3 peer `api`,
and returns when that peer is on the network. Quit the agent and the session
stays. With [sessui](https://github.com/WillyV3/sessui) the new session shows
up in the switcher with its peer beside it.

## Install

Needs `tmux` and `cp3` on PATH, plus the agent you spawn (`claude`, `opencode`
or `codex`).

```sh
git clone https://github.com/WillyV3/cp3
cd cp3/addons/spawn-peer
install -m 755 spawn-peer ~/.local/bin/       # any directory on PATH
cp commands/*.md ~/.claude/commands/          # optional: /spawn-peer and /kill-peer in Claude Code
```

## Use

```sh
spawn-peer <name> [dir] [--harness claude|opencode|codex]
```

`dir` defaults to the current directory, the harness to `claude`. Names are
lowercase letters, digits and hyphens. It refuses a name that a tmux session
or a live peer already has.

From Claude Code, `/spawn-peer` picks a free name for the directory and calls
the script; `/kill-peer <name>` kills the session.

| Variable | |
|---|---|
| `CLAUDE_ARGS` | extra flags for `claude`, e.g. `"--resume <id>"` |
| `CODEX_ARGS` | config overrides for codex, passed after `--` |
| `OPENCODE_MODEL` | `provider/model` for opencode |
| `SPAWN_PEER_SIDE` | command for the right pane; empty for no split. Default: `nvim .` when nvim is installed |

## What it does not do

- **Answer prompts.** In a directory the agent has not seen, its folder-trust
  prompt waits in the new session, and so does a login. spawn-peer reports
  "did not register in 45s" and tells you to attach. Answer once and the next
  spawn there goes straight through.
- **Choose a permission mode.** A peer that should act on messages with nobody
  at the keyboard needs one that does not stop to ask. That is your decision
  to make per project: put the flag in `CLAUDE_ARGS` or `CODEX_ARGS`.

It writes one file: `.cp3-peer-name` in the project directory, holding the
peer's name, unless a marker is already there. Add it to `.gitignore` if you
do not want it tracked.

## Test

`./test.sh` runs the script end to end against a private tmux server and a
throwaway cp3 network, with stand-ins for the agents. It needs localhost port
4222 free and takes about a minute.
