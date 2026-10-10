import { expect, mock, test } from 'claude-code/testing'

// What `cp3 peers --json` answers in these tests.
const ROSTER = JSON.stringify([
  { name: 'keeper', up: true, pending: 0, machine: 'omarchy', cwd: '/home/willy/projects/claude-peers' },
  { name: 'config', up: true, pending: 2, machine: 'omarchy', cwd: '/home/willy/.config', summary: 'fleet config' },
  { name: 'inspiron-omarchy-main', up: false, pending: 1, machine: 'inspiron-omarchy' },
])

// Stubs every cp3 call the mod makes and records the argv of each one, so a
// test can assert what was run. tool.list reports the MCP server present, so
// the mod does not spawn a subscriber (a stream a test cannot stub).
function stubCp3(on, { runs, send }: { runs: string[][]; send?: { exitCode: number; stdout: string; stderr: string } }) {
  on('process.run', ($, e) => {
    runs.push([...e.argv])
    const [, cmd] = e.argv
    if (cmd === 'send' && send) return { value: send }
    if (cmd === 'whoami') return { value: { exitCode: 0, stdout: '{"name":"mod-test","machine":"omarchy","cwd":"/w"}\n', stderr: '' } }
    if (cmd === 'peers') return { value: { exitCode: 0, stdout: ROSTER, stderr: '' } }
    if (cmd === 'send') return { value: { exitCode: 0, stdout: 'delivered to ' + e.argv[e.argv.indexOf('--to') + 1] + ' (live session consuming now)\n', stderr: '' } }
    return { value: { exitCode: 1, stdout: '', stderr: 'unexpected: ' + e.argv.join(' ') } }
  })
  on('tool.list', () => ({ value: [{ name: 'mcp__claude-peers__send_message', description: '', mcp: true }] }))
  on('command.register', () => ({ value: undefined }))
  on('session.start', () => ({ cwd: '/w' }))
  mock.clock(on)
}

async function start($) {
  await $.session.start({ surface: 'terminal', isInteractive: true, cwd: '/w' })
}

test('SendMessage to a cp3 peer goes over cp3 and answers with an object', async ($, on) => {
  const runs: string[][] = []
  stubCp3(on, { runs })
  on('tool.call', () => ({ result: { native: true } })) // Claude Code's own delivery, must NOT be reached
  await start($)

  const out = await $.tool.call({ tool: 'SendMessage', to: 'config', message: 'hello from the test' })

  const send = runs.find((a) => a[1] === 'send')
  expect(send).toEqual(['cp3', 'send', '--from', 'mod-test', '--to', 'config', 'hello from the test'])
  // A string result makes the engine reject the call and the model retry it (double delivery, 2026-10-10).
  expect(typeof out.result).toBe('object')
  expect(out.result).toMatchObject({ via: 'cp3', to: 'config', from: 'mod-test' })
  expect(out.result.status).toMatch(/^delivered to config/)
})

test('SendMessage to a name that is not a cp3 peer is left to Claude Code', async ($, on) => {
  const runs: string[][] = []
  stubCp3(on, { runs })
  on('tool.call', () => ({ result: { native: true } }))
  await start($)

  const out = await $.tool.call({ tool: 'SendMessage', to: 'some-local-session', message: 'hi' })

  expect(out).toEqual({ result: { native: true } })
  expect(runs.some((a) => a[1] === 'send')).toBe(false)
})

test('a structured SendMessage (shutdown_request) is never routed over cp3', async ($, on) => {
  const runs: string[][] = []
  stubCp3(on, { runs })
  on('tool.call', () => ({ result: { native: true } }))
  await start($)

  const out = await $.tool.call({ tool: 'SendMessage', to: 'config', message: { type: 'shutdown_request' } })

  expect(out).toEqual({ result: { native: true } })
  expect(runs.some((a) => a[1] === 'send')).toBe(false)
})

test('ListAgents keeps Claude Code\'s listing and appends the fleet', async ($, on) => {
  const runs: string[][] = []
  stubCp3(on, { runs })
  on('tool.call', () => ({ result: { listing: 'Peer sessions (1):\n  other-terminal [ab12]', sections: [] } }))
  await start($)

  const out = await $.tool.call({ tool: 'ListAgents' })

  expect(out.result.listing).toMatch(/^Peer sessions \(1\):\n  other-terminal \[ab12\]/)
  expect(out.result.listing).toContain('cp3 fleet peers (3)')
  expect(out.result.listing).toContain('  keeper  ·  omarchy')
  expect(out.result.listing).toContain('  config  ·  omarchy  ·  fleet config  ·  ✉2')
  expect(out.result.listing).toContain('  inspiron-omarchy-main  ·  down  ·  inspiron-omarchy  ·  ✉1')
})

test('/cp3 <peer> <message> sends and prints the delivery status', async ($, on) => {
  const runs: string[][] = []
  stubCp3(on, { runs })
  await start($)

  const answer = await $.command.run({ command: 'cp3', args: 'keeper ping from the pane' })

  expect(answer.text).toBe('\u{f1161} keeper') // the receipt, not cp3's sentence
  expect(runs.find((a) => a[1] === 'send')).toEqual(['cp3', 'send', '--from', 'mod-test', '--to', 'keeper', 'ping from the pane'])
})

test('/cp3 to a peer whose inbox is gone says so in full', async ($, on) => {
  const runs: string[][] = []
  stubCp3(on, { runs, send: { exitCode: 1, stdout: '', stderr: 'NOT DELIVERED to ghost: no inbox by that name\n' } })
  await start($)

  const answer = await $.command.run({ command: 'cp3', args: 'ghost hello' })

  expect(answer.text).toBe('\u{f0156} ghost · no inbox by that name')
})

test('/cp3 with a peer but no message explains the usage instead of sending', async ($, on) => {
  const runs: string[][] = []
  stubCp3(on, { runs })
  await start($)

  const answer = await $.command.run({ command: 'cp3', args: 'keeper' })

  expect(answer.text).toBe('usage: /cp3 <peer> <message>')
  expect(runs.some((a) => a[1] === 'send')).toBe(false)
})

const BAND = {
  plugin: 'cp3',
  component: 'AbovePrompt',
  requestId: 'AbovePrompt',
  viewport: { columns: 120, rows: 40 },
  props: { hasSurvey: false, isWorking: false, maxRows: 5, bodyColumns: 100, scroll: { offset: 0, bodyRows: 5 }, view: {} },
} as const

test('the band names this peer and counts the peers that are up', async ($, on) => {
  const runs: string[][] = []
  stubCp3(on, { runs })
  on('ui.render', () => ({ type: 'Text', props: {}, children: ['drawn by Claude Code'] }))
  await start($)

  // mod-test is not on the roster, so the band says so rather than claiming presence
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: 'mod-test' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'not registered' })).toBeDefined()
  await ui.unmount()
})

const PANE = {
  plugin: 'cp3',
  component: 'Pane',
  requestId: 'cp3',
  viewport: { columns: 120, rows: 40 },
  props: { title: 'cp3', isFocused: true, bodyColumns: 90, placement: 'dock', scroll: { offset: 0, bodyRows: 30 }, view: {} },
} as const

test('the pane groups peers by machine, this machine first, when the roster spans more than one', async ($, on) => {
  const runs: string[][] = []
  stubCp3(on, { runs })
  on('ui.render', () => ({ type: 'Text', props: {}, children: ['drawn by Claude Code'] }))
  await start($)

  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: 'omarchy' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: '· here' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'inspiron-omarchy' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: '2 up · 1 away' })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: 'fleet config' })).toBeDefined()
  await ui.unmount()
})
