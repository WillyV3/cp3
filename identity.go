package peers

import (
	"os"
	"path/filepath"
	"strings"
)

// identityMarkerFile is the current, self-documenting marker name: it names
// the peer identity a session in this directory should claim. Renamed from
// the legacy identityMarkerFileLegacy because "which peer does this dir
// claim" wasn't obvious from ".claude-peers-agent" to someone who'd never
// seen it before.
const identityMarkerFile = ".cp3-peer-name"

// identityMarkerFileLegacy is read forever, not for one deprecation window.
// The fleet has ~20 of these already (project dirs, several home dirs with a
// deliberately-chosen name unrelated to the dir's basename). A cutover that
// stops reading it would silently rename every one of those sessions back to
// its dir-basename default the next time cp3 restarts there — exactly the
// astrobot/sontara-mobile involuntary-rename class of incident this resolver
// exists to prevent. One extra os.ReadFile is cheaper than that risk forever.
const identityMarkerFileLegacy = ".claude-peers-agent"

// ResolveIdentity resolves this session's agent name, most explicit wins:
// explicit (--as flag) > CLAUDE_PEERS_AGENT > marker file in cwd > sanitized
// basename of cwd. The dir default makes the common case zero-config: the
// agent working in ~/projects/pith is "pith" — which is how people already
// think about their sessions. Source is "flag", "env", "file", or "default".
func ResolveIdentity(cwd, explicit string) (name, source string) {
	if explicit != "" {
		return SanitizeName(explicit), "flag"
	}
	if v := os.Getenv("CLAUDE_PEERS_AGENT"); v != "" {
		return SanitizeName(v), "env"
	}
	for _, marker := range [...]string{identityMarkerFile, identityMarkerFileLegacy} {
		if b, err := os.ReadFile(filepath.Join(cwd, marker)); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return SanitizeName(v), "file"
			}
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
