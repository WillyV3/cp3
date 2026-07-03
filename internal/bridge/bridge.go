// Package bridge is the shared skeleton for external-runtime adapters: connect
// to the network, claim presence, and inject each inbound peer message into
// the target runtime via a runtime-specific inject func. The runtime only ever
// sees peers.Client — same wall as everything else.
package bridge

import (
	"context"
	"errors"
	"log"
	"time"

	peers "github.com/WillyV3/cp3"
)

// Run claims p (machine-suffix fallback on collision), heartbeats, and
// delivers inbound messages to inject until ctx is cancelled or the name is
// lost to another session. inject errors are logged, not fatal (one bad turn
// shouldn't drop the agent off the network). Losing the name IS fatal: two
// sessions draining one durable inbox split the messages between them, so the
// loser must stop consuming — fail loud, let the supervisor restart us into a
// fresh claim.
func Run(ctx context.Context, c *peers.Client, p peers.Peer, inject func(peers.Message) error) error {
	if err := c.Setup(ctx); err != nil {
		return err
	}
	actual, err := c.ClaimWithFallback(ctx, p)
	if err != nil {
		return err
	}
	if actual.Agent != p.Agent {
		log.Printf("[bridge] name %q taken; running as %q", p.Agent, actual.Agent)
	}
	subCtx, cancel := context.WithCancelCause(ctx)
	go heartbeat(subCtx, cancel, c, actual)
	err = c.Subscribe(subCtx, actual.Agent, func(m peers.Message) {
		if err := inject(m); err != nil {
			log.Printf("[bridge] inject from %s failed: %v", m.From, err)
		}
	})
	if cause := context.Cause(subCtx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}

// Interval between presence heartbeats; a var so tests can shrink it.
var HeartbeatEvery = 15 * time.Second

func heartbeat(ctx context.Context, cancel context.CancelCauseFunc, c *peers.Client, p peers.Peer) {
	tk := time.NewTicker(HeartbeatEvery)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			c.Deregister(context.Background(), p.Agent)
			return
		case <-tk.C:
			err := c.Heartbeat(ctx, p.Agent, p.Session)
			if err == nil {
				continue
			}
			if errors.Is(err, peers.ErrNameLost) {
				cancel(err) // stop consuming the inbox — the name has a new owner
				return
			}
			// Expired (slept past TTL) or transient: try to take the name back.
			if _, cerr := c.Claim(ctx, p); cerr != nil {
				if errors.Is(cerr, peers.ErrNameTaken) {
					cancel(peers.ErrNameLost)
					return
				}
				log.Printf("[bridge] heartbeat %s: %v (reclaim: %v)", p.Agent, err, cerr)
			}
		}
	}
}
