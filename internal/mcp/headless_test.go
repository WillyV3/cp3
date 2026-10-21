package mcp

import (
	"strings"
	"testing"
)

func TestHeadlessReason(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	none := env(nil)
	cases := []struct {
		name   string
		getenv func(string) string
		args   string
		want   string // substring of the reason, "" for a real session
	}{
		{"interactive claude", none, "claude", ""},
		{"cp3 run", none, "claude --dangerously-load-development-channels server:claude-peers", ""},
		{"interactive with a -p inside a word", none, "claude --plugin-dir /x/mod --mcp-config /x/no-p.json", ""},
		{"agent sdk child (pi-claude-bridge)", none, "claude --output-format stream-json --verbose --input-format stream-json --model claude-haiku-5-5 --strict-mcp-config", "stream-json"},
		{"claude -p from a cron", none, `claude -p "summarize the logs"`, "-p"},
		{"claude --print", none, "claude --print hi", "-p"},
		{"bare claude -p at the end", none, "claude -p", "-p"},
		{"sdk env marker", env(map[string]string{"CLAUDE_CODE_ENTRYPOINT": "sdk-ts"}), "claude", "Agent SDK"},
		{"cli env marker is a real session", env(map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"}), "claude", ""},
		{"pi child marker", env(map[string]string{"PI_CODING_AGENT": "true"}), "claude", "pi"},
		{"explicit override", env(map[string]string{"CLAUDE_PEERS_EPHEMERAL": "1"}), "claude", "CLAUDE_PEERS_EPHEMERAL"},
	}
	for _, c := range cases {
		got := headlessReason(c.getenv, c.args)
		if c.want == "" && got != "" {
			t.Errorf("%s: want a real session, got ephemeral: %q", c.name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: want ephemeral because %q, got %q", c.name, c.want, got)
		}
	}
}
