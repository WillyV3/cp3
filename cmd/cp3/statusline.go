package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	peers "github.com/WillyV3/cp3"
)

// statusLine renders the one compact line a coding-agent statusline shows.
// Pure: given the peer list, this agent's undrained inbox count, and any fetch
// error, it produces the line — testable without NATS. Color is always
// emitted (the consumer is Claude's statusline renderer, which reads ANSI
// from a pipe); NO_COLOR still disables via paint.
//
//	no name        -> dim  "○ peers: —"
//	fetch error    -> red  "○ peers: down"
//	not registered -> yell "○ peers: <name> · not registered"
//	registered     -> green"● peers: <name> · <N> online"
//
// Extras: "✉N" (yellow) when N messages sit undrained in this agent's inbox —
// something is queued and nothing is consuming; "⚠ also here: x" (red) when
// other peers share this cwd. v3 presence: in the KV = online, expired = gone.
func statusLine(name, cwd string, list []peers.Peer, pending uint64, err error) string {
	const on = true
	if name == "" {
		return paint(on, cDim, "○ peers: —")
	}
	if err != nil {
		return paint(on, cRed, "○ peers: down")
	}
	found := false
	var here []string
	for _, p := range list {
		if p.Agent == name {
			found = true
		} else if cwd != "" && p.Cwd == cwd {
			label := p.Agent
			if p.Machine != "" {
				label += "@" + p.Machine // same path on another box = synced-dir twin
			}
			here = append(here, label)
		}
	}
	var line string
	if !found {
		line = paint(on, cYellow, "○") + " peers: " + paint(on, cDim, name+" · not registered")
	} else {
		line = paint(on, cGreen, "●") + " peers: " + paint(on, cBold, name) +
			paint(on, cDim, " · ") + paint(on, cCyan, fmt.Sprintf("%d online", len(list)))
	}
	if pending > 0 {
		line += paint(on, cDim, " · ") + paint(on, cYellow, fmt.Sprintf("✉%d", pending))
	}
	if len(here) > 0 {
		line += paint(on, cDim, " · ") + paint(on, cRed, "⚠ also here: "+strings.Join(here, ", "))
	}
	return line
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
	name := fs.String("name", "", "peer name (defaults to this dir's identity)")
	_ = fs.Parse(args)
	drainStdin()
	cwd, _ := os.Getwd()
	n, _ := peers.ResolveIdentity(cwd, *name)

	done := make(chan string, 1)
	go func() {
		c, err := peers.ConnectFromEnv()
		if err != nil {
			done <- statusLine(n, cwd, nil, 0, err)
			return
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		defer cancel()
		list, err := c.Peers(ctx)
		var pending uint64
		if err == nil {
			if cs, cerr := c.Consumers(ctx); cerr == nil {
				for _, s := range cs {
					if s.Name == "inbox-"+n {
						pending = s.Pending
					}
				}
			}
		}
		done <- statusLine(n, cwd, list, pending, err)
	}()
	select {
	case line := <-done:
		fmt.Println(line)
	case <-time.After(700 * time.Millisecond):
		fmt.Println(statusLine(n, cwd, nil, 0, context.DeadlineExceeded))
	}
}
