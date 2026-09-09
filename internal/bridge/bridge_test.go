package bridge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	peers "github.com/WillyV3/cp3"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

func runServer(t *testing.T) *natsserver.Server {
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

// End-to-end name-loss: a bridge adapter holds a name, another session takes
// it (TTL-lapse simulation), and the bridge must EXIT with ErrNameLost —
// never keep consuming the split inbox.
func TestBridgeStopsOnNameLoss(t *testing.T) {
	srv := runServer(t)
	c, err := peers.Connect(srv.ClientURL(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	old := HeartbeatEvery
	HeartbeatEvery = 100 * time.Millisecond
	t.Cleanup(func() { HeartbeatEvery = old })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, c, peers.Peer{Agent: "pi-agent", Machine: "omarchy", Session: "sess-a"}, func(string, peers.Message) error { return nil })
	}()

	// Let it claim + start heartbeating, then steal the name.
	time.Sleep(400 * time.Millisecond)
	thief, err := peers.Connect(srv.ClientURL(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(thief.Close)
	if err := thief.Register(context.Background(), peers.Peer{Agent: "pi-agent", Machine: "macbook1", Session: "sess-b"}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-runErr:
		if !errors.Is(err, peers.ErrNameLost) {
			t.Fatalf("bridge exit: want ErrNameLost, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bridge kept running after losing its name")
	}
}

// Real incident (2026-09-08, inspiron-omarchy): cp3-opencode's injected
// reply-hint told the target runtime to run `cp3 send --to X` with no
// --from. OpenCode's model ran that via its own bash tool from
// opencode-serve's process tree — which never had CLAUDE_PEERS_AGENT set —
// so the reply went out under the machine's home-session identity instead of
// the bridge's. ReplyHint must always name the sender explicitly so the
// target runtime's own environment can never substitute a different one.
func TestReplyHintNamesBothSidesExplicitly(t *testing.T) {
	got := ReplyHint("inspiron-opencode", "deepseek-gpu")
	if !strings.Contains(got, "--from inspiron-opencode") {
		t.Errorf("reply hint does not pin --from: %s", got)
	}
	if !strings.Contains(got, "--to deepseek-gpu") {
		t.Errorf("reply hint does not pin --to: %s", got)
	}
}

// The other half of the fix: inject must receive the name ACTUALLY claimed
// (post-fallback), not the name the adapter asked for — a collision that
// silently fell back to a suffix must not have every reply hint tell the
// runtime to claim a name it doesn't hold.
func TestBridgePassesActualClaimedNameToInject(t *testing.T) {
	srv := runServer(t)
	c, err := peers.Connect(srv.ClientURL(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	ctx := context.Background()
	if err := c.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	// "twin" is already held by a live session on another machine, forcing
	// ClaimWithFallback to suffix.
	if err := c.Register(ctx, peers.Peer{Agent: "twin", Machine: "other", Session: "held"}); err != nil {
		t.Fatal(err)
	}

	got := make(chan string, 1)
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go Run(runCtx, c, peers.Peer{Agent: "twin", Machine: "here", Session: "sess-c"}, func(actual string, m peers.Message) error {
		got <- actual
		return nil
	})

	// The claim (and its fallback) happens in that goroutine; poll rather
	// than sleep-and-hope for it to land before sending.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if list, _ := c.Peers(ctx); slices.ContainsFunc(list, func(p peers.Peer) bool { return p.Agent == "twin-here" }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("twin-here never registered")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := c.Send(ctx, peers.Message{From: "sender", To: "twin-here", Content: "hi"}); err != nil {
		t.Fatal(err)
	}

	select {
	case actual := <-got:
		if actual != "twin-here" {
			t.Errorf("inject got %q, want the fallback name twin-here (not the requested twin)", actual)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("inject never called")
	}
}
