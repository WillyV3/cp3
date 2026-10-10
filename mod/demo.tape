# vhs demo.tape — records the cp3 mod inside Claude Code. Run from mod/.
# Needs vhs, a Nerd Font, cp3 on PATH, and a private tmux server already
# running the session (default bindings, so prefix+d detaches):
#
#   tmux -L cp3demo -f /dev/null new -d -s demo -x 166 -y 43 -c ~/.cache/cp3-mod-demo \
#     "CLAUDE_PEERS_AGENT=demo claude --plugin-dir ~/projects/cp3/mod \
#        --strict-mcp-config --mcp-config '{\"mcpServers\":{}}'"
#
# No MCP servers, so the mod owns the peer identity: plain claude, no
# dev-channel flag. Attaching keeps Claude Code's one-time dialogs out of
# the script.
Output demo.mp4
Output demo.gif
Set Shell bash
Set FontFamily "CaskaydiaMono Nerd Font"
Set FontSize 15
Set Width 1500
Set Height 860
Set Padding 12
Set TypingSpeed 45ms

Hide
Type `(sleep 28; cp3 send --from keeper --to demo 'the roster next to you is live; every name on it is one SendMessage away') >/dev/null 2>&1 & tmux -L cp3demo attach -t demo` Enter
Sleep 3s
Type "/clear" Enter
Sleep 4s
Show

Sleep 2s
Type "/cp3" Enter
Sleep 4s
Type "keeper hello from the demo"
Sleep 1s
Enter
Sleep 5s
Escape
Sleep 16s
Hide
Ctrl+b
Type "d"
