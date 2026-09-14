// End-to-end tests for the server jar's in-game commands (/hub, /server, /global),
// which are implemented at the Netty layer with no platform plugin. Run after clients.cjs.
//   GW_HOST=<docker host> node test/e2e/commands.cjs
'use strict'
const mc = require('minecraft-protocol')
const net = require('net')
const { execFileSync } = require('child_process')

const GW_HOST = process.env.GW_HOST || '10.10.25.155'
const GW_PORT = Number(process.env.GW_PORT || 35565)
const PROJECT = process.env.PROJECT || 'vecta-test'
const DOCKER = process.env.DOCKER || 'docker'
const container = (svc) => `${PROJECT}-${svc}-1`
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

function conn (host, version, username) {
  return mc.createClient({ host, port: 25565, version, username, auth: 'offline', connect: (c) => c.setSocket(net.connect(GW_PORT, GW_HOST)) })
}
// Send a command: chat_command on 1.19+, plain chat "/cmd" on older versions.
function sendCommand (client, version, command) {
  const major = Number(version.split('.')[1])
  if (major >= 19) client.write('chat_command', { command, timestamp: Date.now(), salt: 0, argumentSignatures: [], messageCount: 0, acknowledged: Buffer.alloc(3), checksum: 0 })
  else client.write('chat', { message: '/' + command })
}
function text (p) { // extract readable text from a chat/system_chat/disconnect payload
  return JSON.stringify(p).replace(/\\"/g, '"')
}

const results = []
async function scenario (name, fn) {
  try { const d = await fn(); results.push({ name, ok: true }); console.log(`PASS  ${name}  ${d || ''}`) } catch (e) { results.push({ name, ok: false }); console.log(`FAIL  ${name}  ${e.message}`) }
}
function expect (c, m) { if (!c) throw new Error(m) }

// Waits for `event` on client carrying text matching re, up to timeoutMs.
function waitText (client, events, re, timeoutMs = 8000) {
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error('timeout waiting for ' + re)), timeoutMs)
    for (const ev of events) client.on(ev, (p) => { const s = text(p); if (re.test(s)) { clearTimeout(t); resolve(s) } })
    client.on('error', () => {})
  })
}
function onSpawn (client) { return new Promise((res, rej) => { client.on('login', res); client.on('error', rej); setTimeout(() => rej(new Error('never spawned')), 15000) }) }

async function main () {
  execFileSync(DOCKER, ['exec', container('mc-paper-via'), 'rcon-cli', 'op', 'OpBot'], { stdio: 'ignore' })

  await scenario('C1 /server via chat_command on 1.21.4 -> store_cookie + transfer', async () => {
    const c = conn('paper-via.play.test', '1.21.4', 'CmdA')
    await onSpawn(c)
    let cookie = false; let xfer = null
    c.on('store_cookie', (p) => { if (p.key === 'vecta:route') cookie = true })
    c.on('transfer', (p) => { xfer = p })
    await sleep(1600); sendCommand(c, '1.21.4', 'server paper-via')
    await sleep(2000); c.end()
    expect(cookie && xfer && xfer.port === 35565, `cookie=${cookie} transfer=${JSON.stringify(xfer)}`)
    return 'cookie + transfer to gateway'
  })

  await scenario('C2 /hub on 1.8.8 -> reconnect kick to lobby', async () => {
    const c = conn('paper-legacy.play.test', '1.8.8', 'CmdB')
    await onSpawn(c)
    const kick = waitText(c, ['kick_disconnect'], /Reconnect to lobby/)
    await sleep(1600); sendCommand(c, '1.8.8', 'hub')
    await kick
    return 'kicked with reconnect message'
  })

  await scenario('C3 /global from an operator broadcasts to other players', async () => {
    const recv = conn('paper-via.play.test', '1.21.4', 'RecvBot')
    await onSpawn(recv)
    const got = waitText(recv, ['system_chat'], /\[G\] OpBot.*hello/, 12000)
    await sleep(1600) // let the receiver's interceptor attach
    const op = conn('paper-via.play.test', '1.21.4', 'OpBot')
    await onSpawn(op)
    await sleep(1600); sendCommand(op, '1.21.4', 'global hello world')
    await got; recv.end(); op.end()
    return 'receiver got the broadcast'
  })

  await scenario('C4 /global from a non-operator is refused', async () => {
    const c = conn('paper-via.play.test', '1.21.4', 'NoOp')
    await onSpawn(c)
    const err = waitText(c, ['system_chat'], /must be a server operator/)
    await sleep(1600); sendCommand(c, '1.21.4', 'global nope')
    await err; c.end()
    return 'refused with operator message'
  })

  const failed = results.filter((r) => !r.ok).length
  console.log(`\n${results.length - failed}/${results.length} command scenarios passed`)
  process.exit(failed ? 1 : 0)
}
main().catch((e) => { console.error(e); process.exit(2) })
