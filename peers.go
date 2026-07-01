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
	maxBytes     = 1 << 30         // 1GiB hard cap — drop oldest before disk fills
	dupWindow    = 2 * time.Minute // server-side dedup: a re-published msg-id inside this window is ignored
	inboxIdle    = 30 * 24 * time.Hour // reap an inbox consumer abandoned this long (dead agent name)
)

// EventType is the closed set of event kinds on the log.
type EventType string

const (
	EventRegister   EventType = "register"
	EventDeregister EventType = "deregister"
	EventPresence   EventType = "presence"
	EventMessage    EventType = "message"
	EventDelivered  EventType = "delivered"
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
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://127.0.0.1:4222"
	}
	token := os.Getenv("NATS_TOKEN")
	if token == "" {
		path := os.Getenv("NATS_TOKEN_FILE")
		if path == "" {
			if home, err := os.UserHomeDir(); err == nil {
				path = filepath.Join(home, ".config", "cp3", "token")
			}
		}
		if b, err := os.ReadFile(path); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	return Connect(url, os.Getenv("NATS_CREDS"), token)
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

// Register records presence in the KV and emits a register event.
func (c *Client) Register(ctx context.Context, p Peer) error {
	if p.TS == 0 {
		p.TS = nowMillis()
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

// Heartbeat refreshes an agent's presence key so its TTL doesn't expire.
func (c *Client) Heartbeat(ctx context.Context, agent string) error {
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
	p.TS = nowMillis()
	return c.putPresence(ctx, p)
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
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	kv, err := c.kvBucket(ctx)
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
		cs := ConsumerStatus{Name: info.Name, Pending: info.NumPending, AckPending: info.NumAckPending}
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

// Send appends a message event to peers.msg.<to>.
func (c *Client) Send(ctx context.Context, m Message) error {
	if m.ID == "" {
		m.ID = newID()
	}
	if m.TS == 0 {
		m.TS = nowMillis()
	}
	return c.publish(ctx, "peers.msg."+m.To, EventMessage, m.From, m)
}

// Subscribe delivers messages addressed to agent via a DURABLE consumer, so
// messages sent while offline drain on reconnect. Blocks until ctx is done;
// h is called for each message (auto-acked).
func (c *Client) Subscribe(ctx context.Context, agent string, h func(Message)) error {
	// Durable so messages sent while offline drain on reconnect; InactiveThreshold
	// reaps the consumer if the agent name is abandoned for good — no leaked
	// consumers piling up on the server (the ephemeral-session snag).
	cons, err := c.js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:           "inbox-" + agent,
		FilterSubject:     "peers.msg." + agent,
		AckPolicy:         jetstream.AckExplicitPolicy,
		InactiveThreshold: inboxIdle,
	})
	if err != nil {
		return fmt.Errorf("create inbox consumer: %w", err)
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
