# v3 core build spec

Implement per DESIGN.md (read it — it's the contract). Module github.com/WillyV3/claude-peers-v3.

## PRINCIPLES (acceptance bar)
- effective-go: consumer-defined narrow interfaces; zero-value-friendly where sensible; errors wrapped; named types for the event-type set; NO os.Exit outside main.
- ponytail: NATS does the heavy lifting — do NOT reimplement queueing/presence/durability. Thinnest client over nats.go + JetStream + KV. No new deps beyond github.com/nats-io/nats.go (+ nats-server for the embedded test).
- The event log is the source of truth; state (presence) is a KV projection. Every mutating op publishes an event.

## Package `peers` (client library over NATS JetStream)
Types:
- `type EventType string` + consts: Register, Deregister, Presence, Message, Delivered.
- `type Envelope struct { V int; ID, Type, Actor string; TS int64; Data json.RawMessage }` (json: v,id,type,actor,ts,data).
- `type Peer struct { Agent, Machine, Cwd, Session string; TS int64 }`
- `type Message struct { ID, From, To, Content, DeliverAs string; TS int64 }`
- `type Client struct { ... }` wrapping *nats.Conn, jetstream.JetStream (or nats.JetStreamContext), the KV.

Funcs:
- `Connect(url, creds string) (*Client, error)` — connect (creds optional/empty for no-auth test).
- `(c) Setup() error` — ensure stream PEERS (subjects `peers.>`, file storage, MaxAge 7d, MaxMsgSize 64KB) + KV bucket PEERS_PRESENCE (TTL 30s).
- `(c) Register(p Peer) error` — KV put agent→p JSON + publish Register event to `peers.lifecycle.register`.
- `(c) Heartbeat(agent string) error` — refresh the KV key (re-put with fresh TS).
- `(c) Deregister(agent string) error` — KV delete + publish Deregister.
- `(c) Peers() ([]Peer, error)` — list KV keys → []Peer (all present = online).
- `(c) Send(m Message) error` — publish a Message event to `peers.msg.<to>` (envelope Type=Message).
- `(c) Subscribe(agent string, h func(Message)) error` — DURABLE JetStream consumer filtered to `peers.msg.<agent>`, ack explicitly, call h per message. Durable name = "inbox-"+agent so offline messages drain on reconnect.
- `(c) Watch(h func(Envelope)) error` — ephemeral consumer / core sub on `peers.>` → h per envelope (the firehose = full visibility).
- helper to build/marshal envelopes with a generated id (crypto/rand hex) + ts (pass a now int64 in — do NOT call time.Now in a way that breaks tests; time.Now is fine in library runtime).

## CLI `main.go` (cp3) — subcommands, NATS client
- `cp3 send --from A --to B [--mode steer] <content>` → Client.Send
- `cp3 peers` → Client.Peers, tabwriter table
- `cp3 watch` → Client.Watch, print each envelope (the full-visibility firehose)
- `cp3 register --agent A [--machine --cwd --session]` → Register + hold (heartbeat loop) until ctrl-c
- URL from env `NATS_URL` (default nats://127.0.0.1:4222), creds from `NATS_CREDS` (optional).

## Tests `peers_test.go` — EMBEDDED JetStream server (isolated, no external nats)
Use `github.com/nats-io/nats-server/v2/server` to start an in-process server with JetStream enabled on a random port; connect the client to it. Cover:
1. roundtrip: Setup; Register(alice); Peers() contains alice.
2. message delivery: Subscribe(bob, collect); Send(alice→bob); bob's handler gets it.
3. **offline drain / replay**: Send(alice→bob) BEFORE bob subscribes; then Subscribe(bob) → the durable consumer delivers the queued message (nothing lost).
4. **presence**: Register(alice); Peers() has alice. (TTL expiry: optionally set a short TTL in a test bucket and assert expiry, or skip if slow.)
5. **visibility / firehose**: Watch collector; do Register + Send; assert BOTH a lifecycle event AND a message event appear on `peers.>`.
stdlib testing only (+ the embedded server). Run with -race.

## Verify
go mod tidy; go build ./...; go vet ./...; go test -race ./... all green. Print results.
