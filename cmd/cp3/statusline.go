package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	peers "github.com/WillyV3/claude-peers-v3"
)

// statusLine renders the one compact line a coding-agent statusline shows.
// Pure: given the peer list (or an error) it produces the line — testable
// without NATS. v3 presence semantics: in the KV = online, expired = gone,
// so there is no "offline" state like v2 had.
//
//	no name        -> "○ peers: no name set"
//	fetch error    -> "○ peers: nats down"
//	not registered -> "○ peers: <name> · not registered"
//	registered     -> "● peers: <name> · <N> online"
//
// Other peers sharing this cwd append "· ⚠ also here: <names>".
func statusLine(name, cwd string, list []peers.Peer, err error) string {
	if name == "" {
		return "○ peers: no name set"
	}
	if err != nil {
		return "○ peers: nats down"
	}
	found := false
	var here []string
	for _, p := range list {
		if p.Agent == name {
			found = true
		} else if cwd != "" && p.Cwd == cwd {
			here = append(here, p.Agent)
		}
	}
	var line string
	if !found {
		line = fmt.Sprintf("○ peers: %s · not registered", name)
	} else {
		line = fmt.Sprintf("● peers: %s · %d online", name, len(list))
	}
	if len(here) > 0 {
		line += " · ⚠ also here: " + strings.Join(here, ", ")
	}
	return line
}

// agentNameFromEnv resolves this session's peer name the same way cp3-mcp
// does: CLAUDE_PEERS_AGENT env, then a .claude-peers-agent file in the cwd.
func agentNameFromEnv() string {
	if v := os.Getenv("CLAUDE_PEERS_AGENT"); v != "" {
		return v
	}
	if b, err := os.ReadFile(".claude-peers-agent"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// drainStdin discards piped stdin (Claude pumps session JSON at statusline
// commands; an undrained pipe can block the host). TTY stdin is left alone.
func drainStdin() {
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		io.Copy(io.Discard, os.Stdin)
	}
}

// cmdStatusLine prints the line and ALWAYS exits 0 — a statusline must never
// error out its host. The whole NATS fetch runs behind a 700ms wall.
func cmdStatusLine(args []string) {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // swallow flag errors too
	name := fs.String("name", "", "peer name (defaults to CLAUDE_PEERS_AGENT / .claude-peers-agent)")
	_ = fs.Parse(args)
	drainStdin()
	n := *name
	if n == "" {
		n = agentNameFromEnv()
	}
	cwd, _ := os.Getwd()

	done := make(chan string, 1)
	go func() {
		c, err := peers.ConnectFromEnv()
		if err != nil {
			done <- statusLine(n, cwd, nil, err)
			return
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		defer cancel()
		list, err := c.Peers(ctx)
		done <- statusLine(n, cwd, list, err)
	}()
	select {
	case line := <-done:
		fmt.Println(line)
	case <-time.After(700 * time.Millisecond):
		fmt.Println(statusLine(n, cwd, nil, context.DeadlineExceeded))
	}
}
