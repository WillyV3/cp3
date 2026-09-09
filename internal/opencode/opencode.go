// cp3-opencode — bridges the claude-peers NATS network to a running opencode
// server: each inbound peer message is injected as a steered prompt into one
// opencode session. Same wall as every adapter — it knows only peers.Client.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	peers "github.com/WillyV3/cp3"
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
	client = &http.Client{Timeout: 30 * time.Second}
)

// createSession opens one opencode session with the model pre-selected — the
// model MUST be set at create or opencode raises ModelNotSelectedError at turn.
func createSession(ctx context.Context) (string, error) {
	provider, id, _ := strings.Cut(env("OPENCODE_MODEL", "opencode-go/glm-5.2"), "/")
	body, _ := json.Marshal(map[string]any{"model": map[string]string{"providerID": provider, "id": id}})
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

// Run bridges peer messages into an opencode server session until interrupted.
func Run() {
	name := env("CLAUDE_PEERS_AGENT", os.Getenv("PEER_NAME"))
	if name == "" {
		fmt.Fprintln(os.Stderr, "[cp3-opencode] set CLAUDE_PEERS_AGENT or PEER_NAME")
		os.Exit(1)
	}
	c, err := peers.ConnectFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-opencode] connect:", err)
		os.Exit(1)
	}
	defer c.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	session, err := resolveSession(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-opencode] session:", err)
		os.Exit(1)
	}
	joined := ""
	if os.Getenv("OPENCODE_SESSION_ID") != "" {
		joined = " (joined existing)"
	}
	fmt.Fprintf(os.Stderr, "[cp3-opencode] %s on session %s%s at %s\n", name, session, joined, ocURL)

	cwd, _ := os.Getwd()
	host, _ := os.Hostname()
	p := peers.Peer{Agent: name, Machine: host, Cwd: cwd, Session: fmt.Sprintf("oc-%d", os.Getpid())}
	if err := bridge.Run(ctx, c, p, func(actual string, m peers.Message) error { return inject(ctx, session, actual, m) }); err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-opencode] run:", err)
		os.Exit(1)
	}
}
