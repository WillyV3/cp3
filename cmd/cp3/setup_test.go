package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readServers(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	return servers
}

var entry = map[string]any{"command": "cp3-mcp", "env": map[string]any{"CLAUDE_PEERS_AGENT": "keeper"}}

// create-if-missing: writes our entry, no .bak (nothing to back up).
func TestMergeCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", ".claude.json")
	changed, err := mergeMCPServer(path, "claude-peers", entry)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if _, ok := readServers(t, path)["claude-peers"]; !ok {
		t.Fatal("claude-peers not written")
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatal("unexpected .bak on fresh create")
	}
}

// no-clobber: an existing unrelated server survives; .bak holds the original.
func TestMergePreservesOtherServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	orig := `{"mcpServers":{"other":{"command":"x"}},"unrelatedKey":42}`
	os.WriteFile(path, []byte(orig), 0o644)

	changed, err := mergeMCPServer(path, "claude-peers", entry)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	servers := readServers(t, path)
	if _, ok := servers["other"]; !ok {
		t.Fatal("clobbered the 'other' server")
	}
	if _, ok := servers["claude-peers"]; !ok {
		t.Fatal("did not add claude-peers")
	}
	// top-level unrelated key must survive too
	var doc map[string]any
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &doc)
	if doc["unrelatedKey"] == nil {
		t.Fatal("dropped unrelated top-level key")
	}
	// .bak holds the exact original
	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != orig {
		t.Fatalf(".bak mismatch: %q", string(bak))
	}
}

// idempotent: second identical merge is a no-op.
func TestMergeIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	if _, err := mergeMCPServer(path, "claude-peers", entry); err != nil {
		t.Fatal(err)
	}
	changed, err := mergeMCPServer(path, "claude-peers", entry)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("second identical merge should be a no-op")
	}
}

// parse-error-bail: invalid JSON is never overwritten (config not destroyed).
func TestMergeBailsOnBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	bad := `{ this is not json `
	os.WriteFile(path, []byte(bad), 0o644)
	changed, err := mergeMCPServer(path, "claude-peers", entry)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if changed {
		t.Fatal("must not report change on parse failure")
	}
	data, _ := os.ReadFile(path)
	if string(data) != bad {
		t.Fatal("file was modified despite parse error")
	}
}

func TestAppendAgentsMD(t *testing.T) {
	p := filepath.Join(t.TempDir(), "AGENTS.md")
	// create
	if ch, err := appendAgentsMD(p); err != nil || !ch {
		t.Fatalf("create: changed=%v err=%v", ch, err)
	}
	// idempotent
	if ch, err := appendAgentsMD(p); err != nil || ch {
		t.Fatalf("idempotent: changed=%v err=%v", ch, err)
	}
	// preserves surrounding content and replaces block in place
	orig, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append([]byte("# mine\n\n"), append(orig, []byte("\ntail\n")...)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if ch, err := appendAgentsMD(p); err != nil || ch {
		t.Fatalf("replace-noop: changed=%v err=%v", ch, err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	if !strings.HasPrefix(s, "# mine") || !strings.Contains(s, "tail") || strings.Count(s, agentsMarkerStart) != 1 {
		t.Fatalf("surroundings mangled:\n%s", s)
	}
}
