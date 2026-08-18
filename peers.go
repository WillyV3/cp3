// Package peers is a thin NATS JetStream client for the claude-peers v3 network.
// The durable event log (stream PEERS, subjects peers.>) is the source of truth;
// presence (KV PEERS_PRESENCE) is a projection. Every mutating op publishes an
// event, so any consumer subscribing peers.> sees the whole network. NATS does
// the queueing, durability and presence-TTL — this package only wires to it.
package peers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	streamName   = "PEERS"
	subjectAll   = "peers.>"
	presenceKV   = "PEERS_PRESENCE"
	presenceTTL  = 30 * time.Second
	maxMsgSize   = 64 * 1024
	retentionAge = 7 * 24 * time.Hour
	maxBytes     = 1 << 30            // 1GiB hard cap — drop oldest before disk fills
	dupWindow    = 2 * time.Minute    // server-side dedup: a re-published msg-id inside this window is ignored
	inboxIdle    = 7 * 24 * time.Hour // reap an inbox abandoned this long; 30d let a graveyard accumulate (40 consumers / 12 peers)
)

// EventType is the closed set of event kinds on the log.
type EventType string

const (
	EventRegister   EventType = "register"
	EventDeregister EventType = "deregister"
	EventPresence   EventType = "presence"
	EventMessage    EventType = "message"
)

// Envelope is the versioned wire contract every consumer builds against.
type Envelope struct {
	V     int             `json:"v"`
	ID    string          `json:"id"`
	Type  EventType       `json:"type"`
	TS    int64           `json:"ts"` // unix millis
	Actor string          `json:"actor"`
	Data  json.RawMessage `json:"data"`
}

// Peer is a network participant's presence record (KV value).
type Peer struct {
	Agent   string `json:"agent"`
	Machine string `json:"machine"`
	Cwd     string `json:"cwd"`
	Session string `json:"session"`
	Summary string `json:"summary,omitempty"`
	TS      int64  `json:"ts"`
}

// Message is a peer-to-peer message payload.
type Message struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Content   string `json:"content"`
	DeliverAs string `json:"deliverAs"`
	TS        int64  `json:"ts"`
}

// Client wraps a NATS connection + JetStream + the presence KV.
type Client struct {
	nc *nats.Conn
	js jetstream.JetStream
	kv jetstream.KeyValue
}

// ConnectFromEnv dials using NATS_URL (default nats://127.0.0.1:4222), NATS_CREDS
// (a .creds file) and a token — the single place auth is resolved so every
// binary authenticates the same way. Token resolution: NATS_TOKEN env, then the
// file named by NATS_TOKEN_FILE, then ~/.config/cp3/token. The file paths keep
// the secret out of argv, JSON config, and dotfile-synced shell rc.
func ConnectFromEnv() (*Client, error) {
	url := URLFromEnv()
	token := os.Getenv("NATS_TOKEN")
	if token == "" {
		if path := os.Getenv("NATS_TOKEN_FILE"); path != "" {
			if b, err := os.ReadFile(path); err == nil {
				token = strings.TrimSpace(string(b))
			}
		} else {
			token = strings.TrimSpace(readConfigFile("token"))
		}
	}
	return Connect(url, os.Getenv("NATS_CREDS"), token)
}

// OnReconnect registers f to run each time the underlying NATS connection is
// re-established (laptop wake, server bounce). Presence holders use it to
// re-claim immediately instead of waiting for the next heartbeat tick.
func (c *Client) OnReconnect(f func()) {
	c.nc.SetReconnectHandler(func(*nats.Conn) { f() })
}

// URLFromEnv resolves the server url the same way ConnectFromEnv does:
// NATS_URL env, then ~/.config/cp3/url, then localhost.
func URLFromEnv() string {
	if url := os.Getenv("NATS_URL"); url != "" {
		return url
	}
	if url := strings.TrimSpace(readConfigFile("url")); url != "" {
		return url // hooks/statusline have no shell env
	}
	return "nats://127.0.0.1:4222"
}

// readConfigFile returns the contents of ~/.config/cp3/<name>, or "".
func readConfigFile(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "cp3", name))
	if err != nil {
		return ""
	}
	return string(b)
}

// Connect dials NATS. creds is a path to a .creds file and token is a plain auth
// token; either may be "" (empty both = no auth). creds wins if both are set.
func Connect(url, creds, token string) (*Client, error) {
	// Reconnect forever with backoff so a NATS blip doesn't drop an agent off
	// the network (matches the v1 broker's resilience). Deliberately NOT
	// RetryOnFailedConnect: the INITIAL connect must fail fast on a bad token/URL
	// so misconfig surfaces immediately instead of retrying silently.
	opts := []nats.Option{
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
	}
	switch {
	case creds != "":
		opts = append(opts, nats.UserCredentials(creds))
	case token != "":
		opts = append(opts, nats.Token(token))
	}
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return &Client{nc: nc, js: js}, nil
}

// Close releases the connection.
func (c *Client) Close() { c.nc.Close() }

// NATS exposes the raw connection for advanced uses (e.g. the fleet-compat
// projector publishing legacy fleet.* events). Prefer the typed methods.
func (c *Client) NATS() *nats.Conn { return c.nc }

// Setup ensures the PEERS stream (the log) and PEERS_PRESENCE KV exist.
// Idempotent — safe to call on every start.
func (c *Client) Setup(ctx context.Context) error {
	_, err := c.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       streamName,
		Subjects:   []string{subjectAll},
		Storage:    jetstream.FileStorage,
		MaxAge:     retentionAge,
		MaxBytes:   maxBytes,
		MaxMsgSize: maxMsgSize,
		Discard:    jetstream.DiscardOld, // when full, drop oldest — never reject a new event
		Duplicates: dupWindow,            // idempotent publish: same Nats-Msg-Id inside the window = one event
	})
	if err != nil {
		return fmt.Errorf("create stream: %w", err)
	}
	kv, err := c.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: presenceKV,
		TTL:    presenceTTL,
	})
	if err != nil {
		return fmt.Errorf("create kv: %w", err)
	}
	c.kv = kv
	return nil
}

// kvBucket lazily binds the KV so read-only callers (peers) don't need Setup.
func (c *Client) kvBucket(ctx context.Context) (jetstream.KeyValue, error) {
	if c.kv != nil {
		return c.kv, nil
	}
	kv, err := c.js.KeyValue(ctx, presenceKV)
	if err != nil {
		return nil, fmt.Errorf("bind kv: %w", err)
	}
	c.kv = kv
	return kv, nil
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// publish marshals data into an envelope and appends it to the log.
func (c *Client) publish(ctx context.Context, subject string, t EventType, actor string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal %s data: %w", t, err)
	}
	env := Envelope{V: 1, ID: newID(), Type: t, TS: nowMillis(), Actor: actor, Data: raw}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	// Msg-ID = envelope ID: if the client's internal retry re-sends after a lost
	// ack, JetStream dedups within dupWindow — at-least-once becomes exactly-once
	// on the log, so consumers never see the same event twice.
	if _, err := c.js.Publish(ctx, subject, body, jetstream.WithMsgID(env.ID)); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

// ensureInbox creates (idempotently) the durable consumer that makes a name
// reachable. One definition, shared by Register and Subscribe, so presence and
// deliverability can never be created from two different shapes.
func (c *Client) ensureInbox(ctx context.Context, agent string) (jetstream.Consumer, error) {
	if err := validRecipient(agent); err != nil {
		return nil, err
	}
	// Durable so messages sent while offline drain on reconnect;
	// InactiveThreshold reaps a name abandoned for good.
	cons, err := c.js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:           "inbox-" + agent,
		FilterSubject:     "peers.msg." + agent,
		AckPolicy:         jetstream.AckExplicitPolicy,
		InactiveThreshold: inboxIdle,
	})
	if err != nil {
		return nil, fmt.Errorf("create inbox consumer: %w", err)
	}
	return cons, nil
}

// Register records presence in the KV and emits a register event.
//
// INVARIANT: on the roster means reachable. The inbox is created with the
// presence record, never after it, because the alternative shipped: cp3
// register published presence with no consumer, so every message to that name
// landed in the log addressed to nobody while the sender was told "sent".
// Presence without a mailbox is not a peer, it is a decoy.
func (c *Client) Register(ctx context.Context, p Peer) error {
	if p.TS == 0 {
		p.TS = nowMillis()
	}
	if _, err := c.ensureInbox(ctx, p.Agent); err != nil {
		return err
	}
	if err := c.putPresence(ctx, p); err != nil {
		return err
	}
	return c.publish(ctx, "peers.lifecycle.register", EventRegister, p.Agent, p)
}

func (c *Client) putPresence(ctx context.Context, p Peer) error {
	kv, err := c.kvBucket(ctx)
	if err != nil {
		return err
	}
	val, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal peer: %w", err)
	}
	if _, err := kv.Put(ctx, p.Agent, val); err != nil {
		return fmt.Errorf("kv put %s: %w", p.Agent, err)
	}
	return nil
}

// ErrNameTaken is returned by Claim when a live session already holds the name.
var ErrNameTaken = errors.New("agent name already held")

// Claim registers p only if the name is free (or already held by p's own
// session). Returns ErrNameTaken + the current holder otherwise.
// ponytail: best-effort uniqueness via the presence KV — a dead holder's key
// expires in one TTL (30s), then the name frees. A tighter guarantee would need
// a lock stream; not worth it for a fleet of agents.
func (c *Client) Claim(ctx context.Context, p Peer) (*Peer, error) {
	kv, err := c.kvBucket(ctx)
	if err != nil {
		return nil, err
	}
	if entry, err := kv.Get(ctx, p.Agent); err == nil {
		var holder Peer
		if json.Unmarshal(entry.Value(), &holder) == nil && holder.Session != p.Session {
			return &holder, ErrNameTaken
		}
	}
	return nil, c.Register(ctx, p)
}

// ClaimWithFallback claims p.Agent, falling back to a machine-qualified name
// when another live session holds it: "astrobot" taken -> "astrobot-macbook1".
// Deterministic and self-describing — unlike ordinal suffixes, the fallback
// says WHERE the twin lives (the usual cause: the same synced project dir
// open on two machines). If even that is taken (same dir twice on one
// machine), a short session tag breaks the tie. Returns the Peer actually
// registered.
func (c *Client) ClaimWithFallback(ctx context.Context, p Peer) (Peer, error) {
	holder, err := c.Claim(ctx, p)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, ErrNameTaken) {
		return p, err
	}
	base := p.Agent
	if m := SanitizeName(p.Machine); m != "" {
		p.Agent = base + "-" + m
		if _, err := c.Claim(ctx, p); err == nil {
			return p, nil
		} else if !errors.Is(err, ErrNameTaken) {
			return p, err
		}
	}
	tag := p.Session
	if len(tag) > 4 {
		tag = tag[:4]
	}
	p.Agent = base + "-" + SanitizeName(tag)
	if _, err := c.Claim(ctx, p); err != nil {
		return p, fmt.Errorf("all fallbacks for %q taken (holder: %s on %s): %w", base, holder.Session, holder.Machine, err)
	}
	return p, nil
}

// SetSummary updates an agent's presence summary (re-puts the KV record and
// emits a presence event so consumers see the change).
func (c *Client) SetSummary(ctx context.Context, agent, summary string) error {
	kv, err := c.kvBucket(ctx)
	if err != nil {
		return err
	}
	entry, err := kv.Get(ctx, agent)
	if err != nil {
		return fmt.Errorf("kv get %s: %w", agent, err)
	}
	var p Peer
	if err := json.Unmarshal(entry.Value(), &p); err != nil {
		return fmt.Errorf("unmarshal peer: %w", err)
	}
	p.Summary = summary
	p.TS = nowMillis()
	if err := c.putPresence(ctx, p); err != nil {
		return err
	}
	return c.publish(ctx, "peers.presence", EventPresence, agent, p)
}

// ErrNameLost is returned by Heartbeat when the name is now held by a
// different session — e.g. this machine slept past the TTL and someone else
// claimed it. The caller must stop heartbeating this name: fighting over the
// key makes presence flap between machines and splits the durable inbox
// across two live sessions.
var ErrNameLost = errors.New("agent name now held by another session")

// Heartbeat refreshes presence ONLY while this session still owns the name:
// the record's session must match, and the write is a revision-CAS update so
// a racing claim can't be silently overwritten. session "" skips the
// ownership check (bare CLI use).
func (c *Client) Heartbeat(ctx context.Context, agent, session string) error {
	kv, err := c.kvBucket(ctx)
	if err != nil {
		return err
	}
	entry, err := kv.Get(ctx, agent)
	if err != nil {
		return fmt.Errorf("kv get %s: %w", agent, err)
	}
	var p Peer
	if err := json.Unmarshal(entry.Value(), &p); err != nil {
		return fmt.Errorf("unmarshal peer: %w", err)
	}
	if session != "" && p.Session != session {
		return fmt.Errorf("%s held by session %s on %s: %w", agent, p.Session, p.Machine, ErrNameLost)
	}
	p.TS = nowMillis()
	val, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal peer: %w", err)
	}
	if _, err := kv.Update(ctx, agent, val, entry.Revision()); err != nil {
		return fmt.Errorf("kv update %s (lost race): %w", agent, err)
	}
	return nil
}

// Deregister removes presence and emits a deregister event.
func (c *Client) Deregister(ctx context.Context, agent string) error {
	kv, err := c.kvBucket(ctx)
	if err != nil {
		return err
	}
	if err := kv.Delete(ctx, agent); err != nil {
		return fmt.Errorf("kv delete %s: %w", agent, err)
	}
	return c.publish(ctx, "peers.lifecycle.deregister", EventDeregister, agent, map[string]string{"agent": agent})
}

// Peers returns everyone currently present (KV projection = live view).
// A missing bucket means the network has never been set up here — that is an
// empty network, not an error.
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	kv, err := c.kvBucket(ctx)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("kv keys: %w", err)
	}
	out := make([]Peer, 0, len(keys))
	for _, k := range keys {
		entry, err := kv.Get(ctx, k)
		if err != nil {
			continue // key expired between list and get — skip
		}
		var p Peer
		if err := json.Unmarshal(entry.Value(), &p); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// ConsumerStatus is a liveness snapshot of one consumer on the log. Pending
// piling up with no recent delivery = an abandoned durable (the v1 graveyard).
type ConsumerStatus struct {
	Name         string
	Pending      uint64 // not yet delivered
	AckPending   int    // delivered, unacked
	Waiting      int    // outstanding pull requests: >0 means a session is attached right now
	LastDelivery *time.Time
}

// Consumers lists every consumer attached to the PEERS stream.
func (c *Client) Consumers(ctx context.Context) ([]ConsumerStatus, error) {
	s, err := c.js.Stream(ctx, streamName)
	if err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	var out []ConsumerStatus
	lister := s.ListConsumers(ctx)
	for info := range lister.Info() {
		cs := ConsumerStatus{Name: info.Name, Pending: info.NumPending, AckPending: info.NumAckPending, Waiting: info.NumWaiting}
		if info.Delivered.Last != nil {
			t := *info.Delivered.Last
			cs.LastDelivery = &t
		}
		out = append(out, cs)
	}
	if err := lister.Err(); err != nil {
		return nil, fmt.Errorf("list consumers: %w", err)
	}
	return out, nil
}

// DeliveryStatus is what actually happened to a sent message. Publishing
// always "succeeds" — the log accepts the write regardless — so reporting that
// as delivery is how a message can vanish while the sender is told it worked.
// These are three different facts and callers must see which one they got.
type DeliveryStatus string

const (
	// DeliveredLive: the recipient's inbox exists and a consumer is actively
	// waiting on it — the message goes to a running session now.
	DeliveredLive DeliveryStatus = "delivered"
	// Queued: the inbox exists but nothing is draining it. The message waits
	// durably and lands when that agent reconnects. This is the offline path
	// working as designed, not a failure.
	Queued DeliveryStatus = "queued"
	// NoInbox: NOTHING will ever receive this. No durable consumer exists for
	// the name, so the message sits in the log addressed to nobody. Presence
	// can still list the agent (see cmdRegister), which is exactly how this
	// stayed invisible.
	NoInbox DeliveryStatus = "no-inbox"
)

// Human returns a one-line description suitable for a CLI or a tool result.
func (d DeliveryStatus) Human(to string) string {
	switch d {
	case DeliveredLive:
		return fmt.Sprintf("delivered to %s (live session consuming now)", to)
	case Queued:
		return fmt.Sprintf("queued for %s (inbox exists; delivers when that agent reconnects)", to)
	default:
		return fmt.Sprintf("NOT DELIVERED: %s has no inbox — nothing is subscribed to that name, so this message will never be read. Check `cp3 peers` for the exact name, or ask them to restart their session", to)
	}
}

// DeleteInbox removes a durable inbox consumer. Callers must check it holds
// no undelivered messages first — deleting a consumer discards its backlog.
func (c *Client) DeleteInbox(ctx context.Context, name string) error {
	return c.js.DeleteConsumer(ctx, streamName, name)
}

// ErrInvalidName is returned for a recipient that cannot be a real agent.
var ErrInvalidName = errors.New("invalid agent name")

// validRecipient rejects anything that is not already a sanitized name.
// The recipient becomes a NATS subject token, so an unchecked name is a
// subject-injection vector: ">" and "*" are wildcards, dots add subject
// levels, spaces and newlines make the publish invalid outright. Every real
// agent name is produced by SanitizeName, so a name that is not its own
// sanitized form cannot belong to any peer and must never be reported as
// queued. (Found by torture test, not by inspection.)
func validRecipient(to string) error {
	if to == "" {
		return fmt.Errorf("%w: empty", ErrInvalidName)
	}
	if SanitizeName(to) != to {
		return fmt.Errorf("%w: %q is not a valid agent name (expected %q)", ErrInvalidName, to, SanitizeName(to))
	}
	return nil
}

// Send appends a message event to peers.msg.<to> and reports what will
// actually happen to it.
func (c *Client) Send(ctx context.Context, m Message) (DeliveryStatus, error) {
	if err := validRecipient(m.To); err != nil {
		return NoInbox, err
	}
	if m.ID == "" {
		m.ID = newID()
	}
	if m.TS == 0 {
		m.TS = nowMillis()
	}
	// Probe BEFORE publishing: after the write the consumer may have already
	// picked it up, which would make a live agent look merely queued.
	status := c.deliveryStatus(ctx, m.To)
	if err := c.publish(ctx, "peers.msg."+m.To, EventMessage, m.From, m); err != nil {
		return status, err
	}
	return status, nil
}

// deliveryStatus inspects the recipient's durable inbox. An error reading the
// consumer is reported as Queued, not NoInbox — never claim a message is lost
// on the strength of a failed lookup.
func (c *Client) deliveryStatus(ctx context.Context, to string) DeliveryStatus {
	cons, err := c.js.Consumer(ctx, streamName, "inbox-"+to)
	if err != nil {
		if errors.Is(err, jetstream.ErrConsumerNotFound) {
			return NoInbox
		}
		return Queued
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return Queued
	}
	// NumWaiting counts outstanding pull requests: a session sitting on its
	// inbox waiting for work. Zero means the consumer exists but nobody is
	// draining it right now.
	if info.NumWaiting > 0 {
		return DeliveredLive
	}
	return Queued
}

// Subscribe delivers messages addressed to agent via a DURABLE consumer, so
// messages sent while offline drain on reconnect. Blocks until ctx is done;
// h is called for each message (auto-acked).
func (c *Client) Subscribe(ctx context.Context, agent string, h func(Message)) error {
	// Durable so messages sent while offline drain on reconnect; InactiveThreshold
	// reaps the consumer if the agent name is abandoned for good — no leaked
	// consumers piling up on the server (the ephemeral-session snag).
	cons, err := c.ensureInbox(ctx, agent)
	if err != nil {
		return err
	}
	cctx, err := cons.Consume(func(msg jetstream.Msg) {
		var env Envelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil {
			msg.Ack() // poison message — drop, don't redeliver forever
			return
		}
		var m Message
		if err := json.Unmarshal(env.Data, &m); err != nil {
			msg.Ack()
			return
		}
		h(m)
		msg.Ack()
	})
	if err != nil {
		return fmt.Errorf("consume inbox: %w", err)
	}
	defer cctx.Stop()
	<-ctx.Done()
	return nil
}

// Watch delivers events on the log (the full-visibility firehose). If fromStart
// is true it replays all retained history first, else only events after now.
// Blocks until ctx is done; h is called for each envelope.
func (c *Client) Watch(ctx context.Context, fromStart bool, h func(Envelope)) error {
	policy := jetstream.DeliverNewPolicy
	if fromStart {
		policy = jetstream.DeliverAllPolicy
	}
	cons, err := c.js.OrderedConsumer(ctx, streamName, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subjectAll},
		DeliverPolicy:  policy,
	})
	if err != nil {
		return fmt.Errorf("ordered consumer: %w", err)
	}
	cctx, err := cons.Consume(func(msg jetstream.Msg) {
		var env Envelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil {
			return
		}
		h(env)
	})
	if err != nil {
		return fmt.Errorf("consume firehose: %w", err)
	}
	defer cctx.Stop()
	<-ctx.Done()
	return nil
}
