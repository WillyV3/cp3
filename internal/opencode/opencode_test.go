package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
