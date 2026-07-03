package peers

import (
	"context"
	"errors"
	"testing"
)

// The macbook-sleep bug, distilled: session A holds a name, the machine
// sleeps past the presence TTL, session B claims the name, A wakes and
// heartbeats. Before the ownership check, A's blind re-put made the two
// sessions fight forever (presence flapping, inbox split). Now A must get
// ErrNameLost and back off.
func TestHeartbeatOwnership(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx := context.Background()

	a := Peer{Agent: "astro", Machine: "omarchy", Session: "sess-a"}
	if _, err := c.Claim(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Owner heartbeats fine.
	if err := c.Heartbeat(ctx, "astro", "sess-a"); err != nil {
		t.Fatalf("owner heartbeat: %v", err)
	}
	// Theft: B force-registers (simulates claim-after-TTL; Register is the
	// raw path with no ownership check, like a fresh claim on an empty key).
	b := Peer{Agent: "astro", Machine: "macbook1", Session: "sess-b"}
	if err := c.Register(ctx, b); err != nil {
		t.Fatal(err)
	}
	// A's next heartbeat must refuse to fight.
	err := c.Heartbeat(ctx, "astro", "sess-a")
	if !errors.Is(err, ErrNameLost) {
		t.Fatalf("stale owner heartbeat: want ErrNameLost, got %v", err)
	}
	// B keeps heartbeating undisturbed.
	if err := c.Heartbeat(ctx, "astro", "sess-b"); err != nil {
		t.Fatalf("new owner heartbeat: %v", err)
	}
	// The record must still be B's (A's failed heartbeat wrote nothing).
	list, err := c.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.Agent == "astro" && p.Session != "sess-b" {
			t.Fatalf("presence flapped back to %s", p.Session)
		}
	}
}

// Same synced project dir open on two machines: both resolve the same
// dir-derived name. The second claimant must get a deterministic
// machine-qualified twin, and a third (same machine, same dir) a session tag.
func TestClaimWithFallback(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx := context.Background()

	first := Peer{Agent: "astrobot", Machine: "omarchy", Session: "sess-1"}
	got, err := c.ClaimWithFallback(ctx, first)
	if err != nil || got.Agent != "astrobot" {
		t.Fatalf("first claim: %q %v", got.Agent, err)
	}

	second := Peer{Agent: "astrobot", Machine: "macbook1", Session: "sess-2"}
	got, err = c.ClaimWithFallback(ctx, second)
	if err != nil || got.Agent != "astrobot-macbook1" {
		t.Fatalf("cross-machine fallback: %q %v", got.Agent, err)
	}

	// Same machine as the base holder: machine suffix is still free, so the
	// cascade uses it; a further claimant then needs the session tag.
	third := Peer{Agent: "astrobot", Machine: "omarchy", Session: "zz99xyz"}
	got, err = c.ClaimWithFallback(ctx, third)
	if err != nil || got.Agent != "astrobot-omarchy" {
		t.Fatalf("same-machine suffix: %q %v", got.Agent, err)
	}
	fourth := Peer{Agent: "astrobot", Machine: "omarchy", Session: "ab12cd"}
	got, err = c.ClaimWithFallback(ctx, fourth)
	if err != nil || got.Agent != "astrobot-ab12" {
		t.Fatalf("session tie-break: %q %v", got.Agent, err)
	}

	// Re-claim by the SAME session is idempotent, not a collision.
	got, err = c.ClaimWithFallback(ctx, first)
	if err != nil || got.Agent != "astrobot" {
		t.Fatalf("self re-claim: %q %v", got.Agent, err)
	}
}
