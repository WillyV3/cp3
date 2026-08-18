package peers

import (
	"context"
	"strings"
	"testing"
)

// TORTURE: Send does not sanitize m.To, but To becomes a NATS SUBJECT TOKEN.
// Anything with a dot, wildcard, space, or emptiness is a subject-injection
// vector or an unroutable publish.
func TestTortureHostileRecipientNames(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx := context.Background()

	hostile := []struct {
		name string
		to   string
	}{
		{"empty", ""},
		{"full wildcard", ">"},
		{"token wildcard", "*"},
		{"dotted (extra subject level)", "a.b"},
		{"leading dot", ".x"},
		{"trailing dot", "x."},
		{"space", "a b"},
		{"newline", "a\nb"},
		{"very long", strings.Repeat("z", 300)},
	}
	for _, h := range hostile {
		t.Run(h.name, func(t *testing.T) {
			status, err := c.Send(ctx, Message{From: "attacker", To: h.to, Content: "x"})
			t.Logf("to=%q -> status=%q err=%v", h.to, status, err)
			// The contract: either it errors, or it reports NoInbox. What it
			// must NEVER do is claim delivered/queued for an unroutable name.
			if err == nil && status != NoInbox {
				t.Errorf("hostile name %q reported %q with no error — a caller would believe this landed", h.to, status)
			}
		})
	}
}

// TORTURE: Register with an empty agent name. Creates "inbox-" and a presence
// key for nobody?
func TestTortureRegisterEmptyAgent(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx := context.Background()
	err := c.Register(ctx, Peer{Agent: "", Machine: "m", Session: "s"})
	t.Logf("Register(agent=\"\") -> %v", err)
	if err == nil {
		list, _ := c.Peers(ctx)
		for _, p := range list {
			if p.Agent == "" {
				t.Error("registered a nameless peer onto the roster — it can never be addressed")
			}
		}
	}
}

// TORTURE: the probe-then-publish window. A consumer that appears between the
// status probe and the publish makes us under-report (say queued when it went
// live). That direction is safe. The unsafe direction is over-reporting:
// claiming delivered when the consumer vanished. Assert we never over-report
// after a consumer is deleted mid-flight.
func TestTortureConsumerDeletedBeforeSend(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx := context.Background()

	if _, err := c.ensureInbox(ctx, "vanishing"); err != nil {
		t.Fatal(err)
	}
	if got := c.deliveryStatus(ctx, "vanishing"); got != Queued {
		t.Fatalf("precondition: want queued, got %q", got)
	}
	if err := c.DeleteInbox(ctx, "inbox-vanishing"); err != nil {
		t.Fatal(err)
	}
	got, err := c.Send(ctx, Message{From: "a", To: "vanishing", Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got != NoInbox {
		t.Errorf("consumer deleted before send: got %q, want no-inbox — this is the void case", got)
	}
}
