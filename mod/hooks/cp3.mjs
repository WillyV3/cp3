// cp3 mod — the cp3 peer network inside Claude Code.
//
// What it adds:
//   • a band above the prompt: ● <me> · N peers · ✉k, live
//   • /cp3 — a pane with the fleet roster and a field to message any peer
//   • SendMessage / ListAgents, Claude's own cross-session tools, reach every
//     cp3 peer on the fleet by name (the native feature only spans one machine
//     without Anthropic's servers; cp3 is the self-hosted transport for the rest)
//   • inbound mail lands as a native cross-session message, mid-turn, with no
//     --dangerously-load-development-channels flag: the subscriber posts it to
//     this session's inbox socket
//
// The cp3 binary is the one wall: this file never speaks NATS or parses cp3's
// internals. It runs `cp3 whoami`, `cp3 peers --json`, `cp3 send`, and
// supervises `cp3 subscribe --deliver claude-socket`.
//
// Identity: when the session already runs the cp3 MCP server (`cp3 run`), that
// server owns the name and the inbox and this mod only draws and bridges —
// otherwise two subscribers would split one inbox and the second would get a
// machine-suffixed twin name. Without MCP, this mod owns both.

const PANE = 'cp3'
const REFRESH_MS = 5000
const INBOX_KEEP = 20

let me = { name: '', machine: '', cwd: '' }
let roster = []        // rows of `cp3 peers --json`
let mcpOwned = false   // mcp__claude-peers__* tools present → the MCP server holds the name
let subscriber = null  // the HookStream of the spawned `cp3 subscribe`
let inbox = []         // newest first: { from, content, at }
let lastError = ''
let draft = { to: '' }

export function register(on) {
  on('session.start', async ($, e, next) => {
    await identify($)
    const tools = await $.tool.list()
    mcpOwned = tools.some((t) => t.name.startsWith('mcp__claude-peers__'))
    if (!mcpOwned && me.name) startSubscriber($)
    await refresh($)
    $.clock.every(REFRESH_MS, () => refresh($))
    // Commands last: a taken name throws and would skip the rest of this hook.
    try {
      await $.command.register({
        name: 'cp3',
        description: 'cp3 peers: the fleet roster, your inbox, and a field to message any peer',
        argumentHint: '[peer message…]',
        immediate: true,
      })
    } catch {}
    return next(e)
  })

  on('session.end', async ($, e, next) => {
    stopSubscriber()
    return next(e)
  })

  // /cp3            → open the pane
  // /cp3 jim hello  → send "hello" to jim, print the delivery status
  on('command.run', { command: 'cp3' }, async ($, e) => {
    const args = (e.args ?? '').trim()
    if (!args) {
      await $.ui.open({ id: PANE, title: 'cp3 peers', focus: true, closeOnEscape: true, rows: Math.min(roster.length + 6, 24) })
      return {}
    }
    const [to, ...rest] = args.split(/\s+/)
    if (rest.length === 0) return { text: 'usage: /cp3 <peer> <message>' }
    return { text: await send($, to, rest.join(' ')) }
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

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const theirs = await next(e)
    if (e.props.hasSurvey || !me.name) return theirs
    const { Box, Text } = $.ui.resolve(e)
    const self = roster.find((p) => p.name === me.name)
    const mine = self ? '● ' : '○ '
    const parts = [
      Text({ color: self ? 'green' : 'yellow', children: [mine] }),
      Text({ bold: true, children: [me.name] }),
      Text({ dimColor: true, children: [self ? ' · ' + roster.filter((p) => p.up).length + ' peers' : ' · not registered'] }),
    ]
    if (self && self.pending > 0) parts.push(Text({ color: 'yellow', children: [' · ✉' + self.pending] }))
    if (inbox.length > 0) parts.push(Text({ dimColor: true, children: [' · last: ' + inbox[0].from] }))
    if (lastError) parts.push(Text({ color: 'red', children: [' · ' + lastError] }))
    const band = Box({ flexDirection: 'row', paddingX: 1, children: parts })
    return theirs && theirs.type !== 'engine' ? Box({ flexDirection: 'column', children: [theirs, band] }) : band
  })

  on('ui.render', { component: 'Pane' }, async ($, e, next) => {
    if (e.requestId !== PANE) return next(e)
    const { Box, Text, Input, Button } = $.ui.resolve(e)
    const width = e.props.bodyColumns ?? 80
    const rows = roster.map((p) =>
      Box({
        flexDirection: 'row',
        columnGap: 1,
        children: [
          Text({ color: p.up ? 'green' : 'red', children: [p.up ? '●' : '○'] }),
          Button({
            key: 'to-' + p.name,
            label: p.name,
            plain: true,
            onPress: () => {
              draft.to = p.name
              $.ui.invalidate('ui.render')
            },
          }),
          Text({ dimColor: true, wrap: 'truncate', children: [[p.machine, shortPath(p.cwd), p.summary].filter(Boolean).join(' · ')] }),
          ...(p.pending ? [Text({ color: 'yellow', children: ['✉' + p.pending] })] : []),
        ],
      }),
    )
    const recent = inbox.slice(0, 5).map((m) =>
      Text({ wrap: 'truncate', children: [m.from + ': ' + m.content.split('\n')[0]] }),
    )
    return Box({
      flexDirection: 'column',
      children: [
        Text({ dimColor: true, children: [(mcpOwned ? 'identity: cp3 mcp' : 'identity: this mod') + ' · ' + roster.filter((p) => p.up).length + ' up · ' + (me.machine || '')] }),
        ...rows,
        Text({ children: [' '] }),
        Input({
          key: 'send',
          label: draft.to ? 'to ' + draft.to : 'to <peer> message',
          placeholder: draft.to ? 'message, Enter sends' : 'peer message  (or press a name above)',
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
            $.ui.toast('cp3: ' + status)
            draft.to = ''
            $.ui.invalidate('ui.render')
          },
        }),
        ...(recent.length ? [Text({ children: [' '] }), Text({ dimColor: true, children: ['recent inbound'] }), ...recent] : []),
        ...(width < 60 ? [Text({ dimColor: true, children: ['(narrow)'] })] : []),
      ],
    })
  })
}

async function identify($) {
  try {
    const r = await $.process.run(['cp3', 'whoami', '--json'], { timeoutMs: 5000 })
    if (r.exitCode === 0) {
      const w = JSON.parse(r.stdout)
      me = { name: w.name ?? '', machine: w.machine ?? '', cwd: w.cwd ?? '' }
    }
  } catch (err) {
    lastError = 'whoami failed'
  }
}

async function refresh($) {
  try {
    const r = await $.process.run(['cp3', 'peers', '--json'], { timeoutMs: 8000 })
    if (r.exitCode !== 0) {
      lastError = 'peers down'
    } else {
      roster = JSON.parse(r.stdout) ?? []
      lastError = ''
    }
  } catch {
    lastError = 'peers down'
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
    await $.store.set('subscriber:' + sid, ev.pid)
    $.ui.invalidate('ui.render')
    return
  }
  if (ev.from && ev.content) {
    inbox = [{ from: ev.from, content: ev.content, at: Date.now() }, ...inbox].slice(0, INBOX_KEEP)
    refresh($)
  }
}

// A hot reload drops this module's variables but not the child it spawned; a
// surviving child would hold the name and deliver to a session that no longer
// draws it. The pid from the hello line is kept per session so the next
// instance can end it first.
async function killStale($) {
  try {
    const sid = await $.session.id()
    const pid = await $.store.get('subscriber:' + sid)
    if (typeof pid === 'number') {
      await $.process.run(['kill', String(pid)], { timeoutMs: 3000 })
      await $.store.delete('subscriber:' + sid)
    }
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
