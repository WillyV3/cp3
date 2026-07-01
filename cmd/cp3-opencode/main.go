// cp3-opencode — bridges the claude-peers NATS network to a running opencode
// server: each inbound peer message is injected as a steered prompt into one
// opencode session. Same wall as every adapter — it knows only peers.Client.
package main

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

	peers "github.com/WillyV3/claude-peers-v3"
	"github.com/WillyV3/claude-peers-v3/internal/bridge"
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

func inject(ctx context.Context, session string, m peers.Message) error {
	delivery := "queue"
	if m.DeliverAs == "steer" {
		delivery = "steer"
	}
	body, _ := json.Marshal(map[string]any{
		"prompt":   map[string]string{"text": fmt.Sprintf("[peer %s] %s", m.From, m.Content)},
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

func main() {
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

	session, err := createSession(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-opencode] create session:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[cp3-opencode] %s on session %s at %s\n", name, session, ocURL)

	cwd, _ := os.Getwd()
	host, _ := os.Hostname()
	p := peers.Peer{Agent: name, Machine: host, Cwd: cwd, Session: fmt.Sprintf("oc-%d", os.Getpid())}
	if err := bridge.Run(ctx, c, p, func(m peers.Message) error { return inject(ctx, session, m) }); err != nil {
		fmt.Fprintln(os.Stderr, "[cp3-opencode] run:", err)
		os.Exit(1)
	}
}
