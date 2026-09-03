package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTeamConfig drops a minimal team config fixture under $HOME/.claude/teams,
// shaped like the real files Claude Code writes (verified against this
// fleet's own ~/.claude/teams/*/config.json).
func writeTeamConfig(t *testing.T, team string, members ...[2]string) {
	t.Helper()
	dir := filepath.Join(teamsDir(), team)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b []byte
	b = append(b, `{"members":[`...)
	for i, m := range members {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"name":"`...)
		b = append(b, m[0]...)
		b = append(b, `","tmuxPaneId":"`...)
		b = append(b, m[1]...)
		b = append(b, `"}`...)
	}
	b = append(b, `]}`...)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTeammateName(t *testing.T) {
	t.Run("no tmux, no signal", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("TMUX_PANE", "")
		if _, ok := teammateName(); ok {
			t.Error("claimed a match with no TMUX_PANE")
		}
	})

	t.Run("under tmux but no team ever ran here", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("TMUX_PANE", "%15")
		if _, ok := teammateName(); ok {
			t.Error("claimed a match with no ~/.claude/teams at all")
		}
	})

	t.Run("real match: the pipeline-overhaul shape, 3 teammates one dir", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		writeTeamConfig(t, "pipeline-overhaul",
			[2]string{"team-lead", ""},
			[2]string{"graph-agent", "%134"},
			[2]string{"enrich-agent", "%135"},
			[2]string{"transcribe-agent", "%136"},
		)
		t.Setenv("TMUX_PANE", "%135")
		name, ok := teammateName()
		if !ok || name != "enrich-agent" {
			t.Errorf("got (%q, %v), want (enrich-agent, true)", name, ok)
		}
	})

	t.Run("the lead's sentinel pane id can never match a real pane", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		writeTeamConfig(t, "solo-lead", [2]string{"team-lead", ""})
		t.Setenv("TMUX_PANE", "%15") // the lead's own real pane, running in tmux itself
		if _, ok := teammateName(); ok {
			t.Error("matched the lead's own session via its sentinel tmuxPaneId")
		}
		writeTeamConfig(t, "split-lead", [2]string{"team-lead", "leader"})
		if _, ok := teammateName(); ok {
			t.Error("matched the \"leader\" sentinel against a real pane id")
		}
	})

	t.Run("team-assigned name is sanitized before it can reach a NATS subject", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		// Claude Code names teammates in free text; nothing there enforces
		// subject-safe characters. "." and spaces are exactly what
		// SanitizeName exists to fold before a name rides in peers.msg.<name>.
		writeTeamConfig(t, "onboarding", [2]string{"API Gateway v2.migration", "%50"})
		t.Setenv("TMUX_PANE", "%50")
		name, ok := teammateName()
		if !ok {
			t.Fatal("expected a match")
		}
		if name != "api-gateway-v2-migration" {
			t.Errorf("unsanitized name reached the caller: %q", name)
		}
	})

	t.Run("malformed sibling config does not hide a valid match elsewhere", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		dir := filepath.Join(teamsDir(), "broken")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o644)
		writeTeamConfig(t, "good", [2]string{"oauth", "%48"})
		t.Setenv("TMUX_PANE", "%48")
		name, ok := teammateName()
		if !ok || name != "oauth" {
			t.Errorf("got (%q, %v), want (oauth, true) despite a broken sibling file", name, ok)
		}
	})
}

func TestPreferTeammateName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeTeamConfig(t, "onboarding", [2]string{"ui", "%49"})
	t.Setenv("TMUX_PANE", "%49")

	for _, source := range []string{"flag", "env"} {
		if name, got := preferTeammateName("astrobot", source); name != "astrobot" || got != source {
			t.Errorf("source=%s: explicit identity overridden: got (%q, %q)", source, name, got)
		}
	}
	for _, source := range []string{"file", "default"} {
		name, got := preferTeammateName("astrobot", source)
		if name != "ui" || got != "team" {
			t.Errorf("source=%s: want (ui, team), got (%q, %q)", source, name, got)
		}
	}

	t.Setenv("TMUX_PANE", "%999") // no match on this box
	if name, got := preferTeammateName("astrobot", "file"); name != "astrobot" || got != "file" {
		t.Errorf("no match: want passthrough (astrobot, file), got (%q, %q)", name, got)
	}
}
