package claudecode

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

	peers "github.com/WillyV3/cp3"
	"github.com/WillyV3/cp3/internal/mcp"
)

// MCPState is what the cp3 MCP server recorded for its Claude session, when
// one is running in the process tree above us.
type MCPState struct {
	Claimed   string `json:"claimed"`
	Wanted    string `json:"wanted"`
	NoChannel bool   `json:"noChannel"`
}

// Session is who a cp3 process is, seen from inside a Claude Code session.
type Session struct {
	// Name is the identity a cp3 command run here sends as: the MCP server's
	// claimed name when there is one, else the directory identity.
	Name string `json:"name"`
	// Source says where Name came from: "mcp", or ResolveIdentity's own
	// "env", "file", "default" (or "" when there is no identity at all).
	Source  string `json:"source"`
	Machine string `json:"machine"`
	Cwd     string `json:"cwd"`
	// MCP is set when the cp3 MCP server is part of this session. A mod must
	// then leave the name and the inbox to it, or two subscribers split one
	// inbox and the second gets a machine-suffixed twin name.
	MCP *MCPState `json:"mcp,omitempty"`
	// ChannelFlag is true when the Claude process was launched with
	// --dangerously-load-development-channels (how `cp3 run` starts it): the
	// MCP server's channel delivery is displayed. Without it, and without the
	// MCP state yet (the server connects after the session starts), a mod
	// still knows an MCP server is coming.
	ChannelFlag bool `json:"channelFlag"`
}

// Tree is the process-tree access Describe needs; a var so tests can fake it.
var Tree = struct {
	ParentOf func(pid int) int
	Cmdline  func(pid int) string
	State    func(pid int) (claimed, wanted string, noChannel bool)
}{
	ParentOf: parentOf,
	Cmdline:  cmdline,
	State: func(pid int) (string, string, bool) {
		st := mcp.ReadState(pid)
		return st.Claimed, st.Wanted, st.NoChannel
	},
}

// Describe walks up from pid (normally our parent) looking for the Claude
// session we run under: cp3 <- bash <- claude is the usual chain, four hops
// covers every wrapper seen so far. The MCP server keys its state by the
// Claude pid, so a hit there is this session's name. The chain is also where
// the channel flag shows, on the Claude process's own command line.
func Describe(pid int, cwd string) Session {
	s := Session{Cwd: cwd}
	s.Machine, _ = os.Hostname()
	for range 4 {
		if pid <= 1 {
			break
		}
		if claimed, wanted, noChannel := Tree.State(pid); claimed != "" || wanted != "" {
			s.MCP = &MCPState{Claimed: claimed, Wanted: wanted, NoChannel: noChannel}
		}
		if strings.Contains(Tree.Cmdline(pid), "--dangerously-load-development-channels") {
			s.ChannelFlag = true
		}
		if s.MCP != nil && s.MCP.Claimed != "" {
			s.Name, s.Source = s.MCP.Claimed, "mcp"
			return s
		}
		pid = Tree.ParentOf(pid)
	}
	s.Name, s.Source = peers.ResolveIdentity(cwd, "")
	return s
}

func parentOf(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}

func cmdline(pid int) string {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return string(out)
}
