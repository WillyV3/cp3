package peers

import (
	"context"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// serverAt starts a JetStream server on a fixed store dir (survives restart).
func serverAt(t *testing.T, storeDir string) *natsserver.Server {
	t.Helper()
	s, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: storeDir,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("server not ready")
	}
	return s
}

// Chaos: message sent, server killed, server restarted from the same file store,
// recipient connects to the new server and the durable inbox still drains it.
// Proves "nothing lost" across a broker crash — the design's core guarantee.
func TestChaosServerRestart(t *testing.T) {
	store := t.TempDir()
	ctx := context.Background()

	s1 := serverAt(t, store)
	alice, err := Connect(s1.ClientURL(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	// Create bob's durable inbox up front so it's persisted, then bob goes away.
	bob1, _ := Connect(s1.ClientURL(), "", "")
	bctx, bcancel := context.WithCancel(ctx)
	go bob1.Subscribe(bctx, "bob", func(Message) {})
	time.Sleep(150 * time.Millisecond)
	bcancel()
	bob1.Close()

	if err := alice.Send(ctx, Message{From: "alice", To: "bob", Content: "survive the crash"}); err != nil {
		t.Fatal(err)
	}
	alice.Close()

	// Crash.
	s1.Shutdown()
	s1.WaitForShutdown()

	// Restart from the same store.
	s2 := serverAt(t, store)
	t.Cleanup(s2.Shutdown)
	bob2, err := Connect(s2.ClientURL(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bob2.Close)

	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	var mu sync.Mutex
	var got []Message
	go bob2.Subscribe(rctx, "bob", func(m Message) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	waitFor(t, "message to survive restart", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	if got[0].Content != "survive the crash" {
		t.Fatalf("bad message after restart: %+v", got[0])
	}
}
