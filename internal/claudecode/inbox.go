// Package claudecode is cp3's knowledge of the Claude Code host: how to find
// the session a process runs under, and how to put a message in front of it.
// Everything a Claude Code mod needs from cp3 is answered here and exposed as
// plain commands (cp3 whoami, cp3 subscribe --deliver claude-socket), so the
// mod itself stays a thin view with no protocol in it.
package claudecode

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

// Inbox is a Claude Code session's cross-session message socket. Claude Code
// exports the path and a per-session token to every child process it starts
// (hooks, Bash commands, MCP servers, a mod's spawned processes).
type Inbox struct {
	Socket string
	Token  string
}

// InboxFromEnv reads the inbox of the enclosing Claude Code session. ok is
// false when this process is not a child of one.
func InboxFromEnv() (in Inbox, ok bool) {
	in.Socket = os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")
	in.Token = os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN")
	return in, in.Socket != ""
}

// Post delivers one message to the session as a native cross-session message.
// It lands the way a message from another session does: between tool calls
// while Claude is working, or as a new turn when idle. No launch flag needed.
//
// Wire format, as Claude Code itself documents for a child posting back to
// its own session: an auth line, then one user message, each a JSON line on
// a fresh connection. The auth line always goes — on macOS Claude Code can
// only verify an exited child by its token. Verified on Claude Code 2.1.289
// and 2.1.296, 2026-10-10.
func (in Inbox) Post(text string) error {
	if in.Socket == "" {
		return fmt.Errorf("claude inbox: no socket")
	}
	conn, err := net.DialTimeout("unix", in.Socket, 3*time.Second)
	if err != nil {
		return fmt.Errorf("claude inbox %s: %w", in.Socket, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	enc := json.NewEncoder(conn)
	if in.Token != "" {
		if err := enc.Encode(authLine{Type: "auth", Token: in.Token}); err != nil {
			return err
		}
	}
	return enc.Encode(userLine{Type: "user", Message: userMessage{Role: "user", Content: text}})
}

type authLine struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

type userLine struct {
	Type    string      `json:"type"`
	Message userMessage `json:"message"`
}

type userMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
