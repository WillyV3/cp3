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
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	peers "github.com/WillyV3/cp3"
	"github.com/WillyV3/cp3/internal/boot"
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

func asFlag() string {
	for _, i := range os.Args[1:] {
		if strings.HasPrefix(i, "--as=") {
			return strings.TrimPrefix(i, "--as=")
		}
	}
	return ""
}

// claimIdentity resolves and claims this session's name. Collision policy by
// how deliberate the name was: a dir-derived default quietly takes a -2/-3
// suffix (zero-config must never error), an explicit name falls back to
// ephemeral with a loud stderr line (silently renaming a chosen identity
// would misroute messages).
func claimIdentity(ctx context.Context, c *peers.Client, p peers.Peer, source string) string {
	if p.Agent == "" {
		return ""
	}
	// Explicit names (--as flag / env, human-typed at launch) are strict: a
	// collision means ephemeral + loud, never a silent rename — messages
	// addressed to that exact name must not route to an impostor.
	if source == "flag" || source == "env" {
		holder, err := c.Claim(ctx, p)
		if err == nil {
			return p.Agent
		}
		if errors.Is(err, peers.ErrNameTaken) {
			fmt.Fprintf(os.Stderr, "[cp3-mcp] name %q held by session %s on %s; running ephemeral (pick another with --as or CLAUDE_PEERS_AGENT)\n",
				p.Agent, holder.Session, holder.Machine)
		} else {
			fmt.Fprintln(os.Stderr, "[cp3-mcp] claim:", err)
		}
		return ""
	}
	// Brief retry before falling back: with release-on-close + CAS
	// heartbeats, a held name during startup is usually a dying predecessor
	// (reconnect race), gone within a couple seconds. A live legit holder
	// costs us ~2s once, then we suffix.
	for range 2 {
		if _, err := c.Claim(ctx, p); err == nil {
			return p.Agent
		} else if !errors.Is(err, peers.ErrNameTaken) {
			break
		}
		time.Sleep(time.Second)
	}
	// File/dir-derived names fall back to a machine-qualified twin — the
	// common cause is the same synced project dir open on two machines.
	actual, err := c.ClaimWithFallback(ctx, p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-mcp] claim:", err)
		return ""
	}
	if actual.Agent != p.Agent {
		fmt.Fprintf(os.Stderr, "[cp3-mcp] name %q taken; running as %q\n", p.Agent, actual.Agent)
	}
	return actual.Agent
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- server state ----

type server struct {
	t          *transport
	c          *peers.Client
	me         string // claimed name; access via name()/setName()
	configured string // what identity resolution wanted (drift target)
	machine    string
	cwd        string
	session    string

	parentPID  int                // the claude process; keys the statusline state file
	pushCancel context.CancelFunc // stops inbox consumption if the name is lost

	mu     sync.Mutex
	unread []peers.Message // buffered for check_messages fallback
}

// Run serves MCP over stdio until stdin closes.
func Run() {
	c, err := boot.Connect()
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
		machine: env("CLAUDE_PEERS_MACHINE", hostname()),
		cwd:     cwd,
		session: env("CLAUDE_SESSION_ID", newSession()),
	}
	s.parentPID = os.Getppid()
	// Headless one-shots (claude -p from daemons/crons) must not become
	// addressable residents: no claim, no presence, no register event — they
	// can still send and list. The dispatcher sets CLAUDE_PEERS_EPHEMERAL=1.
	if os.Getenv("CLAUDE_PEERS_EPHEMERAL") == "" {
		name, source := peers.ResolveIdentity(cwd, asFlag())
		s.configured = name
		s.me = name
		s.me = claimIdentity(ctx, c, s.peer(), source)
	} else {
		fmt.Fprintln(os.Stderr, "[cp3-mcp] CLAUDE_PEERS_EPHEMERAL set: send-only, no presence")
	}
	s.writeState() // statusline reads identity by claude-pid, not by cwd

	if s.me != "" {
		pushCtx, pushCancel := context.WithCancel(ctx)
		s.pushCancel = pushCancel
		go s.pushLoop(pushCtx, s.me) // inject inbound peer messages into the live session
	}
	if s.me != "" || s.configured != "" {
		// Runs even when ephemeral: a session that WANTED a name keeps
		// trying to pick it up as soon as it frees (zero-touch repair).
		go s.heartbeat(ctx)
	}

	// Names release the moment the session ends — the TTL is only the crash
	// backstop. Claude closing the MCP channel lands on the stdin-EOF path;
	// a kill lands on the signal path. Either way a restart within the same
	// second re-claims its own name instead of a -machine fallback.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		s.release()
		os.Exit(0)
	}()
	s.serve(ctx)
	s.release()
}

// release deregisters this session's name (bounded; best effort).
func (s *server) release() {
	s.removeState()
	if s.name() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.c.Deregister(ctx, s.name()); err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-mcp] deregister:", err)
	}
}

func (s *server) name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.me
}

func (s *server) setName(n string) {
	s.mu.Lock()
	s.me = n
	s.mu.Unlock()
}

// tryReclaim is the zero-touch identity repair: while claimed ≠ configured,
// each heartbeat tick tries the configured name; the moment it frees (old
// holder released or expired) we take it, move the inbox, drop the suffix,
// and tell the session. Claim is only-if-free + CAS heartbeats, so there is
// no fight risk — just a quiet upgrade.
func (s *server) tryReclaim(ctx context.Context) {
	want := s.configured
	cur := s.name()
	if want == "" || cur == want {
		return
	}
	p := peers.Peer{Agent: want, Machine: s.machine, Cwd: s.cwd, Session: s.session}
	if _, err := s.c.Claim(ctx, p); err != nil {
		return // still held; try again next tick
	}
	old := cur
	s.setName(want)
	if s.pushCancel != nil {
		s.pushCancel() // stop draining inbox-<suffix>
	}
	pushCtx, cancel := context.WithCancel(ctx)
	s.pushCancel = cancel
	go s.pushLoop(pushCtx, want)
	was := fmt.Sprintf("you were temporarily %q", old)
	if old == "" {
		was = "you had been running unnamed"
	} else {
		dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
		s.c.Deregister(dctx, old)
		dcancel()
	}
	s.writeState()
	fmt.Fprintf(os.Stderr, "[cp3-mcp] reclaimed %q (was %q)\n", want, old)
	s.t.notify("notifications/claude/channel", map[string]any{
		"content": fmt.Sprintf("Identity repaired: you are now %q (the name freed up; %s). No action needed.", want, was),
		"meta":    map[string]string{"from_agent": "cp3", "sent_at": time.Now().Format(time.RFC3339)},
	})
}

func (s *server) peer() peers.Peer {
	return peers.Peer{Agent: s.name(), Machine: s.machine, Cwd: s.cwd, Session: s.session}
}

func (s *server) heartbeat(ctx context.Context) {
	tk := time.NewTicker(15 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			s.tryReclaim(ctx)
			cur := s.name()
			if cur == "" {
				continue // ephemeral: nothing to keep alive yet
			}
			err := s.c.Heartbeat(ctx, cur, s.session)
			if err == nil {
				continue
			}
			if errors.Is(err, peers.ErrNameLost) {
				s.nameLost(err)
				continue // stay in the loop: tryReclaim keeps working the repair
			}
			// Expired (machine slept past the TTL) or transient: reclaim.
			if _, cerr := s.c.Claim(ctx, s.peer()); cerr != nil {
				if errors.Is(cerr, peers.ErrNameTaken) {
					s.nameLost(cerr)
					continue
				}
				fmt.Fprintf(os.Stderr, "[cp3-mcp] heartbeat %s: %v (reclaim: %v)\n", s.name(), err, cerr)
			}
		}
	}
}

// nameLost stops inbox consumption (two sessions draining one durable inbox
// split the messages) and tells the live session in plain language.
func (s *server) nameLost(cause error) {
	if s.pushCancel != nil {
		s.pushCancel()
	}
	s.setName("") // ephemeral until tryReclaim wins the name back
	s.writeState()
	fmt.Fprintf(os.Stderr, "[cp3-mcp] NAME LOST: %v — inbox consumption stopped\n", cause)
	s.t.notify("notifications/claude/channel", map[string]any{
		"content": fmt.Sprintf("Your peer name %q is now held by another session (%v). You have stopped receiving peer messages to avoid splitting the inbox. You can still send. No action needed: this session keeps watching and will re-claim the name automatically the moment it frees.", s.me, cause),
		"meta": map[string]string{
			"from_agent": "cp3",
			"sent_at":    time.Now().Format(time.RFC3339),
		},
	})
}

// pushLoop subscribes the durable inbox and injects each message into the live
// Claude session as a channel notification (meta values MUST all be strings —
// Claude Code silently drops notifications with non-string meta).
func (s *server) pushLoop(ctx context.Context, name string) {
	s.c.Subscribe(ctx, name, func(m peers.Message) {
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
				// A naive session mid-context may have lost the init
				// instructions — the notification itself carries the verb.
				"how_to_reply": fmt.Sprintf("call send_message with to=%q", m.From),
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
		if err := s.c.Send(ctx, peers.Message{From: s.name(), To: a.To, Content: a.Message, DeliverAs: "steer"}); err != nil {
			toolErr(s.t, id, "send failed: %v", err)
			return
		}
		toolText(s.t, id, "Message sent to %s (queues if offline, delivers on reconnect).", a.To)

	case "set_summary":
		var a struct{ Summary string }
		json.Unmarshal(call.Args, &a)
		if s.name() == "" {
			toolErr(s.t, id, "ephemeral session has no presence to summarize; start with --as=<name>")
			return
		}
		if err := s.c.SetSummary(ctx, s.name(), a.Summary); err != nil {
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
		wasEphemeral := s.name() == ""
		s.setName(a.Name)
		s.configured = a.Name
		if s.pushCancel != nil {
			s.pushCancel() // stop draining the old name's inbox
		}
		pushCtx, cancel := context.WithCancel(ctx)
		s.pushCancel = cancel
		go s.pushLoop(pushCtx, a.Name)
		if wasEphemeral {
			go s.heartbeat(ctx) // named for the first time: start presence upkeep
		}
		s.writeState()
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
