// Package bridge is the shared skeleton for external-runtime adapters: connect
// to the network, register presence, and inject each inbound peer message into
// the target runtime via a runtime-specific inject func. The runtime only ever
// sees peers.Client — same wall as everything else.
package bridge

import (
	"context"
	"log"
	"time"

	peers "github.com/WillyV3/cp3"
)

// Run registers p, heartbeats, and delivers inbound messages to inject until
// ctx is cancelled. inject errors are logged, not fatal (one bad turn shouldn't
// drop the agent off the network).
func Run(ctx context.Context, c *peers.Client, p peers.Peer, inject func(peers.Message) error) error {
	if err := c.Setup(ctx); err != nil {
		return err
	}
	if err := c.Register(ctx, p); err != nil {
		return err
	}
	go heartbeat(ctx, c, p.Agent)
	return c.Subscribe(ctx, p.Agent, func(m peers.Message) {
		if err := inject(m); err != nil {
			log.Printf("[bridge] inject from %s failed: %v", m.From, err)
		}
	})
}

func heartbeat(ctx context.Context, c *peers.Client, agent string) {
	tk := time.NewTicker(15 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			c.Deregister(context.Background(), agent)
			return
		case <-tk.C:
			c.Heartbeat(ctx, agent)
		}
	}
}
