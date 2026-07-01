package peers

import (
	"os"
	"path/filepath"
	"strings"
)

// ResolveIdentity resolves this session's agent name, most explicit wins:
// explicit (--as flag) > CLAUDE_PEERS_AGENT > .claude-peers-agent file in cwd >
// sanitized basename of cwd. The dir default makes the common case zero-config:
// the agent working in ~/projects/pith is "pith" — which is how people already
// think about their sessions. Source is "flag", "env", "file", or "default".
func ResolveIdentity(cwd, explicit string) (name, source string) {
	if explicit != "" {
		return SanitizeName(explicit), "flag"
	}
	if v := os.Getenv("CLAUDE_PEERS_AGENT"); v != "" {
		return SanitizeName(v), "env"
	}
	if b, err := os.ReadFile(filepath.Join(cwd, ".claude-peers-agent")); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return SanitizeName(v), "file"
		}
	}
	if n := SanitizeName(filepath.Base(cwd)); n != "" && n != "/" {
		return n, "default"
	}
	return "", "none"
}

// SanitizeName makes a string safe as a NATS subject token (names ride in
// peers.msg.<name>): lowercase, [a-z0-9_-] only, everything else collapses to
// a single '-', trimmed, capped at 32 chars. Returns "" if nothing survives.
func SanitizeName(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	return out
}
