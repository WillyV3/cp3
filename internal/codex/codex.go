// Package codex bridges the peer network into a codex session via the codex
// app-server (JSON-RPC over stdio, newline-delimited).
//
// Protocol verified live against codex-cli 0.142: initialize with clientInfo,
// thread/start returns result.thread.id, turn/start{threadId,input} runs a
// turn, turn/steer{threadId,input,expectedTurnId} steers an active one, and
// turn/started / turn/completed notifications carry the nested turn object.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

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
func Run() {
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
	name, _ := peers.ResolveIdentity(cwd, "")
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
func (s *appServer) deliver(m peers.Message) error {
	s.mu.Lock()
	thread, turn := s.thread, s.turn
	s.mu.Unlock()
	text := fmt.Sprintf("[peer message from %s — to reply, run: cp3 send --to %s \"...\"]\n%s", m.From, m.From, m.Content)
	input := []map[string]any{{"type": "text", "text": text}}
	if turn != "" {
		_, err := s.request("turn/steer", map[string]any{"threadId": thread, "input": input, "expectedTurnId": turn})
		return err
	}
	_, err := s.request("turn/start", map[string]any{"threadId": thread, "input": input})
	return err
}
