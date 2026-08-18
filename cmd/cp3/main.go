// cp3 — CLI for the claude-peers v3 NATS-native network.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	peers "github.com/WillyV3/cp3"
	"github.com/WillyV3/cp3/internal/boot"
	"github.com/WillyV3/cp3/internal/bridge"
	"github.com/WillyV3/cp3/internal/codex"
	"github.com/WillyV3/cp3/internal/mcp"
	"github.com/WillyV3/cp3/internal/opencode"
)

func connect() *peers.Client {
	c, err := boot.Connect() // auto-serves a local network if none is up
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	return c
}

var version = "dev" // set by goreleaser ldflags

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "send":
		cmdSend(os.Args[2:])
	case "peers":
		cmdPeers(os.Args[2:])
	case "watch":
		cmdWatch(os.Args[2:])
	case "register":
		cmdRegister(os.Args[2:])
	case "subscribe":
		cmdSubscribe(os.Args[2:])
	case "setup":
		cmdSetup(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "consumers":
		cmdConsumers(os.Args[2:])
	case "statusline":
		cmdStatusLine(os.Args[2:])
	case "version":
		fmt.Println("cp3", version)
	case "prompt":
		cmdPrompt(os.Args[2:])
	case "doctor":
		cmdDoctor(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "mcp":
		mcp.Run()
	case "opencode":
		opencode.Run()
	case "codex":
		codex.Run()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cp3 <serve|send|version|prompt|peers|watch|register|subscribe|consumers|statusline|doctor|setup|run|mcp|opencode|codex> [flags]")
}

// consumerVerdict classifies a consumer's liveness — the check that would have
// caught the five dead v1 durables.
func consumerVerdict(cs peers.ConsumerStatus, now time.Time) string {
	switch {
	case cs.LastDelivery != nil && now.Sub(*cs.LastDelivery) < 2*time.Minute:
		return "active"
	case cs.Pending > 0:
		return "STALE" // backlog piling up, nobody draining
	default:
		return "idle"
	}
}

// heartbeatOrDie keeps presence fresh for a CLI holder (watch --as /
// register). Losing the name to another session is fatal-loud: a silent
// zombie holder is exactly the flapping bug this exists to prevent.
func heartbeatOrDie(ctx context.Context, c *peers.Client, agent, session string) {
	err := c.Heartbeat(ctx, agent, session)
	if err == nil {
		return
	}
	if errors.Is(err, peers.ErrNameLost) {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
	// expired/transient: best effort re-claim; loss stays fatal
	if _, cerr := c.Claim(ctx, peers.Peer{Agent: agent, Session: session}); cerr != nil && errors.Is(cerr, peers.ErrNameTaken) {
		fmt.Fprintln(os.Stderr, "fatal: name reclaimed by another session:", cerr)
		os.Exit(1)
	}
}

// reapable reports whether an inbox can be deleted without losing anything.
// Three conditions, all required: nobody attached (Waiting==0), nothing
// undelivered (Pending==0 — deleting a consumer discards its backlog, and
// inbox-sontara-web sat on a real message for 307h), and quiet longer than
// the age cutoff. Teardown is TTL-shaped on purpose: a durable inbox
// outliving its session IS the offline-delivery feature, so exit-time
// cleanup would delete the product.
func reapable(cs peers.ConsumerStatus, now time.Time, olderThan time.Duration) bool {
	if cs.Waiting > 0 || cs.Pending > 0 || cs.AckPending > 0 {
		return false
	}
	if cs.LastDelivery == nil {
		return true // created, never used
	}
	return now.Sub(*cs.LastDelivery) > olderThan
}

func cmdConsumers(args []string) {
	fs := flag.NewFlagSet("consumers", flag.ExitOnError)
	reap := fs.Bool("reap", false, "delete inboxes that are detached, empty, and idle past --older-than")
	olderThan := fs.Duration("older-than", 7*24*time.Hour, "idle cutoff for --reap")
	fs.Parse(args)
	c := connect()
	defer c.Close()
	list, err := c.Consumers(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "consumers:", err)
		os.Exit(1)
	}
	now := time.Now()
	if *reap {
		var gone, kept int
		for _, cs := range list {
			if !reapable(cs, now, *olderThan) {
				kept++
				continue
			}
			if err := c.DeleteInbox(context.Background(), cs.Name); err != nil {
				fmt.Fprintf(os.Stderr, "reap %s: %v\n", cs.Name, err)
				continue
			}
			fmt.Println("reaped", cs.Name)
			gone++
		}
		fmt.Printf("%d reaped, %d kept (attached, holding mail, or newer than %s)\n", gone, kept, *olderThan)
		return
	}
	tty := stdoutIsTTY()
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "CONSUMER\tPENDING\tACK-PENDING\tLAST-DELIVERY\tVERDICT")
	for _, cs := range list {
		last := "never"
		if cs.LastDelivery != nil {
			last = now.Sub(*cs.LastDelivery).Round(time.Second).String() + " ago"
		}
		verdict := consumerVerdict(cs, now)
		switch verdict { // last column only: ANSI would break tabwriter widths elsewhere
		case "active":
			verdict = paint(tty, cGreen, verdict)
		case "STALE":
			verdict = paint(tty, cRed, verdict)
		default:
			verdict = paint(tty, cDim, verdict)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", cs.Name, cs.Pending, cs.AckPending, last, verdict)
	}
	w.Flush()
}

// cmdSubscribe registers an agent and streams each inbound message to stdout as
// one JSON line — the sidecar interface for foreign-runtime adapters (e.g. the
// pi extension) that can't embed a NATS client but can read a subprocess.
func cmdSubscribe(args []string) {
	fs := flag.NewFlagSet("subscribe", flag.ExitOnError)
	agent := fs.String("agent", "", "agent name")
	machine := fs.String("machine", "", "machine")
	cwd := fs.String("cwd", "", "working directory")
	fs.Parse(args)
	if *agent == "" {
		fmt.Fprintln(os.Stderr, "subscribe: --agent required")
		os.Exit(2)
	}
	c := connect()
	defer c.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	enc := json.NewEncoder(os.Stdout)
	p := peers.Peer{Agent: *agent, Machine: *machine, Cwd: *cwd, Session: fmt.Sprintf("sub-%d", os.Getpid())}
	err := bridge.Run(ctx, c, p, func(m peers.Message) error {
		return enc.Encode(m) // one JSON message per line, flushed by Encode
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "subscribe:", err)
		os.Exit(1)
	}
}

func cmdSend(args []string) {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	from := fs.String("from", "", "sender agent (defaults to this dir's identity)")
	to := fs.String("to", "", "recipient agent")
	mode := fs.String("mode", "steer", "delivery mode: steer|queue")
	fs.Parse(args)
	content := strings.Join(fs.Args(), " ")
	if *from == "" {
		*from = callerIdentity()
	}
	if *from == "" || *to == "" || content == "" {
		fmt.Fprintln(os.Stderr, "send: --to and a message are required (--from resolved from identity)")
		os.Exit(2)
	}
	c := connect()
	defer c.Close()
	ctx := context.Background()
	if err := c.Setup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
	status, err := c.Send(ctx, peers.Message{From: *from, To: *to, Content: content, DeliverAs: *mode})
	if err != nil {
		fmt.Fprintln(os.Stderr, "send:", err)
		os.Exit(1)
	}
	fmt.Println(status.Human(*to))
	if status == peers.NoInbox {
		os.Exit(3) // scripts must be able to detect a message that went nowhere
	}
}

// callerIdentity resolves who is sending. A `cp3 send` run from inside a
// Claude session should be attributed to that SESSION, not to whatever
// directory the shell happens to sit in — jim sent from a shell in another
// project and the message arrived attributed to "wedding-tracker", a name no
// agent has ever held. Walk up the process tree (cp3 <- bash <- claude)
// looking for a session state file; fall back to directory identity for
// genuine non-session use like crons.
func callerIdentity() string {
	pid := os.Getppid()
	for range 4 {
		if pid <= 1 {
			break
		}
		if st := mcp.ReadState(pid); st.Claimed != "" {
			return st.Claimed
		}
		pid = parentOf(pid)
	}
	cwd, _ := os.Getwd()
	name, _ := peers.ResolveIdentity(cwd, "")
	return name
}

// parentOf returns a process's parent pid, or 0.
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

// waitFor blocks until agent appears in presence. Spawning a peer is
// asynchronous — tmux returns long before the session's MCP server finishes
// its handshake and claims a name — so scripts otherwise guess with `sleep N`
// and a guess that's too short reads as broken software. Polls the presence
// KV (a tiny read) rather than watching: no extra machinery, and a poll can't
// miss an agent that registered before we started looking.
func waitFor(c *peers.Client, agent string, timeout time.Duration) (peers.Peer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		list, err := c.Peers(ctx)
		if err == nil {
			for _, p := range list {
				if p.Agent == agent {
					return p, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return peers.Peer{}, fmt.Errorf("%s did not register within %s", agent, timeout)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func cmdPeers(args []string) {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	wait := fs.String("wait", "", "block until this agent registers; exit 0 when it does, non-zero on timeout")
	timeout := fs.Duration("timeout", 30*time.Second, "how long --wait waits")
	fs.Parse(args)
	c := connect()
	defer c.Close()
	if *wait != "" {
		p, err := waitFor(c, *wait, *timeout)
		if err != nil {
			fmt.Fprintln(os.Stderr, "peers --wait:", err)
			os.Exit(1)
		}
		fmt.Printf("%s registered on %s (%s)\n", p.Agent, p.Machine, p.Cwd)
		return
	}
	list, err := c.Peers(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "peers:", err)
		os.Exit(1)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tMACHINE\tCWD")
	for _, p := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\n", p.Agent, p.Machine, p.Cwd)
	}
	w.Flush()
}

func cmdWatch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	as := fs.String("as", "", "register this watcher in presence (visible in cp3 peers, dies visibly)")
	fromStart := fs.Bool("from-start", true, "replay retained history before live events")
	fs.Parse(args)
	c := connect()
	defer c.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := c.Setup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
	if *as != "" {
		// A long-lived consumer that registers presence is a consumer whose
		// death is visible within one TTL — the anti-graveyard rule.
		host, _ := os.Hostname()
		cwd, _ := os.Getwd()
		p := peers.Peer{Agent: *as, Machine: host, Cwd: cwd, Session: fmt.Sprintf("watch-%d", os.Getpid())}
		if err := c.Register(ctx, p); err != nil {
			fmt.Fprintln(os.Stderr, "watch: register:", err)
			os.Exit(1)
		}
		defer c.Deregister(context.Background(), *as)
		go func() {
			tk := time.NewTicker(15 * time.Second)
			defer tk.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tk.C:
					heartbeatOrDie(ctx, c, *as, p.Session)
				}
			}
		}()
	}
	err := c.Watch(ctx, *fromStart, func(e peers.Envelope) {
		fmt.Printf("%s  %-11s  %s  %s\n", time.UnixMilli(e.TS).Format("15:04:05"), e.Type, e.Actor, string(e.Data))
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "watch:", err)
		os.Exit(1)
	}
}

func cmdRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	agent := fs.String("agent", "", "agent name")
	machine := fs.String("machine", "", "machine")
	cwd := fs.String("cwd", "", "working directory")
	session := fs.String("session", "", "session id")
	fs.Parse(args)
	if *agent == "" {
		fmt.Fprintln(os.Stderr, "register: --agent required")
		os.Exit(2)
	}
	c := connect()
	defer c.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := c.Setup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
	p := peers.Peer{Agent: *agent, Machine: *machine, Cwd: *cwd, Session: *session}
	if err := c.Register(ctx, p); err != nil {
		fmt.Fprintln(os.Stderr, "register:", err)
		os.Exit(1)
	}
	fmt.Printf("registered %s — heartbeating every 15s (ctrl-c to leave)\n", *agent)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = c.Deregister(context.Background(), *agent)
			fmt.Printf("deregistered %s\n", *agent)
			return
		case <-ticker.C:
			heartbeatOrDie(ctx, c, *agent, p.Session)
		}
	}
}
