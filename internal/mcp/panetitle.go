package mcp

import (
	"os"
	"os/exec"
	"strings"

	peers "github.com/WillyV3/cp3"
)

// paneTitle reads this agent's OWN tmux pane title and reduces it to something
// worth publishing. Claude Code writes a "✳ <label>" terminal title, and tmux
// hands it back as #{pane_title}; TMUX_PANE is inherited all the way down into
// this process, so no pane discovery is needed.
//
// Returns "" whenever there is nothing useful — not in tmux, tmux gone, no
// marker, or a title that only echoes a name the caller already has. "" is the
// right answer for those: Heartbeat writes it through, so a title that goes
// stale clears itself within one interval.
func (s *server) paneTitle() string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return "" // not under tmux: nothing to harvest, not an error
	}
	// One exec for both fields — the session name is needed to spot echoes and
	// is not otherwise known here.
	out, err := exec.Command("tmux", "display-message", "-p", "-t", pane,
		"#{session_name}\t#{pane_title}").Output()
	if err != nil {
		return "" // tmux died, pane closed, server unreachable: all just "no title"
	}
	sess, raw, ok := strings.Cut(strings.TrimRight(string(out), "\n"), "\t")
	if !ok {
		return ""
	}
	return harvestTitle(raw, s.name(), sess)
}

// harvestTitle is the whole policy, kept pure so it can be tested without tmux.
//
// Two rules, both learned from dumping all 39 panes on the dev box:
//
//   - REQUIRE the ✳ marker, don't merely strip it. 30 of 39 panes carried a
//     shell-prompt title like "willy@omarchy:~/projects/butler". Harvesting
//     whatever set the terminal title fills the field with noise; requiring the
//     marker degrades to empty instead.
//   - Drop titles that only echo a name. 6 of the 9 marked panes read back the
//     agent or tmux session name ("✳ plugin-dev", "✳ peer mcp maintainer"),
//     which is strictly worse than empty for a reader that already has the name
//     column.
//
// ponytail: name-equality only. "✳ sontara-omarchy" on session "sontara" and a
// bare "✳ impl" still slip through — fuzzy matching would start rejecting real
// summaries, and a wrong-but-plausible title is worse than a redundant one.
func harvestTitle(raw, agent, tmuxSession string) string {
	t := strings.TrimSpace(raw)
	rest, ok := strings.CutPrefix(t, "✳")
	if !ok {
		return ""
	}
	t = strings.TrimSpace(rest)
	if t == "" {
		return ""
	}
	// SanitizeName folds case and spaces the same way agent names are built, so
	// "Jim" matches jim and "peer mcp maintainer" matches peer-mcp-maintainer.
	switch n := peers.SanitizeName(t); n {
	case agent, peers.SanitizeName(tmuxSession):
		return ""
	}
	return t
}
