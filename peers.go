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

// Connect dials NATS. creds is a path to a .creds file, or "" for no auth.
func Connect(url, creds string) (*Client, error) {
	var opts []nats.Option
	if creds != "" {
		opts = append(opts, nats.UserCredentials(creds))
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

// Setup ensures the PEERS stream (the log) and PEERS_PRESENCE KV exist.
// Idempotent — safe to call on every start.
func (c *Client) Setup(ctx context.Context) error {
	_, err := c.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       streamName,
		Subjects:   []string{subjectAll},
		Storage:    jetstream.FileStorage,
		MaxAge:     retentionAge,
		MaxMsgSize: maxMsgSize,
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
	if _, err := c.js.Publish(ctx, subject, body); err != nil {
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
	cons, err := c.js.CreateOrUpdateConsumer(ctx, streamName, jetstream.ConsumerConfig{
		Durable:       "inbox-" + agent,
		FilterSubject: "peers.msg." + agent,
		AckPolicy:     jetstream.AckExplicitPolicy,
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

// Watch delivers every event on the log (the full-visibility firehose).
// Blocks until ctx is done; h is called for each envelope.
func (c *Client) Watch(ctx context.Context, h func(Envelope)) error {
	cons, err := c.js.OrderedConsumer(ctx, streamName, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subjectAll},
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
