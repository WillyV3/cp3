// cp3 adapter for the pi coding agent.
//
// pi injection is in-process only, so this is a pi extension — but it embeds NO
// NATS client. It shells out to the `cp3` Go binary (the one wall): `cp3
// subscribe` claims the peer name and streams inbound messages as JSONL, which
// we inject via pi.sendUserMessage(...steer); the tools shell `cp3 send` and
// `cp3 peers`. All network logic stays in one Go implementation.
//
// Install: copy to ~/.pi/agent/extensions/peer.ts (or .pi/extensions/peer.ts).
// Identity is cp3's own: --agent from CLAUDE_PEERS_AGENT (or PEER_NAME) when
// set, otherwise `cp3 subscribe` resolves it from the directory exactly as it
// does for Claude Code. The name actually held (a collision fallback such as
// "pith-pi" included) comes back on the hello line; that is the only name this
// adapter ever uses. NATS_URL / NATS_CREDS pass through to cp3; CP3_BIN
// overrides the binary (default "cp3").
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"
import { execFile, spawn, type ChildProcess } from "node:child_process"
import { createInterface } from "node:readline"
import { hostname } from "node:os"

const CP3 = process.env.CP3_BIN ?? "cp3"
const CONFIGURED = process.env.CLAUDE_PEERS_AGENT ?? process.env.PEER_NAME ?? ""
const STATUS_KEY = "cp3"
const ROSTER_EVERY_MS = 15_000

type Hello = { type: "hello"; agent: string }
type Inbound = { from?: string; content?: string }
type RosterRow = { name: string; up: boolean; pending: number; runtime?: string; machine?: string; cwd?: string; summary?: string }

export default function (pi: ExtensionAPI) {
  let sub: ChildProcess | undefined
  let roster: ReturnType<typeof setInterval> | undefined
  let me = "" // the name cp3 says we hold; empty until the hello line arrives

  pi.on("session_start", async (_event, ctx) => {
    const args = ["subscribe", "--runtime", "pi", "--machine", hostname(), "--cwd", ctx.cwd]
    if (CONFIGURED) args.push("--agent", CONFIGURED)
    sub = spawn(CP3, args, { cwd: ctx.cwd, stdio: ["ignore", "pipe", "pipe"] })
    sub.on("error", (err) => status(ctx, `○ cp3 unavailable · ${err.message}`))

    createInterface({ input: sub.stdout! }).on("line", (line) => {
      const msg = parse(line)
      if (!msg) return
      if ((msg as Hello).type === "hello") {
        me = (msg as Hello).agent
        refreshStatus(ctx)
        return
      }
      const m = msg as Inbound
      if (m.content) pi.sendUserMessage(`[peer message from ${m.from} over cp3 — reply with the peer_send tool: to="${m.from}"]
${m.content}`, { deliverAs: "steer" })
    })

    // The subscriber holding the name is the peer. If it dies, say so where
    // the user looks instead of leaving a healthy-looking session that is deaf.
    sub.on("exit", (code) => {
      me = ""
      status(ctx, `○ cp3 subscriber exited (${code})`)
    })

    roster = setInterval(() => refreshStatus(ctx), ROSTER_EVERY_MS)
  })

  // The subscriber holds the peer name. If it outlives pi, the name stays "up"
  // and mail is delivered to nobody. pi's teardown event is session_shutdown;
  // the process "exit" hook covers any path that skips it.
  const stop = () => {
    if (roster) clearInterval(roster)
    roster = undefined
    sub?.kill()
    sub = undefined
  }
  pi.on("session_shutdown", async () => stop())
  process.on("exit", stop)

  pi.registerTool({
    name: "peer_send",
    label: "Send peer message",
    description:
      "Send a message to another coding-agent session on the cp3 peer network. Messages queue if the recipient is offline and deliver on reconnect. Use list_peers to see who is there.",
    parameters: Type.Object({
      to: Type.String({ description: "Recipient agent name, as list_peers shows it." }),
      message: Type.String({ description: "The message to send." }),
    }),
    async execute(_id, params) {
      if (!me) throw new Error("not on the cp3 network yet (no peer name held)")
      // cp3 reports delivered / queued / NOT DELIVERED; pass that through verbatim.
      const { code, out } = await run(CP3, ["send", "--from", me, "--to", params.to, params.message])
      if (code !== 0 && code !== 3) throw new Error(out || `cp3 send exited ${code}`)
      return { content: [{ type: "text", text: out }], details: { to: params.to, code } }
    },
  })

  pi.registerTool({
    name: "list_peers",
    label: "List peers",
    description:
      "List the coding-agent sessions on the cp3 peer network: name, runtime, whether it is up, mail waiting, machine, and directory.",
    parameters: Type.Object({}),
    async execute() {
      const rows = await fetchRoster()
      const lines = rows.map((r) =>
        [r.up ? "up" : "down", r.name + (r.name === me ? " (you)" : ""), r.runtime ?? "-", r.machine ?? "-", r.cwd ?? "-",
          r.pending ? `${r.pending} waiting` : "", r.summary ?? ""].filter(Boolean).join("  "))
      return { content: [{ type: "text", text: lines.join("\n") || "no peers" }], details: { peers: rows } }
    },
  })

  async function refreshStatus(ctx: ExtensionContext) {
    if (!me) return
    try {
      const rows = await fetchRoster()
      const up = rows.filter((r) => r.up).length
      const mine = rows.find((r) => r.name === me)
      const mail = mine?.pending ? ` · ✉${mine.pending}` : ""
      const drift = CONFIGURED && me !== CONFIGURED ? ` · wanted ${CONFIGURED}` : ""
      status(ctx, `● ${me} · ${up} up${mail}${drift}`)
    } catch {
      status(ctx, `○ ${me} · peers down`)
    }
  }
}

async function fetchRoster(): Promise<RosterRow[]> {
  const { code, out } = await run(CP3, ["peers", "--json"])
  if (code !== 0) throw new Error(out || `cp3 peers exited ${code}`)
  return JSON.parse(out) as RosterRow[]
}

function status(ctx: ExtensionContext, text: string) {
  if (ctx.hasUI) ctx.ui.setStatus(STATUS_KEY, text)
}

function parse(line: string): Hello | Inbound | undefined {
  if (!line.trim()) return undefined
  try {
    return JSON.parse(line)
  } catch {
    return undefined // not a protocol line (log leakage) — ignore
  }
}

function run(cmd: string, args: string[]): Promise<{ code: number; out: string }> {
  return new Promise((resolve) => {
    execFile(cmd, args, { encoding: "utf8" }, (err, stdout, stderr) => {
      const code = err ? (typeof err.code === "number" ? err.code : 1) : 0
      resolve({ code, out: (stdout + stderr).trim() })
    })
  })
}
