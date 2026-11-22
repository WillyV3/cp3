---
description: Spawn a cp3 peer in its own tmux workspace (claude, opencode or codex TUI + editor split), for a new thread
argument-hint: "[agent-name] [project-dir] [--harness claude|opencode|codex]"
allowed-tools: Bash(spawn-peer:*), Bash(tmux list-sessions:*), Bash(tmux switch-client:*), Bash(cp3 peers:*), Bash(basename:*)
---

Start a new cp3 peer in its own tmux session: the agent on the left, an editor on the right, in the
project directory. The user runs this to open a **new thread** on a codebase.

**Do not hand-run `tmux new-session`, `claude` or `cp3 run`.** The `spawn-peer` script owns the
invocation. Your job is to choose a good name and call it.

## 1. Resolve the project dir

`$1` if given, else the current working directory. If it does not exist, stop and say so. Do not
create it.

## 2. Choose a name that is free

If `$0` was given and is lowercase letters, digits and hyphens, use it. Otherwise derive one:

- Run `tmux list-sessions -F '#{session_name}'` and `cp3 peers` to see which names are taken.
- Base the name on the project (usually `basename` of the dir). If that is taken, pick a variant
  that says what this thread is for (`shop` becomes `shop-checkout`), or `-2`, `-3` if you do not
  know. Keep it short: it is a label the user scans in a session list.
- Confirm the name is free in both lists before continuing.

## 3. Spawn

```
spawn-peer <name> <project-dir> [--harness claude|opencode|codex]
```

`--harness` defaults to `claude`. Pass another only when the user asks for it. If the user names a
model for opencode, pass it as `OPENCODE_MODEL=<provider/model> spawn-peer ...`. Extra flags for
claude go in `CLAUDE_ARGS`, for example `CLAUDE_ARGS="--resume <id>"`.

Relay the script's output as it is. Exit 0 means the peer registered on the network and can be
messaged. Non-zero: report exactly what it said (name taken, dir missing, or "did not register in
45s") and do not claim a spawn. The usual cause of the last one is a first-run prompt in the new
session, such as folder trust or a login, which only the user can answer: tell them to
`tmux attach -t '=<name>'`. Do not answer that prompt for them.

## 4. Offer to switch

If it registered and the user is inside tmux, offer `tmux switch-client -t '=<name>'`. Do not
switch without being asked.
