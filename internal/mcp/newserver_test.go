package mcp

import (
	"bytes"
	"context"
	"os"
	"testing"

	peers "github.com/WillyV3/cp3"
)

// newTestClient gives a real NATS-backed client on an isolated embedded server.
func newTestClient(t *testing.T) *peers.Client {
	t.Helper()
	ns := runServer(t)
	c, err := peers.Connect(ns.ClientURL(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if err := c.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

func envFor(dir string, pid int) sessionEnv {
	return sessionEnv{cwd: dir, machine: "testbox", session: "sess-1", parentPID: pid}
}

// Startup identity is where every incident of the last two weeks lived.
func TestNewServerIdentity(t *testing.T) {
	t.Run("dir basename is the zero-config default", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		c := newTestClient(t)
		dir := t.TempDir() + "/my-project"
		if err := mkdir(dir); err != nil {
			t.Fatal(err)
		}
		s, _ := newServer(context.Background(), c, &transport{out: &bytes.Buffer{}}, envFor(dir, 4242))
		if s.me != "my-project" {
			t.Errorf("got %q, want dir basename my-project", s.me)
		}
	})

	t.Run("ephemeral claims nothing", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		c := newTestClient(t)
		e := envFor(t.TempDir(), 4243)
		e.ephemeral = true
		s, _ := newServer(context.Background(), c, &transport{out: &bytes.Buffer{}}, e)
		if s.me != "" || s.configured != "" {
			t.Errorf("headless one-shot became addressable: me=%q configured=%q", s.me, s.configured)
		}
		list, _ := c.Peers(context.Background())
		if len(list) != 0 {
			t.Errorf("ephemeral session registered presence: %+v", list)
		}
	})

	t.Run("session state beats the directory (the astrobot rename bug)", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		c := newTestClient(t)
		// A previous process for THIS claude pid claimed "astrobot"...
		prev := &server{parentPID: 777, me: "astrobot", configured: "astrobot"}
		prev.writeState()
		// ...while the (syncthing-shared) directory now says something else.
		dir := t.TempDir() + "/sontara-mobile"
		if err := mkdir(dir); err != nil {
			t.Fatal(err)
		}
		s, _ := newServer(context.Background(), c, &transport{out: &bytes.Buffer{}}, envFor(dir, 777))
		if s.me != "astrobot" {
			t.Errorf("continuity lost: got %q, want astrobot — the directory must not rename a live session", s.me)
		}
	})

	t.Run("collision falls back to a machine-qualified twin", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		c := newTestClient(t)
		ctx := context.Background()
		if err := c.Register(ctx, peers.Peer{Agent: "shared", Machine: "other", Session: "someone-else"}); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir() + "/shared"
		if err := mkdir(dir); err != nil {
			t.Fatal(err)
		}
		s, _ := newServer(ctx, c, &transport{out: &bytes.Buffer{}}, envFor(dir, 4244))
		if s.me == "shared" {
			t.Error("stole a live agent's name")
		}
		if s.me != "shared-testbox" {
			t.Errorf("got %q, want the deterministic twin shared-testbox", s.me)
		}
		if s.configured != "shared" {
			t.Errorf("configured should record the wanted name for drift repair, got %q", s.configured)
		}
	})
}

// Spool replay: the rover fix. A message received by a dead process must be
// returned for replay, and drained so it cannot double-deliver.
func TestNewServerSpoolReplay(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := newTestClient(t)
	dir := t.TempDir() + "/rover"
	if err := mkdir(dir); err != nil {
		t.Fatal(err)
	}
	spoolAppend("rover", peers.Message{ID: "m1", From: "caretaker", Content: "the briefing"})

	s, missed := newServer(context.Background(), c, &transport{out: &bytes.Buffer{}}, envFor(dir, 4245))
	if s.me != "rover" {
		t.Fatalf("identity: got %q", s.me)
	}
	if len(missed) != 1 || missed[0].Content != "the briefing" {
		t.Fatalf("spool not returned for replay: %+v", missed)
	}
	// Drained: a second startup must not replay it again.
	_, again := newServer(context.Background(), c, &transport{out: &bytes.Buffer{}}, envFor(dir, 4245))
	if len(again) != 0 {
		t.Errorf("spool replayed twice: %+v", again)
	}
}

// A reconnect landing on a suffixed twin must still find mail addressed to the
// name the sender actually used.
func TestNewServerSpoolFollowsConfiguredName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := newTestClient(t)
	ctx := context.Background()
	if err := c.Register(ctx, peers.Peer{Agent: "twin", Machine: "other", Session: "held"}); err != nil {
		t.Fatal(err)
	}
	spoolAppend("twin", peers.Message{ID: "m2", From: "someone", Content: "addressed to the real name"})
	dir := t.TempDir() + "/twin"
	if err := mkdir(dir); err != nil {
		t.Fatal(err)
	}
	s, missed := newServer(ctx, c, &transport{out: &bytes.Buffer{}}, envFor(dir, 4246))
	if s.me == "twin" {
		t.Fatal("should have fallen back; the name was held")
	}
	if len(missed) != 1 {
		t.Fatalf("orphaned the spool under the configured name: got %d messages, want 1", len(missed))
	}
}

func mkdir(p string) error { return os.MkdirAll(p, 0o755) }

// Mutation survivors exposed this gap: every prior test wrote BOTH Claimed and
// Wanted, so the "||" and the prev=="" fallback were never actually exercised.
// The real case is a session that ran EPHEMERAL (claimed nothing) while still
// wanting a name — its state has Wanted set and Claimed empty. If startup
// ignored that, the session would silently fall back to directory identity and
// lose the name it has been waiting to reclaim.
func TestNewServerResumesWantedNameAfterEphemeral(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := newTestClient(t)
	prev := &server{parentPID: 909, me: "", configured: "rover"} // ephemeral, still wanted "rover"
	prev.writeState()
	if st := ReadState(909); st.Claimed != "" || st.Wanted != "rover" {
		t.Fatalf("precondition: state should be claimed=\"\" wanted=rover, got %+v", st)
	}
	dir := t.TempDir() + "/some-other-dir"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, _ := newServer(context.Background(), c, &transport{out: &bytes.Buffer{}}, envFor(dir, 909))
	if s.configured != "rover" {
		t.Errorf("lost the wanted name: configured=%q, want rover", s.configured)
	}
	if s.me != "rover" {
		t.Errorf("did not reclaim the free wanted name: me=%q, want rover (directory identity would be some-other-dir)", s.me)
	}
}

// The no-drift path: configured == claimed. Spool must be drained exactly
// once, never appended twice for the same name.
func TestNewServerNoDriftDrainsSpoolOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := newTestClient(t)
	dir := t.TempDir() + "/solo"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	spoolAppend("solo", peers.Message{ID: "x", From: "a", Content: "once"})
	s, missed := newServer(context.Background(), c, &transport{out: &bytes.Buffer{}}, envFor(dir, 910))
	if s.me != s.configured {
		t.Fatalf("expected no drift, got me=%q configured=%q", s.me, s.configured)
	}
	if len(missed) != 1 {
		t.Errorf("no-drift spool drained %d times, want exactly 1: %+v", len(missed), missed)
	}
}
