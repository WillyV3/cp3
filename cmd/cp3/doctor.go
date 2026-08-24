package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	peers "github.com/WillyV3/cp3"
)

// cmdDoctor walks the setup chain in dependency order and says the one thing
// that's wrong. Every support question becomes "run cp3 doctor".
// readCfg returns ~/.config/cp3/<name> contents, or "".
func readCfg(home, name string) string {
	b, err := os.ReadFile(filepath.Join(home, ".config", "cp3", name))
	if err != nil {
		return ""
	}
	return string(b)
}

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

	// 0. version: "what is the fleet running" had no answer, and every
	// incident this month was a divergence problem. Fleet machines track
	// releases; only the development box runs an untagged build.
	build := version
	if build == "dev" {
		build = "dev (untagged build — expected only on the machine where cp3 is developed)"
	}
	report(true, version == "dev", "version", build, "")

	// 1. config: where do url + token come from?
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	urlSrc := "localhost default (auto-serves on demand)"
	if os.Getenv("NATS_URL") != "" {
		urlSrc = "env NATS_URL"
	} else if _, err := os.Stat(filepath.Join(home, ".config", "cp3", "url")); err == nil {
		urlSrc = "~/.config/cp3/url"
	}
	report(true, false, "config", peers.URLFromEnv()+" via "+urlSrc, "")

	// The xps outage: an MCP env var pinned localhost while the config file
	// pointed at the fleet, so sessions registered on an island of one and
	// every check passed. If the two disagree, say so — the env wins, and
	// that is exactly what nobody expects.
	if envURL := os.Getenv("NATS_URL"); envURL != "" {
		if fileURL := strings.TrimSpace(readCfg(home, "url")); fileURL != "" && fileURL != envURL {
			report(false, false, "network split",
				fmt.Sprintf("NATS_URL=%s overrides ~/.config/cp3/url=%s — this session is on a DIFFERENT network than the fleet", envURL, fileURL),
				"unset NATS_URL, or remove it from the claude-peers env block in ~/.claude.json")
		}
	}

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

		// "8 peers online" says the NETWORK is healthy; it says nothing about
		// whether THIS session is reachable. doctor reported ALL PASS through
		// a total registration outage on xps because it never asked that.
		name, src := peers.ResolveIdentity(cwd, "")
		if name != "" {
			var found bool
			for _, p := range list {
				if p.Agent == name {
					found = true
					break
				}
			}
			report(found, false, "my identity",
				fmt.Sprintf("%q (from %s) %s", name, src, map[bool]string{true: "is on the roster", false: "is NOT on the roster — messages to it go nowhere"}[found]),
				"no live session holds this name here; launch with `cp3 run`, or check for a stale ~/.local/share/cp3/by-claude-pid state file")
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
