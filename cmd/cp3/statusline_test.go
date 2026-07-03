package main

import (
	"errors"
	"testing"
	"time"

	peers "github.com/WillyV3/cp3"
)

func TestStatusLine(t *testing.T) {
	t.Setenv("NO_COLOR", "1") // golden strings stay readable; paint() no-ops
	list := []peers.Peer{
		{Agent: "keeper", Cwd: "/w"},
		{Agent: "jim", Cwd: "/w"},
		{Agent: "far", Cwd: "/elsewhere"},
		{Agent: "twin", Cwd: "/w", Machine: "macbook1"},
	}
	cases := []struct {
		name, cwd string
		list      []peers.Peer
		pending   uint64
		err       error
		want      string
	}{
		{"", "/w", list, 0, nil, "○ peers: —"},
		{"keeper", "/w", nil, 0, errors.New("boom"), "○ peers: down"},
		{"ghost", "", list, 0, nil, "○ peers: ghost · not registered"},
		{"keeper", "/elsewhere", list, 0, nil, "● peers: keeper · 4 online · ⚠ also here: far"},
		{"keeper", "/w", list, 0, nil, "● peers: keeper · 4 online · ⚠ also here: jim, twin@macbook1"},
		{"keeper", "/solo", list, 0, nil, "● peers: keeper · 4 online"},
		{"keeper", "/solo", list, 4, nil, "● peers: keeper · 4 online · ✉4"},
	}
	for _, c := range cases {
		if got := statusLine(c.name, c.cwd, c.list, c.pending, c.err); got != c.want {
			t.Errorf("statusLine(%q,%q,pending=%d): got %q want %q", c.name, c.cwd, c.pending, got, c.want)
		}
	}
}

func TestStatusLineHasColor(t *testing.T) {
	// Without NO_COLOR the line must actually carry ANSI — the whole point.
	if got := statusLine("keeper", "/w", []peers.Peer{{Agent: "keeper"}}, 0, nil); !containsANSI(got) {
		t.Errorf("expected ANSI codes in %q", got)
	}
}

func containsANSI(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '\x1b' && s[i+1] == '[' {
			return true
		}
	}
	return false
}

func TestConsumerVerdict(t *testing.T) {
	now := time.Now()
	recent := now.Add(-30 * time.Second)
	old := now.Add(-3 * time.Hour)
	cases := []struct {
		cs   peers.ConsumerStatus
		want string
	}{
		{peers.ConsumerStatus{LastDelivery: &recent, Pending: 5}, "active"}, // draining now
		{peers.ConsumerStatus{LastDelivery: &old, Pending: 29000}, "STALE"}, // the v1 graveyard shape
		{peers.ConsumerStatus{Pending: 100}, "STALE"},                       // never delivered, backlog
		{peers.ConsumerStatus{LastDelivery: &old, Pending: 0}, "idle"},      // caught up, quiet
		{peers.ConsumerStatus{}, "idle"},
	}
	for i, c := range cases {
		if got := consumerVerdict(c.cs, now); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}
