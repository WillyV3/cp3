package main

import (
	"testing"
	"time"

	peers "github.com/WillyV3/cp3"
)

// The row that justifies the union: sontara-web sat DOWN holding one real
// message. Presence alone cannot see it, and an intersection would drop it.
func TestRosterJSONKeepsDownPeerHoldingMail(t *testing.T) {
	now := time.Now()
	last := now.Add(-90 * time.Second)
	got := rosterJSON(
		[]peers.Peer{{Agent: "keeper", Machine: "omarchy", Cwd: "/p/cp3", Summary: "s"}},
		[]peers.ConsumerStatus{
			{Name: "inbox-keeper", Pending: 0, LastDelivery: &last},
			{Name: "inbox-sontara-web", Pending: 1},
			{Name: "inbox-dead", Pending: 0}, // graveyard: no mail, must not appear
			{Name: "not-an-inbox", Pending: 9},
		},
		now,
	)
	if len(got) != 2 {
		t.Fatalf("want 2 rows (up + down-with-mail), got %d: %+v", len(got), got)
	}
	if got[0].Name != "keeper" || !got[0].Up || got[0].Cwd != "/p/cp3" || got[0].Summary != "s" {
		t.Errorf("presence fields lost: %+v", got[0])
	}
	if got[0].LastMailSecs == nil || *got[0].LastMailSecs != 90 {
		t.Errorf("last_mail_secs: want 90, got %v", got[0].LastMailSecs)
	}
	if got[1].Name != "sontara-web" || got[1].Up || got[1].Pending != 1 {
		t.Errorf("down-with-mail row wrong: %+v", got[1])
	}
}

// ClaimWithFallback makes `<name>-<machine>` a real live name, so a caller
// matching on the marker name alone gets a false DOWN. Both must be present.
func TestRosterJSONKeepsFallbackClaimNames(t *testing.T) {
	got := rosterJSON(
		[]peers.Peer{
			{Agent: "astrobot", Cwd: "/a"},
			{Agent: "astrobot-omarchy", Cwd: "/a"},
		},
		nil, time.Now(),
	)
	if len(got) != 2 || got[0].Name != "astrobot" || got[1].Name != "astrobot-omarchy" {
		t.Fatalf("both claim names must survive: %+v", got)
	}
}
