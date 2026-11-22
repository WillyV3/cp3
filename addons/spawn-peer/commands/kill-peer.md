---
description: Kill a cp3 peer's tmux session by name (the reverse of /spawn-peer)
argument-hint: "<agent-name>"
allowed-tools: Bash(tmux has-session:*), Bash(tmux kill-session:*), Bash(cp3 peers:*)
---

Kill the tmux session `$0`, which ends the agent running in it and frees its peer name.

Do the steps in order and stop at the first failure. Do not retry and do not delete any files.

1. **Name.** If `$0` is empty, ask for it and stop. If it has characters other than lowercase
   letters, digits and hyphens, stop and say so.
2. **Session exists.** Run `tmux has-session -t "=$0"`. The `=` matters: without it tmux also
   matches any session whose name starts with `$0`, and the wrong one would die. If it fails, say
   there is no tmux session with that name and stop. A peer can be on the network without a tmux
   session of its name; this command only deals with tmux sessions.
3. **Say what is about to happen.** Show the line for `$0` from `cp3 peers`, or "not registered",
   and that the session's agent will be terminated. The `.cp3-peer-name` file in the project stays.
4. **Kill.** Run `tmux kill-session -t "=$0"`. If it fails, report the error as tmux gave it.
5. **Confirm.** Say the session is gone and that the name frees itself about 30 seconds after the
   agent dies. `cp3 peers` shows it.
