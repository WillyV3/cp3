package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// sessionState is what the statusline needs to render THIS session truthfully:
// the claimed name (may be a fallback twin or "" while ephemeral) and the name
// identity resolution wanted at session start. Keyed by the Claude process's
// pid — the one handle the MCP server (child of claude) and the statusline
// command (also spawned by claude) both share. Local-only by nature: the
// statusline always runs on the same machine as its session's MCP server.
type sessionState struct {
	Claimed string `json:"claimed"`
	Wanted  string `json:"wanted"`
}

func stateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "cp3", "by-claude-pid")
}

// StatePath returns the session-state file for a given claude pid.
func StatePath(pid int) string {
	d := stateDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, strconv.Itoa(pid))
}

// ReadState loads the state for a claude pid ("" fields if absent/corrupt).
func ReadState(pid int) sessionState {
	var st sessionState
	p := StatePath(pid)
	if p == "" {
		return st
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return st
	}
	json.Unmarshal(b, &st)
	return st
}

func (s *server) writeState() {
	p := StatePath(s.parentPID)
	if p == "" {
		return
	}
	os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.Marshal(sessionState{Claimed: s.name(), Wanted: s.configured})
	os.WriteFile(p, b, 0o600)
}

func (s *server) removeState() {
	if p := StatePath(s.parentPID); p != "" {
		os.Remove(p)
	}
}
