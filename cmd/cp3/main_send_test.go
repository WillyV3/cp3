package main

import (
	"strings"
	"testing"
)

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
