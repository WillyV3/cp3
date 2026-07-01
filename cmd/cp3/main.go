// cp3 — CLI for the claude-peers v3 NATS-native network.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	peers "github.com/WillyV3/cp3"
	"github.com/WillyV3/cp3/internal/boot"
	"github.com/WillyV3/cp3/internal/bridge"
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
	case "doctor":
		cmdDoctor(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "mcp":
		mcp.Run()
	case "opencode":
		opencode.Run()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cp3 <serve|send|version|peers|watch|register|subscribe|consumers|statusline|doctor|setup|run|mcp|opencode> [flags]")
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

func cmdConsumers(args []string) {
	fs := flag.NewFlagSet("consumers", flag.ExitOnError)
	fs.Parse(args)
	c := connect()
	defer c.Close()
	list, err := c.Consumers(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "consumers:", err)
		os.Exit(1)
	}
	now := time.Now()
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
		cwd, _ := os.Getwd()
		*from, _ = peers.ResolveIdentity(cwd, "")
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
	if err := c.Send(ctx, peers.Message{From: *from, To: *to, Content: content, DeliverAs: *mode}); err != nil {
		fmt.Fprintln(os.Stderr, "send:", err)
		os.Exit(1)
	}
	fmt.Printf("sent %s -> %s\n", *from, *to)
}

func cmdPeers(args []string) {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	fs.Parse(args)
	c := connect()
	defer c.Close()
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
					c.Heartbeat(ctx, *as)
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
			if err := c.Heartbeat(ctx, *agent); err != nil {
				fmt.Fprintln(os.Stderr, "heartbeat:", err)
			}
		}
	}
}
