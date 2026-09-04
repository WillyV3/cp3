package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"

	peers "github.com/WillyV3/cp3"
)

// teamConfig mirrors the subset of Claude Code's own
// ~/.claude/teams/{team}/config.json that identity resolution needs. Claude
// Code writes and cleans this file up itself (removed when the lead's session
// ends) — cp3 only reads it, best-effort, never treats it as authority.
type teamConfig struct {
	Members []struct {
		Name       string `json:"name"`
		TmuxPaneID string `json:"tmuxPaneId"`
	} `json:"members"`
}

// teammateName finds the name Claude Code's own Agent Teams feature already
// assigned to THIS process, when it was spawned as a split-pane tmux
// teammate. Returns ok=false for everyone else: no team, an in-process
// teammate (no distinct pane), or the lead itself.
//
// Why this beats the shared-directory default: a tmux teammate is launched
// fresh into the same project dir as its lead, so ordinary zero-config
// resolution (peers.ResolveIdentity) reads the SAME identity marker file
// the lead already holds — confirmed live on this fleet, where a
// single team spawning N teammates in one dir produced N collisions against
// that one marker (11 dead inbox-astrobot-s-NN consumers are the fossil
// record). Claude Code already solved the naming problem when it spawned the
// teammate — every member gets a distinct, deliberate name — and records
// exactly the fact needed to find it: which tmux pane holds which name.
// TMUX_PANE is inherited into this process the same way it is for the
// pane-title harvest (panetitle.go).
//
// The lead can never match by construction: its own member entry always
// carries a sentinel tmuxPaneId ("leader" or "") from Claude Code, never a
// real "%NNN" pane id — verified against this box's own live team config —
// so it can never equal a real $TMUX_PANE value.
func teammateName() (name string, ok bool) {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return "", false // not under tmux: no signal, not an error
	}
	dir := teamsDir()
	if dir == "" {
		return "", false
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*", "config.json"))
	if err != nil {
		return "", false
	}
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			continue // team torn down mid-glob, or unreadable: not fatal
		}
		var cfg teamConfig
		if json.Unmarshal(b, &cfg) != nil {
			continue // not ours to interpret; skip rather than error
		}
		for _, m := range cfg.Members {
			if m.TmuxPaneID != pane || m.Name == "" {
				continue
			}
			// Every other identity source (ResolveIdentity's flag/env/file/
			// default, the CLI's --as) sanitizes before a name ever reaches
			// Claim/Register — Claim uses p.Agent raw as a NATS subject
			// token, with no validation at that layer. m.Name comes from
			// Claude Code's own JSON, outside cp3's control: skipping
			// SanitizeName here would let a teammate name containing a
			// space, ".", or a NATS wildcard character produce a malformed
			// or over-broad subject, the same class of bug validRecipient
			// already closed for --to.
			if n := peers.SanitizeName(m.Name); n != "" {
				return n, true
			}
		}
	}
	return "", false
}

// preferTeammateName upgrades a resolved (name, source) pair to the team's own
// name for this pane, when one exists. Only overrides the LENIENT tiers
// ("file", "default") — an explicit --as/CLAUDE_PEERS_AGENT is a human's
// deliberate choice and must win regardless of what tmux pane it runs in.
func preferTeammateName(name, source string) (string, string) {
	switch source {
	case "file", "default":
		if tn, ok := teammateName(); ok {
			return tn, "team"
		}
	}
	return name, source
}

func teamsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "teams")
}
