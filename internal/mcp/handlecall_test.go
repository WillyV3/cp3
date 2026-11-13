package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// jim's 26-hour replay: a spooled message that never left the spool replayed
// on every restart. A message posted into the session must leave the spool; one
// that failed to post must stay, and be handed over by check_messages.
func TestSpoolClearsOnlyOnASuccessfulPost(t *testing.T) {
	s, _ := testServer(t, "reader")
	var posted []string
	s.post = func(text string) error { posted = append(posted, text); return nil }

	m1 := peers.Message{ID: "m1", From: "sender", Content: "first"}
	spoolAppend("reader", m1)
	s.notifyMessage(m1, false)
	if len(posted) != 1 || !strings.Contains(posted[0], "first") || !strings.Contains(posted[0], "send_message") {
		t.Errorf("posted %q", posted)
	}
	if got := spoolPeek(t, "reader"); len(got) != 0 {
		t.Errorf("posted message still spooled, it will replay on restart: %+v", got)
	}

	s.post = func(string) error { return errors.New("socket gone") }
	m2 := peers.Message{ID: "m2", From: "sender", Content: "second"}
	spoolAppend("reader", m2)
	s.notifyMessage(m2, false)
	if got := spoolPeek(t, "reader"); len(got) != 1 || got[0].ID != "m2" {
		t.Errorf("unposted message must survive for replay, got %+v", got)
	}
	if len(s.unread) != 1 || s.unread[0].ID != "m2" {
		t.Errorf("unposted message must wait for check_messages, unread=%+v", s.unread)
	}
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

// The reply link through the MCP server: the header carries the id to answer
// with, a successful post records the receipt, the reply links back, and
// message_status reads each stage without waiting on anyone.
func TestReplyLinkThroughTheServer(t *testing.T) {
	s, buf := testServer(t, "reader")
	ctx := context.Background()
	if err := s.c.Register(ctx, peers.Peer{Agent: "asker", Session: "s-asker"}); err != nil {
		t.Fatal(err)
	}
	var posted []string
	s.post = func(text string) error { posted = append(posted, text); return nil }

	q := peers.Message{ID: peers.NewID(), From: "asker", To: "reader", Content: "which port?"}
	if out := call(t, s, buf, "message_status", map[string]string{"id": q.ID}); !strings.Contains(out, "no receipt") {
		t.Errorf("before delivery: %s", out)
	}
	s.notifyMessage(q, false)
	if len(posted) != 1 || !strings.Contains(posted[0], "[cp3 message "+q.ID+" from asker") || !strings.Contains(posted[0], `reply_to="`+q.ID+`"`) {
		t.Fatalf("header does not carry the id to reply with: %q", posted)
	}
	if out := call(t, s, buf, "message_status", map[string]string{"id": q.ID}); !strings.Contains(out, "shown") || !strings.Contains(out, "reader") {
		t.Errorf("after post: %s", out)
	}

	out := call(t, s, buf, "send_message", map[string]string{"to": "asker", "message": "4222", "reply_to": q.ID})
	if !strings.Contains(out, " · id ") {
		t.Errorf("send result carries no id: %s", out)
	}
	if out := call(t, s, buf, "message_status", map[string]string{"id": q.ID}); !strings.Contains(out, "replied") {
		t.Errorf("after reply: %s", out)
	}
	if out := call(t, s, buf, "send_message", map[string]string{"to": "asker", "message": "x", "reply_to": "deadbeef"}); !strings.Contains(out, "no such message") {
		t.Errorf("a wrong reply_to must fail loudly: %s", out)
	}

	// A reply arriving here names the message it answers and quotes it.
	s.notifyMessage(peers.Message{ID: peers.NewID(), From: "asker", Content: "thanks", ReplyTo: "abc", Quote: "4222"}, false)
	if last := posted[len(posted)-1]; !strings.Contains(last, `replying to your message abc ("4222")`) {
		t.Errorf("reply header: %q", last)
	}

	// A post that failed is not a receipt.
	s.post = func(string) error { return errors.New("socket gone") }
	lost := peers.Message{ID: peers.NewID(), From: "asker", To: "reader", Content: "lost"}
	s.notifyMessage(lost, false)
	if out := call(t, s, buf, "message_status", map[string]string{"id": lost.ID}); !strings.Contains(out, "no receipt") {
		t.Errorf("failed post recorded as shown: %s", out)
	}
}
