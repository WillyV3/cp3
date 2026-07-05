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
		{Agent: "keeper", Cwd: "/w", Session: "s-keeper"},
		{Agent: "jim", Cwd: "/w", Session: "s-jim"},
		{Agent: "far", Cwd: "/elsewhere", Session: "s-far"},
		{Agent: "twin", Cwd: "/w", Machine: "macbook1", Session: "s-twin"},
	}
	cases := []struct {
		name          string
		conf, session string
		cwd           string
		list          []peers.Peer
		pending       uint64
		err           error
		want          string
	}{
		{"no identity", "", "", "/w", list, 0, nil, "○ peers: —"},
		{"down", "keeper", "s-keeper", "/w", nil, 0, errors.New("boom"), "○ peers: down"},
		{"not registered", "ghost", "s-ghost", "", list, 0, nil, "○ peers: ghost · not registered"},
		{"co-location neutral", "keeper", "s-keeper", "/w", list, 0, nil, "● peers: keeper · 4 online · with: jim, twin@macbook1"},
		{"solo", "keeper", "s-keeper", "/solo", list, 0, nil, "● peers: keeper · 4 online"},
		{"pending badge", "keeper", "s-keeper", "/solo", list, 4, nil, "● peers: keeper · 4 online · ✉4"},
		// jim's bug: configured "jim" but THIS session claimed "jim-omarchy"
		// (stale "jim" from the previous session still in presence). Must
		// show the claimed name, warn about the drift, and NOT list either
		// of our own names in "with".
		{"claimed-name drift", "jim", "s-new", "/w",
			[]peers.Peer{
				{Agent: "jim", Cwd: "/w", Session: "s-old"}, // dead session's TTL remnant
				{Agent: "jim-omarchy", Cwd: "/w", Machine: "omarchy", Session: "s-new"}, // me
				{Agent: "keeper", Cwd: "/w", Session: "s-keeper"},
			}, 0, nil,
			"● peers: jim-omarchy · 3 online · ⚠ wanted: jim · with: jim, keeper"},
	}
	for _, c := range cases {
		if got := statusLine(c.conf, c.session, c.cwd, c.list, c.pending, c.err); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestStatusLineHasColor(t *testing.T) {
	// Without NO_COLOR the line must actually carry ANSI — the whole point.
	got := statusLine("keeper", "s1", "/w", []peers.Peer{{Agent: "keeper", Session: "s1"}}, 0, nil)
	if !containsANSI(got) {
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
		{peers.ConsumerStatus{LastDelivery: &recent, Pending: 5}, "active"},
		{peers.ConsumerStatus{LastDelivery: &old, Pending: 29000}, "STALE"},
		{peers.ConsumerStatus{Pending: 100}, "STALE"},
		{peers.ConsumerStatus{LastDelivery: &old, Pending: 0}, "idle"},
		{peers.ConsumerStatus{}, "idle"},
	}
	for i, c := range cases {
		if got := consumerVerdict(c.cs, now); got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}
