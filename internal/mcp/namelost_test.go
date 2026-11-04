package mcp

import (
	"errors"
	"strings"
	"testing"
)

// Field report from inspiron-omarchy: the takeover notice read `Your peer
// name "" is now held by another session (inspiron-omarchy-main held by
// session s-93220 ...)` — the real name only visible in the wrapped error's
// own text, the quoted slot empty. Root cause: nameLost formatted the notice
// AFTER already clearing s.me to "" via setName(""), so %q had nothing left
// to quote.
func TestNameLostNoticeNamesTheLostIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // writeState() must not touch the real ~/.local/share/cp3
	var posted string
	s := &server{me: "inspiron-omarchy-main", post: func(text string) error { posted = text; return nil }}

	s.nameLost(errors.New("inspiron-omarchy-main held by session s-93220 on inspiron-omarchy: agent name now held by another session"))

	if strings.Contains(posted, `peer name "" is`) {
		t.Fatalf("regression: notice quotes an empty name: %s", posted)
	}
	if !strings.Contains(posted, `peer name "inspiron-omarchy-main" is`) {
		t.Errorf("notice does not name the lost identity: %s", posted)
	}
	if s.name() != "" {
		t.Errorf("session should go ephemeral (nameless) until tryReclaim wins it back, got %q", s.name())
	}
}

// The other half of the same field report: a session that never holds a
// name at all (loses the race at boot, via claimIdentity) got NO notice
// whatsoever — claimIdentity only logs to stderr, which nobody
// watches in a live TUI session. Silently nameless is worse than failing
// loud; this is nameLost's missing startup-time counterpart.
func TestStartupCollisionNoticeNamesTheWantedIdentity(t *testing.T) {
	var posted string
	s := &server{post: func(text string) error { posted = text; return nil }}

	s.notifyStartupCollision("inspiron-omarchy-main")

	if !strings.Contains(posted, `peer name "inspiron-omarchy-main" is currently held`) {
		t.Errorf("notice does not name the wanted identity: %s", posted)
	}
	// Must not borrow nameLost's "stopped receiving" framing — this session
	// never had reachability to lose.
	if strings.Contains(posted, "stopped receiving") {
		t.Errorf("startup notice falsely implies it was previously reachable: %s", posted)
	}
}
