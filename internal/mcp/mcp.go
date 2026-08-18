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
	"os/exec"
	"os/signal"
	"strconv"
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
	// out is where JSON-RPC goes. Injectable because hardcoding os.Stdout
	// made every tool handler untestable — which is how the delivery bugs
	// (ack-without-injection, success-into-the-void) shipped from a function
	// with 0% coverage. The untestability was the defect; CC was the symptom.
	out io.Writer
	mu  sync.Mutex
}

func newTransport() *transport {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	return &transport{scanner: sc, out: os.Stdout}
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
	w := t.out
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, "%s\n", data)
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

// parentHasChannelFlag reports whether the launching Claude loaded dev
// channels. ps (not /proc) so this works on macOS too.
func parentHasChannelFlag(pid int) bool {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return true // can't tell — don't cry wolf
	}
	return strings.Contains(string(out), "--dangerously-load-development-channels")
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
	noChannel  bool               // parent lacks the dev-channel flag: injection is a no-op
	pushCancel context.CancelFunc // stops inbox consumption if the name is lost

	mu     sync.Mutex
	unread []peers.Message // buffered for check_messages fallback
}

// sessionEnv is everything Run reads from the process world. Extracting it
// is what makes session startup testable at all: identity continuity, claim
// fallback, and spool replay are the highest-risk logic in cp3 (every
// identity incident so far lived here) and they were welded to boot.Connect,
// os.Stdin, and os.Exit — the last of which would kill a test binary outright.
type sessionEnv struct {
	cwd       string
	machine   string
	session   string
	asName    string // --as flag
	parentPID int
	ephemeral bool // CLAUDE_PEERS_EPHEMERAL: send-only, no presence
	noChannel bool // parent lacks the dev-channel flag
}

// newServer resolves this session's identity and returns the server plus any
// messages a previous process received but never surfaced. Pure policy: no
// dialing, no stdio, no exits, no goroutines — the caller owns those.
func newServer(ctx context.Context, c *peers.Client, t *transport, e sessionEnv) (*server, []peers.Message) {
	s := &server{
		t:         t,
		c:         c,
		machine:   e.machine,
		cwd:       e.cwd,
		session:   e.session,
		parentPID: e.parentPID,
		noChannel: e.noChannel,
	}
	// Headless one-shots (claude -p from daemons/crons) must not become
	// addressable residents: no claim, no presence, no register event — they
	// can still send and list.
	if !e.ephemeral {
		// Session continuity beats directory discovery: if THIS Claude
		// session already had an identity (state file survives MCP
		// reconnects — same claude pid), take it back. Without this, an
		// MCP restart re-reads the dir file, and in syncthing-shared dirs
		// that file may name a DIFFERENT machine's agent — the root of the
		// astrobot/sontara-mobile involuntary-rename incident.
		if st := ReadState(e.parentPID); st.Claimed != "" || st.Wanted != "" {
			s.configured = st.Wanted
			prev := st.Claimed
			if prev == "" {
				prev = st.Wanted
			}
			p := s.peer()
			p.Agent = prev
			if _, err := c.Claim(ctx, p); err == nil {
				s.me = prev
				fmt.Fprintf(os.Stderr, "[cp3-mcp] resumed session identity %q\n", prev)
			} else {
				fmt.Fprintf(os.Stderr, "[cp3-mcp] session identity %q not reclaimable (%v); ephemeral until it frees\n", prev, err)
			}
		} else {
			name, source := peers.ResolveIdentity(e.cwd, e.asName)
			s.configured = name
			s.me = name
			s.me = claimIdentity(ctx, c, s.peer(), source)
		}
	} else {
		fmt.Fprintln(os.Stderr, "[cp3-mcp] CLAUDE_PEERS_EPHEMERAL set: send-only, no presence")
	}
	s.writeState() // statusline reads identity by claude-pid, not by cwd

	// Anything a previous process received but never got in front of a human:
	// rover's exact case — acked by a process that died, so a reconnect had
	// nothing to replay from the stream. The spool has it. Drain the claimed
	// name AND the configured one: a reconnect may land on a suffixed twin
	// (rovertest -> rovertest-omarchy) while the spool sits under the name the
	// sender actually addressed. Missing that orphans the message.
	var missed []peers.Message
	if n := s.me; n != "" {
		missed = spoolDrain(n)
		if s.configured != "" && s.configured != n {
			missed = append(missed, spoolDrain(s.configured)...)
		}
	}
	return s, missed
}

// replayDelay lets the session finish initializing before late messages land.
var replayDelay = 2 * time.Second

// Run serves MCP over stdio until stdin closes. Thin shell: dial, read the
// world, hand policy to newServer, own the goroutines and exit codes.
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
	parentPID := os.Getppid()
	// Claude Code discards channel notifications from third-party MCP servers
	// unless launched with --dangerously-load-development-channels. It reports
	// nothing when it does — messages arrive, get written, and vanish.
	noChannel := !parentHasChannelFlag(parentPID)
	if noChannel {
		fmt.Fprintln(os.Stderr, "[cp3-mcp] WARNING: this Claude was launched without --dangerously-load-development-channels;")
		fmt.Fprintln(os.Stderr, "[cp3-mcp] peer messages will be received and spooled but NEVER DISPLAYED. Relaunch with: cp3 run")
	}
	s, missed := newServer(ctx, c, newTransport(), sessionEnv{
		cwd:       cwd,
		machine:   env("CLAUDE_PEERS_MACHINE", hostname()),
		session:   env("CLAUDE_SESSION_ID", newSession()),
		asName:    asFlag(),
		parentPID: parentPID,
		ephemeral: os.Getenv("CLAUDE_PEERS_EPHEMERAL") != "",
		noChannel: noChannel,
	})

	if len(missed) > 0 {
		go func(ms []peers.Message) {
			time.Sleep(replayDelay)
			for _, m := range ms {
				s.notifyMessage(m, true)
			}
		}(missed)
	}

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

	// Laptop wake / server bounce: re-assert presence the moment the
	// connection returns.
	c.OnReconnect(func() {
		if n := s.name(); n != "" {
			rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
			defer rcancel()
			if _, err := s.c.Claim(rctx, s.peer()); err == nil {
				s.writeState()
				fmt.Fprintf(os.Stderr, "[cp3-mcp] reconnected, presence re-asserted for %q\n", n)
			}
		}
	})

	// Names release the moment the session ends — the TTL is only the crash
	// backstop. Claude closing the MCP channel lands on the stdin-EOF path;
	// a kill lands on the signal path.
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

// release deregisters this session's name (bounded; best effort). The state
// file deliberately SURVIVES: /mcp reconnect SIGTERMs this process while the
// claude session lives on, and the successor MCP resumes identity from it.
func (s *server) release() {
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
	if missed := spoolDrain(want); len(missed) > 0 {
		go func(ms []peers.Message) {
			for _, m := range ms {
				s.notifyMessage(m, true)
			}
		}(missed)
	}
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
		// Durable FIRST: the notify below is fire-and-forget with no receipt,
		// so a message must survive this process dying before it is seen.
		spoolAppend(name, m)
		s.mu.Lock()
		s.unread = append(s.unread, m)
		s.mu.Unlock()

		s.notifyMessage(m, false)
	})
}

// notifyMessage injects one peer message into the live session. replay marks a
// message a previous process received but never surfaced (spool replay), so
// the agent knows why it is arriving late rather than treating it as new.
func (s *server) notifyMessage(m peers.Message, replay bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var from peers.Peer
	for _, p := range s.peersList(ctx) {
		if p.Agent == m.From {
			from = p
			break
		}
	}
	content := m.Content
	if replay {
		content = "[delivered late — received while this session was restarting]\n" + content
	}
	s.t.notify("notifications/claude/channel", map[string]any{
		"content": content,
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
		status, serr := s.c.Send(ctx, peers.Message{From: s.name(), To: a.To, Content: a.Message, DeliverAs: "steer"})
		if err := serr; err != nil {
			toolErr(s.t, id, "send failed: %v", err)
			return
		}
		// This string is what every agent on the fleet reads to decide whether
		// its message landed. It said "queues if offline" unconditionally,
		// which is the lie that hid the rover bug for two days.
		toolText(s.t, id, "%s", status.Human(a.To))

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
		// The spool is the durable record; the memory buffer only covers
		// messages this process saw. Merge, dedup by id, clear both.
		msgs := spoolDrain(s.name())
		s.mu.Lock()
		seen := map[string]bool{}
		for _, m := range msgs {
			seen[m.ID] = true
		}
		for _, m := range s.unread {
			if !seen[m.ID] {
				msgs = append(msgs, m)
			}
		}
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
		// Sanitize: the name becomes a NATS subject token, and an unsanitized
		// claim would create an inbox nobody can address.
		a.Name = peers.SanitizeName(a.Name)
		if a.Name == "" {
			toolErr(s.t, id, "invalid agent name")
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

// mutate4go-manifest-begin
// {"version":1,"tested_at":"2026-08-18T13:02:16-04:00","module_hash":"b226870edf65c1ff1df59ec5cfb3dd1b313b779e190e20255911ac2bc15106ea","functions":[{"id":"func/newTransport","name":"newTransport","line":53,"end_line":57,"hash":"e0161ccb4339cf38c21f6d5987c769fb45a5da0aaad75c959603c1af64bce4b8"},{"id":"func/transport.read","name":"transport.read","line":59,"end_line":68,"hash":"fb40d242352229dbcb9025da67e0d867d64da0676b11e16b9f8b3011951cdfa1"},{"id":"func/transport.write","name":"transport.write","line":70,"end_line":79,"hash":"cc4e739beb63e333627992bc46d778ff6b3decc186d01fb9e8dc6e1e55ff2c3b"},{"id":"func/transport.respond","name":"transport.respond","line":81,"end_line":83,"hash":"2a2edbd65ddd12f73c91cb3c7b6568e2fa59851637fb92a073aa752069d073f9"},{"id":"func/transport.respondErr","name":"transport.respondErr","line":85,"end_line":87,"hash":"344ea7d4f031a8f001065bcc0cdde707d5d0decbf33c189a36187117c609c833"},{"id":"func/transport.notify","name":"transport.notify","line":89,"end_line":91,"hash":"8132d3f479e57e9c648e3e7e0f1181dbffc3ffa3ea05208dc388213af0ba74e2"},{"id":"func/asFlag","name":"asFlag","line":95,"end_line":102,"hash":"e69fedf5e963944f0e5d0c354af478358d52cab331680f9fabb0c38c4810b93e"},{"id":"func/claimIdentity","name":"claimIdentity","line":109,"end_line":152,"hash":"9896437b1a73af43027472ffcb4879f856d7bc8bd8f68e3480a2aa53c96b7861"},{"id":"func/parentHasChannelFlag","name":"parentHasChannelFlag","line":156,"end_line":162,"hash":"9d5fd4120e9bfca28b76b629e40799be7cbc1a0a966054e110790bcd5fccfb73"},{"id":"func/env","name":"env","line":164,"end_line":169,"hash":"d627d6d31f4af41051f59ef3993d05f8109737be419ec76b8719f4f9dda93424"},{"id":"func/newServer","name":"newServer","line":208,"end_line":267,"hash":"1c96f608dd5cf67d70b506943dec1c94ca9da1e8c0c0e3ded3df0423f10a7d92"},{"id":"func/Run","name":"Run","line":274,"end_line":353,"hash":"dd95d5173545cc013bc5d41ffc8612e337d82b7f9dc06abea3164554006e481a"},{"id":"func/server.release","name":"server.release","line":358,"end_line":367,"hash":"8f62fdd0c8b8b62cab0339b265d8c471c8c144d7fe10391b5b20d0d72d043991"},{"id":"func/server.name","name":"server.name","line":369,"end_line":373,"hash":"10578a2ed047cd88ef2c2986211bb7ba6998a2fa723f4200cd6deb66489d60fe"},{"id":"func/server.setName","name":"server.setName","line":375,"end_line":379,"hash":"6c0267825659af6901c5f114f537dffc127638c884cc6e04a08e7584895a846f"},{"id":"func/server.tryReclaim","name":"server.tryReclaim","line":386,"end_line":425,"hash":"1956899da156cd665c8e394b846bd71aa2bc2422ab91d51adbb2d4466c002f1c"},{"id":"func/server.peer","name":"server.peer","line":427,"end_line":429,"hash":"88f7d220300f3826fcebf00a89786c6998f323c91f5a0dd8d39aa5d1407deb9b"},{"id":"func/server.heartbeat","name":"server.heartbeat","line":431,"end_line":462,"hash":"084905d9f9b89c0b6c5fa018405a9ab34bd7f747a77e44c47be42fbde00d2562"},{"id":"func/server.nameLost","name":"server.nameLost","line":466,"end_line":480,"hash":"941f1beaf2b30827c7a79a11f944f5fc01d56fc2af2f3d64f20579dde6af1a7f"},{"id":"func/server.pushLoop","name":"server.pushLoop","line":485,"end_line":496,"hash":"2d9550fd60ad358c4eda7f2ed612b153d237733042c6add102f783cb6e98d4f9"},{"id":"func/server.notifyMessage","name":"server.notifyMessage","line":501,"end_line":529,"hash":"42f2b6d19617c16fa84221ea5bf79c10798d0e3fc77084dcad9a7f4ccc514eaf"},{"id":"func/server.peersList","name":"server.peersList","line":531,"end_line":534,"hash":"553b2ef919fd32d72ad52a26624dd8f3dfbe42c91d5e2190f346c06349bd49e1"},{"id":"func/server.serve","name":"server.serve","line":540,"end_line":561,"hash":"e1b1c25a40f2cc777eb56d4541def865a0755328f7d6c1f5b5ed582b2c0a9a9f"},{"id":"func/server.handleInit","name":"server.handleInit","line":563,"end_line":573,"hash":"367c23564429f50127cefa4acaa9393015c40dd08bc00acb3ba056c7c30e12b5"},{"id":"func/server.fleetContext","name":"server.fleetContext","line":575,"end_line":593,"hash":"fb9bd92a8d2a9d2c542301b3189ae870789ac1da1a7ec73b18860a4b1b173db3"},{"id":"func/toolText","name":"toolText","line":595,"end_line":597,"hash":"382773c52a6a03dc1a423d16db1c3584a70e361a4f77f9b349f4a8b3501cbba4"},{"id":"func/toolErr","name":"toolErr","line":599,"end_line":604,"hash":"98a94c26311ec12fa55b661c0143164cb193a9f732f6c44e5a85b90e452df5d2"},{"id":"func/server.handleCall","name":"server.handleCall","line":606,"end_line":731,"hash":"0a6053252649ed6d59cc14324d6b973d5d596b201ea23a79f6db15503ec5162c"},{"id":"func/hostname","name":"hostname","line":735,"end_line":738,"hash":"9818d312243f8ceb0f204bb80cb4a9c42f7a04bba87ebed368fbcebf9cbdb2b0"},{"id":"func/newSession","name":"newSession","line":740,"end_line":743,"hash":"4c4b84ea32012efb794d57b62502618474ad6f5a823cc891517f6eadf7758bfb"}]}
// mutate4go-manifest-end
