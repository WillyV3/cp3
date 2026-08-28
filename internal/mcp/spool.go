package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	peers "github.com/WillyV3/cp3"
)

// The JetStream ack means "this process received the bytes" — it does NOT mean
// the human's session displayed them. Claude Code returns no receipt for a
// channel notification, so a session that drops one (channel not loaded, busy,
// mid-reconnect) used to lose the message permanently: the ack had already
// advanced the durable consumer, and the only copy lived in a memory buffer
// that died with the process.
//
// So user-delivery gets its own durable record: append to a per-agent spool
// BEFORE notifying, and only remove once the agent has actually taken the
// message (check_messages, or a startup replay into a live session). Transport
// ack and user delivery are now separate facts, which is what they always were.
var spoolMu sync.Mutex

func spoolPath(agent string) string {
	home, err := os.UserHomeDir()
	if err != nil || agent == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "cp3", "spool", peers.SanitizeName(agent)+".jsonl")
}

// spoolAppend records a message as received-but-not-yet-seen-by-the-agent.
func spoolAppend(agent string, m peers.Message) {
	p := spoolPath(agent)
	if p == "" {
		return
	}
	spoolMu.Lock()
	defer spoolMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
}

// spoolDrain returns everything the agent hasn't taken yet and clears the file.
// Called when the agent demonstrably has the messages: check_messages, or a
// startup replay into a live session.
func spoolDrain(agent string) []peers.Message {
	p := spoolPath(agent)
	if p == "" {
		return nil
	}
	spoolMu.Lock()
	defer spoolMu.Unlock()
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []peers.Message
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m peers.Message
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	os.Remove(p)
	return out
}

// spoolRemove drops specific message ids, leaving the rest queued. This is the
// counterpart spoolAppend needed and did not have: the spool cleared only via
// check_messages and startup replay — both FALLBACK paths — so a normally
// delivered message stayed queued forever and every restart replayed the whole
// history. rover's file held 13 days of already-read mail.
func spoolRemove(agent string, ids []string) {
	if len(ids) == 0 {
		return
	}
	p := spoolPath(agent)
	if p == "" {
		return
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	spoolMu.Lock()
	defer spoolMu.Unlock()
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var keep []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m peers.Message
		if json.Unmarshal([]byte(line), &m) == nil && drop[m.ID] {
			continue
		}
		keep = append(keep, line)
	}
	if len(keep) == 0 {
		os.Remove(p)
		return
	}
	os.WriteFile(p, []byte(strings.Join(keep, "\n")+"\n"), 0o600)
}
