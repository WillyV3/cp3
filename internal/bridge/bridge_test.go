package bridge

import (
	"context"
	"errors"
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
		runErr <- Run(ctx, c, peers.Peer{Agent: "pi-agent", Machine: "omarchy", Session: "sess-a"}, func(peers.Message) error { return nil })
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
