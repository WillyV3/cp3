// cp3-mcp — the Claude Code injection adapter for claude-peers v3.
//
// It is an MCP server (stdio JSON-RPC) that is ALSO a thin NATS client: peer
// messages arrive on the durable inbox and are injected into the live Claude
// session as notifications/claude/channel (the proven channel contract), while
// the five tools (list_peers/send_message/set_summary/check_messages/
// claim_agent_name) are backed by the NATS event log + presence KV.
//
// The wall: this binary knows only the peers.Client API. Swapping NATS_URL /
// NATS_CREDS from localhost to a managed endpoint (e.g. Synadia Cloud) makes it
// multiplayer with zero code change — hosting is a config choice, not a rebuild.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	peers "github.com/WillyV3/claude-peers-v3"
)

// ---- JSON-RPC transport (stdio) ----

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type transport struct {
	scanner *bufio.Scanner
	mu      sync.Mutex
}

func newTransport() *transport {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	return &transport{scanner: sc}
}

func (t *transport) read() (rpcReq, error) {
	if !t.scanner.Scan() {
		if err := t.scanner.Err(); err != nil {
			return rpcReq{}, err
		}
		return rpcReq{}, io.EOF
	}
	var r rpcReq
	return r, json.Unmarshal(t.scanner.Bytes(), &r)
}

func (t *transport) write(v any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	data, _ := json.Marshal(v)
	fmt.Fprintf(os.Stdout, "%s\n", data)
}

func (t *transport) respond(id, result any) {
	t.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (t *transport) respondErr(id any, code int, msg string) {
	t.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (t *transport) notify(method string, params any) {
	t.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// ---- identity ----

func agentName() string {
	for _, i := range os.Args[1:] {
		if strings.HasPrefix(i, "--as=") {
			return strings.TrimPrefix(i, "--as=")
		}
	}
	if v := os.Getenv("CLAUDE_PEERS_AGENT"); v != "" {
		return v
	}
	if b, err := os.ReadFile(".claude-peers-agent"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return "" // ephemeral
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- server state ----

type server struct {
	t       *transport
	c       *peers.Client
	me      string
	machine string
	cwd     string
	session string

	mu     sync.Mutex
	unread []peers.Message // buffered for check_messages fallback
}

func main() {
	url := env("NATS_URL", "nats://127.0.0.1:4222")
	c, err := peers.Connect(url, os.Getenv("NATS_CREDS"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-mcp] connect:", err)
		os.Exit(1)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Setup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-mcp] setup:", err)
		os.Exit(1)
	}

	cwd, _ := os.Getwd()
	s := &server{
		t:       newTransport(),
		c:       c,
		me:      agentName(),
		machine: env("CLAUDE_PEERS_MACHINE", hostname()),
		cwd:     cwd,
		session: env("CLAUDE_SESSION_ID", newSession()),
	}

	if s.me != "" {
		if err := c.Register(ctx, s.peer()); err != nil {
			fmt.Fprintln(os.Stderr, "[cp3-mcp] register:", err)
		}
		go s.heartbeat(ctx)
		go s.pushLoop(ctx) // inject inbound peer messages into the live session
	}

	s.serve(ctx)
}

func (s *server) peer() peers.Peer {
	return peers.Peer{Agent: s.me, Machine: s.machine, Cwd: s.cwd, Session: s.session}
}

func (s *server) heartbeat(ctx context.Context) {
	tk := time.NewTicker(15 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			s.c.Heartbeat(ctx, s.me)
		}
	}
}

// pushLoop subscribes the durable inbox and injects each message into the live
// Claude session as a channel notification (meta values MUST all be strings —
// Claude Code silently drops notifications with non-string meta).
func (s *server) pushLoop(ctx context.Context) {
	s.c.Subscribe(ctx, s.me, func(m peers.Message) {
		s.mu.Lock()
		s.unread = append(s.unread, m)
		s.mu.Unlock()

		var from peers.Peer
		for _, p := range s.peersList(ctx) {
			if p.Agent == m.From {
				from = p
				break
			}
		}
		s.t.notify("notifications/claude/channel", map[string]any{
			"content": m.Content,
			"meta": map[string]string{
				"message_id":   m.ID,
				"from_agent":   m.From,
				"from_machine": from.Machine,
				"from_summary": from.Summary,
				"from_cwd":     from.Cwd,
				"sent_at":      time.UnixMilli(m.TS).Format(time.RFC3339),
			},
		})
	})
}

func (s *server) peersList(ctx context.Context) []peers.Peer {
	list, _ := s.c.Peers(ctx)
	return list
}

// ---- MCP loop ----

const protocolVersion = "2025-03-26"

func (s *server) serve(ctx context.Context) {
	for {
		req, err := s.t.read()
		if err != nil {
			return
		}
		switch req.Method {
		case "initialize":
			s.handleInit(req.ID)
		case "notifications/initialized":
			// ack, no-op
		case "tools/list":
			s.t.respond(req.ID, map[string]any{"tools": toolSchemas})
		case "tools/call":
			s.handleCall(ctx, req.ID, req.Params)
		default:
			if req.ID != nil {
				s.t.respondErr(req.ID, -32601, "method not found: "+req.Method)
			}
		}
	}
}

func (s *server) handleInit(id any) {
	s.t.respond(id, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"experimental": map[string]any{"claude/channel": map[string]any{}},
			"tools":        map[string]any{},
		},
		"serverInfo":   map[string]any{"name": "claude-peers", "version": "3.0.0"},
		"instructions": instructions + s.fleetContext(context.Background()),
	})
}

func (s *server) fleetContext(ctx context.Context) string {
	list := s.peersList(ctx)
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n--- FLEET CONTEXT (injected at session start) ---\n%d peer(s) on the network:", len(list))
	for _, p := range list {
		label := p.Agent
		if label == "" {
			label = "session " + p.Session + " (ephemeral)"
		}
		fmt.Fprintf(&b, "\n- %s on %s", label, p.Machine)
		if p.Summary != "" {
			fmt.Fprintf(&b, " — %s", p.Summary)
		}
	}
	return b.String()
}

func toolText(t *transport, id any, format string, args ...any) {
	t.respond(id, map[string]any{"content": []map[string]any{{"type": "text", "text": fmt.Sprintf(format, args...)}}})
}

func toolErr(t *transport, id any, format string, args ...any) {
	t.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
		"content": []map[string]any{{"type": "text", "text": fmt.Sprintf(format, args...)}},
		"isError": true,
	}})
}

func (s *server) handleCall(ctx context.Context, id any, params json.RawMessage) {
	var call struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"arguments"`
	}
	json.Unmarshal(params, &call)

	switch call.Name {
	case "list_peers":
		list := s.peersList(ctx)
		var b strings.Builder
		fmt.Fprintf(&b, "%d peer(s) on the network:\n", len(list))
		for _, p := range list {
			label := p.Agent
			if label == "" {
				label = "session " + p.Session + " (ephemeral — not addressable by name)"
			}
			self := ""
			if p.Session == s.session {
				self = "  ← this session"
			}
			fmt.Fprintf(&b, "\n%s on %s%s\n  CWD: %s\n", label, p.Machine, self, p.Cwd)
			if p.Summary != "" {
				fmt.Fprintf(&b, "  Summary: %s\n", p.Summary)
			}
		}
		toolText(s.t, id, "%s", b.String())

	case "send_message":
		var a struct{ To, Message string }
		json.Unmarshal(call.Args, &a)
		if a.To == "" || a.Message == "" {
			toolErr(s.t, id, "send_message requires 'to' and 'message'")
			return
		}
		if err := s.c.Send(ctx, peers.Message{From: s.me, To: a.To, Content: a.Message, DeliverAs: "steer"}); err != nil {
			toolErr(s.t, id, "send failed: %v", err)
			return
		}
		toolText(s.t, id, "Message sent to %s (queues if offline, delivers on reconnect).", a.To)

	case "set_summary":
		var a struct{ Summary string }
		json.Unmarshal(call.Args, &a)
		if s.me == "" {
			toolErr(s.t, id, "ephemeral session has no presence to summarize; start with --as=<name>")
			return
		}
		if err := s.c.SetSummary(ctx, s.me, a.Summary); err != nil {
			toolErr(s.t, id, "set_summary failed: %v", err)
			return
		}
		toolText(s.t, id, "Summary updated.")

	case "check_messages":
		s.mu.Lock()
		msgs := s.unread
		s.unread = nil
		s.mu.Unlock()
		if len(msgs) == 0 {
			toolText(s.t, id, "No new messages.")
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d message(s):\n", len(msgs))
		for _, m := range msgs {
			fmt.Fprintf(&b, "\n[from %s] %s\n", m.From, m.Content)
		}
		toolText(s.t, id, "%s", b.String())

	case "claim_agent_name":
		var a struct{ Name string }
		json.Unmarshal(call.Args, &a)
		if a.Name == "" {
			toolErr(s.t, id, "claim_agent_name requires 'name'")
			return
		}
		p := peers.Peer{Agent: a.Name, Machine: s.machine, Cwd: s.cwd, Session: s.session}
		if holder, err := s.c.Claim(ctx, p); err == peers.ErrNameTaken {
			toolErr(s.t, id, "name %q already held by session %s on %s", a.Name, holder.Session, holder.Machine)
			return
		} else if err != nil {
			toolErr(s.t, id, "claim failed: %v", err)
			return
		}
		s.mu.Lock()
		s.me = a.Name
		s.mu.Unlock()
		go s.heartbeat(ctx)
		go s.pushLoop(ctx)
		toolText(s.t, id, "Claimed agent name %q for this session.", a.Name)

	default:
		s.t.respondErr(id, -32602, "unknown tool: "+call.Name)
	}
}

// ---- helpers ----

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func newSession() string {
	// ponytail: pid is unique enough for a session handle; no uuid dep.
	return fmt.Sprintf("s-%d", os.Getpid())
}
