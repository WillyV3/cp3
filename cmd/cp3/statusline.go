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
func statusLine(configured, claimed, session, cwd, machine string, list []peers.Peer, pending uint64, noChannel bool, err error) string {
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

	// A path is only unique WITHIN a machine. Every box in the fleet has a
	// /home/<user>, so comparing cwd alone made a session on another machine
	// read as sitting in your directory — "⚠ also here: jim" on a fresh box
	// where jim is 200 miles away. Same path here is a real double-edit
	// warning; same path elsewhere is fleet awareness, not a fault.
	var with []string
	var elsewhereOrder []string
	for i := range list {
		p := list[i]
		if self != nil && p.Agent == self.Agent {
			continue // me
		}
		if cwd == "" || p.Cwd != cwd {
			continue
		}
		if machine == "" || p.Machine == "" {
			// Can't tell which box this is: label it rather than assert
			// co-location, which is the assertion that was wrong.
			label := p.Agent
			if p.Machine != "" {
				label += "@" + p.Machine
			}
			with = append(with, label)
			continue
		}
		if p.Machine == machine {
			with = append(with, p.Agent)
			continue
		}
		elsewhereOrder = append(elsewhereOrder, p.Agent+" ("+p.Machine+")")
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
	if noChannel {
		// The one failure that looks completely healthy: presence green,
		// messages arriving, nothing ever shown. Say so in the line itself.
		line += paint(on, cDim, " · ") + paint(on, cRed, "✖ NO CHANNEL (relaunch: cp3 run)")
	}
	if pending > 0 {
		line += paint(on, cDim, " · ") + paint(on, cYellow, fmt.Sprintf("✉%d", pending))
	}
	if len(with) > 0 {
		// Soft yellow noticing aid — co-location is legitimate, just worth seeing.
		line += paint(on, cDim, " · with ") + paint(on, cYellow, strings.Join(with, ", "))
	}
	if len(elsewhereOrder) > 0 {
		// Dim, never a warning: you cannot double-edit across machines by
		// accident. The PATH is the reason the notice exists, so it stays;
		// the machine in parens is what makes "here" honest.
		line += paint(on, cDim, " · also in "+shortPath(cwd)+" — "+strings.Join(elsewhereOrder, ", "))
	}
	return line
}

// shortPath renders a path the way a human reads it: a home directory as
// ~user, something under this machine's home as ~/rest, anything else
// verbatim. Cross-machine paths keep THEIR owner's name (a mac peer shows
// ~williamvansickleiii), which is part of what disambiguates them.
func shortPath(p string) string {
	if p == "" {
		return p
	}
	// An exact home directory renders as ~user, never bare "~": this string
	// describes a path on someone ELSE'S machine, and "~" would read as the
	// viewer's own home.
	for _, root := range []string{"/home/", "/Users/"} {
		if strings.HasPrefix(p, root) {
			rest := strings.TrimPrefix(p, root)
			if user, tail, found := strings.Cut(rest, "/"); found {
				if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+"/") {
					return "~" + strings.TrimPrefix(p, home) // under my own home
				}
				return "~" + user + "/" + tail
			}
			return "~" + rest
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home+"/") {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
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
	machine := os.Getenv("CLAUDE_PEERS_MACHINE")
	if machine == "" {
		machine, _ = os.Hostname()
	}
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
			done <- statusLine(configured, claimed, sid, cwd, machine, nil, 0, st.NoChannel, err)
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
		done <- statusLine(configured, claimed, sid, cwd, machine, list, pending, st.NoChannel, err)
	}()
	select {
	case line := <-done:
		fmt.Println(line)
	case <-time.After(700 * time.Millisecond):
		fmt.Println(statusLine(configured, claimed, sid, cwd, machine, nil, 0, st.NoChannel, context.DeadlineExceeded))
	}
}
