package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	peers "github.com/WillyV3/cp3"
)

// cmdDoctor walks the setup chain in dependency order and says the one thing
// that's wrong. Every support question becomes "run cp3 doctor".
func cmdDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	fs.Parse(args)
	tty := stdoutIsTTY()
	failed := false
	var firstFix string

	report := func(ok bool, warn bool, what, detail, fix string) {
		tag := paint(tty, cGreen, "PASS")
		if warn {
			tag = paint(tty, cYellow, "WARN")
		} else if !ok {
			tag = paint(tty, cRed, "FAIL")
			failed = true
			if firstFix == "" {
				firstFix = fix
			}
		}
		fmt.Printf("%s  %-18s %s\n", tag, what, detail)
	}

	// 1. config: where do url + token come from?
	home, _ := os.UserHomeDir()
	urlSrc := "localhost default (auto-serves on demand)"
	if os.Getenv("NATS_URL") != "" {
		urlSrc = "env NATS_URL"
	} else if _, err := os.Stat(filepath.Join(home, ".config", "cp3", "url")); err == nil {
		urlSrc = "~/.config/cp3/url"
	}
	report(true, false, "config", peers.URLFromEnv()+" via "+urlSrc, "")

	// 2. server reachable + authed (one dial answers both).
	c, err := peers.ConnectFromEnv()
	if err != nil {
		report(false, false, "server", err.Error(),
			"is the server up? check the url/token in ~/.config/cp3/")
	} else {
		defer c.Close()
		report(true, false, "server", "connected + authenticated", "")
	}

	// 3-5 need the connection.
	if c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		list, perr := c.Peers(ctx)
		switch {
		case perr != nil:
			report(false, false, "presence", perr.Error(), "server reachable but JetStream unhealthy — check server logs")
		case len(list) == 0:
			report(true, true, "presence", "0 peers online (network idle or brand new)", "")
		default:
			report(true, false, "presence", fmt.Sprintf("%d peers online", len(list)), "")
		}

		if cs, cerr := c.Consumers(ctx); cerr == nil {
			stale := 0
			now := time.Now()
			for _, s := range cs {
				if consumerVerdict(s, now) == "STALE" {
					stale++
				}
			}
			report(stale == 0, stale > 0, "consumers",
				fmt.Sprintf("%d total, %d stale", len(cs), stale),
				"")
		} else {
			report(true, true, "consumers", "stream not created yet (first agent creates it)", "")
		}
	}

	// 6. MCP entry wired into Claude?
	mcpDetail, mcpOK := "no claude-peers entry in ~/.claude.json", false
	if b, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		var cfg struct {
			MCPServers map[string]struct {
				Command string `json:"command"`
			} `json:"mcpServers"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			if e, ok := cfg.MCPServers["claude-peers"]; ok {
				if _, lerr := exec.LookPath(e.Command); lerr == nil {
					mcpOK, mcpDetail = true, "claude-peers → "+e.Command
				} else {
					mcpDetail = "entry present but command not executable: " + e.Command
				}
			}
		}
	}
	report(mcpOK, false, "claude mcp", mcpDetail, "run: cp3 setup")

	// 7. identity: what would THIS dir be called?
	cwd, _ := os.Getwd()
	name, source := peers.ResolveIdentity(cwd, "")
	report(name != "", false, "identity",
		fmt.Sprintf("%q (from %s)", name, source),
		"cd into a project dir, or set CLAUDE_PEERS_AGENT")

	fmt.Println()
	if failed {
		fmt.Printf("%s %s\n", paint(tty, cRed, "→ fix:"), firstFix)
		os.Exit(1)
	}
	fmt.Println(paint(tty, cGreen, "everything healthy") +
		paint(tty, cDim, " — launch with the peers channel: cp3 run"))
}
