package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// a stand-in for the stale server: any process whose pid we can put in the
// state file. processInfo is faked to describe it as our sibling.
func staleStandIn(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

func writeStateFor(t *testing.T, parentPID int, st sessionState) {
	t.Helper()
	p := StatePath(parentPID)
	os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.Marshal(st)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestYieldOlderSiblingStopsTheStaleServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stale := staleStandIn(t)
	me := os.Getpid()
	writeStateFor(t, me, sessionState{Claimed: "cad-guy", MCPPID: stale.Process.Pid})
	orig := processInfo
	processInfo = func(pid int) (int, string) { return me, "/usr/local/bin/cp3 mcp" }
	t.Cleanup(func() { processInfo = orig })

	if got := yieldOlderSibling(me); got != stale.Process.Pid {
		t.Fatalf("yield returned %d, want the stale pid %d", got, stale.Process.Pid)
	}
	done := make(chan error, 1)
	go func() { done <- stale.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stale server still running 5s after yield")
	}
	ws, ok := stale.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("stale server ended with %v, want SIGTERM (the signal path releases the name)", stale.ProcessState)
	}
}

func TestYieldOlderSiblingLeavesAReusedPidAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	other := staleStandIn(t)
	me := os.Getpid()
	writeStateFor(t, me, sessionState{Claimed: "cad-guy", MCPPID: other.Process.Pid})
	orig := processInfo
	processInfo = func(pid int) (int, string) { return 1, "/usr/bin/some-daemon" } // pid reused by an unrelated process
	t.Cleanup(func() { processInfo = orig })

	if got := yieldOlderSibling(me); got != 0 {
		t.Fatalf("yield stopped %d, which is not a cp3 mcp under this claude", got)
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the unrelated process was killed: %v", err)
	}
}

func TestYieldOlderSiblingIgnoresItselfAndAbsence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	me := os.Getpid()
	if got := yieldOlderSibling(me); got != 0 {
		t.Fatalf("no state file, yet yield stopped %d", got)
	}
	writeStateFor(t, me, sessionState{Claimed: "x", MCPPID: me})
	if got := yieldOlderSibling(me); got != 0 {
		t.Fatalf("state names this very process, yet yield stopped %d", got)
	}
}

func TestWriteStateRecordsTheServerPid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &server{parentPID: 4321, configured: "cad-guy"}
	s.writeState()
	if got := ReadState(4321).MCPPID; got != os.Getpid() {
		t.Fatalf("state records mcpPid %d, want this process %d", got, os.Getpid())
	}
}
