// Package bridge is the shared skeleton for external-runtime adapters: connect
// to the network, claim presence, and inject each inbound peer message into
// the target runtime via a runtime-specific inject func. The runtime only ever
// sees peers.Client — same wall as everything else.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	peers "github.com/WillyV3/cp3"
)

// ReplyHint is the instruction embedded in every message an adapter injects
// into its target runtime, telling it exactly how to answer — SELF-CONTAINED,
// not dependent on the runtime's own process environment to know its own
// identity.
//
// Real incident this closes: cp3-opencode injected "...run: cp3 send --to
// %s..." with no --from. OpenCode's model ran that command via its own bash
// tool from inside `opencode serve`'s process tree, which never had
// CLAUDE_PEERS_AGENT set (only the separate cp3-opencode bridge process did),
// so cp3 send's callerIdentity() fell through to the $HOME marker file and
// replied as the machine's home-session identity instead of the bridge's.
// Naming --from explicitly removes the dependency on that environment
// entirely, regardless of what the target runtime's tool execution actually
// inherits.
func ReplyHint(me, sender string) string {
	return fmt.Sprintf("[peer message from %s — to reply, run: cp3 send --from %s --to %s \"...\"]", sender, me, sender)
}

// Run claims p (machine-suffix fallback on collision), heartbeats, and
// delivers inbound messages to inject until ctx is cancelled or the name is
// lost to another session. inject receives the ACTUALLY claimed name (not
// necessarily p.Agent — ClaimWithFallback may have suffixed it), so a caller
// building a ReplyHint always names the identity this session really holds.
// inject errors are logged, not fatal (one bad turn shouldn't drop the agent
// off the network). Losing the name IS fatal: two sessions draining one
// durable inbox split the messages between them, so the loser must stop
// consuming — fail loud, let the supervisor restart us into a fresh claim.
func Run(ctx context.Context, c *peers.Client, p peers.Peer, inject func(actual string, m peers.Message) error) error {
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
	// Run must not return before the name is released: callers exit the
	// process right after, and an un-awaited Deregister loses that race —
	// the name then stays "up" until its TTL lapses (~30s) and mail sent in
	// that window reports "delivered" to nobody. Reproduced 2026-09-18 with
	// SIGINT+SIGTERM to `cp3 subscribe` (30s ghost vs ~100ms on one signal).
	released := make(chan struct{})
	go func() { defer close(released); heartbeat(subCtx, cancel, c, actual) }()
	defer func() { cancel(nil); <-released }()
	err = c.Subscribe(subCtx, actual.Agent, func(m peers.Message) {
		if err := inject(actual.Agent, m); err != nil {
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
			err := c.Heartbeat(ctx, p.Agent, p.Session, "")
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
