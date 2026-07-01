// Package boot connects to the peers network, bringing a local network up on
// demand: when the target is localhost and nothing is listening, it spawns a
// detached `cp3 serve` and retries. Solo users manage no daemon — the network
// comes up because an agent showed up.
package boot

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	peers "github.com/WillyV3/cp3"
)

// Connect dials like peers.ConnectFromEnv, plus the lazy auto-serve: localhost
// target + connection refused -> spawn `cp3 serve` detached, retry ~2s. Remote
// urls never auto-serve — a down fleet server is the operator's news, not
// something to paper over.
func Connect() (*peers.Client, error) {
	c, err := peers.ConnectFromEnv()
	if err == nil {
		return c, nil
	}
	url := peers.URLFromEnv()
	if !strings.Contains(url, "127.0.0.1") && !strings.Contains(url, "localhost") {
		return nil, err
	}
	if !spawnServe() {
		return nil, err
	}
	for range 20 {
		time.Sleep(100 * time.Millisecond)
		if c, e := peers.ConnectFromEnv(); e == nil {
			return c, nil
		}
	}
	return nil, err
}

// DataDir is where the embedded server keeps JetStream state (and serve.log).
func DataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "cp3-data"
	}
	return filepath.Join(home, ".local", "share", "cp3")
}

// EnsureToken returns the token from ~/.config/cp3/token, generating and
// persisting one (0600) on first run.
func EnsureToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".config", "cp3", "token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := "cp3-" + hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

func spawnServe() bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	if err := os.MkdirAll(DataDir(), 0o700); err != nil {
		return false
	}
	logf, err := os.OpenFile(filepath.Join(DataDir(), "serve.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	defer logf.Close()
	cmd := exec.Command(self, "serve")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survives this process
	if err := cmd.Start(); err != nil {
		return false
	}
	go cmd.Wait() // reap if it exits while we're still alive
	return true
}
