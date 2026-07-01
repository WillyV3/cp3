package peers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// Proves the token-auth path used by the fleet NATS: a token-secured server
// accepts the right token (and does JetStream), and rejects no/wrong token.
func TestTokenAuth(t *testing.T) {
	const token = "s3cr3t-fleet-token"
	s, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true,
		StoreDir: t.TempDir(), Authorization: token,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	url := s.ClientURL()

	// No token -> rejected.
	if _, err := Connect(url, "", ""); err == nil {
		t.Fatal("expected auth failure with no token")
	}
	// Wrong token -> rejected.
	if _, err := Connect(url, "", "wrong"); err == nil {
		t.Fatal("expected auth failure with wrong token")
	}
	// Token from a file via NATS_TOKEN_FILE (the on-disk secret path) -> connects.
	tf := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tf, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NATS_URL", url)
	t.Setenv("NATS_TOKEN", "")
	t.Setenv("NATS_TOKEN_FILE", tf)
	cf, err := ConnectFromEnv()
	if err != nil {
		t.Fatalf("connect via token file: %v", err)
	}
	cf.Close()

	// Correct token -> connects AND can do JetStream (Setup + Register).
	c, err := Connect(url, "", token)
	if err != nil {
		t.Fatalf("connect with token: %v", err)
	}
	t.Cleanup(c.Close)
	ctx := context.Background()
	if err := c.Setup(ctx); err != nil {
		t.Fatalf("setup with token: %v", err)
	}
	if err := c.Register(ctx, Peer{Agent: "tok-agent"}); err != nil {
		t.Fatalf("register with token: %v", err)
	}
	list, err := c.Peers(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("want 1 peer over token-auth, got %v err=%v", list, err)
	}
}
