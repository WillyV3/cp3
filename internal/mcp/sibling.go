package mcp

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// processInfo reports a live process's parent pid and command line, or
// (0, "") when it is gone. A variable so tests can describe a stand-in.
var processInfo = func(pid int) (ppid int, args string) {
	out, err := exec.Command("ps", "-o", "ppid=,args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, ""
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return 0, ""
	}
	ppid, _ = strconv.Atoi(f[0])
	return ppid, strings.Join(f[1:], " ")
}

// yieldOlderSibling stops a previous `cp3 mcp` that the same Claude process
// still has running. Claude respawns its MCP server on a reconnect without
// killing the old child (seen 2026-10-19 on macbook1: two live servers under
// one claude pid, both wanting "cad-guy", trading the name at every presence
// expiry and announcing a repair each time). Claude talks to the newer one,
// so the older must go. SIGTERM takes its signal path, which releases the
// name before exit, so claimIdentity here gets the name instead of a twin.
// The state file names the pid; ps confirms it is still our sibling and not
// a reused pid. Returns the pid stopped, or 0.
func yieldOlderSibling(parentPID int) int {
	old := ReadState(parentPID).MCPPID
	if old == 0 || old == os.Getpid() {
		return 0
	}
	ppid, args := processInfo(old)
	if ppid != parentPID || !strings.Contains(args, "cp3") || !strings.Contains(args, "mcp") {
		return 0
	}
	if err := syscall.Kill(old, syscall.SIGTERM); err != nil {
		return 0
	}
	return old
}
