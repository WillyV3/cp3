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
		"description": "Send a message to another session by agent name. It never waits for the recipient. READ THE RESULT: it reports one of three real outcomes — \"delivered\" (a live session is consuming it now), \"queued\" (their inbox exists; it arrives when they reconnect), or \"NOT DELIVERED\" (no inbox exists for that name — nothing is subscribed, so the message will never be read; verify the name with list_peers). Do not assume success. The result ends with the message's id, which message_status takes.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":       map[string]any{"type": "string", "description": "Recipient agent name (from list_peers)."},
				"message":  map[string]any{"type": "string", "description": "The message to send."},
				"reply_to": map[string]any{"type": "string", "description": "When answering a peer message: the id from its header. Links your reply to their message."},
			},
			"required": []string{"to", "message"},
		},
	},
	{
		"name":        "message_status",
		"description": "How far a message you sent got: no receipt yet (still queued), shown (it reached the recipient's live session), or replied (they answered it with reply_to). Returns at once; use it instead of waiting.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "The message id send_message returned."},
			},
			"required": []string{"id"},
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
		"description": "Hand over peer messages that were received but could not be posted into this session (they are reported in the server log). Messages normally arrive on their own.",
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
- Your identity is your "agent name" — declared at startup via --as, CLAUDE_PEERS_AGENT, or a .cp3-peer-name file. Without one you are ephemeral: visible in list_peers but not addressable by name.
- Agent names are globally unique while held. A live holder keeps the name; a dead session's name frees automatically.

MESSAGING:
- Delivery: a peer message arrives in this session as a message headed "[cp3 message <id> from <name> ...]". You don't need to poll.
- To reply, call this server's send_message with to = that name and reply_to = that id. Claude Code's own SendMessage tool cannot reach cp3 peers.
- A reply to something you sent is headed "... replying to your message <id> (its first line)".
- Sending never blocks. To learn whether a message you sent was shown or answered, call message_status with the id send_message returned.
- send_message reports which of three things actually happened, and you should read it: "delivered" (a live session is consuming now), "queued" (their inbox exists; it lands when they reconnect), or "NOT DELIVERED ... has no inbox" (nothing is subscribed to that name — the message will never be read; check the exact name in list_peers). Only the third is a failure, and it is loud on purpose.


TOOLS:
- list_peers: Discover sessions on the network.
- send_message(to, message, reply_to?): Send a message by agent name.
- message_status(id): Whether a message you sent is still queued, was shown, or was replied to.
- set_summary(summary): Set your current-work summary.
- check_messages: Messages that arrived but could not be posted into this session.
- claim_agent_name(name): Claim a stable name mid-session.`
