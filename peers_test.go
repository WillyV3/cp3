package peers

import (
	"context"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
)

// runServer starts an in-process NATS server with JetStream (isolated per test).
func runServer(t *testing.T) *natsserver.Server {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // random free port
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

func newClient(t *testing.T, s *natsserver.Server) *Client {
	t.Helper()
	c, err := Connect(s.ClientURL(), "", "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Close)
	if err := c.Setup(context.Background()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return c
}

// waitFor polls cond until true or the deadline; fails the test on timeout.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", msg)
}

// 1. roundtrip: register shows up in Peers().
func TestRegisterAndList(t *testing.T) {
	c := newClient(t, runServer(t))
	ctx := context.Background()
	if err := c.Register(ctx, Peer{Agent: "alice", Machine: "m1", Cwd: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	list, err := c.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Agent != "alice" {
		t.Fatalf("want [alice], got %+v", list)
	}
}

// 2. message delivery: subscribed agent receives a sent message.
func TestMessageDelivery(t *testing.T) {
	s := runServer(t)
	alice := newClient(t, s)
	bob := newClient(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var got []Message
	go bob.Subscribe(ctx, "bob", func(m Message) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	time.Sleep(100 * time.Millisecond) // let the consumer attach

	if err := alice.Send(ctx, Message{From: "alice", To: "bob", Content: "hi", DeliverAs: "steer"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bob to receive", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	if got[0].Content != "hi" || got[0].From != "alice" {
		t.Fatalf("bad message: %+v", got[0])
	}
}

// 3. offline drain / replay: a message sent BEFORE the recipient subscribes is
// delivered when the durable consumer later attaches (nothing lost).
func TestOfflineDrain(t *testing.T) {
	s := runServer(t)
	alice := newClient(t, s)
	bob := newClient(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// bob is offline — send anyway.
	if err := alice.Send(ctx, Message{From: "alice", To: "bob", Content: "while you were out"}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []Message
	go bob.Subscribe(ctx, "bob", func(m Message) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	waitFor(t, "queued message to drain", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	if got[0].Content != "while you were out" {
		t.Fatalf("bad drained message: %+v", got[0])
	}
}

// 4. presence: registered peer is present; deregister removes it.
func TestPresenceLifecycle(t *testing.T) {
	c := newClient(t, runServer(t))
	ctx := context.Background()
	if err := c.Register(ctx, Peer{Agent: "carol"}); err != nil {
		t.Fatal(err)
	}
	list, _ := c.Peers(ctx)
	if len(list) != 1 {
		t.Fatalf("want carol present, got %+v", list)
	}
	if err := c.Deregister(ctx, "carol"); err != nil {
		t.Fatal(err)
	}
	list, _ = c.Peers(ctx)
	if len(list) != 0 {
		t.Fatalf("want empty after deregister, got %+v", list)
	}
}

// 6. idempotency: re-publishing the same Nats-Msg-Id inside the dedup window is
// collapsed to a single event on the log (no duplicate delivery on ack loss).
func TestPublishDedup(t *testing.T) {
	c := newClient(t, runServer(t))
	ctx := context.Background()
	subj := "peers.presence" // any subject the stream owns
	for i := 0; i < 3; i++ {
		if _, err := c.js.Publish(ctx, subj, []byte(`{"x":1}`), jetstream.WithMsgID("same-id")); err != nil {
			t.Fatal(err)
		}
	}
	s, err := c.js.Stream(ctx, streamName)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("want 1 msg after 3 dup publishes, got %d", info.State.Msgs)
	}
}

// 5. visibility / firehose: a lifecycle event AND a message event both land on
// peers.> — a consumer subscribing the firehose sees every state change.
func TestFirehoseVisibility(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	seen := map[EventType]int{}
	go c.Watch(ctx, true, func(e Envelope) {
		mu.Lock()
		seen[e.Type]++
		mu.Unlock()
	})
	time.Sleep(100 * time.Millisecond)

	if err := c.Register(ctx, Peer{Agent: "dave"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(ctx, Message{From: "dave", To: "eve", Content: "yo"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both events on firehose", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen[EventRegister] >= 1 && seen[EventMessage] >= 1
	})
}
