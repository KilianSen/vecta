// End-to-end scenarios driven by real protocol clients (node-minecraft-protocol)
// against the gateway and real servers from compose.yaml.
//   NODE_PATH=<dir with minecraft-protocol> node test/e2e/clients.cjs
'use strict'

const mc = require('minecraft-protocol')
const net = require('net')
const { execSync } = require('child_process')

const GW_HOST = process.env.GW_HOST || '10.10.25.155'
const GW_PORT = Number(process.env.GW_PORT || 35565)
const API = process.env.API || `http://${GW_HOST}:38080`
const PROJECT = process.env.PROJECT || 'anymcp-test'
const DOCKER = process.env.DOCKER || 'docker'

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const container = (svc) => `${PROJECT}-${svc}-1`

function docker (args) {
  return execSync(`${DOCKER} ${args}`, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
}

function mcString (s) {
  const b = Buffer.from(s, 'utf8')
  const len = []
  let n = b.length
  do {
    let byte = n & 0x7f
    n >>>= 7
    if (n) byte |= 0x80
    len.push(byte)
  } while (n)
  return Buffer.concat([Buffer.from(len), b])
}

// connectOnce performs one connection and resolves with what happened:
// spawned | transfer | kicked | ended.
function connectOnce (o) {
  return new Promise((resolve) => {
    const res = { outcome: 'ended', reason: '', dialogIds: [] }
    let done = false
    const finish = (extra) => {
      if (done) return
      done = true
      Object.assign(res, extra)
      clearTimeout(timer)
      try { client.end() } catch {}
      resolve(res)
    }
    const timer = setTimeout(() => finish({ outcome: 'timeout' }), o.timeout || 45000)

    const client = mc.createClient({
      version: o.version,
      username: o.username,
      auth: 'offline',
      host: o.host || 'play.test',
      port: 25565,
      hideErrors: true,
      connect: (c) => c.setSocket(net.connect(o.tcpPort || GW_PORT, o.tcpHost || GW_HOST))
    })
    if (o.intent === 3) {
      const write = client.write.bind(client)
      client.write = (name, params) => write(name, name === 'set_protocol' ? { ...params, nextState: 3 } : params)
    }

    client.on('cookie_request', (p) => {
      res.cookieRequested = true
      client.write('cookie_response', { key: p.cookie, value: o.cookies?.[p.cookie] })
    })
    client.on('success', () => {
      setImmediate(() => {
        if (o.brand) client.write('custom_payload', { channel: 'minecraft:brand', data: mcString(o.brand) })
        if (o.channels) client.write('custom_payload', { channel: 'minecraft:register', data: Buffer.from(o.channels.join('\0')) })
      })
    })
    client.on('store_cookie', (p) => { o.cookies[p.key] = p.value })
    client.on('transfer', (p) => finish({ outcome: 'transfer', transfer: { host: p.host, port: p.port } }))
    client.on('show_dialog', (p) => {
      res.dialogIds = [...new Set(JSON.stringify(p).match(/anymcp:join\/[a-z0-9-]+/g) || [])]
      const choice = o.chooseDialog?.(res.dialogIds)
      if (choice) client.write('custom_click_action', { id: choice, nbt: undefined })
    })
    client.on('login', () => {
      res.spawned = true
      setTimeout(() => finish({ outcome: 'spawned' }), 1500)
    })
    client.on('disconnect', (p) => finish({ outcome: res.spawned ? 'spawned' : 'kicked', reason: JSON.stringify(p.reason) }))
    client.on('kick_disconnect', (p) => finish({ outcome: res.spawned ? 'spawned' : 'kicked', reason: JSON.stringify(p.reason) }))
    client.on('error', (e) => { res.error = e.message })
    client.on('end', (r) => finish({ outcome: res.spawned ? 'spawned' : 'ended', reason: res.reason || String(r || '') }))
  })
}

// join follows transfers like a vanilla client would.
async function join (o) {
  const cookies = {}
  const hops = []
  let cur = { ...o, cookies }
  for (let i = 0; i < 3; i++) {
    const r = await connectOnce(cur)
    hops.push(r)
    if (r.outcome !== 'transfer') break
    cur = { ...o, cookies, intent: 3, host: r.transfer.host, tcpHost: r.transfer.host, tcpPort: r.transfer.port }
  }
  return { final: hops[hops.length - 1], hops }
}

async function api (path, init) {
  const r = await fetch(API + path, init)
  return r.headers.get('content-type')?.includes('json') ? r.json() : r.text()
}

async function waitFor (desc, fn, timeoutMs = 300000) {
  const start = Date.now()
  let last
  while (Date.now() - start < timeoutMs) {
    try {
      last = await fn()
      if (last) return last
    } catch (e) { last = e.message }
    await sleep(3000)
  }
  throw new Error(`timed out waiting for ${desc} (last: ${JSON.stringify(last)})`)
}

const onlineIds = async () => (await api('/api/v1/servers')).filter((s) => s.online).map((s) => s.id).sort()

function backendSawNow (svc, username) {
  return docker(`logs --since 10m ${container(svc)} 2>&1`).includes(username)
}

// Servers may log a player only when the connection ends, so poll briefly.
async function backendSaw (svc, username, timeoutMs = 15000) {
  const start = Date.now()
  while (Date.now() - start < timeoutMs) {
    if (backendSawNow(svc, username)) return true
    await sleep(1000)
  }
  return false
}

// backendLine returns the most recent log line of svc mentioning username.
function backendLine (svc, username) {
  return docker(`logs --since 10m ${container(svc)} 2>&1`).split('\n').findLast((l) => l.includes(username)) || ''
}

function gatewayLog () {
  return docker(`logs --since 30m ${container('gateway')} 2>&1`)
}

// ---------------------------------------------------------------------------

const results = []
async function scenario (name, fn) {
  const t0 = Date.now()
  try {
    const detail = await fn()
    results.push({ name, ok: true, detail, ms: Date.now() - t0 })
    console.log(`PASS  ${name}  ${detail || ''}`)
  } catch (e) {
    results.push({ name, ok: false, detail: e.message, ms: Date.now() - t0 })
    console.log(`FAIL  ${name}  ${e.message}`)
  }
}

function expect (cond, msg) {
  if (!cond) throw new Error(msg)
}

function describe (r) {
  return r.hops.map((h) => h.outcome + (h.transfer ? `(${h.transfer.host}:${h.transfer.port})` : '')).join(' -> ')
}

async function main () {
  const all = ['fabric-pack', 'neo-pack', 'paper-a', 'paper-via', 'vanilla-old']
  console.log('waiting for all 5 servers to be registered and online...')
  await waitFor('all servers online', async () => JSON.stringify(await onlineIds()) === JSON.stringify(all), 600000)

  await scenario('S1 status ping lists all servers', async () => {
    const st = await mc.ping({ host: 'play.test', port: 25565, version: '1.21.11', connect: (c) => c.setSocket(net.connect(GW_PORT, GW_HOST)) })
    const names = st.players.sample.map((s) => s.name).join(' | ')
    expect(st.version.protocol === 774, `protocol ${st.version.protocol}`)
    expect(st.players.sample.length === 5, `sample: ${names}`)
    return names.replace(/§./g, '')
  })

  await scenario('S2 1.21.11 vanilla: ambiguous -> dialog -> pick paper-a -> transfer -> cookie -> spawn', async () => {
    const r = await join({ version: '1.21.11', username: 'dialog774', brand: 'vanilla', chooseDialog: (ids) => ids.includes('anymcp:join/paper-a') && 'anymcp:join/paper-a' })
    expect(JSON.stringify(r.hops[0].dialogIds.sort()) === JSON.stringify(['anymcp:join/paper-a', 'anymcp:join/paper-via']), `dialog ids ${r.hops[0].dialogIds}`)
    expect(r.hops[1]?.cookieRequested, 'no cookie request after transfer')
    expect(r.final.outcome === 'spawned', `hops ${describe(r)} ${r.final.reason}`)
    expect(await backendSaw('mc-paper-a', 'dialog774'), 'paper-a did not see player')
    // paper-a gets PROXY protocol v2: it must log the real client address, not
    // the gateway container's docker network address.
    const line = backendLine('mc-paper-a', 'dialog774[/')
    const ip = (line.match(/dialog774\[\/([0-9.]+):/) || [])[1]
    expect(ip && !/^172\.(1[6-9]|2[0-9]|3[01])\./.test(ip), `paper-a saw ${ip || 'no address'}: ${line}`)
    return `${describe(r)}; paper-a saw client ${ip}`
  })

  await scenario('S3 1.21.4 unknown client: single candidate -> direct to paper-via', async () => {
    const r = await join({ version: '1.21.4', username: 'direct769' })
    expect(r.hops.length === 1 && r.final.outcome === 'spawned', `hops ${describe(r)} ${r.final.reason}`)
    expect(await backendSaw('mc-paper-via', 'direct769'), 'paper-via did not see player')
    return describe(r)
  })

  await scenario('S4 1.21.1 fabric client with appleskin -> auto-match fabric-pack', async () => {
    const r = await join({ version: '1.21.1', username: 'fabric767', brand: 'fabric', channels: ['appleskin:sync', 'fabric:registry/sync'] })
    expect(r.hops[0].outcome === 'transfer', `first hop ${r.hops[0].outcome} ${r.hops[0].reason}`)
    expect(r.final.outcome === 'spawned', `hops ${describe(r)} ${r.final.reason}`)
    expect(await backendSaw('mc-fabric', 'fabric767'), 'fabric server did not see player')
    return describe(r)
  })

  await scenario('S5 1.21.1 vanilla client -> lobby -> auto-match paper-via (packs excluded)', async () => {
    const r = await join({ version: '1.21.1', username: 'vanilla767', brand: 'vanilla' })
    expect(r.hops[0].outcome === 'transfer' && r.final.outcome === 'spawned', `hops ${describe(r)} ${r.final.reason}`)
    expect(await backendSaw('mc-paper-via', 'vanilla767'), 'paper-via did not see player')
    return describe(r)
  })

  await scenario('S6 1.20.1 client: ambiguous legacy -> address list', async () => {
    const r = await join({ version: '1.20.1', username: 'legacy763' })
    expect(r.final.outcome === 'kicked', `outcome ${r.final.outcome}`)
    expect(r.final.reason.includes('vanilla-old.play.test') && r.final.reason.includes('paper-via.play.test'), r.final.reason)
    return 'kick lists vanilla-old + paper-via addresses'
  })

  await scenario('S7 1.20.1 via subdomain vanilla-old.play.test -> spawn on vanilla 1.20.1', async () => {
    const r = await join({ version: '1.20.1', username: 'sub763', host: 'vanilla-old.play.test' })
    expect(r.final.outcome === 'spawned', `hops ${describe(r)} ${r.final.reason}`)
    expect(await backendSaw('mc-vanilla-old', 'sub763'), 'vanilla-old did not see player')
    return describe(r)
  })

  await scenario('S8 1.21.1 via subdomain neo-pack.play.test -> routed to NeoForge server', async () => {
    // A plain client cannot finish NeoForge's configuration-phase negotiation,
    // so success means the gateway routed it and NeoForge received the login.
    const r = await join({ version: '1.21.1', username: 'neo767', host: 'neo-pack.play.test', timeout: 15000 })
    expect(/routing player.*player=neo767 server=neo-pack via=subdomain/.test(gatewayLog()), 'gateway did not route neo767 to neo-pack')
    expect(await backendSaw('mc-neoforge', 'neo767'), `neoforge did not see player (${describe(r)} ${r.final.reason})`)
    return `routed; NeoForge client-side outcome: ${describe(r)}`
  })

  await scenario('S9 1.19.4 client -> no compatible server message', async () => {
    const r = await join({ version: '1.19.4', username: 'tooold' })
    expect(r.final.outcome === 'kicked' && r.final.reason.includes('No online server accepts 1.19.4'), `${r.final.outcome} ${r.final.reason}`)
    return 'kicked with explanation'
  })

  await scenario('S10 lobby.play.test forces lobby for 1.21.4 -> transfer -> paper-via', async () => {
    const r = await join({ version: '1.21.4', username: 'lobby769', host: 'lobby.play.test' })
    expect(r.hops[0].outcome === 'transfer' && r.final.outcome === 'spawned', `hops ${describe(r)} ${r.final.reason}`)
    expect(await backendSaw('mc-paper-via', 'lobby769'), 'paper-via did not see player')
    return describe(r)
  })

  await scenario('S11 forged route cookie is rejected', async () => {
    const r = await connectOnce({ version: '1.21.4', username: 'forger', intent: 3, cookies: { 'anymcp:route': Buffer.from('v1|fabric-pack|forger|9999999999|AAAA') } })
    expect(r.cookieRequested, 'no cookie request')
    await sleep(2000)
    expect(!backendSawNow('mc-fabric', 'forger'), 'forged cookie reached fabric-pack')
    expect(gatewayLog().includes('bad route cookie signature'), 'gateway did not log rejection')
    return `first hop ${r.outcome}`
  })

  await scenario('S12 agent stop deregisters fabric-pack; fabric client falls back; restart re-registers', async () => {
    docker(`stop ${container('agent-d')}`)
    let r1
    try {
      await waitFor('fabric-pack gone', async () => !(await onlineIds()).includes('fabric-pack'), 30000)
      r1 = await join({ version: '1.21.1', username: 'fabricNoPack', brand: 'fabric', channels: ['appleskin:sync'] })
      expect(r1.final.outcome === 'spawned' && await backendSaw('mc-paper-via', 'fabricNoPack'), `fallback ${describe(r1)} ${r1.final.reason}`)
    } finally {
      docker(`start ${container('agent-d')}`)
    }
    await waitFor('fabric-pack back', async () => (await onlineIds()).includes('fabric-pack'), 60000)
    const r2 = await join({ version: '1.21.1', username: 'fabricBack', brand: 'fabric', channels: ['appleskin:sync'] })
    expect(r2.final.outcome === 'spawned' && await backendSaw('mc-fabric', 'fabricBack'), `after restart ${describe(r2)} ${r2.final.reason}`)
    return `without pack: ${describe(r1)}; after restart: ${describe(r2)}`
  })

  await scenario('S13 static server paper-a goes offline -> 1.21.11 routed directly to paper-via', async () => {
    docker(`stop ${container('mc-paper-a')}`)
    await waitFor('paper-a offline', async () => !(await onlineIds()).includes('paper-a'), 30000)
    const r = await join({ version: '1.21.11', username: 'afterStop', brand: 'vanilla' })
    docker(`start ${container('mc-paper-a')}`)
    expect(r.final.outcome === 'spawned' && await backendSaw('mc-paper-via', 'afterStop'), `${describe(r)} ${r.final.reason}`)
    return describe(r)
  })

  const failed = results.filter((r) => !r.ok)
  console.log(`\n${results.length - failed.length}/${results.length} scenarios passed`)
  console.log('RESULTS_JSON ' + JSON.stringify(results))
  process.exit(failed.length ? 1 : 0)
}

main().catch((e) => {
  console.error(e)
  process.exit(2)
})
