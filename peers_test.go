package peers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
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

	if _, err := alice.Send(ctx, Message{From: "alice", To: "bob", Content: "hi", DeliverAs: "steer"}); err != nil {
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
	if _, err := alice.Send(ctx, Message{From: "alice", To: "bob", Content: "while you were out"}); err != nil {
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

// 7. consumer observability: a durable inbox shows up in Consumers(); a message
// sent with nobody draining shows as pending backlog (the dead-durable shape).
func TestConsumersList(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// bob subscribes (creates durable inbox-bob), then goes away.
	bctx, bcancel := context.WithCancel(ctx)
	go c.Subscribe(bctx, "bob", func(Message) {})
	time.Sleep(150 * time.Millisecond)
	bcancel()
	time.Sleep(50 * time.Millisecond)

	// a message lands with nobody draining -> pending on the durable.
	if _, err := c.Send(ctx, Message{From: "alice", To: "bob", Content: "backlog"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "inbox-bob pending backlog", func() bool {
		list, err := c.Consumers(ctx)
		if err != nil {
			return false
		}
		for _, cs := range list {
			if cs.Name == "inbox-bob" && cs.Pending >= 1 {
				return true
			}
		}
		return false
	})
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
	if _, err := c.Send(ctx, Message{From: "dave", To: "eve", Content: "yo"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both events on firehose", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen[EventRegister] >= 1 && seen[EventMessage] >= 1
	})
}

// The class of bug this whole change exists to kill: publishing always
// "succeeds", so a message addressed to a name nobody subscribes to was
// reported as sent. Each outcome must be distinguishable.
func TestDeliveryStatus(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. No inbox at all -> NoInbox. This is the void case.
	got, err := c.Send(ctx, Message{From: "alice", To: "nobody-home", Content: "into the void"})
	if err != nil {
		t.Fatal(err)
	}
	if got != NoInbox {
		t.Errorf("no consumer: got %q want %q", got, NoInbox)
	}

	// 2. Inbox exists and a session is actively consuming -> DeliveredLive.
	sctx, scancel := context.WithCancel(ctx)
	go c.Subscribe(sctx, "bob", func(Message) {})
	waitFor(t, "bob's inbox to start pulling", func() bool {
		st := c.deliveryStatus(ctx, "bob")
		return st == DeliveredLive
	})
	if got, err = c.Send(ctx, Message{From: "alice", To: "bob", Content: "live"}); err != nil || got != DeliveredLive {
		t.Errorf("live consumer: got %q err=%v want %q", got, err, DeliveredLive)
	}

	// 3. Same inbox after the session goes away -> Queued, NOT NoInbox: the
	// durable consumer survives, which is the offline-delivery feature.
	scancel()
	waitFor(t, "bob's inbox to stop pulling", func() bool {
		return c.deliveryStatus(ctx, "bob") == Queued
	})
	if got, err = c.Send(ctx, Message{From: "alice", To: "bob", Content: "offline"}); err != nil || got != Queued {
		t.Errorf("offline consumer: got %q err=%v want %q", got, err, Queued)
	}

	// Human text must name the failure unmistakably.
	if msg := NoInbox.Human("rover"); !strings.Contains(msg, "NOT DELIVERED") {
		t.Errorf("NoInbox.Human must be loud, got %q", msg)
	}
}

// INVARIANT: anything on the roster is reachable. cp3 register used to
// publish presence with no consumer, producing an agent you could see and
// address but never actually reach.
func TestRegisterIsReachable(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx := context.Background()

	if c.deliveryStatus(ctx, "ghost") != NoInbox {
		t.Fatal("precondition: unregistered name should have no inbox")
	}
	if err := c.Register(ctx, Peer{Agent: "ghost", Machine: "m", Session: "s"}); err != nil {
		t.Fatal(err)
	}
	// On the roster...
	list, err := c.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var onRoster bool
	for _, p := range list {
		if p.Agent == "ghost" {
			onRoster = true
		}
	}
	if !onRoster {
		t.Fatal("register did not put the agent on the roster")
	}
	// ...therefore reachable. Never NoInbox again.
	got, err := c.Send(ctx, Message{From: "alice", To: "ghost", Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got == NoInbox {
		t.Error("INVARIANT VIOLATED: agent is on the roster but has no inbox")
	}
}

// Two runtimes in one directory on one machine: the second gets a name that
// says which runtime it is, not the machine it is on (both are the same box).
func TestClaimWithFallbackRuntimeTwin(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx := context.Background()

	holder := Peer{Agent: "proj", Machine: "m1", Session: "sess-claude", Runtime: "claude"}
	if _, err := c.ClaimWithFallback(ctx, holder); err != nil {
		t.Fatal(err)
	}
	pi := Peer{Agent: "proj", Machine: "m1", Session: "sess-pi", Runtime: "pi"}
	got, err := c.ClaimWithFallback(ctx, pi)
	if err != nil || got.Agent != "proj-pi" {
		t.Fatalf("same-machine runtime twin: %q %v", got.Agent, err)
	}
}

// A runtime twin on another machine keeps the machine suffix: the machine is
// the distinguishing fact there, not the runtime.
func TestClaimWithFallbackCrossMachineKeepsMachineSuffix(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx := context.Background()

	holder := Peer{Agent: "proj", Machine: "m1", Session: "sess-claude", Runtime: "claude"}
	if _, err := c.ClaimWithFallback(ctx, holder); err != nil {
		t.Fatal(err)
	}
	pi := Peer{Agent: "proj", Machine: "m2", Session: "sess-pi", Runtime: "pi"}
	got, err := c.ClaimWithFallback(ctx, pi)
	if err != nil || got.Agent != "proj-m2" {
		t.Fatalf("cross-machine twin: %q %v", got.Agent, err)
	}
}

// Same runtime twice on one machine is the old case: machine suffix, then
// the session tag. A runtime suffix here would name two identical claude
// sessions "proj-claude", which says nothing new.
func TestClaimWithFallbackSameRuntimeKeepsOldBehavior(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx := context.Background()

	holder := Peer{Agent: "proj", Machine: "m1", Session: "sess-a", Runtime: "claude"}
	if _, err := c.ClaimWithFallback(ctx, holder); err != nil {
		t.Fatal(err)
	}
	second := Peer{Agent: "proj", Machine: "m1", Session: "sess-b", Runtime: "claude"}
	got, err := c.ClaimWithFallback(ctx, second)
	if err != nil || got.Agent != "proj-m1" {
		t.Fatalf("same runtime twice: %q %v", got.Agent, err)
	}
}

// A claimant that did not say its runtime gets the old machine suffix.
func TestClaimWithFallbackNoRuntimeKeepsOldBehavior(t *testing.T) {
	s := runServer(t)
	c := newClient(t, s)
	ctx := context.Background()

	holder := Peer{Agent: "proj", Machine: "m1", Session: "sess-a", Runtime: "claude"}
	if _, err := c.ClaimWithFallback(ctx, holder); err != nil {
		t.Fatal(err)
	}
	unknown := Peer{Agent: "proj", Machine: "m1", Session: "sess-b"}
	got, err := c.ClaimWithFallback(ctx, unknown)
	if err != nil || got.Agent != "proj-m1" {
		t.Fatalf("no runtime given: %q %v", got.Agent, err)
	}
}

// The reply link end to end: a message is shown, answered with its id, and
// the sender can read each stage off the log without ever waiting.
func TestReplyLinkAndStatus(t *testing.T) {
	c := newClient(t, runServer(t))
	ctx := context.Background()
	for _, n := range []string{"asker", "answerer"} {
		if err := c.Register(ctx, Peer{Agent: n, Session: "s-" + n}); err != nil {
			t.Fatal(err)
		}
	}
	q := Message{ID: NewID(), From: "asker", To: "answerer", Content: "\n  does /api/v2/users paginate?\nsecond line"}
	if _, err := c.Send(ctx, q); err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(ctx, q.ID)
	if err != nil || st.Shown != nil || st.Replied != nil || !strings.HasPrefix(st.Human(), "no receipt") {
		t.Fatalf("before delivery: %+v / %s, %v", st, st.Human(), err)
	}

	// A reply to a message nobody was shown is a wrong id, and says so.
	if _, err := c.Send(ctx, Message{From: "answerer", To: "asker", Content: "yes", ReplyTo: q.ID}); !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("reply before shown: want ErrUnknownMessage, got %v", err)
	}
	if _, err := c.Send(ctx, Message{From: "answerer", To: "asker", Content: "yes", ReplyTo: "bad.id >"}); !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("hostile id: want ErrUnknownMessage, got %v", err)
	}

	if err := c.Shown(ctx, q, "answerer"); err != nil {
		t.Fatal(err)
	}
	st, _ = c.Status(ctx, q.ID)
	if st.Shown == nil || st.Shown.By != "answerer" || st.Replied != nil || !strings.HasPrefix(st.Human(), "shown:") {
		t.Fatalf("after shown: %+v / %s", st, st.Human())
	}

	var got Message
	var mu sync.Mutex
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.Subscribe(subCtx, "asker", func(m Message) { mu.Lock(); got = m; mu.Unlock() })
	r := Message{ID: NewID(), From: "answerer", To: "asker", Content: "yes, cursor-based", ReplyTo: q.ID}
	if _, err := c.Send(ctx, r); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reply delivered", func() bool { mu.Lock(); defer mu.Unlock(); return got.ID == r.ID })
	if got.TS == 0 {
		t.Error("a message sent without a timestamp must get one")
	}
	if got.ReplyTo != q.ID || got.Quote != "does /api/v2/users paginate?" {
		t.Errorf("reply lost its link or quote: %+v", got)
	}
	if want := `, replying to your message ` + q.ID + ` ("does /api/v2/users paginate?")`; got.ReplyClause() != want {
		t.Errorf("clause %q, want %q", got.ReplyClause(), want)
	}
	if got := (Message{ReplyTo: "abc"}).ReplyClause(); got != ", replying to your message abc" {
		t.Errorf("clause with no quote: %q", got)
	}
	if got := (Message{}).ReplyClause(); got != "" {
		t.Errorf("a message that is not a reply has a clause: %q", got)
	}
	if got := []rune(firstLine(strings.Repeat("é", 200))); len(got) != 80 || got[79] != '…' {
		t.Errorf("a long first line must be cut to 80 characters, got %d", len(got))
	}
	if got := firstLine(" \n\t\n"); got != "" {
		t.Errorf("blank message quote: %q", got)
	}
	if exact := strings.Repeat("x", 80); firstLine(exact) != exact {
		t.Error("a first line of exactly 80 characters must not be cut")
	}
	// An id becomes part of a subject on the log: only plain tokens pass.
	for id, ok := range map[string]bool{
		q.ID: true, strings.Repeat("a", 64): true, "A-b_9": true,
		"": false, strings.Repeat("a", 65): false, "a.b": false, "a b": false, "*": false, ">": false,
	} {
		if err := validMessageID(id); (err == nil) != ok {
			t.Errorf("validMessageID(%q) = %v, want ok=%v", id, err, ok)
		}
	}
	st, _ = c.Status(ctx, q.ID)
	if st.Replied == nil || st.Replied.By != "answerer" || st.Replied.ReplyID != r.ID || !strings.HasPrefix(st.Human(), "replied:") {
		t.Fatalf("after reply: %+v / %s", st, st.Human())
	}
}

// The roster carries the build that holds each name, for debugging only.
func TestPresenceCarriesVersion(t *testing.T) {
	c := newClient(t, runServer(t))
	old := Version
	Version = "9.9.9"
	t.Cleanup(func() { Version = old })
	if err := c.Register(context.Background(), Peer{Agent: "versioned", Session: "s"}); err != nil {
		t.Fatal(err)
	}
	list, _ := c.Peers(context.Background())
	if len(list) != 1 || list[0].Version != "9.9.9" {
		t.Fatalf("roster: %+v", list)
	}
}

// runCluster starts three clustered in-process servers and waits for JetStream
// to elect a leader.
func runCluster(t *testing.T) []*natsserver.Server {
	t.Helper()
	var ports [3]int
	var routes []*url.URL
	for i := range ports {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = l.Addr().(*net.TCPAddr).Port
		l.Close()
		routes = append(routes, &url.URL{Scheme: "nats", Host: fmt.Sprintf("127.0.0.1:%d", ports[i])})
	}
	var servers []*natsserver.Server
	for i, port := range ports {
		s, err := natsserver.NewServer(&natsserver.Options{
			ServerName: fmt.Sprintf("n%d", i),
			Host:       "127.0.0.1",
			Port:       -1,
			JetStream:  true,
			StoreDir:   t.TempDir(),
			Cluster:    natsserver.ClusterOpts{Name: "cp3test", Host: "127.0.0.1", Port: port},
			Routes:     routes,
		})
		if err != nil {
			t.Fatal(err)
		}
		go s.Start()
		t.Cleanup(s.Shutdown)
		servers = append(servers, s)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range servers {
			if s.JetStreamIsLeader() {
				return servers
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("cluster never elected a JetStream leader")
	return nil
}

// On a cluster the log and roster are kept on as many servers as configured,
// and a client that has no setting keeps that count instead of undoing it.
func TestReplicasOnACluster(t *testing.T) {
	servers := runCluster(t)
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	replicasOf := func(c *Client, stream string) int {
		s, err := c.js.Stream(ctx, stream)
		if err != nil {
			t.Fatalf("%s: %v", stream, err)
		}
		return s.CachedInfo().Config.Replicas
	}

	t.Setenv("CP3_REPLICAS", "3")
	urls := servers[0].ClientURL() + "," + servers[1].ClientURL() + "," + servers[2].ClientURL()
	a, err := Connect(urls, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// The meta leader is up before every peer has joined; retry until placement works.
	waitLong := time.Now().Add(20 * time.Second)
	for err = a.Setup(ctx); err != nil && time.Now().Before(waitLong); err = a.Setup(ctx) {
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("setup with 3 replicas: %v", err)
	}
	if got := replicasOf(a, streamName); got != 3 {
		t.Fatalf("log replicas = %d, want 3", got)
	}
	if got := replicasOf(a, "KV_"+presenceKV); got != 3 {
		t.Fatalf("roster replicas = %d, want 3", got)
	}

	t.Setenv("CP3_REPLICAS", "")
	b, err := Connect(servers[1].ClientURL(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Setup(ctx); err != nil {
		t.Fatalf("setup with no setting: %v", err)
	}
	if got := replicasOf(b, streamName); got != 3 {
		t.Fatalf("a client with no setting scaled the log to %d", got)
	}
	if got := replicasOf(b, "KV_"+presenceKV); got != 3 {
		t.Fatalf("a client with no setting scaled the roster to %d", got)
	}

	// One server down: the other two still take and deliver mail.
	servers[0].Shutdown()
	// Losing a server can cost a leader election; calls fail until it settles.
	waitLong = time.Now().Add(30 * time.Second)
	for err = b.Register(ctx, Peer{Agent: "survivor", Session: "s"}); err != nil && time.Now().Before(waitLong); err = b.Register(ctx, Peer{Agent: "survivor", Session: "s"}) {
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("register with one server down: %v", err)
	}
	if st, err := b.Send(ctx, Message{From: "x", To: "survivor", Content: "still here"}); err != nil || st == NoInbox {
		t.Fatalf("send with one server down: %v %v", st, err)
	}
}

// A single server cannot keep three copies; asking for them fails loudly.
func TestReplicasRefusedOnASingleServer(t *testing.T) {
	s := runServer(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CP3_REPLICAS", "3")
	c, err := Connect(s.ClientURL(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Setup(context.Background()); err == nil {
		t.Fatal("3 replicas on one server must fail")
	}
	t.Setenv("CP3_REPLICAS", "lots")
	if err := c.Setup(context.Background()); err == nil || !strings.Contains(err.Error(), "replicas") {
		t.Fatalf("a bad setting must be named, got %v", err)
	}
}
