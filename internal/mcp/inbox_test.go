package mcp

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

// The wire format is the whole contract: two JSON lines, auth first.
func TestInboxPostWritesAuthThenUserMessage(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var lines []string
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		got <- lines
	}()

	if err := (Inbox{Socket: sock, Token: "tok-1"}).Post("hello\nthere"); err != nil {
		t.Fatal(err)
	}
	lines := <-got
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), lines)
	}
	var auth map[string]string
	json.Unmarshal([]byte(lines[0]), &auth)
	if auth["type"] != "auth" || auth["token"] != "tok-1" {
		t.Errorf("auth line: %s", lines[0])
	}
	var user struct {
		Type    string
		Message struct{ Role, Content string }
	}
	json.Unmarshal([]byte(lines[1]), &user)
	if user.Type != "user" || user.Message.Role != "user" || user.Message.Content != "hello\nthere" {
		t.Errorf("user line: %s", lines[1])
	}
}

func TestInboxPostWithoutTokenSendsOnlyTheMessage(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, _ := net.Listen("unix", sock)
	defer ln.Close()
	n := make(chan int, 1)
	go func() {
		c, _ := ln.Accept()
		defer c.Close()
		sc := bufio.NewScanner(c)
		k := 0
		for sc.Scan() {
			k++
		}
		n <- k
	}()
	if err := (Inbox{Socket: sock}).Post("x"); err != nil {
		t.Fatal(err)
	}
	if k := <-n; k != 1 {
		t.Errorf("want 1 line without a token, got %d", k)
	}
}

func TestInboxPostFailsLoudlyWithNoSocket(t *testing.T) {
	if err := (Inbox{}).Post("x"); err == nil {
		t.Fatal("want an error with no socket")
	}
	if err := (Inbox{Socket: filepath.Join(t.TempDir(), "none.sock")}).Post("x"); err == nil {
		t.Fatal("want an error when nothing listens")
	}
}

func TestInboxFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	if _, ok := InboxFromEnv(); ok {
		t.Error("ok without a socket var")
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/tmp/x.sock")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "t")
	in, ok := InboxFromEnv()
	if !ok || in.Socket != "/tmp/x.sock" || in.Token != "t" {
		t.Errorf("got %+v ok=%v", in, ok)
	}
}
