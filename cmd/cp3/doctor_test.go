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
	cases := []struct {
		name, claudeJSON string
		want             bool
	}{
		{"no claude.json", "", false},
		{"no entry", `{"mcpServers":{}}`, false},
		{"runnable entry", `{"mcpServers":{"claude-peers":{"command":"sh"}}}`, true},
		{"not executable", `{"mcpServers":{"claude-peers":{"command":"/nope/cp3"}}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.claudeJSON != "" {
				write(t, home, ".claude.json", tc.claudeJSON)
			}
			got, detail := claudeDelivery(home)
			if got != tc.want {
				t.Fatalf("ok = %v (%s), want %v", got, detail, tc.want)
			}
		})
	}
}
