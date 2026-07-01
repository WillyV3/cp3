package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// agentPrompt is the canonical agent-facing usage block — the one source of
// truth injected into AGENTS.md (setup --agents-md), printable for any harness
// (cp3 prompt). Written for a language model: verbs first, no flag reference.
const agentPrompt = `## Peer network (cp3)

You are on a peer network with other coding agents. Your name is this
directory's name unless CLAUDE_PEERS_AGENT or a .claude-peers-agent file says
otherwise.

- See who's online: run ` + "`cp3 peers`" + ` (name, machine, working directory).
- Message an agent: run ` + "`cp3 send --to <name> \"your message\"`" + `. It injects
  into their live session; they can act and reply without a human present.
- When a message arrives tagged [peer message from X], reply to X the same
  way: ` + "`cp3 send --to X \"...\"`" + `. Answer peers like you'd answer a teammate:
  do the small lookup they asked for, don't block on your human.
- Messages to offline agents queue and deliver when they return.
- If something seems broken, run ` + "`cp3 doctor`" + `.`

const agentsMarkerStart = "<!-- cp3:start -->"
const agentsMarkerEnd = "<!-- cp3:end -->"

func cmdPrompt(args []string) {
	fs := flag.NewFlagSet("prompt", flag.ExitOnError)
	fs.Parse(args)
	fmt.Println(agentPrompt)
}

// appendAgentsMD writes the prompt block into path between markers —
// idempotent: replaces an existing block, appends if absent, creates the file
// if missing.
func appendAgentsMD(path string) (changed bool, err error) {
	block := agentsMarkerStart + "\n" + agentPrompt + "\n" + agentsMarkerEnd + "\n"
	data, rerr := os.ReadFile(path)
	if rerr != nil && !os.IsNotExist(rerr) {
		return false, rerr
	}
	s := string(data)
	if i := strings.Index(s, agentsMarkerStart); i >= 0 {
		j := strings.Index(s, agentsMarkerEnd)
		if j < i {
			return false, fmt.Errorf("%s: markers malformed (end before start)", path)
		}
		updated := s[:i] + block + s[j+len(agentsMarkerEnd)+1:]
		if updated == s {
			return false, nil
		}
		return true, os.WriteFile(path, []byte(updated), 0o644)
	}
	sep := "\n"
	if len(s) == 0 || strings.HasSuffix(s, "\n\n") {
		sep = ""
	}
	return true, os.WriteFile(path, []byte(s+sep+block), 0o644)
}
