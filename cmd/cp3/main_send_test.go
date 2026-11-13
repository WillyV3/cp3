package main

import (
	"bytes"
	"strings"
	"testing"

	peers "github.com/WillyV3/cp3"
)

// The version column appears only when asked for, and a peer on a build that
// reports none shows a dash, not a blank that shifts the table.
func TestWritePeers(t *testing.T) {
	list := []peers.Peer{{Agent: "new", Runtime: "claude", Machine: "m", Cwd: "/w", Version: "0.6.0"}, {Agent: "old", Machine: "m", Cwd: "/w"}}
	var plain, verbose bytes.Buffer
	writePeers(&plain, list, false)
	writePeers(&verbose, list, true)
	if strings.Contains(plain.String(), "CP3") || strings.Contains(plain.String(), "0.6.0") {
		t.Errorf("version shown without --verbose:\n%s", plain.String())
	}
	lines := strings.Split(strings.TrimSpace(verbose.String()), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], "CP3") || !strings.HasSuffix(lines[1], "0.6.0") || !strings.HasSuffix(lines[2], "-") {
		t.Errorf("verbose table:\n%s", verbose.String())
	}
	if f := strings.Fields(lines[2]); len(f) != 5 || f[1] != "-" {
		t.Errorf("a peer with no runtime must show a dash: %q", lines[2])
	}
}

// A shell-driven agent's reply must survive cp3 send byte for byte: the
// characters that break a quoted argv argument travel through stdin untouched.
func TestMessageTextFromStdinIsVerbatim(t *testing.T) {
	body := "line one\nhe said \"hi\", it's `x` and $(rm -rf /) and $HOME\n\nlast"
	for _, args := range [][]string{nil, {"-"}} {
		got, err := messageText(args, strings.NewReader(body+"\n"))
		if err != nil || got != body {
			t.Errorf("args %q: got %q, %v", args, got, err)
		}
	}
	if got, _ := messageText([]string{"short", "phrase"}, strings.NewReader("ignored")); got != "short phrase" {
		t.Errorf("argv form: %q", got)
	}
}
