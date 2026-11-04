package claudecode

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	peers "github.com/WillyV3/cp3"
)

// fakeTree stands in for ps and the MCP state files: a chain of pids, and MCP
// state on whichever pid the test says.
func fakeTree(t *testing.T, parents map[int]int, state map[int][2]string) {
	old := Tree
	t.Cleanup(func() { Tree = old })
	Tree.ParentOf = func(pid int) int { return parents[pid] }
	Tree.State = func(pid int) (string, string) { return state[pid][0], state[pid][1] }
}

// cp3 <- bash <- claude: the MCP server's claimed name wins over the directory.
func TestDescribeFindsTheMCPSessionUpTheTree(t *testing.T) {
	fakeTree(t,
		map[int]int{300: 200, 200: 100, 100: 1},
		map[int][2]string{100: {"keeper", "keeper"}},
	)
	s := Describe(300, "/home/w/projects/other-dir")
	if s.Name != "keeper" || s.Source != "mcp" {
		t.Errorf("name/source: %q/%q", s.Name, s.Source)
	}
	if s.MCP == nil || s.MCP.Claimed != "keeper" {
		t.Errorf("mcp=%+v", s.MCP)
	}
}

// The MCP server records a wanted name before it claims one; until the claim,
// a cp3 command sends as the directory identity.
func TestDescribeReportsMCPEvenBeforeAClaim(t *testing.T) {
	fakeTree(t,
		map[int]int{300: 100, 100: 1},
		map[int][2]string{100: {"", "wanted-name"}},
	)
	s := Describe(300, "/home/w/projects/dirname")
	if s.MCP == nil || s.MCP.Wanted != "wanted-name" {
		t.Errorf("mcp=%+v", s.MCP)
	}
	if s.Name != "dirname" || s.Source != "default" {
		t.Errorf("falls back to the directory identity: %q/%q", s.Name, s.Source)
	}
}

func TestDescribeWithNoClaudeAboveUsesTheDirectory(t *testing.T) {
	fakeTree(t, map[int]int{300: 1}, map[int][2]string{})
	t.Setenv("CLAUDE_PEERS_AGENT", "")
	s := Describe(300, "/x/My Project")
	if s.MCP != nil || s.Source != "default" || s.Name != peers.SanitizeName("My Project") {
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
