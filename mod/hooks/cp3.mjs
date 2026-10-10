// cp3 mod — the cp3 peer network inside Claude Code.
//
//   • a line above the prompt: who you are here, how many peers are up, mail waiting
//   • /cp3 — the roster by machine, a field to message any peer, the last mail in
//   • SendMessage / ListAgents, Claude's own cross-session tools, reach every cp3
//     peer by name — same machine or another one (the native feature spans one
//     machine unless both sides go through Anthropic's servers)
//   • inbound mail lands as a native cross-session message, mid-turn, with no
//     --dangerously-load-development-channels flag: the subscriber posts it to
//     this session's inbox socket
//
// The cp3 binary is the one wall: this file never speaks NATS. It runs
// `cp3 whoami`, `cp3 peers --json`, `cp3 send`, and supervises
// `cp3 subscribe --deliver claude-socket`.
//
// Identity: when the session already runs the cp3 MCP server (`cp3 run`), that
// server owns the name and the inbox and this mod only draws and bridges —
// otherwise two subscribers would split one inbox and the second would get a
// machine-suffixed twin name. Without MCP, this mod owns both.
//
// Colors are theme keys only (success, warning, error, claude, inactive), so
// the mod follows the person's Claude Code theme. Glyphs are Nerd Font
// codepoints, each named by its nerdfonts.com name (glyphnames.json 3.5.1); a
// terminal without a Nerd Font shows boxes, the same trade every Nerd Font
// statusline makes.

const PANE = 'cp3'
const REFRESH_MS = 15000 // plus a refresh on every inbound message; 15 sessions × 5s was 3 broker connections a second
const INBOX_KEEP = 20

const G = {
  net: '\uf43c',       // oct-broadcast       the network
  up: '\u{f0765}',     // md-circle           a live peer
  away: '\u{f0766}',   // md-circle_outline   a peer that is down
  mail: '\u{f01ee}',   // md-email            mail waiting in an inbox
  send: '\u{f048a}',   // md-send
  inbox: '\u{f02fb}',  // md-inbox_arrow_down
  here: '\uf489',      // oct-terminal        this machine
  box: '\u{f048b}',    // md-server           another machine
  mac: '\ue711',       // dev-apple
  pi: '\u{f043f}',     // md-raspberry_pi
  cut: '\u{f0319}',    // md-lan_disconnect   no channel / no network
  got: '\u{f1161}',    // md-send_check       the peer's session took it
  held: '\u{f1163}',   // md-send_clock       queued, lands when the peer is back
  lost: '\u{f0156}',   // md-close            no inbox by that name
}

let me = { name: '', machine: '', cwd: '' }
let roster = []        // rows of `cp3 peers --json`
let mcpOwned = false   // the cp3 MCP server is part of this session → it holds the name and the inbox
let noChannel = false  // MCP server present but Claude launched without the dev-channel flag: its mail is never displayed
let subscriber = null  // the HookStream of the spawned `cp3 subscribe`
let inbox = []         // newest first: { from, content, at }
let lastError = ''
let draft = { to: '' }

export function register(on) {
  on('session.start', async ($, e, next) => {
    await identify($)
    // The MCP server connects AFTER session.start, so the tool list alone misses
    // it (seen 2026-10-10: a twin "name-omarchy" was claimed). cp3 whoami sees
    // the server's state file and the launch flag up the process tree; if
    // neither is there yet, wait once more before owning the inbox.
    if (!mcpOwned && me.name) {
      if (await mcpPresent($)) mcpOwned = true
      else $.clock.after(4000, async () => {
        if (await mcpPresent($)) mcpOwned = true
        else startSubscriber($)
        $.ui.invalidate('ui.render')
      })
    }
    await refresh($)
    $.clock.every(REFRESH_MS, () => refresh($))
    // Commands last: a taken name throws and would skip the rest of this hook.
    try {
      await $.command.register({
        name: 'cp3',
        description: 'cp3 peers by machine, mail waiting, and a field to message any of them',
        argumentHint: '[peer message…]',
        immediate: true,
      })
    } catch {}
    return next(e)
  })

  // /clear, /resume and /branch also fire session.end — and session.start does
  // NOT fire again after them. The process and its inbox socket are unchanged,
  // so the subscriber stays; only a real exit stops it.
  on('session.end', async ($, e, next) => {
    if (e.reason !== 'clear' && e.reason !== 'resume') stopSubscriber()
    return next(e)
  })

  // /cp3            → open the pane
  // /cp3 jim hello  → send "hello" to jim, print the delivery status
  on('command.run', { command: 'cp3' }, async ($, e) => {
    const args = (e.args ?? '').trim()
    if (!args) {
      const machines = new Set(roster.map((p) => p.machine || '')).size
      await $.ui.open({ id: PANE, title: 'cp3', focus: true, closeOnEscape: true, rows: Math.min(roster.length + machines * 2 + 8, 30) })
      return {}
    }
    const [to, ...rest] = args.split(/\s+/)
    if (rest.length === 0) return { text: 'usage: /cp3 <peer> <message>' }
    return { text: receipt(to, await send($, to, rest.join(' '))) }
  })

  // Claude's own SendMessage reaches a cp3 peer by name. A cp3 name wins over a
  // same-named local session: the fleet name is the invariant people manage.
  on('tool.call', { tool: 'SendMessage' }, async ($, e, next) => {
    if (typeof e.message !== 'string' || !isPeer(e.to)) return next(e)
    if (!me.name) return { deny: 'cp3: this session has no peer identity, so it cannot send to ' + e.to + ' over cp3.' }
    const status = await send($, e.to, e.message)
    // The engine requires an object here; a bare string makes the model retry
    // the call, and the peer gets the message twice (seen 2026-10-10).
    return { result: { via: 'cp3', to: e.to, from: me.name, status } }
  }).catch(async ($, e, next) => next(e)) // fail open to Claude Code's own delivery

  // Claude's own ListAgents also lists the fleet, so it never has to be told.
  on('tool.call', { tool: 'ListAgents' }, async ($, e, next) => {
    const r = await next(e)
    if (!r || r.deny || !r.result || typeof r.result.listing !== 'string' || roster.length === 0) return r
    const lines = roster.map((p) => '  ' + p.name + (p.up ? '' : '  ·  down') + (p.machine ? '  ·  ' + p.machine : '') + (p.summary ? '  ·  ' + p.summary : '') + (p.pending ? '  ·  ✉' + p.pending : ''))
    const listing = r.result.listing + '\n\ncp3 fleet peers (' + roster.length + ') — SendMessage reaches each by this exact name, on any machine:\n' + lines.join('\n')
    return { ...r, result: { ...r.result, listing } }
  }).catch(async ($, e, next) => next(e))

  // One line: am I on the network, under what name, how many are up, is there
  // mail. Nothing else unless something is wrong.
  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const theirs = await next(e)
    if (e.props.hasSurvey || !me.name) return theirs
    const { Box, Text } = $.ui.resolve(e)
    const self = roster.find((p) => p.name === me.name)
    const up = roster.filter((p) => p.up).length
    const machines = new Set(roster.filter((p) => p.up).map((p) => p.machine || '')).size
    const parts = [
      Box({
        flexDirection: 'row',
        columnGap: 1,
        children: [
          Text({ color: self ? 'success' : 'warning', children: [self ? G.up : G.away] }),
          Text({ color: 'claude', children: [me.name] }),
        ],
      }),
    ]
    if (self) parts.push(Text({ dimColor: true, children: [up + ' up' + (machines > 1 ? ' · ' + machines + ' machines' : '')] }))
    else parts.push(Text({ color: 'warning', children: ['not registered'] }))
    if (self && self.pending > 0) parts.push(Text({ color: 'warning', children: [G.mail + ' ' + self.pending] }))
    if (noChannel) parts.push(Text({ color: 'error', children: [G.cut + ' no channel · relaunch with cp3 run'] }))
    if (lastError) parts.push(Text({ color: 'error', children: [G.cut + ' ' + lastError] }))
    const band = Box({ flexDirection: 'row', columnGap: 2, paddingX: 1, children: parts })
    return theirs && theirs.type !== 'engine' ? Box({ flexDirection: 'column', children: [theirs, band] }) : band
  })

  // The pane: peers grouped by machine (this one first, no group when there is
  // only one), you first in your group, the ones that are down last and faint.
  // One row per peer: presence · name · mail · what it is doing.
  on('ui.render', { component: 'Pane' }, async ($, e, next) => {
    if (e.requestId !== PANE) return next(e)
    const { Box, Text, Input, Button } = $.ui.resolve(e)
    const upCount = roster.filter((p) => p.up).length
    const awayCount = roster.length - upCount
    const nameWidth = Math.max(4, ...roster.map((p) => p.name.length))

    const row = (p) => {
      const isMe = p.name === me.name
      const doing = isMe ? 'you' : p.summary || p.title || shortPath(p.cwd)
      return Box({
        flexDirection: 'row',
        columnGap: 1,
        children: [
          Text({ color: p.up ? (isMe ? 'claude' : 'success') : 'inactive', children: [p.up ? G.up : G.away] }),
          Box({
            width: nameWidth + 1,
            children: [Button({ key: 'to-' + p.name, label: p.name, plain: true, dimColor: !p.up, onPress: () => pick($, p.name) })],
          }),
          ...(p.pending ? [Text({ color: 'warning', children: [G.mail + ' ' + p.pending] })] : []),
          Text({ dimColor: true, wrap: 'truncate-end', children: [doing] }),
        ],
      })
    }

    const groups = byMachine(roster, me.machine)
    const body = []
    for (const g of groups) {
      if (groups.length > 1) {
        body.push(Box({
          flexDirection: 'row',
          columnGap: 1,
          marginTop: body.length ? 1 : 0,
          children: [
            Text({ dimColor: true, children: [machineGlyph(g.machine, me.machine)] }),
            Text({ dimColor: true, children: [g.machine || 'unknown machine'] }),
            ...(g.machine === me.machine ? [Text({ dimColor: true, children: ['· here'] })] : []),
          ],
        }))
      }
      body.push(...g.peers.map(row))
    }

    const recent = inbox.slice(0, 4).map((m) =>
      Box({
        flexDirection: 'row',
        columnGap: 1,
        children: [
          Box({ width: nameWidth + 1, children: [Text({ children: [m.from] })] }),
          Text({ dimColor: true, wrap: 'truncate-end', children: [firstLine(m.content)] }),
        ],
      }),
    )

    return Box({
      flexDirection: 'column',
      paddingX: 1,
      children: [
        Box({
          flexDirection: 'row',
          justifyContent: 'space-between',
          children: [
            Box({ flexDirection: 'row', columnGap: 1, children: [Text({ color: 'claude', children: [G.net] }), Text({ bold: true, children: ['cp3'] })] }),
            Text({ dimColor: true, children: [upCount + ' up' + (awayCount ? ' · ' + awayCount + ' away' : '')] }),
          ],
        }),
        ...(noChannel ? [Text({ color: 'error', children: [G.cut + ' no channel · this session was not launched with cp3 run, so mail to it is never shown'] })] : []),
        Box({ marginTop: 1, flexDirection: 'column', children: body }),
        Box({
          marginTop: 1,
          children: [
            Input({
              key: 'send',
              autoFocus: true,
              label: G.send + (draft.to ? ' to ' + draft.to : ''),
              placeholder: draft.to ? 'message' : 'name message',
              value: '',
              submitLabel: 'send',
              onSubmit: async (value) => {
                const text = value.trim()
                if (!text) return
                let to = draft.to
                let body = text
                if (!to) {
                  const sp = text.indexOf(' ')
                  if (sp < 0) return
                  to = text.slice(0, sp)
                  body = text.slice(sp + 1)
                }
                const status = await send($, to, body)
                $.ui.toast(receipt(to, status))
                draft.to = ''
                $.ui.invalidate('ui.render')
              },
            }),
          ],
        }),
        ...(recent.length
          ? [
              Box({ marginTop: 1, flexDirection: 'row', columnGap: 1, children: [Text({ dimColor: true, children: [G.inbox] }), Text({ dimColor: true, children: ['inbox'] })] }),
              ...recent,
            ]
          : []),
      ],
    })
  })
}

// cp3 send says one of three things; the person needs one glyph and the name.
// "delivered" means a live session consumed it — that is the received mark.
// Anything else is shown whole: it is an error, and errors are not shortened.
function receipt(to, status) {
  if (/^delivered/.test(status)) return G.got + ' ' + to
  if (/^queued/.test(status)) return G.held + ' ' + to + ' · queued, lands when it is back'
  if (/^NOT DELIVERED/.test(status)) return G.lost + ' ' + to + ' · no inbox by that name'
  return status
}

function pick($, name) {
  draft.to = draft.to === name ? '' : name
  $.ui.invalidate('ui.render')
}

// Peers grouped by machine: this machine first, the rest by name. Inside a
// group: you, then the live peers by name, then the ones that are down.
function byMachine(rows, here) {
  const m = new Map()
  for (const p of rows) {
    const k = p.machine || ''
    if (!m.has(k)) m.set(k, [])
    m.get(k).push(p)
  }
  const rank = (p) => (p.name === (me.name) ? 0 : p.up ? 1 : 2)
  const order = (a, b) => rank(a) - rank(b) || byName(a, b)
  return [...m.entries()]
    .sort(([a], [b]) => (a === here ? -1 : b === here ? 1 : a < b ? -1 : a > b ? 1 : 0))
    .map(([machine, peers]) => ({ machine, peers: peers.sort(order) }))
}

// ponytail: the fleet's hostnames say what they are; a real hardware field on
// the peer record replaces this map the day one does not.
function machineGlyph(machine, here) {
  if (machine === here) return G.here
  if (/mac/i.test(machine)) return G.mac
  if (/pi|rasp/i.test(machine)) return G.pi
  return G.box
}

function firstLine(content) {
  return content.split('\n').find((l) => l.trim() && !l.startsWith('[')) ?? ''
}

function byName(a, b) {
  return a.name < b.name ? -1 : a.name > b.name ? 1 : 0
}

async function identify($) {
  try {
    const r = await $.process.run(['cp3', 'whoami', '--json'], { timeoutMs: 5000 })
    if (r.exitCode === 0) {
      const w = JSON.parse(r.stdout)
      me = { name: w.name ?? '', machine: w.machine ?? '', cwd: w.cwd ?? '' }
      if (w.mcp || w.channelFlag) mcpOwned = true
      noChannel = Boolean(w.mcp && w.mcp.noChannel)
    }
  } catch (err) {
    lastError = 'whoami failed'
  }
}

// Is the cp3 MCP server part of this session? Either its tools have appeared,
// or cp3 whoami sees its state / the launch flag up the process tree.
async function mcpPresent($) {
  try {
    const tools = await $.tool.list()
    if (tools.some((t) => t.name.startsWith('mcp__claude-peers__'))) return true
  } catch {}
  await identify($)
  return mcpOwned
}

async function refresh($) {
  try {
    const r = await $.process.run(['cp3', 'peers', '--json'], { timeoutMs: 8000 })
    if (r.exitCode !== 0) {
      lastError = 'no network'
    } else {
      roster = JSON.parse(r.stdout) ?? []
      lastError = ''
    }
  } catch {
    lastError = 'no network'
  }
  $.ui.invalidate('ui.render')
}

function isPeer(to) {
  return typeof to === 'string' && roster.some((p) => p.name === to)
}

async function send($, to, text) {
  try {
    const argv = ['cp3', 'send', '--to', to, text]
    if (me.name) argv.splice(2, 0, '--from', me.name)
    const r = await $.process.run(argv, { timeoutMs: 15000 })
    const out = (r.stdout + r.stderr).trim()
    return out || (r.exitCode === 0 ? 'sent to ' + to : 'cp3 send exited ' + r.exitCode)
  } catch (err) {
    return 'cp3 send failed: ' + String(err)
  }
}

// The subscriber holds the peer name and delivers mail into this session's
// inbox socket. Its stdout is a JSON line per event: a `hello` with the name
// it actually claimed, then one line per message delivered.
async function startSubscriber($) {
  await killStale($)
  const sid = await $.session.id()
  const stream = $.process.spawn({ argv: ['cp3', 'subscribe', '--deliver', 'claude-socket', '--session', 'mod-' + sid] })
  subscriber = stream
  let buf = ''
  try {
    for await (const chunk of stream) {
      if (chunk.stream !== 'stdout') {
        if (/name .* taken|error|refused/i.test(chunk.text)) lastError = chunk.text.trim().slice(0, 60)
        continue
      }
      buf += chunk.text
      let nl
      while ((nl = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, nl)
        buf = buf.slice(nl + 1)
        if (line.trim()) await onLine($, sid, line)
      }
    }
  } catch {
    // stream closed: session ended or module reloaded
  }
  if (subscriber === stream) {
    subscriber = null
    lastError = 'subscriber exited'
    $.ui.invalidate('ui.render')
  }
}

async function onLine($, sid, line) {
  let ev
  try {
    ev = JSON.parse(line)
  } catch {
    return
  }
  if (ev.type === 'hello') {
    me = { name: ev.agent, machine: ev.machine, cwd: ev.cwd }
    refresh($) // the roster now has us; do not wait a tick to say so
    return
  }
  if (ev.from && ev.content) {
    inbox = [{ from: ev.from, content: ev.content, at: Date.now() }, ...inbox].slice(0, INBOX_KEEP)
    refresh($)
  }
}

// A hot reload drops this module's variables but not the child it spawned; a
// surviving child would hold the name and deliver to a session that no longer
// draws it. The child is found by the session id in its argv — a pid could
// have been reused by an unrelated process after a crash, the id cannot.
async function killStale($) {
  try {
    const sid = await $.session.id()
    await $.process.run(['pkill', '-f', '--', 'cp3 subscribe .*--session mod-' + sid], { timeoutMs: 3000 })
  } catch {}
}

function stopSubscriber() {
  const s = subscriber
  subscriber = null
  if (s && typeof s.return === 'function') {
    try { s.return() } catch {}
  }
}

function shortPath(p) {
  if (!p) return ''
  const parts = p.split('/')
  return parts.length > 2 ? '…/' + parts.slice(-2).join('/') : p
}
