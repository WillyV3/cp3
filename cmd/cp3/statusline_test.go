package main

import (
	"errors"
	"testing"
	"time"

	peers "github.com/WillyV3/claude-peers-v3"
)

func TestStatusLine(t *testing.T) {
	list := []peers.Peer{
		{Agent: "keeper", Cwd: "/w"},
		{Agent: "jim", Cwd: "/w"},
		{Agent: "far", Cwd: "/elsewhere"},
	}
	cases := []struct {
		name, cwd string
		list      []peers.Peer
		err       error
		want      string
	}{
		{"", "/w", list, nil, "○ peers: no name set"},
		{"keeper", "/w", nil, errors.New("boom"), "○ peers: nats down"},
		{"ghost", "", list, nil, "○ peers: ghost · not registered"},
		{"keeper", "/elsewhere", list, nil, "● peers: keeper · 3 online · ⚠ also here: far"},
		{"keeper", "/solo", list, nil, "● peers: keeper · 3 online"},
	}
	for _, c := range cases {
		if got := statusLine(c.name, c.cwd, c.list, c.err); got != c.want {
			t.Errorf("statusLine(%q,%q): got %q want %q", c.name, c.cwd, got, c.want)
		}
	}
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
