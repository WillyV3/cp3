package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeDelivery(t *testing.T) {
	write := func(t *testing.T, home, rel, body string) {
		t.Helper()
		p := filepath.Join(home, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installed := `{"plugins":{"cp3@cp3":[{"version":"0.4.3"}]}}`
	mcp := `{"mcpServers":{"claude-peers":{"command":"sh"}}}`

	cases := []struct {
		name               string
		installed, enabled string
		claudeJSON         string
		want               string
	}{
		{"neither", "", "", "", ""},
		{"mod enabled", installed, `{"enabledPlugins":{"cp3@cp3":true}}`, "", "mod"},
		{"mod wins over mcp", installed, `{"enabledPlugins":{"cp3@cp3":true}}`, mcp, "mod"},
		{"mod disabled, no mcp", installed, `{"enabledPlugins":{"cp3@cp3":false}}`, "", ""},
		{"mod disabled, mcp", installed, `{"enabledPlugins":{}}`, mcp, "mcp"},
		{"mcp only", "", "", mcp, "mcp"},
		{"mcp not executable", "", "", `{"mcpServers":{"claude-peers":{"command":"/nope/cp3"}}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.installed != "" {
				write(t, home, ".claude/plugins/installed_plugins.json", tc.installed)
			}
			if tc.enabled != "" {
				write(t, home, ".claude/settings.json", tc.enabled)
			}
			if tc.claudeJSON != "" {
				write(t, home, ".claude.json", tc.claudeJSON)
			}
			got, detail := claudeDelivery(home)
			if got != tc.want {
				t.Fatalf("path = %q (%s), want %q", got, detail, tc.want)
			}
		})
	}
}
