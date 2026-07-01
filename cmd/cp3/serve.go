package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/WillyV3/claude-peers-v3/internal/boot"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

// cmdServe runs the network backbone in-process: an embedded NATS server with
// JetStream on disk. Users never install or learn NATS — `cp3 serve` IS the
// server. The token is generated on first run (0600) and required for every
// connection, localhost included: fail closed, zero user effort.
func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "listen address (0.0.0.0 to serve a fleet)")
	port := fs.Int("port", 4222, "listen port")
	dataDir := fs.String("data", boot.DataDir(), "JetStream storage directory")
	fs.Parse(args)

	token, err := boot.EnsureToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve: token:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}

	s, err := natsserver.NewServer(&natsserver.Options{
		Host:          *host,
		Port:          *port,
		JetStream:     true,
		StoreDir:      *dataDir,
		Authorization: token,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		fmt.Fprintln(os.Stderr, "serve: server failed to become ready")
		os.Exit(1)
	}

	fmt.Printf("cp3 network up on nats://%s:%d (data: %s)\n", *host, *port, *dataDir)
	if *host != "127.0.0.1" && *host != "localhost" {
		fmt.Println("join from another machine:")
		fmt.Printf("  cp3 setup --nats nats://<this-host>:%d   # + copy ~/.config/cp3/token across once\n", *port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	s.Shutdown()
}
