package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// mergeMCPServer merges a single server entry under an MCP config file's
// "mcpServers" object without clobbering the other servers. Safety rules:
//   - missing file: create parent dirs + write with just our entry (no .bak).
//   - existing file: write a .bak of the original bytes BEFORE overwriting;
//     on parse error, bail without touching the file (never destroy config).
//   - idempotent: if the entry already marshals equal, no write, no .bak.
func mergeMCPServer(path, server string, entry map[string]any) (changed bool, err error) {
	data, rerr := os.ReadFile(path)
	exists := rerr == nil
	if rerr != nil && !os.IsNotExist(rerr) {
		return false, fmt.Errorf("read %s: %w", path, rerr)
	}

	doc := map[string]any{}
	if exists && len(data) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return false, fmt.Errorf("parse %s: %w (left untouched)", path, err)
		}
	}

	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if old, ok := servers[server]; ok {
		if ob, e1 := json.Marshal(old); e1 == nil {
			if nb, e2 := json.Marshal(entry); e2 == nil && bytes.Equal(ob, nb) {
				return false, nil // already identical: no-op
			}
		}
	}

	servers[server] = entry
	doc["mcpServers"] = servers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')

	if exists {
		if err := os.WriteFile(path+".bak", data, 0o644); err != nil {
			return false, fmt.Errorf("write %s.bak: %w", path, err)
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func defaultMCPPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

func cmdSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	agent := fs.String("agent", "", "fixed peer name (omit for per-session identity via CLAUDE_PEERS_AGENT / .claude-peers-agent)")
	mcp := fs.String("mcp", defaultMCPPath(), "MCP config file to merge the claude-peers server into")
	nats := fs.String("nats", "nats://127.0.0.1:4222", "NATS URL")
	agentsMD := fs.String("agents-md", "", "also write the agent usage block into this AGENTS.md (idempotent, marker-delimited)")
	fs.Parse(args)
	// The MCP entry points at THIS binary (`cp3 mcp`) by absolute path —
	// os.Executable, not a PATH lookup, because Claude may launch from a shell
	// (GUI, launchd) whose PATH never saw the install dir.
	bin, err := os.Executable()
	if err != nil {
		bin = "cp3"
	}
	// Secrets are deliberately NOT written here — the token resolves at runtime
	// from NATS_TOKEN / NATS_TOKEN_FILE / ~/.config/cp3/token. And the agent
	// name is usually per-session (dir identity, env, or .claude-peers-agent
	// file), not baked into shared config where every session would collide.
	env := map[string]any{"NATS_URL": *nats}
	if *agent != "" {
		env["CLAUDE_PEERS_AGENT"] = *agent
	}
	entry := map[string]any{
		"type":    "stdio",
		"command": bin,
		"args":    []any{"mcp"},
		"env":     env,
	}
	changed, err := mergeMCPServer(*mcp, "claude-peers", entry)
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
	if changed {
		fmt.Printf("merged claude-peers MCP server into %s (backup at %s.bak)\n", *mcp, *mcp)
	} else {
		fmt.Printf("claude-peers already configured in %s (no change)\n", *mcp)
	}
	if *agentsMD != "" {
		wrote, err := appendAgentsMD(*agentsMD)
		if err != nil {
			fmt.Fprintln(os.Stderr, "setup: agents-md:", err)
			os.Exit(1)
		}
		if wrote {
			fmt.Printf("wrote agent usage block into %s\n", *agentsMD)
		} else {
			fmt.Printf("agent usage block already current in %s\n", *agentsMD)
		}
	}
	fmt.Println()
	fmt.Println("Token: put the fleet token in ~/.config/cp3/token (0600) — never in config/argv.")
	fmt.Println("Launch: cp3 run --as <name>   (execs claude with the peers dev-channel loaded;")
	fmt.Println("        or rely on a .claude-peers-agent file in the project dir)")
}

// cmdRun execs the real `claude` with the dev-channel flag injected. --as/--name
// is stripped and exported as CLAUDE_PEERS_AGENT. syscall.Exec replaces the process.
func cmdRun(args []string) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cp3 run: claude not found on PATH")
		os.Exit(1)
	}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--as" || a == "--name":
			if i+1 < len(args) {
				os.Setenv("CLAUDE_PEERS_AGENT", args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--as="):
			os.Setenv("CLAUDE_PEERS_AGENT", strings.TrimPrefix(a, "--as="))
		default:
			rest = append(rest, a)
		}
	}
	argv := append([]string{"claude", "--dangerously-load-development-channels", "server:claude-peers"}, rest...)
	if err := syscall.Exec(claudePath, argv, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "cp3 run: exec:", err)
		os.Exit(1)
	}
}
