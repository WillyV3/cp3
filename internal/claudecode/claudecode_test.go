package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	peers "github.com/WillyV3/cp3"
)

// The wire format is the whole contract: two JSON lines, auth first.
func TestInboxPostWritesAuthThenUserMessage(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var lines []string
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		got <- lines
	}()

	if err := (Inbox{Socket: sock, Token: "tok-1"}).Post("hello\nthere"); err != nil {
		t.Fatal(err)
	}
	lines := <-got
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), lines)
	}
	var auth map[string]string
	json.Unmarshal([]byte(lines[0]), &auth)
	if auth["type"] != "auth" || auth["token"] != "tok-1" {
		t.Errorf("auth line: %s", lines[0])
	}
	var user struct {
		Type    string
		Message struct{ Role, Content string }
	}
	json.Unmarshal([]byte(lines[1]), &user)
	if user.Type != "user" || user.Message.Role != "user" || user.Message.Content != "hello\nthere" {
		t.Errorf("user line: %s", lines[1])
	}
}

func TestInboxPostWithoutTokenSendsOnlyTheMessage(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, _ := net.Listen("unix", sock)
	defer ln.Close()
	n := make(chan int, 1)
	go func() {
		c, _ := ln.Accept()
		defer c.Close()
		sc := bufio.NewScanner(c)
		k := 0
		for sc.Scan() {
			k++
		}
		n <- k
	}()
	if err := (Inbox{Socket: sock}).Post("x"); err != nil {
		t.Fatal(err)
	}
	if k := <-n; k != 1 {
		t.Errorf("want 1 line without a token, got %d", k)
	}
}

func TestInboxPostFailsLoudlyWithNoSocket(t *testing.T) {
	if err := (Inbox{}).Post("x"); err == nil {
		t.Fatal("want an error with no socket")
	}
	if err := (Inbox{Socket: filepath.Join(t.TempDir(), "none.sock")}).Post("x"); err == nil {
		t.Fatal("want an error when nothing listens")
	}
}

func TestInboxFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	if _, ok := InboxFromEnv(); ok {
		t.Error("ok without a socket var")
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/tmp/x.sock")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "t")
	in, ok := InboxFromEnv()
	if !ok || in.Socket != "/tmp/x.sock" || in.Token != "t" {
		t.Errorf("got %+v ok=%v", in, ok)
	}
}

// fakeTree stands in for ps and the MCP state files: a chain of pids with a
// command line each, and MCP state on whichever pid the test says.
func fakeTree(t *testing.T, parents map[int]int, cmd map[int]string, state map[int][3]any) {
	old := Tree
	t.Cleanup(func() { Tree = old })
	Tree.ParentOf = func(pid int) int { return parents[pid] }
	Tree.Cmdline = func(pid int) string { return cmd[pid] }
	Tree.State = func(pid int) (string, string, bool) {
		s, ok := state[pid]
		if !ok {
			return "", "", false
		}
		return s[0].(string), s[1].(string), s[2].(bool)
	}
}

// cp3 <- bash <- claude (cp3 run): the MCP server's claimed name wins over the
// directory, and the channel flag on claude's command line is seen.
func TestDescribeFindsTheMCPSessionUpTheTree(t *testing.T) {
	fakeTree(t,
		map[int]int{300: 200, 200: 100, 100: 1},
		map[int]string{100: "claude --dangerously-load-development-channels server:claude-peers"},
		map[int][3]any{100: {"keeper", "keeper", false}},
	)
	s := Describe(300, "/home/w/projects/other-dir")
	if s.Name != "keeper" || s.Source != "mcp" {
		t.Errorf("name/source: %q/%q", s.Name, s.Source)
	}
	if s.MCP == nil || s.MCP.Claimed != "keeper" || !s.ChannelFlag {
		t.Errorf("mcp=%+v channelFlag=%v", s.MCP, s.ChannelFlag)
	}
}

// The MCP server records a wanted name before it claims one; the mod must
// still learn an MCP server is there, or it spawns a twin.
func TestDescribeReportsMCPEvenBeforeAClaim(t *testing.T) {
	fakeTree(t,
		map[int]int{300: 100, 100: 1},
		map[int]string{100: "claude"},
		map[int][3]any{100: {"", "wanted-name", true}},
	)
	s := Describe(300, "/home/w/projects/dirname")
	if s.MCP == nil || s.MCP.Wanted != "wanted-name" || !s.MCP.NoChannel {
		t.Errorf("mcp=%+v", s.MCP)
	}
	if s.Name != "dirname" || s.Source != "default" {
		t.Errorf("falls back to the directory identity: %q/%q", s.Name, s.Source)
	}
	if s.ChannelFlag {
		t.Error("no flag on this claude's command line")
	}
}

func TestDescribeWithNoClaudeAboveUsesTheDirectory(t *testing.T) {
	fakeTree(t, map[int]int{300: 1}, map[int]string{}, map[int][3]any{})
	t.Setenv("CLAUDE_PEERS_AGENT", "")
	s := Describe(300, "/x/My Project")
	if s.MCP != nil || s.ChannelFlag || s.Source != "default" || s.Name != peers.SanitizeName("My Project") {
		t.Errorf("%+v", s)
	}
}

func TestStreamHelloThenMessages(t *testing.T) {
	var buf bytes.Buffer
	st := NewStream(&buf)
	st.Hello(peers.Peer{Agent: "x-omarchy", Machine: "omarchy", Cwd: "/w"}, 4242)
	st.Message(peers.Message{From: "keeper", To: "x-omarchy", Content: "hi"})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", lines)
	}
	var h Hello
	json.Unmarshal([]byte(lines[0]), &h)
	if h.Type != "hello" || h.Agent != "x-omarchy" || h.PID != 4242 {
		t.Errorf("hello: %s", lines[0])
	}
	var m map[string]any
	json.Unmarshal([]byte(lines[1]), &m)
	if _, hasType := m["type"]; hasType || m["from"] != "keeper" || m["content"] != "hi" {
		t.Errorf("message line must carry from/content and no type: %s", lines[1])
	}
}

func TestReplyHintNamesSendMessageAndTheSender(t *testing.T) {
	h := ReplyHint("config")
	if !strings.Contains(h, "SendMessage") || !strings.Contains(h, `to="config"`) || strings.Contains(h, "cp3 send") {
		t.Errorf("hint: %s", h)
	}
}
