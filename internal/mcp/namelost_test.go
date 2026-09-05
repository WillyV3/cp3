package mcp

import (
	"bytes"
	"encoding/json"
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
	var out bytes.Buffer
	s := &server{t: &transport{out: &out}, me: "inspiron-omarchy-main"}

	s.nameLost(errors.New("inspiron-omarchy-main held by session s-93220 on inspiron-omarchy: agent name now held by another session"))

	// Parse the actual notification content rather than string-matching the
	// raw JSON line — the content is itself JSON-escaped, and a raw substring
	// check on `"X" is now held` would need to know that escaping to match.
	var note struct {
		Params struct {
			Content string `json:"content"`
		} `json:"params"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &note); err != nil {
		t.Fatalf("notify wrote unparseable JSON-RPC: %v\nraw: %s", err, out.String())
	}
	if strings.Contains(note.Params.Content, `peer name "" is`) {
		t.Fatalf("regression: notice quotes an empty name: %s", note.Params.Content)
	}
	if !strings.Contains(note.Params.Content, `peer name "inspiron-omarchy-main" is`) {
		t.Errorf("notice does not name the lost identity: %s", note.Params.Content)
	}
	if s.name() != "" {
		t.Errorf("session should go ephemeral (nameless) until tryReclaim wins it back, got %q", s.name())
	}
}

// The other half of the same field report: a session that never holds a
// name at all (loses the race at boot, via claimIdentity) got NO channel
// notification whatsoever — claimIdentity only logs to stderr, which nobody
// watches in a live TUI session. Silently nameless is worse than failing
// loud; this is nameLost's missing startup-time counterpart.
func TestStartupCollisionNoticeNamesTheWantedIdentity(t *testing.T) {
	var out bytes.Buffer
	s := &server{t: &transport{out: &out}}

	s.notifyStartupCollision("inspiron-omarchy-main")

	var note struct {
		Params struct {
			Content string `json:"content"`
		} `json:"params"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &note); err != nil {
		t.Fatalf("notify wrote unparseable JSON-RPC: %v\nraw: %s", err, out.String())
	}
	if !strings.Contains(note.Params.Content, `peer name "inspiron-omarchy-main" is currently held`) {
		t.Errorf("notice does not name the wanted identity: %s", note.Params.Content)
	}
	// Must not borrow nameLost's "stopped receiving" framing — this session
	// never had reachability to lose.
	if strings.Contains(note.Params.Content, "stopped receiving") {
		t.Errorf("startup notice falsely implies it was previously reachable: %s", note.Params.Content)
	}
}
