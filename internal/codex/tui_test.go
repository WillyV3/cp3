package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	peers "github.com/WillyV3/cp3"
)

// Real `codex exec --json` output, codex-cli 0.155.0 (2026-09-18).
func TestThreadFromExec(t *testing.T) {
	out := `{"type":"thread.started","thread_id":"01a0b579-ff9c-79d1-89a6-59164068e661"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"READY"}}
{"type":"turn.completed","usage":{"input_tokens":9709}}`
	if got := threadFromExec([]byte(out)); got != "01a0b579-ff9c-79d1-89a6-59164068e661" {
		t.Errorf("got %q", got)
	}
	if got := threadFromExec([]byte("Error loading config.toml\n")); got != "" {
		t.Errorf("non-JSON failure output must yield no thread, got %q", got)
	}
}

// queue must hit the server the TUI is attached to, on the right thread, with
// the reply hint naming the identity this session actually holds.
func TestQueueInvokesCodexQueueWithHint(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "codex")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+rec+"\n"), 0o755)
	old := codexBin
	codexBin = fake
	t.Cleanup(func() { codexBin = old })

	err := queue(context.Background(), "ws://127.0.0.1:1", "thr_1", "cx-omarchy", peers.Message{From: "keeper", Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(rec)
	got := string(b)
	for _, want := range []string{"queue\n--remote\nws://127.0.0.1:1\n--thread\nthr_1\n--message\n", "--from cx-omarchy --to keeper", "\nhello\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("args missing %q in:\n%s", want, got)
		}
	}
}

// A failed queue must surface codex's own words, not a bare exit status.
func TestQueueSurfacesCodexError(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'no such thread' >&2\nexit 1\n"), 0o755)
	old := codexBin
	codexBin = fake
	t.Cleanup(func() { codexBin = old })
	err := queue(context.Background(), "ws://x", "t", "me", peers.Message{From: "a", Content: "b"})
	if err == nil || !strings.Contains(err.Error(), "no such thread") {
		t.Errorf("want codex's stderr in the error, got %v", err)
	}
}
