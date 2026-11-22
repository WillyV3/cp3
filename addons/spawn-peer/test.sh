#!/usr/bin/env bash
# test.sh [path-to-spawn-peer]: checks the script end to end without touching
# your tmux server, your cp3 network or a real agent. It runs against a private
# tmux server and a temp HOME, where cp3 starts its own network on localhost
# (so nothing else may be listening on port 4222), with stand-ins for claude,
# opencode and codex. Takes about a minute: one case waits out the 45s timeout.
set -u
ADDON=${1:-$(cd "$(dirname "$0")" && pwd)/spawn-peer}
REALCP3=$(command -v cp3)
T=$(mktemp -d /tmp/sp.XXXXXX)
for v in $(env | grep -oE '^(CLAUDE_PEERS|CP3|NATS)[A-Z_]*'); do unset "$v"; done
unset TMUX XDG_CONFIG_HOME CLAUDE_ARGS CODEX_ARGS OPENCODE_MODEL
export HOME=$T/home TMUX_TMPDIR=$T SHELL=/bin/sh   # sh reads no rc file, so the pane keeps this PATH and finds the stand-ins
mkdir -p "$HOME" "$T/bin" "$T"/p{1,2,3,4,5,6,7,8}
cat >"$T/bin/claude" <<S
#!/bin/sh
[ -e "$T/noreg" ] && exec sleep 600
exec cp3 subscribe --agent "\$CLAUDE_PEERS_AGENT" --runtime claude
S
cat >"$T/bin/cp3" <<S
#!/bin/sh
[ -e "$T/fakecp3" ] || exec "$REALCP3" "\$@"
if [ "\$1 \$2" = "peers --wait" ]; then
  i=0; while [ \$i -lt 20 ]; do [ -e "$T/launched" ] && exit 0; [ -e "$T/prelaunch" ] || { : >"$T/prelaunch"; exit 1; }; sleep 0.25; i=\$((i+1)); done; exit 1
fi
printf '%s|%s\n' "\${OPENCODE_MODEL-unset}" "\$*" >"$T/launched"; exec sleep 600
S
printf '#!/bin/sh\nexit 0\n' >"$T/bin/opencode"; cp "$T/bin/opencode" "$T/bin/codex"
chmod +x "$T/bin/"*
export PATH=$T/bin:$PATH SPAWN_PEER_SIDE='sleep 600'
# tmux starts panes as login shells, and /etc/profile may put other directories ahead of
# the stand-ins. A plain sh as the default command keeps the server's PATH.
tmux new-session -d -s keep 'sleep 600' && tmux set-option -g default-command /bin/sh
pgrep -f "cp3 serve" | sort >"$T/serve.before"
finish() {
  echo "--- sessions on the private server:"; tmux list-sessions -F '#{session_name}' | tr '\n' ' '; echo
  tmux kill-server
  spid=$(pgrep -f "cp3 serve" | sort | comm -13 "$T/serve.before" - | tr "\n" " ")
  echo "--- private cp3 server pid: ${spid:-none}"
  # shellcheck disable=SC2086
  [ -z "${spid// /}" ] || kill $spid
  rm -rf "$T"
}
trap finish EXIT
pass=0 fail=0
ok() { if [ "$2" = "$3" ]; then pass=$((pass+1)); echo "PASS $1"; else fail=$((fail+1)); echo "FAIL $1: got [$2] want [$3]"; fi; }
has() { case "$2" in *"$3"*) pass=$((pass+1)); echo "PASS $1";; *) fail=$((fail+1)); echo "FAIL $1: [$2] lacks [$3]";; esac; }
run() { out=$("$@" 2>&1); rc=$?; }

run "$ADDON" zz-spawntest "$T/p1"
[ "$rc" = 0 ] || { echo "ABORT, first spawn failed: $out"; tmux capture-pane -p -t "=zz-spawntest:.0" | grep -v "^$" | tail -6; exit 1; }
ok "1 spawn exit" $rc 0; has "1 says live" "$out" "peer 'zz-spawntest' is live and addressable"
ok "1 two panes" "$(tmux list-panes -t '=zz-spawntest:' | wc -l | tr -d ' ')" 2
ok "1 pane cwd" "$(tmux list-panes -t '=zz-spawntest:' -F '#{pane_current_path}' | head -1)" "$(cd "$T/p1" && pwd -P)"
ok "1 marker written" "$(cat "$T/p1/.cp3-peer-name")" zz-spawntest
has "1 on the network" "$(cp3 peers)" zz-spawntest

run "$ADDON" zz-spawntest "$T/p1"
ok "2 same name refused" $rc 1; has "2 reason" "$out" "already exists"

run "$ADDON" zz-spawn "$T/p2"
ok "3 a name that prefixes a live session still spawns" $rc 0
tmux kill-session -t '=zz-spawn'
tmux has-session -t zz-spawn 2>/dev/null; ok "3 bare target matches the longer name (the trap)" $? 0
tmux has-session -t '=zz-spawn' 2>/dev/null; ok "3 exact target does not" $? 1

tmux rename-session -t '=zz-spawntest' renamed
run "$ADDON" zz-spawntest "$T/p1"
ok "4 live peer refused" $rc 1; has "4 reason" "$out" "already live"
tmux has-session -t '=zz-spawntest' 2>/dev/null; ok "4 no session left behind" $? 1

echo other >"$T/p3/.cp3-peer-name"
run "$ADDON" zz-mk "$T/p3"
ok "5 spawn beside another marker" $rc 0; ok "5 marker untouched" "$(cat "$T/p3/.cp3-peer-name")" other
has "5 registered under --as" "$(cp3 peers)" zz-mk

run "$ADDON";                              ok "6 no name" $rc 2
run "$ADDON" Bad_Name "$T/p4";             ok "6 bad name" $rc 2
run "$ADDON" zz-x "$T/nope";               ok "6 missing dir" $rc 2; has "6 names the dir" "$out" "$T/nope"
run "$ADDON" zz-x "$T/p4" --harness foo;   ok "6 unknown harness" $rc 2
tmux has-session -t '=zz-x' 2>/dev/null;   ok "6 nothing created" $? 1

SPAWN_PEER_SIDE='' run "$ADDON" zz-nosplit "$T/p4"
ok "7 no split exit" $rc 0; ok "7 one pane" "$(tmux list-panes -t '=zz-nosplit:' | wc -l | tr -d ' ')" 1

: >"$T/fakecp3"
OPENCODE_MODEL='prov/my model' run "$ADDON" zz-oc "$T/p5" --harness opencode
ok "8 opencode exit" $rc 0; ok "8 opencode launch + model in pane env" "$(cat "$T/launched")" "prov/my model|opencode --as zz-oc"
rm -f "$T/launched" "$T/prelaunch"
CODEX_ARGS='-c approval_policy=never' run "$ADDON" zz-cx "$T/p6" --harness codex
ok "8 codex launch" "$(cat "$T/launched")" "unset|codex --as zz-cx -- -c approval_policy=never"
rm -f "$T/launched" "$T/prelaunch"
CLAUDE_ARGS='--resume abc' run "$ADDON" zz-ca "$T/p7"
ok "8 claude launch" "$(cat "$T/launched")" "unset|run --as zz-ca --resume abc"
rm -f "$T/fakecp3" "$T/launched" "$T/prelaunch"

: >"$T/noreg"
run "$ADDON" zz-stuck "$T/p8"
ok "9 never registers: exit" $rc 1; has "9 says to attach" "$out" "tmux attach -t '=zz-stuck'"
tmux has-session -t '=zz-stuck' 2>/dev/null; ok "9 session kept for the user" $? 0

echo "RESULT pass=$pass fail=$fail"
[ "$fail" = 0 ]
