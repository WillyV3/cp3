package mcp

import "testing"

// Cases are the real pane titles from a 39-pane dump on the dev box — the
// three that carry work, the six that only echo a name, and the shell prompts
// that make up the other 30 panes.
func TestHarvestTitle(t *testing.T) {
	const agent, sess = "keeper", "peer-mcp-maintainer"
	for _, tc := range []struct {
		name, raw, agent, sess, want string
	}{
		{"real summary", "✳ Galaxy tablets setup and redirection", "doorboard", "doorboard",
			"Galaxy tablets setup and redirection"},
		{"real summary 2", "✳ Report to caretaker", "ltv-dev", "ltv-dev", "Report to caretaker"},

		{"echoes agent", "✳ plugin-dev", "plugin-dev", "plugin-dev", ""},
		{"echoes agent, case-folded", "✳ Jim", "jim", "Jim", ""},
		{"echoes tmux session, spaced", "✳ peer mcp maintainer", agent, sess, ""},
		{"echoes tmux session", "✳ working-on-devices", "caretaker", "working-on-devices", ""},

		{"shell prompt", "willy@omarchy:~/projects/butler", "butler", "butler", ""},
		{"bare hostname", "omarchy", agent, sess, ""},
		{"empty", "", agent, sess, ""},
		{"marker only", "✳", agent, sess, ""},
		{"marker and space", "✳   ", agent, sess, ""},
		{"surrounding whitespace kept out", "  ✳  Plan email infrastructure  ", "stilgar", "sontara-sales",
			"Plan email infrastructure"},
	} {
		if got := harvestTitle(tc.raw, tc.agent, tc.sess); got != tc.want {
			t.Errorf("%s: harvestTitle(%q, %q, %q) = %q, want %q",
				tc.name, tc.raw, tc.agent, tc.sess, got, tc.want)
		}
	}
}

// The documented ceiling: name-equality only. These are wrong-but-harmless and
// exist so the limit is visible if someone tightens the matcher later.
func TestHarvestTitleKnownEscapes(t *testing.T) {
	if got := harvestTitle("✳ sontara-omarchy", "astrobot-omarchy", "sontara"); got != "sontara-omarchy" {
		t.Errorf("ceiling moved: got %q", got)
	}
	if got := harvestTitle("✳ impl", "astrobot", "sontara"); got != "impl" {
		t.Errorf("ceiling moved: got %q", got)
	}
}
