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

func TestMergeStatusLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	// absent file -> creates with statusLine
	ch, ex, err := mergeStatusLine(p, "/usr/local/bin/cp3")
	if err != nil || !ch || ex {
		t.Fatalf("create: ch=%v ex=%v err=%v", ch, ex, err)
	}
	// second run: statusLine now exists (ours) -> untouched
	ch, ex, err = mergeStatusLine(p, "/usr/local/bin/cp3")
	if err != nil || ch || !ex {
		t.Fatalf("idempotent: ch=%v ex=%v err=%v", ch, ex, err)
	}
	// user's custom statusline is never replaced
	if err := os.WriteFile(p, []byte(`{"statusLine":{"type":"command","command":"my-script"},"env":{"X":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ch, ex, err = mergeStatusLine(p, "/usr/local/bin/cp3")
	if err != nil || ch || !ex {
		t.Fatalf("custom preserved: ch=%v ex=%v err=%v", ch, ex, err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "my-script") {
		t.Fatalf("custom statusline clobbered: %s", b)
	}
}

// The xps outage: `cp3 setup` with no --nats wrote the LOCALHOST DEFAULT into
// the MCP env, and an env var there permanently overrides ~/.config/cp3/url.
// Every Claude session on that machine silently joined a local island of one
// while every CLI tool used the fleet — and doctor, inheriting the same env,
// reported PASS. Setup must not pin a server nobody asked for.
func TestSetupDoesNotPinDefaultNATSURL(t *testing.T) {
	readEnv := func(t *testing.T, path string) map[string]any {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			MCPServers map[string]struct {
				Env map[string]any `json:"env"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.MCPServers["claude-peers"].Env
	}

	t.Run("no --nats leaves the config file authoritative", func(t *testing.T) {
		dir := t.TempDir()
		mcpPath := filepath.Join(dir, "claude.json")
		cmdSetup([]string{"--mcp", mcpPath, "--settings", filepath.Join(dir, "settings.json")})
		if url, ok := readEnv(t, mcpPath)["NATS_URL"]; ok {
			t.Errorf("setup pinned NATS_URL=%v without being asked — this is the island bug", url)
		}
	})

	t.Run("explicit --nats is honored", func(t *testing.T) {
		dir := t.TempDir()
		mcpPath := filepath.Join(dir, "claude.json")
		cmdSetup([]string{"--mcp", mcpPath, "--settings", filepath.Join(dir, "settings.json"), "--nats", "nats://10.0.0.5:4222"})
		if got := readEnv(t, mcpPath)["NATS_URL"]; got != "nats://10.0.0.5:4222" {
			t.Errorf("explicit --nats not written: got %v", got)
		}
	})
}
