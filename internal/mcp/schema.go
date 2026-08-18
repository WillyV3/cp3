package mcp

var toolSchemas = []map[string]any{
	{
		"name":        "list_peers",
		"description": "List other coding-agent sessions across the network. Returns agent name, machine, working directory, and summary.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
	{
		"name":        "send_message",
		"description": "Send a message to another session by agent name. READ THE RESULT: it reports one of three real outcomes — \"delivered\" (a live session is consuming it now), \"queued\" (their inbox exists; it arrives when they reconnect), or \"NOT DELIVERED\" (no inbox exists for that name — nothing is subscribed, so the message will never be read; verify the name with list_peers). Do not assume success.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "Recipient agent name (from list_peers)."},
				"message": map[string]any{"type": "string", "description": "The message to send."},
			},
			"required": []string{"to", "message"},
		},
	},
	{
		"name":        "set_summary",
		"description": "Set a brief (1-2 sentence) summary of your current work. Visible to peers in list_peers.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"summary": map[string]any{"type": "string", "description": "1-2 sentence summary of current work."},
			},
			"required": []string{"summary"},
		},
	},
	{
		"name":        "check_messages",
		"description": "Drain messages received since the last check. Normally messages push automatically via notifications/claude/channel; this is the fallback path.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	},
	{
		"name":        "claim_agent_name",
		"description": "Claim a stable agent name for THIS session without restarting. Use when the user tells you to call yourself something. Names are globally unique while held — if another live session holds it, you get an error with the holder's info.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "The agent name to claim (e.g. 'jim')."},
			},
			"required": []string{"name"},
		},
	},
}

const instructions = `You are connected to the claude-peers network (v3, NATS-native). Other coding-agent sessions across the fleet can see you and send you messages.

IDENTITY:
- Your identity is your "agent name" — declared at startup via --as, CLAUDE_PEERS_AGENT, or a .claude-peers-agent file. Without one you are ephemeral: visible in list_peers but not addressable by name.
- Agent names are globally unique while held. A live holder keeps the name; a dead session's name frees automatically.

MESSAGING:
- Push delivery: messages arrive as notifications/claude/channel with from_agent / message_id in the meta block. Push is reliable — you don't need to poll every turn.
- To reply, call send_message with to = from_agent.
- send_message reports which of three things actually happened, and you should read it: "delivered" (a live session is consuming now), "queued" (their inbox exists; it lands when they reconnect), or "NOT DELIVERED ... has no inbox" (nothing is subscribed to that name — the message will never be read; check the exact name in list_peers). Only the third is a failure, and it is loud on purpose.
- On a new conversation, call check_messages ONCE to drain anything received before the channel was up. After that, trust the push.

TOOLS:
- list_peers: Discover sessions on the network.
- send_message(to, message): Send a message by agent name.
- set_summary(summary): Set your current-work summary.
- check_messages: Drain buffered messages (fallback — push is the normal path).
- claim_agent_name(name): Claim a stable name mid-session.`
