package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	peers "github.com/WillyV3/cp3"
	"github.com/WillyV3/cp3/internal/mcp"
)

// statusLine renders the one compact line a coding-agent statusline shows.
// Pure and testable without NATS.
//
// Identity truth: the peer whose Session matches ours IS us — display its
// CLAIMED name (which may be a collision fallback like "jim-omarchy"), never
// a configured name presence doesn't hold. When claimed ≠ configured, that
// identity drift is the one thing worth a ⚠. Co-located peers are neutral
// info ("with: x"), not a warning — agents may share a directory on purpose.
//
// Quiet when normal, loud only when abnormal (ANSI 0-15 throughout):
//
//	no identity     -> ""   (wrappers hide the empty segment)
//	fetch error     -> red  "○ peers down"
//	not registered  -> yell "○ <configured> · not registered"
//	registered      -> green"● <claimed> · <N> peers"   (the quiet normal)
//	claimed drifted -> yell " · ⚠ wanted <configured>"
//	undrained inbox -> yell " · ✉N"
//	co-located      -> soft " · with a@machine, b"  (yellow names, not a fault)
func statusLine(configured, claimed, session, cwd string, list []peers.Peer, pending uint64, err error) string {
	const on = true
	if err != nil {
		return paint(on, cRed, "○ peers down")
	}
	var self *peers.Peer
	for i := range list {
		// The session's own MCP state (claimed) is the strongest key, then
		// the session id, and only then the configured-name guess.
		if claimed != "" && list[i].Agent == claimed {
			self = &list[i]
			break
		}
		if session != "" && list[i].Session == session {
			self = &list[i]
			break
		}
	}
	if self == nil && claimed == "" && configured != "" {
		for i := range list {
			if list[i].Agent == configured {
				self = &list[i]
				break
			}
		}
	}
	shown := configured
	if claimed != "" {
		shown = claimed
	}
	if self != nil {
		shown = self.Agent
	}
	if shown == "" {
		return "" // no identity: emit nothing, wrappers hide the empty segment
	}

	var with []string
	for i := range list {
		p := list[i]
		if self != nil && p.Agent == self.Agent {
			continue // me
		}
		if cwd != "" && p.Cwd == cwd {
			label := p.Agent
			if p.Machine != "" {
				label += "@" + p.Machine
			}
			with = append(with, label)
		}
	}

	var line string
	if self == nil {
		line = paint(on, cYellow, "○") + " " + paint(on, cBold, shown) + paint(on, cDim, " · not registered")
	} else {
		line = paint(on, cGreen, "●") + " " + paint(on, cBold, shown) +
			paint(on, cDim, fmt.Sprintf(" · %d peers", len(list)))
		if configured != "" && shown != configured {
			line += paint(on, cDim, " · ") + paint(on, cYellow, "⚠ wanted "+configured)
		}
	}
	if pending > 0 {
		line += paint(on, cDim, " · ") + paint(on, cYellow, fmt.Sprintf("✉%d", pending))
	}
	if len(with) > 0 {
		// Soft yellow noticing aid — co-location is legitimate, just worth seeing.
		line += paint(on, cDim, " · with ") + paint(on, cYellow, strings.Join(with, ", "))
	}
	return line
}

// readStdin drains piped stdin (Claude pumps session JSON at statusline
// commands; an undrained pipe can block the host) and returns whatever was
// read. TTY stdin is left alone.
func readStdin() []byte {
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		b, _ := io.ReadAll(os.Stdin)
		return b
	}
	return nil
}

// cmdStatusLine prints the line and ALWAYS exits 0 — a statusline must never
// error out its host. The whole NATS fetch runs behind a 700ms wall.
// The session id comes from --session, or from the "session_id" field of the
// statusline JSON Claude pipes on stdin.
func cmdStatusLine(args []string) {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // swallow flag errors too
	name := fs.String("name", "", "configured peer name (defaults to this dir's identity)")
	session := fs.String("session", "", "this session's id (for self-recognition; default: stdin session_id)")
	parent := fs.Int("parent", 0, "the claude process pid (wrappers pass $PPID; default: our own parent)")
	_ = fs.Parse(args)
	stdin := readStdin()
	sid := *session
	if sid == "" && len(stdin) > 0 {
		var in struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(stdin, &in) == nil {
			sid = in.SessionID
		}
	}
	cwd, _ := os.Getwd()
	// Identity is SESSION-bound, not directory-bound: the session's MCP server
	// records {claimed, wanted} keyed by the claude pid, and that beats
	// re-resolving from whatever directory the session has wandered into.
	// cwd-based resolution remains only for non-session contexts (no state file).
	ppid := *parent
	if ppid == 0 {
		ppid = os.Getppid()
	}
	st := mcp.ReadState(ppid)
	configured := st.Wanted
	claimed := st.Claimed
	if configured == "" && claimed == "" {
		configured, _ = peers.ResolveIdentity(cwd, *name)
	}

	done := make(chan string, 1)
	go func() {
		c, err := peers.ConnectFromEnv()
		if err != nil {
			done <- statusLine(configured, claimed, sid, cwd, nil, 0, err)
			return
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		defer cancel()
		list, err := c.Peers(ctx)
		// Pending tracks the name we actually display (claimed beats
		// configured), so the badge follows the real inbox.
		shown := configured
		if claimed != "" {
			shown = claimed
		} else if sid != "" {
			for _, p := range list {
				if p.Session == sid {
					shown = p.Agent
					break
				}
			}
		}
		var pending uint64
		if err == nil && shown != "" {
			if cs, cerr := c.Consumers(ctx); cerr == nil {
				for _, s := range cs {
					if s.Name == "inbox-"+shown {
						pending = s.Pending
					}
				}
			}
		}
		done <- statusLine(configured, claimed, sid, cwd, list, pending, err)
	}()
	select {
	case line := <-done:
		fmt.Println(line)
	case <-time.After(700 * time.Millisecond):
		fmt.Println(statusLine(configured, claimed, sid, cwd, nil, 0, context.DeadlineExceeded))
	}
}
