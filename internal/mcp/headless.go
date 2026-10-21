package mcp

import (
	"os/exec"
	"strconv"
	"strings"
)

// parentArgs is the launching Claude's command line, "" when it cannot be
// read. ps, not /proc, so it works on macOS too. A variable for tests.
var parentArgs = func(pid int) string {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// headlessReason says why this Claude must stay ephemeral (send and list,
// but no name, no presence), or "" when it is a session a person sits in.
//
// A Claude that another program drives is not a peer anyone can talk to:
// pi-claude-bridge runs Claude Code as pi's model backend, and that child
// loaded the claude-peers MCP from ~/.claude.json and claimed the directory's
// name as a "claude" peer, racing the pi session that was the real peer
// (homelab, 2026-10-21: pi got "homelab-pi-test-pi", the bridge child held
// "homelab-pi-test", mail to the bare name went to a process that never
// read it). The same is true of claude -p from a cron or a daemon. Three
// signals, any one is enough: the Agent SDK marks its child's environment,
// pi marks every child it spawns, and a headless Claude's command line says
// so. CLAUDE_PEERS_EPHEMERAL stays as the explicit override.
func headlessReason(getenv func(string) string, args string) string {
	switch {
	case getenv("CLAUDE_PEERS_EPHEMERAL") != "":
		return "CLAUDE_PEERS_EPHEMERAL set"
	case strings.HasPrefix(getenv("CLAUDE_CODE_ENTRYPOINT"), "sdk-"):
		return "this Claude is driven by the Agent SDK (CLAUDE_CODE_ENTRYPOINT=" + getenv("CLAUDE_CODE_ENTRYPOINT") + ")"
	case getenv("PI_CODING_AGENT") != "":
		return "this Claude is pi's model backend (PI_CODING_AGENT set); the pi session is the peer"
	}
	a := " " + args + " "
	switch {
	case strings.Contains(a, " --output-format stream-json "), strings.Contains(a, " --input-format stream-json "):
		return "this Claude is headless (stream-json)"
	case strings.Contains(a, " -p "), strings.Contains(a, " --print "), strings.HasSuffix(a, " -p  "), strings.HasSuffix(a, " --print  "):
		return "this Claude is headless (-p)"
	}
	return ""
}
