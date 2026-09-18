// Package codex makes a codex session a peer. Two modes:
//
// TUI (default): the interactive codex TUI is the peer. Built only on codex's
// public CLI, verified live on codex-cli 0.155.0 (2026-09-18):
//   - cp3 owns a `codex app-server --listen ws://127.0.0.1:PORT`. A plain
//     `codex` TUI runs its own private in-process server that nothing outside
//     can reach, so injection needs a server both sides attach to. (The shared
//     "daemon" codex documents needs OpenAI's standalone installer — not used.)
//   - `codex exec --json` runs a one-line briefing turn. That yields the thread
//     id AND writes the rollout to disk — a thread with no turns cannot be
//     resumed ("no rollout found"), so the briefing is load-bearing.
//   - `codex resume <id> --remote ws://...` is the foreground TUI.
//   - each peer message is `codex queue --remote ... --thread <id>`: renders
//     in the TUI and runs a turn (queued behind an active turn, not steered).
//
// --headless: no TUI; drives `codex app-server` over stdio JSON-RPC and can
// steer an active turn. Protocol verified live against codex-cli 0.142 and
// re-run on 0.155.0 (2026-09-18): initialize with clientInfo,
// thread/start returns result.thread.id, turn/start{threadId,input} runs a
// turn, turn/steer{threadId,input,expectedTurnId} steers an active one, and
// turn/started / turn/completed notifications carry the nested turn object.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	peers "github.com/WillyV3/cp3"
	"github.com/WillyV3/cp3/internal/boot"
	"github.com/WillyV3/cp3/internal/bridge"
)

type appServer struct {
	stdin io.Writer

	mu      sync.Mutex
	nextID  int
	pending map[int]chan json.RawMessage
	thread  string // thread id from thread/start
	turn    string // active turn id ("" when idle)
}

// Run connects the network and the app-server; blocks until interrupted.
// --as pins the peer name; unset, identity falls through peers.ResolveIdentity
// same as every other adapter (env, .cp3-peer-name file, dir name) — opening a
// new peer on a new box never needs more than `cp3 codex` or `cp3 codex --as X`.
func Run(args []string) {
	fs := flag.NewFlagSet("codex", flag.ExitOnError)
	as := fs.String("as", "", "peer name (overrides CLAUDE_PEERS_AGENT / .cp3-peer-name)")
	headless := fs.Bool("headless", false, "no TUI — drive codex app-server over stdio (can steer active turns)")
	fs.Parse(args)
	if !*headless {
		runTUI(*as, fs.Args())
		return
	}

	c, err := boot.Connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3 codex] connect:", err)
		os.Exit(1)
	}
	defer c.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := c.Setup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "[cp3 codex] setup:", err)
		os.Exit(1)
	}

	cmd := exec.CommandContext(ctx, "codex", "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3 codex]", err)
		os.Exit(1)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3 codex]", err)
		os.Exit(1)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "[cp3 codex] spawn codex app-server:", err)
		os.Exit(1)
	}

	srv := &appServer{stdin: stdin, pending: map[int]chan json.RawMessage{}}
	go srv.readLoop(stdout)

	if _, err := srv.request("initialize", map[string]any{
		"clientInfo": map[string]any{"name": "cp3", "title": "cp3 peer bridge", "version": "0.1.0"},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "[cp3 codex] initialize:", err)
		os.Exit(1)
	}
	res, err := srv.request("thread/start", map[string]any{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3 codex] thread/start:", err)
		os.Exit(1)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(res, &started); err != nil || started.Thread.ID == "" {
		fmt.Fprintln(os.Stderr, "[cp3 codex] thread/start: no thread id in response")
		os.Exit(1)
	}
	srv.mu.Lock()
	srv.thread = started.Thread.ID
	srv.mu.Unlock()

	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	name, _ := peers.ResolveIdentity(cwd, *as)
	if name == "" {
		name = "codex-agent"
	}
	p := peers.Peer{Agent: name, Machine: host, Cwd: cwd, Session: fmt.Sprintf("codex-%d", os.Getpid())}
	fmt.Fprintf(os.Stderr, "[cp3 codex] %s online, codex thread %s\n", name, started.Thread.ID)

	err = bridge.Run(ctx, c, p, srv.deliver)
	stdin.Close()
	cmd.Wait()
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "[cp3 codex]", err)
		os.Exit(1)
	}
}

// readLoop consumes newline-delimited JSON-RPC from the app-server: tracks
// turn state from notifications, resolves pending requests by id.
func (s *appServer) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
			Params struct {
				Turn struct {
					ID string `json:"id"`
				} `json:"turn"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		s.mu.Lock()
		switch msg.Method {
		case "turn/started":
			s.turn = msg.Params.Turn.ID
		case "turn/completed", "turn/aborted", "turn/failed":
			s.turn = ""
		}
		if msg.ID != nil {
			if ch, ok := s.pending[*msg.ID]; ok {
				delete(s.pending, *msg.ID)
				if msg.Error != nil {
					// Deliver the error as a JSON string so request can surface it.
					b, _ := json.Marshal(map[string]string{"__error": msg.Error.Message})
					ch <- b
				} else {
					ch <- msg.Result
				}
			}
		}
		s.mu.Unlock()
	}
}

// request writes a JSON-RPC request and waits for its response result.
func (s *appServer) request(method string, params map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan json.RawMessage, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	res := <-ch
	var e struct {
		Err string `json:"__error"`
	}
	if json.Unmarshal(res, &e) == nil && e.Err != "" {
		return nil, fmt.Errorf("%s: %s", method, e.Err)
	}
	return res, nil
}

// deliver injects one peer message: steer the active turn, else start one.
func (s *appServer) deliver(actual string, m peers.Message) error {
	s.mu.Lock()
	thread, turn := s.thread, s.turn
	s.mu.Unlock()
	text := bridge.ReplyHint(actual, m.From) + "\n" + m.Content
	input := []map[string]any{{"type": "text", "text": text}}
	if turn != "" {
		_, err := s.request("turn/steer", map[string]any{"threadId": thread, "input": input, "expectedTurnId": turn})
		return err
	}
	_, err := s.request("turn/start", map[string]any{"threadId": thread, "input": input})
	return err
}

// --- TUI mode ---------------------------------------------------------------

var codexBin = "codex" // var so tests can point at a fake binary

// briefing is the first turn. It is deliberately name-free: the peer name is
// claimed after this runs and may get a machine suffix on collision, and every
// delivered message carries the exact reply command anyway.
const briefing = "You are a peer on the cp3 network: other coding agents can message you, and their messages arrive here as user turns. Each one starts with the exact `cp3 send` shell command to reply with — run it to answer. Acknowledge with the single word READY."

// threadFromExec pulls the thread id out of `codex exec --json` output.
func threadFromExec(jsonl []byte) string {
	for _, line := range strings.Split(string(jsonl), "\n") {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "thread.started" && ev.ThreadID != "" {
			return ev.ThreadID
		}
	}
	return ""
}

// queue injects one peer message into the live thread.
func queue(ctx context.Context, remote, thread, actual string, m peers.Message) error {
	text := bridge.ReplyHint(actual, m.From) + "\n" + m.Content
	out, err := exec.CommandContext(ctx, codexBin, "queue", "--remote", remote, "--thread", thread, "--message", text).CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex queue: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// serving reports whether the app-server answers HTTP yet. TCP-open is not
// enough (the opencode lesson): any HTTP response at all means it is serving.
func serving(addr string) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr + "/")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func runTUI(as string, tuiArgs []string) {
	fail := func(a ...any) {
		fmt.Fprintln(os.Stderr, append([]any{"[cp3 codex]"}, a...)...)
		os.Exit(1)
	}
	cwd, _ := os.Getwd()
	name, _ := peers.ResolveIdentity(cwd, as)
	if name == "" {
		fail("set --as, CLAUDE_PEERS_AGENT, or run from a dir with a .cp3-peer-name file")
	}
	c, err := boot.Connect()
	if err != nil {
		fail("connect:", err)
	}
	defer c.Close()
	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	// A free loopback port per peer, so several codex peers coexist.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail("pick port:", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	remote := "ws://" + addr

	logPath := filepath.Join(boot.DataDir(), "codex-app-server-"+name+".log")
	os.MkdirAll(filepath.Dir(logPath), 0o700)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fail("server log:", err)
	}
	srv := exec.Command(codexBin, "app-server", "--listen", remote)
	srv.Stdout, srv.Stderr = logf, logf
	if err := srv.Start(); err != nil {
		fail("spawn codex app-server:", err)
	}
	stopSrv := func() { srv.Process.Kill(); srv.Wait() }

	fmt.Fprintln(os.Stderr, "[cp3 codex] starting", name, "— briefing turn…")
	out, err := exec.CommandContext(ctx, codexBin, "exec", "--json", "--skip-git-repo-check", briefing).Output()
	thread := threadFromExec(out)
	if thread == "" {
		stopSrv()
		detail := ""
		if ee, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		fail("briefing turn gave no thread id (is codex signed in / a provider configured?):", err, detail)
	}

	ready := false
	for i := 0; i < 100 && !ready; i++ { // the briefing usually outlasts startup; 15s ceiling
		if ready = serving(addr); !ready {
			time.Sleep(150 * time.Millisecond)
		}
	}
	if !ready {
		stopSrv()
		b, _ := os.ReadFile(logPath)
		fail("codex app-server did not come up at", remote, "\n"+string(b))
	}

	host, _ := os.Hostname()
	p := peers.Peer{Agent: name, Machine: host, Cwd: cwd, Session: fmt.Sprintf("codex-%d", os.Getpid())}
	go func() {
		err := bridge.Run(ctx, c, p, func(actual string, m peers.Message) error { return queue(ctx, remote, thread, actual, m) })
		if err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "[cp3 codex] bridge:", err)
		}
	}()

	tui := exec.Command(codexBin, append([]string{"resume", thread, "--remote", remote}, tuiArgs...)...)
	tui.Stdin, tui.Stdout, tui.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := tui.Run()
	cancel()
	stopSrv()
	if ee, ok := runErr.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	} else if runErr != nil {
		fail("codex resume:", runErr)
	}
}
