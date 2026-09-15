package opencode

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	peers "github.com/WillyV3/cp3"
)

// withServer points the package's opencode client at a test server for the
// duration of the test and restores it after — ocURL is a package var read
// once at init, so tests must override it directly rather than via env.
func withServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := ocURL
	ocURL = srv.URL
	t.Cleanup(func() { ocURL = old })
}

func TestResolveSessionCreatesNewByDefault(t *testing.T) {
	t.Setenv("OPENCODE_SESSION_ID", "")
	var createCalls, getCalls int
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/session":
			createCalls++
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"id": "ses_new"}})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/session/"):
			getCalls++
			w.WriteHeader(200)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	got, err := resolveSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "ses_new" {
		t.Errorf("got %q, want the created session id", got)
	}
	if createCalls != 1 || getCalls != 0 {
		t.Errorf("default path should create once and never GET: create=%d get=%d", createCalls, getCalls)
	}
}

// The actual feature Willy asked for: peer messages landing in the session
// he's already looking at, not a new one nobody's watching.
func TestResolveSessionJoinsExistingWhenSet(t *testing.T) {
	t.Setenv("OPENCODE_SESSION_ID", "ses_live_tui")
	var createCalls int
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/session/ses_live_tui":
			w.WriteHeader(200)
		case r.Method == "POST" && r.URL.Path == "/api/session":
			createCalls++
			t.Error("joined an existing session but still created a new one")
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	got, err := resolveSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "ses_live_tui" {
		t.Errorf("got %q, want the targeted session id unchanged", got)
	}
	if createCalls != 0 {
		t.Error("must not create a session when joining an existing one")
	}
}

// A typo'd or stale OPENCODE_SESSION_ID must fail loudly at startup, not as
// a confusing 404 buried inside the first inject() call.
func TestResolveSessionRejectsUnknownExistingID(t *testing.T) {
	t.Setenv("OPENCODE_SESSION_ID", "ses_gone")
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})

	_, err := resolveSession(context.Background())
	if err == nil {
		t.Fatal("want an error for a session id that does not exist")
	}
	if !strings.Contains(err.Error(), "ses_gone") {
		t.Errorf("error should name the bad id, got: %v", err)
	}
}

// setOCURL points ocURL at addr for the duration of the test — same override
// pattern as withServer, for the plain-TCP checks that don't need a fake
// opencode HTTP server, just something (or nothing) listening.
func setOCURL(t *testing.T, addr string) {
	t.Helper()
	old := ocURL
	ocURL = "http://" + addr
	t.Cleanup(func() { ocURL = old })
}

// The one-word-start path (Run's auto-serve) decides whether to spawn
// `opencode serve` based on this: nothing listening -> spawn. Wrong in
// either direction is a real bug — spawn-when-already-up wastes a port and
// orphans a process; never-detect-listening breaks the whole feature.
func TestServerUpDetectsListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	setOCURL(t, ln.Addr().String())
	if !serverUp(context.Background()) {
		t.Error("serverUp false with a real listener present")
	}
	ln.Close()
	if serverUp(context.Background()) {
		t.Error("serverUp true after the listener closed")
	}
}

// waitForServer must give up on a real timeout, not hang — the caller
// (Run) treats "never came up" as a hard startup failure, not a silent stall.
func TestWaitForServerTimesOutWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing answers here now
	setOCURL(t, addr)

	start := time.Now()
	if waitForServer(context.Background(), 400*time.Millisecond) {
		t.Fatal("waitForServer reported success against a closed port")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waitForServer took %v to give up on a 400ms timeout", elapsed)
	}
}

// Measured 2026-09-15: opencode serve accepts TCP ~1.1s before it serves
// HTTP, and a request in that window is never answered. Readiness must be
// HTTP-level — a listener that accepts and says nothing is NOT ready, even
// though serverUp (the spawn decision) correctly reports something there.
func TestWaitForServerIgnoresTCPOpenButNotServing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { // accept and hold every connection, never respond
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	setOCURL(t, ln.Addr().String())
	if !serverUp(context.Background()) {
		t.Fatal("serverUp must see the listener (that is the spawn decision)")
	}
	if waitForServer(context.Background(), 1500*time.Millisecond) {
		t.Error("waitForServer reported ready for a listener that never serves HTTP")
	}
}

// The other half: even if one create request gets orphaned, the next
// attempt must land. First request hangs well past createAttempt; the
// second is answered.
func TestCreateSessionRetriesPastAnOrphanedRequest(t *testing.T) {
	t.Setenv("OPENCODE_MODEL", "")
	old := createAttempt
	createAttempt = 300 * time.Millisecond
	t.Cleanup(func() { createAttempt = old })

	var calls int
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// Orphaned: say nothing until well after the client has given up.
			// (Not <-r.Context().Done(): the server only notices a vanished
			// client via a background read that never starts here, so that
			// would hold the handler — and srv.Close — forever.)
			time.Sleep(4 * createAttempt)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"id": "ses_second"}})
	})
	got, err := createSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "ses_second" || calls != 2 {
		t.Errorf("got %q after %d calls; want ses_second on the 2nd attempt", got, calls)
	}
}

// Real incident (2026-09-15): inject used /api/session/{id}/prompt, which on
// opencode 1.18.31 admitted the input and never ran it — the attached TUI
// showed nothing. /session/{id}/prompt_async runs the turn and the TUI
// renders it. Pin the route and the body shape it requires.
func TestInjectUsesPromptAsyncWithTextPart(t *testing.T) {
	var gotPath, gotText string
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		var body struct {
			Parts []struct{ Type, Text string } `json:"parts"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Parts) == 1 && body.Parts[0].Type == "text" {
			gotText = body.Parts[0].Text
		}
		w.WriteHeader(204)
	})
	err := inject(context.Background(), "ses_x", "oc-test", peers.Message{From: "keeper", Content: "hello there"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /session/ses_x/prompt_async" {
		t.Errorf("route: got %q", gotPath)
	}
	if !strings.Contains(gotText, "--from oc-test --to keeper") || !strings.HasSuffix(gotText, "\nhello there") {
		t.Errorf("text part missing reply hint or content: %q", gotText)
	}
}

// Real incident (2026-09-10): opencode serve died instantly on a config
// validation error, but Run's timeout handler only printed a bare "did not
// come up" — the actual reason sat silently in serveLogPath() the whole
// time. spawnOpencodeServe must capture a child's stderr there even when the
// child never manages to listen at all, or that fix has nothing to read.
func TestSpawnOpencodeServeCapturesChildStderrOnInstantFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // serveLogPath lives under boot.DataDir() == $HOME/.local/share/cp3
	fake := writeFakeOpencodeBin(t, `#!/bin/sh
echo "Configuration is invalid at /home/willy/.config/opencode/opencode.json" >&2
exit 1
`)
	old := opencodeBin
	opencodeBin = fake
	t.Cleanup(func() { opencodeBin = old })

	cmd, err := spawnOpencodeServe()
	if err != nil {
		t.Fatalf("spawnOpencodeServe: %v", err)
	}
	cmd.Wait() // the fake exits immediately; real spawnOpencodeServe never waits itself

	b, err := os.ReadFile(serveLogPath())
	if err != nil {
		t.Fatalf("serve log: %v", err)
	}
	if !strings.Contains(string(b), "Configuration is invalid") {
		t.Errorf("serve log missing the child's failure output, got: %q", b)
	}
}

func writeFakeOpencodeBin(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
