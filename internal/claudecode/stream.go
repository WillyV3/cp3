package claudecode

import (
	"encoding/json"
	"io"

	peers "github.com/WillyV3/cp3"
)

// Hello is the first line `cp3 subscribe` writes: who the subscriber actually
// is once the name is held (collision fallback included) and its pid, so a
// supervisor never guesses the name and can end a stale child by identity.
// Message lines follow, one per delivery; they have "from"/"content" and no
// "type", so a reader tells the two apart by that field.
type Hello struct {
	Type    string `json:"type"`
	Agent   string `json:"agent"`
	Machine string `json:"machine"`
	Cwd     string `json:"cwd"`
	Runtime string `json:"runtime,omitempty"`
	PID     int    `json:"pid"`
}

// Stream writes the subscribe protocol: a Hello, then messages, one JSON
// line each, flushed per line.
type Stream struct{ enc *json.Encoder }

func NewStream(w io.Writer) *Stream { return &Stream{enc: json.NewEncoder(w)} }

func (s *Stream) Hello(p peers.Peer, pid int) error {
	return s.enc.Encode(Hello{Type: "hello", Agent: p.Agent, Machine: p.Machine, Cwd: p.Cwd, Runtime: p.Runtime, PID: pid})
}

func (s *Stream) Message(m peers.Message) error { return s.enc.Encode(m) }

// ReplyHint is the line a message carries into a Claude Code session when it
// arrives as a native message. The cp3 mod routes SendMessage to a peer name
// over cp3, so the hint names that tool, not a shell command.
func ReplyHint(from string) string {
	return "[peer message from " + from + " over cp3 — reply with the SendMessage tool: to=\"" + from + "\"]"
}
