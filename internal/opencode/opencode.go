// cp3-opencode — bridges the claude-peers NATS network to a running opencode
// server: each inbound peer message is injected as a steered prompt into one
// opencode session. Same wall as every adapter — it knows only peers.Client.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	peers "github.com/WillyV3/cp3"
	"github.com/WillyV3/cp3/internal/boot"
	"github.com/WillyV3/cp3/internal/bridge"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var (
	ocURL  = env("OPENCODE_URL", "http://127.0.0.1:4096")
	// 120s, not 30s: verified live that a machine's FIRST-EVER `opencode serve`
	// pays a one-time provider-package install cost during the FIRST
	// /api/session call — the TCP port accepts connections immediately
	// (serverUp is not enough to prove the server can do real work yet), but
	// createSession hit the old 30s ceiling and failed with a deadline
	// exceeded; a second server on the same warmed machine created a session
	// in 60ms. Exactly the case a fresh-box one-word start must survive.
	client = &http.Client{Timeout: 120 * time.Second}
)

// createSession opens one opencode session. OPENCODE_MODEL, if set, pins
// providerID/id explicitly (opencode raises ModelNotSelectedError at turn
// without ANY model ever set on the session). Left unset, the model field is
// omitted entirely so the server's own configured default provider applies —
// a hardcoded default here was wrong on every box whose opencode.json names a
// different provider (e.g. inspiron's hfl/vllm/*, no opencode-go/* models at all).
func createSession(ctx context.Context) (string, error) {
	payload := map[string]any{}
	if m := os.Getenv("OPENCODE_MODEL"); m != "" {
		provider, id, _ := strings.Cut(m, "/")
		payload["model"] = map[string]string{"providerID": provider, "id": id}
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", ocURL+"/api/session", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Data struct{ ID string } `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Data.ID == "" {
		return "", fmt.Errorf("opencode returned no session id (status %d)", resp.StatusCode)
	}
	return out.Data.ID, nil
}

// verifySession confirms an existing opencode session id is real before the
// bridge commits to it — a typo'd or stale id would otherwise surface as a
// confusing 404 on the first inject() instead of a clear failure at startup.
func verifySession(ctx context.Context, session string) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", ocURL+"/api/session/"+session, nil)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("opencode session %s: status %d", session, resp.StatusCode)
	}
	return nil
}

// resolveSession picks the session inject() will post into. OPENCODE_SESSION_ID
// targets an EXISTING session — the interactive TUI's own live conversation,
// so peer messages land where a human is actually looking, matching the
// Claude Code experience of messages appearing in the session you're in.
// Unset (the default) keeps the original behavior: a fresh, dedicated session
// nobody else is watching.
func resolveSession(ctx context.Context) (string, error) {
	if id := os.Getenv("OPENCODE_SESSION_ID"); id != "" {
		if err := verifySession(ctx, id); err != nil {
			return "", fmt.Errorf("OPENCODE_SESSION_ID=%s: %w", id, err)
		}
		return id, nil
	}
	return createSession(ctx)
}

// serverUp is a plain TCP dial against ocURL's host:port — cheap enough to
// call before every Run and inside waitForServer's poll loop, no HTTP round
// trip needed just to know "is anything listening".
func serverUp(ctx context.Context) bool {
	u, err := url.Parse(ocURL)
	if err != nil {
		return false
	}
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// spawnOpencodeServe starts `opencode serve` bound to ocURL's own host:port,
// logging to the same ~/.local/share/cp3 dir cp3 serve itself uses. Local
// only — a remote OPENCODE_URL with nothing listening is the operator's news,
// same reasoning as boot.Connect's own auto-serve for cp3's NATS server.
// Deliberately NOT detached (unlike boot.spawnServe): this instance belongs
// to one cp3-opencode process and must die when it does, not survive it.
func spawnOpencodeServe() (*exec.Cmd, error) {
	u, err := url.Parse(ocURL)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" {
		return nil, fmt.Errorf("OPENCODE_URL %s is not local — start opencode serve there yourself", ocURL)
	}
	port := u.Port()
	if port == "" {
		port = "4096"
	}
	if err := os.MkdirAll(boot.DataDir(), 0o700); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(boot.DataDir(), "opencode-serve.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("opencode", "serve", "--hostname", host, "--port", port)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	go func() { cmd.Wait(); logf.Close() }() // reap; also closes the log fd once it exits
	return cmd, nil
}

// waitForServer polls serverUp until it answers or timeout/ctx elapses.
func waitForServer(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if serverUp(ctx) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

func inject(ctx context.Context, session, actual string, m peers.Message) error {
	delivery := "queue"
	if m.DeliverAs == "steer" {
		delivery = "steer"
	}
	body, _ := json.Marshal(map[string]any{
		"prompt":   map[string]string{"text": bridge.ReplyHint(actual, m.From) + "\n" + m.Content},
		"delivery": delivery,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", ocURL+"/api/session/"+session+"/prompt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("opencode prompt %d: %.200s", resp.StatusCode, b)
	}
	return nil
}

// Run is the one-word peer start: ensure an opencode server is up (spawning
// one if nothing's listening locally), bridge peer messages into a session,
// and put the human in front of it via `opencode attach` in the foreground —
// one command, one terminal, ctrl-c (or quitting the TUI) ends everything.
// --headless skips the attach step for the old dedicated-sidecar deployment
// (a server nobody's watching, e.g. a fleet box driven only by peers).
// Identity resolves the same way every other adapter does (peers.ResolveIdentity):
// --as > CLAUDE_PEERS_AGENT > .cp3-peer-name file > dir name.
func Run(args []string) {
	fs := flag.NewFlagSet("opencode", flag.ExitOnError)
	as := fs.String("as", "", "peer name (overrides CLAUDE_PEERS_AGENT / .cp3-peer-name)")
	headless := fs.Bool("headless", false, "bridge only — don't spawn/attach the interactive TUI")
	fs.Parse(args)

	cwd, _ := os.Getwd()
	name, _ := peers.ResolveIdentity(cwd, *as)
	if name == "" {
		fmt.Fprintln(os.Stderr, "[cp3-opencode] set --as, CLAUDE_PEERS_AGENT, or run from a dir with a .cp3-peer-name file")
		os.Exit(1)
	}
	c, err := boot.Connect() // auto-serves a local NATS network if none is up
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-opencode] connect:", err)
		os.Exit(1)
	}
	defer c.Close()

	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	var serveCmd *exec.Cmd
	stopServe := func() {
		if serveCmd != nil && serveCmd.Process != nil {
			serveCmd.Process.Kill()
		}
	}
	if !serverUp(ctx) {
		cmd, err := spawnOpencodeServe()
		if err != nil {
			fmt.Fprintln(os.Stderr, "[cp3-opencode] spawn opencode serve:", err)
			os.Exit(1)
		}
		serveCmd = cmd
		if !waitForServer(ctx, 10*time.Second) {
			fmt.Fprintln(os.Stderr, "[cp3-opencode] opencode serve did not come up at", ocURL)
			stopServe()
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "[cp3-opencode] spawned opencode serve at", ocURL)
	}

	session, err := resolveSession(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-opencode] session:", err)
		stopServe()
		os.Exit(1)
	}
	joined := ""
	if os.Getenv("OPENCODE_SESSION_ID") != "" {
		joined = " (joined existing)"
	}
	fmt.Fprintf(os.Stderr, "[cp3-opencode] %s on session %s%s at %s\n", name, session, joined, ocURL)

	host, _ := os.Hostname()
	p := peers.Peer{Agent: name, Machine: host, Cwd: cwd, Session: fmt.Sprintf("oc-%d", os.Getpid())}
	go func() {
		if err := bridge.Run(ctx, c, p, func(actual string, m peers.Message) error { return inject(ctx, session, actual, m) }); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "[cp3-opencode] bridge:", err)
		}
	}()

	if *headless {
		<-ctx.Done()
		stopServe()
		return
	}

	attach := exec.Command("opencode", "attach", ocURL, "--session", session)
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := attach.Run()
	cancel()
	stopServe()
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "[cp3-opencode] attach:", runErr)
		os.Exit(1)
	}
}
