package main

import (
	"context"
	"errors"
	"testing"
	"time"

	peers "github.com/WillyV3/cp3"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

func TestStatusLine(t *testing.T) {
	t.Setenv("NO_COLOR", "1") // golden strings stay readable; paint() no-ops
	list := []peers.Peer{
		{Agent: "keeper", Cwd: "/w", Session: "s-keeper"},
		{Agent: "jim", Cwd: "/w", Session: "s-jim"},
		{Agent: "far", Cwd: "/elsewhere", Session: "s-far"},
		{Agent: "twin", Cwd: "/w", Machine: "macbook1", Session: "s-twin"},
	}
	cases := []struct {
		name          string
		conf, claimed string
		session       string
		cwd           string
		list          []peers.Peer
		pending       uint64
		err           error
		want          string
	}{
		{"no identity", "", "", "", "/w", list, 0, nil, ""},
		{"down", "keeper", "", "s-keeper", "/w", nil, 0, errors.New("boom"), "○ peers down"},
		{"not registered", "ghost", "", "s-ghost", "", list, 0, nil, "○ ghost · not registered"},
		{"co-location neutral", "keeper", "", "s-keeper", "/w", list, 0, nil, "● keeper · 4 peers · with jim, twin@macbook1"},
		{"solo", "keeper", "", "s-keeper", "/solo", list, 0, nil, "● keeper · 4 peers"},
		{"pending badge", "keeper", "", "s-keeper", "/solo", list, 4, nil, "● keeper · 4 peers · ✉4"},
		// jim's bug: configured "jim" but THIS session claimed "jim-omarchy"
		// (stale "jim" from the previous session still in presence). Must
		// show the claimed name, warn about the drift, and NOT list either
		// of our own names in "with".
		{"claimed-name drift", "jim", "jim-omarchy", "s-new", "/w",
			[]peers.Peer{
				{Agent: "jim", Cwd: "/w", Session: "s-old"},                             // dead session's TTL remnant
				{Agent: "jim-omarchy", Cwd: "/w", Machine: "omarchy", Session: "s-new"}, // me
				{Agent: "keeper", Cwd: "/w", Session: "s-keeper"},
			}, 0, nil,
			"● jim-omarchy · 3 peers · ⚠ wanted jim · with jim, keeper"},
		// Willy's cd-flip bug: session is jim (state file says claimed=jim,
		// wanted=jim), but the session has WANDERED into the claude-daemon
		// repo whose .claude-peers-agent says "investigate" — and investigate
		// itself is live there. Identity must stay jim (session-bound), no
		// drift warning, and investigate shows only as a co-located neighbor.
		{"cd into another repo", "jim", "jim", "s-jim", "/daemon",
			[]peers.Peer{
				{Agent: "jim", Cwd: "/w", Session: "s-jim"},
				{Agent: "investigate", Cwd: "/daemon", Session: "s-inv"},
			}, 0, nil,
			"● jim · 2 peers · with investigate"},
	}
	for _, c := range cases {
		if got := statusLine(c.conf, c.claimed, c.session, c.cwd, c.list, c.pending, false, c.err); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestStatusLineNoChannel(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	list := []peers.Peer{{Agent: "rover", Cwd: "/r", Session: "s1"}}
	got := statusLine("rover", "rover", "s1", "/r", list, 0, true, nil)
	want := "● rover · 1 peers · ✖ NO CHANNEL (relaunch: cp3 run)"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestStatusLineHasColor(t *testing.T) {
	// Without NO_COLOR the line must actually carry ANSI — the whole point.
	got := statusLine("keeper", "keeper", "s1", "/w", []peers.Peer{{Agent: "keeper", Session: "s1"}}, 0, false, nil)
	if !containsANSI(got) {
		t.Errorf("expected ANSI codes in %q", got)
	}
}

func containsANSI(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '\x1b' && s[i+1] == '[' {
			return true
		}
	}
	return false
}

func TestConsumerVerdict(t *testing.T) {
	now := time.Now()
	recent := now.Add(-30 * time.Second)
	old := now.Add(-3 * time.Hour)
	cases := []struct {
		cs   peers.ConsumerStatus
		want string
	}{
		{peers.ConsumerStatus{LastDelivery: &recent, Pending: 5}, "active"},
		{peers.ConsumerStatus{LastDelivery: &old, Pending: 29000}, "STALE"},
		{peers.ConsumerStatus{Pending: 100}, "STALE"},
		{peers.ConsumerStatus{LastDelivery: &old, Pending: 0}, "idle"},
		{peers.ConsumerStatus{}, "idle"},
	}
	for i, c := range cases {
		if got := consumerVerdict(c.cs, now); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}

// waitFor is the anti-`sleep 10` primitive: it must return as soon as the
// agent shows up, and fail cleanly (not hang) when it never does.
func TestWaitFor(t *testing.T) {
	s := runTestServer(t)
	c := newTestClient(t, s)
	ctx := context.Background()

	// Already present -> returns immediately.
	if err := c.Register(ctx, peers.Peer{Agent: "early", Machine: "m1", Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got, err := waitFor(c, "early", 5*time.Second)
	if err != nil || got.Machine != "m1" {
		t.Fatalf("already-present: %v %+v", err, got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("already-present took %s, should be instant", elapsed)
	}

	// Registers late -> returns once it lands, well before the timeout.
	go func() {
		time.Sleep(1200 * time.Millisecond)
		c.Register(ctx, peers.Peer{Agent: "late", Machine: "m2", Session: "s2"})
	}()
	start = time.Now()
	if got, err = waitFor(c, "late", 15*time.Second); err != nil || got.Machine != "m2" {
		t.Fatalf("late-arrival: %v %+v", err, got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("late-arrival took %s — not returning promptly", elapsed)
	}

	// Never registers -> times out with an error, does not hang.
	start = time.Now()
	if _, err = waitFor(c, "ghost", 2*time.Second); err == nil {
		t.Fatal("expected timeout error for an agent that never registers")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout overran: %s", elapsed)
	}
}

// --- embedded JetStream helpers (main package copy; the root package's are
// unexported to it) ---

func runTestServer(t *testing.T) *natsserver.Server {
	t.Helper()
	s, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

func newTestClient(t *testing.T, s *natsserver.Server) *peers.Client {
	t.Helper()
	c, err := peers.Connect(s.ClientURL(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if err := c.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

// Reaping must never destroy undelivered mail — inbox-sontara-web held a real
// message for 307h, and a naive "delete stale consumers" would have eaten it.
func TestReapable(t *testing.T) {
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	recent := now.Add(-1 * time.Minute)
	week := 7 * 24 * time.Hour
	cases := []struct {
		name string
		cs   peers.ConsumerStatus
		want bool
	}{
		{"attached session", peers.ConsumerStatus{Waiting: 1, LastDelivery: &old}, false},
		{"holds undelivered mail", peers.ConsumerStatus{Pending: 1, LastDelivery: &old}, false},
		{"delivered but unacked", peers.ConsumerStatus{AckPending: 1, LastDelivery: &old}, false},
		{"recently active", peers.ConsumerStatus{LastDelivery: &recent}, false},
		{"detached, empty, ancient", peers.ConsumerStatus{LastDelivery: &old}, true},
		{"created and never used", peers.ConsumerStatus{}, true},
	}
	for _, c := range cases {
		if got := reapable(c.cs, now, week); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
