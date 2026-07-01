// claude-peers v3 adapter for the pi coding agent.
//
// pi injection is in-process only, so this is a pi extension — but it embeds NO
// NATS client. It shells out to the `cp3` Go binary (the one wall): `cp3
// subscribe` streams inbound peer messages as JSONL, which we inject via
// pi.sendUserMessage(...steer); the peer_send tool shells `cp3 send`. All
// network logic stays in one Go implementation.
//
// Install: copy to ~/.pi/agent/extensions/peer.ts (or .pi/extensions/peer.ts).
// Config via env: CLAUDE_PEERS_AGENT (or PEER_NAME) — this agent's name;
// NATS_URL / NATS_CREDS (passed through to cp3); CP3_BIN (default "cp3").
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"
import { spawn } from "node:child_process"
import { createInterface } from "node:readline"
import { hostname } from "node:os"

const CP3 = process.env.CP3_BIN ?? "cp3"
const ME = process.env.CLAUDE_PEERS_AGENT ?? process.env.PEER_NAME ?? ""

export default function (pi: ExtensionAPI) {
  let sub: ReturnType<typeof spawn> | undefined

  pi.on("session_start", async (_event, ctx) => {
    if (!ME) {
      ctx.ui.notify("claude-peers: set CLAUDE_PEERS_AGENT to join the network", "warn")
      return
    }
    sub = spawn(CP3, ["subscribe", "--agent", ME, "--machine", hostname()], {
      stdio: ["ignore", "pipe", "pipe"],
    })
    createInterface({ input: sub.stdout! }).on("line", (line) => {
      if (!line.trim()) return
      let m: { from?: string; content?: string }
      try {
        m = JSON.parse(line)
      } catch {
        return // not a message line (stderr/log leakage) — ignore
      }
      if (m.content) pi.sendUserMessage(`[peer message from ${m.from} — to reply, use the peer_send tool with to="${m.from}"]
${m.content}`, { deliverAs: "steer" })
    })
    sub.on("exit", (code) => ctx.ui.notify(`claude-peers: subscriber exited (${code})`, "warn"))
    ctx.ui.notify(`claude-peers: ${ME} online`, "info")
  })

  pi.on("session_end", async () => sub?.kill())

  pi.registerTool({
    name: "peer_send",
    label: "Send peer message",
    description:
      "Send a message to another coding-agent session on the claude-peers network. Messages queue if the recipient is offline and deliver on reconnect.",
    parameters: Type.Object({
      to: Type.String({ description: "Recipient agent name (see the peers you were told about)." }),
      message: Type.String({ description: "The message to send." }),
    }),
    async execute(_id, params) {
      if (!ME) return { content: [{ type: "text", text: "No agent name set (CLAUDE_PEERS_AGENT)." }], details: {} }
      const code = await run(CP3, ["send", "--from", ME, "--to", params.to, params.message])
      return {
        content: [{ type: "text", text: code === 0 ? `Sent to ${params.to}.` : `Send failed (exit ${code}).` }],
        details: {},
      }
    },
  })
}

function run(cmd: string, args: string[]): Promise<number> {
  return new Promise((resolve) => {
    const p = spawn(cmd, args, { stdio: "ignore" })
    p.on("exit", (code) => resolve(code ?? 1))
    p.on("error", () => resolve(1))
  })
}
