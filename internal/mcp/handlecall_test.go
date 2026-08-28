package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	peers "github.com/WillyV3/cp3"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

func runServer(t *testing.T) *natsserver.Server {
	t.Helper()
	s, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

// testServer wires a real NATS-backed server whose JSON-RPC output is
// captured instead of written to stdout.
func testServer(t *testing.T, agent string) (*server, *bytes.Buffer) {
	t.Helper()
	ns := runServer(t)
	c, err := peers.Connect(ns.ClientURL(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if err := c.Setup(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir()) // spool writes stay in the sandbox
	buf := &bytes.Buffer{}
	s := &server{
		t:       &transport{out: buf, mu: sync.Mutex{}},
		c:       c,
		me:      agent,
		machine: "testbox",
		cwd:     "/test",
		session: "sess-test",
	}
	if agent != "" {
		if err := c.Register(context.Background(), s.peer()); err != nil {
			t.Fatal(err)
		}
	}
	return s, buf
}

func call(t *testing.T, s *server, buf *bytes.Buffer, name string, args any) string {
	t.Helper()
	buf.Reset()
	raw, _ := json.Marshal(args)
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(raw)})
	s.handleCall(context.Background(), 1, params)
	return buf.String()
}

// send_message is where the delivery bugs lived. Every outcome must be
// distinguishable in what the AGENT actually reads back.
func TestHandleCallSendMessage(t *testing.T) {
	s, buf := testServer(t, "sender")

	t.Run("missing args rejected", func(t *testing.T) {
		if out := call(t, s, buf, "send_message", map[string]string{"to": "x"}); !strings.Contains(out, "requires") {
			t.Errorf("expected validation error, got %s", out)
		}
	})

	t.Run("no inbox is reported as NOT DELIVERED", func(t *testing.T) {
		out := call(t, s, buf, "send_message", map[string]string{"to": "ghost-town", "message": "hi"})
		if !strings.Contains(out, "NOT DELIVERED") {
			t.Errorf("void send must be loud, got %s", out)
		}
	})

	t.Run("hostile recipient cannot be sent to", func(t *testing.T) {
		out := call(t, s, buf, "send_message", map[string]string{"to": ">", "message": "hi"})
		if strings.Contains(out, "delivered to") || strings.Contains(out, "queued for") {
			t.Errorf("wildcard recipient reported as sent: %s", out)
		}
	})

	t.Run("existing inbox reports queued", func(t *testing.T) {
		if err := s.c.Register(context.Background(), peers.Peer{Agent: "bob", Machine: "m", Session: "s"}); err != nil {
			t.Fatal(err)
		}
		out := call(t, s, buf, "send_message", map[string]string{"to": "bob", "message": "hi"})
		if !strings.Contains(out, "queued for bob") {
			t.Errorf("expected queued, got %s", out)
		}
	})
}

func TestHandleCallListPeers(t *testing.T) {
	s, buf := testServer(t, "watcher")
	out := call(t, s, buf, "list_peers", map[string]string{})
	if !strings.Contains(out, "watcher") {
		t.Errorf("list_peers omitted the caller: %s", out)
	}
	if !strings.Contains(out, "this session") {
		t.Errorf("list_peers should mark the calling session: %s", out)
	}
}

func TestHandleCallCheckMessagesDrainsSpool(t *testing.T) {
	s, buf := testServer(t, "reader")
	// A message the notification never surfaced still has to be retrievable.
	spoolAppend("reader", peers.Message{ID: "m1", From: "caretaker", Content: "the lost briefing"})
	out := call(t, s, buf, "check_messages", map[string]string{})
	if !strings.Contains(out, "the lost briefing") {
		t.Errorf("check_messages did not drain the spool: %s", out)
	}
	// Draining is destructive-once: a second call must not replay it.
	if out = call(t, s, buf, "check_messages", map[string]string{}); strings.Contains(out, "the lost briefing") {
		t.Errorf("spool replayed after drain: %s", out)
	}
}

func TestHandleCallSetSummary(t *testing.T) {
	t.Run("ephemeral session refused", func(t *testing.T) {
		s, buf := testServer(t, "")
		if out := call(t, s, buf, "set_summary", map[string]string{"summary": "x"}); !strings.Contains(out, "ephemeral") {
			t.Errorf("expected ephemeral refusal, got %s", out)
		}
	})
	t.Run("named session accepted", func(t *testing.T) {
		s, buf := testServer(t, "worker")
		out := call(t, s, buf, "set_summary", map[string]string{"summary": "building the thing"})
		if strings.Contains(out, "error") {
			t.Errorf("set_summary failed: %s", out)
		}
	})
}

func TestHandleCallClaimAgentName(t *testing.T) {
	s, buf := testServer(t, "")
	t.Run("sanitizes hostile names", func(t *testing.T) {
		call(t, s, buf, "claim_agent_name", map[string]string{"name": "Bad.Name>"})
		if got := s.name(); got != "" && peers.SanitizeName(got) != got {
			t.Errorf("claimed an unsanitized name %q — it would create an unaddressable inbox", got)
		}
	})
	t.Run("claims a clean name", func(t *testing.T) {
		call(t, s, buf, "claim_agent_name", map[string]string{"name": "fresh-agent"})
		if s.name() != "fresh-agent" {
			t.Errorf("claim failed, name is %q", s.name())
		}
	})
}

func TestHandleCallUnknownTool(t *testing.T) {
	s, buf := testServer(t, "x")
	if out := call(t, s, buf, "no_such_tool", map[string]string{}); !strings.Contains(out, "unknown tool") {
		t.Errorf("expected unknown-tool error, got %s", out)
	}
}

// jim's 26-hour replay: spoolAppend ran on every delivery but the spool only
// cleared via check_messages and startup replay — both FALLBACK paths. A
// normally-delivered message therefore stayed queued forever, and each restart
// replayed the entire history (rover's file held 13 days). A message the client
// demonstrably read must leave the spool; one it never read must survive.
func TestSpoolClearsOnceTheClientProvesItRead(t *testing.T) {
	s, buf := testServer(t, "reader")

	deliver := func(id, body string) {
		spoolAppend("reader", peers.Message{ID: id, From: "sender", Content: body})
		s.mu.Lock()
		s.notified = append(s.notified, id)
		s.mu.Unlock()
	}

	deliver("m1", "first")
	deliver("m2", "second")
	if got := len(spoolPeek(t, "reader")); got != 2 {
		t.Fatalf("precondition: want 2 spooled, got %d", got)
	}

	// An inbound request proves the client is alive and reading this pipe.
	s.confirmNotified()
	if got := spoolPeek(t, "reader"); len(got) != 0 {
		t.Errorf("read messages still spooled — they will replay on restart: %+v", got)
	}

	// A message notified but NOT yet confirmed must survive: this is rover's
	// case, where the process died before the human saw anything.
	spoolAppend("reader", peers.Message{ID: "m3", From: "sender", Content: "never seen"})
	s.mu.Lock()
	s.notified = nil // process dies before any further client traffic
	s.mu.Unlock()
	if got := spoolPeek(t, "reader"); len(got) != 1 || got[0].ID != "m3" {
		t.Errorf("unconfirmed message must survive for replay, got %+v", got)
	}

	// And a partial confirmation must not take the innocent with it.
	spoolAppend("reader", peers.Message{ID: "m4", From: "sender", Content: "also unseen"})
	s.mu.Lock()
	s.notified = []string{"m4"}
	s.mu.Unlock()
	s.confirmNotified()
	got := spoolPeek(t, "reader")
	if len(got) != 1 || got[0].ID != "m3" {
		t.Errorf("selective removal wrong: want only m3 left, got %+v", got)
	}
	_ = buf
}

// spoolPeek reads the spool without draining it.
func spoolPeek(t *testing.T, agent string) []peers.Message {
	t.Helper()
	msgs := spoolDrain(agent)
	for _, m := range msgs { // put them back
		spoolAppend(agent, m)
	}
	return msgs
}
