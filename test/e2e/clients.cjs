// End-to-end scenarios driven by real protocol clients (node-minecraft-protocol)
// against the gateway and real servers from compose.yaml. Normally run by
// test/e2e/run.sh; standalone:
//   npm ci --prefix test/e2e
//   GW_HOST=<docker host> node test/e2e/clients.cjs
// Backend logs are read through the local docker CLI (context = Docker host).
'use strict'

const mc = require('minecraft-protocol')
const net = require('net')
const { execSync, execFileSync } = require('child_process')

const GW_HOST = process.env.GW_HOST || '10.10.25.155'
const GW_PORT = Number(process.env.GW_PORT || 35565)
const API = process.env.API || `http://${GW_HOST}:38080`
const PROJECT = process.env.PROJECT || 'vecta-test'
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
      res.dialogIds = [...new Set(JSON.stringify(p).match(/vecta:join\/[a-z0-9-]+/g) || [])]
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

// limboChoose joins a pre-1.20.5 client, waits for the limbo menu, runs
// "/join <id>" and resolves with the disconnect reason.
function limboChoose (o) {
  return new Promise((resolve) => {
    const res = { menu: false, reason: '' }
    let done = false
    const finish = (extra) => {
      if (done) return
      done = true
      Object.assign(res, extra)
      clearTimeout(timer)
      try { client.end() } catch {}
      resolve(res)
    }
    const timer = setTimeout(() => finish({ reason: 'timeout' }), o.timeout || 45000)
    const client = mc.createClient({
      version: o.version,
      username: o.username,
      auth: 'offline',
      host: o.host || 'play.test',
      port: 25565,
      hideErrors: true,
      connect: (c) => c.setSocket(net.connect(GW_PORT, GW_HOST))
    })
    const onText = (text) => {
      if (res.menu || !text.includes('/join ' + o.choose)) return
      res.menu = true
      setTimeout(() => {
        if (client.state !== 'play') return
        if (['1.19.2', '1.19.3', '1.19.4', '1.20', '1.20.1', '1.20.2', '1.20.4'].includes(o.version)) {
          client.write('chat_command', { command: 'join ' + o.choose, timestamp: 0n, salt: 0n, argumentSignatures: [], messageCount: 0, acknowledged: Buffer.alloc(3) })
        } else {
          client.write('chat', { message: '/join ' + o.choose })
        }
      }, 500)
    }
    client.on('chat', (p) => onText(JSON.stringify(p)))
    client.on('system_chat', (p) => onText(JSON.stringify(p)))
    client.on('kick_disconnect', (p) => finish({ reason: JSON.stringify(p.reason) }))
    client.on('disconnect', (p) => finish({ reason: JSON.stringify(p.reason) }))
    client.on('error', (e) => { res.error = e.message })
    client.on('end', () => finish({}))
  })
}

// inGameCommand joins a backend through the gateway, runs a chat command once
// spawned, and resolves with what the server did: transfer (collecting route
// cookies), kicked, or timeout.
function inGameCommand (o) {
  return new Promise((resolve) => {
    const res = { outcome: 'ended', reason: '', cookies: {}, chat: '' }
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
      host: o.host,
      port: 25565,
      hideErrors: true,
      connect: (c) => c.setSocket(net.connect(GW_PORT, GW_HOST))
    })
    client.on('login', () => {
      res.spawned = true
      setTimeout(() => {
        if (o.version === '1.20.1') {
          client.write('chat_command', { command: o.command, timestamp: 0n, salt: 0n, argumentSignatures: [], messageCount: 0, acknowledged: Buffer.alloc(3) })
        } else {
          client.write('chat_command', { command: o.command })
        }
      }, 2000)
    })
    client.on('store_cookie', (p) => { res.cookies[p.key] = p.value })
    client.on('transfer', (p) => finish({ outcome: 'transfer', transfer: { host: p.host, port: p.port } }))
    client.on('system_chat', (p) => { res.chat += JSON.stringify(p) })
    client.on('kick_disconnect', (p) => finish({ outcome: 'kicked', reason: JSON.stringify(p.reason) }))
    client.on('disconnect', (p) => finish({ outcome: 'kicked', reason: JSON.stringify(p.reason) }))
    client.on('error', (e) => { res.error = e.message })
    client.on('end', () => finish({}))
  })
}

// followTransfers reconnects with the collected cookies like a vanilla client.
async function followTransfers (o, cookies, transfer) {
  const hops = []
  let t = transfer
  for (let i = 0; i < 3 && t; i++) {
    const r = await connectOnce({ ...o, cookies, intent: 3, host: t.host, tcpHost: t.host, tcpPort: t.port })
    hops.push(r)
    t = r.outcome === 'transfer' ? r.transfer : null
  }
  return hops
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
  const all = ['fabric-pack', 'forge-pack', 'neo-pack', 'paper-a', 'paper-legacy', 'paper-via', 'vanilla-old']
  console.log(`waiting for all ${all.length} servers to be registered and online...`)
  await waitFor('all servers online', async () => JSON.stringify(await onlineIds()) === JSON.stringify(all), 600000)

  await scenario('S1 status ping lists all servers', async () => {
    const st = await mc.ping({ host: 'play.test', port: 25565, version: '1.21.11', connect: (c) => c.setSocket(net.connect(GW_PORT, GW_HOST)) })
    const names = st.players.sample.map((s) => s.name).join(' | ')
    expect(st.version.protocol === 774, `protocol ${st.version.protocol}`)
    expect(st.players.sample.length === all.length, `sample: ${names}`)
    return names.replace(/§./g, '')
  })

  await scenario('J1 server jar registrations carry runtime-detected loader, software, mods and protocols', async () => {
    const byId = Object.fromEntries((await api('/api/v1/servers')).map((s) => [s.id, s]))
    const check = (id, loader, software, mods, protocol) => {
      const s = byId[id]
      expect(s, `${id} missing`)
      expect(s.loader === loader, `${id} loader ${s.loader}`)
      expect(software.test(s.software || ''), `${id} software ${s.software}`)
      for (const m of mods) expect((s.mods || []).includes(m), `${id} mods ${JSON.stringify(s.mods)} lack ${m}`)
      for (const m of ['minecraft', 'java', 'fabricloader', 'neoforge', 'forge']) expect(!(s.mods || []).includes(m), `${id} reports platform id ${m}`)
      expect(protocol(s), `${id} protocols ${s.minProtocol}-${s.maxProtocol}`)
      return `${id}: ${s.loader} "${s.software}" ${(s.mods || []).length} mods ${s.minProtocol}-${s.maxProtocol}`
    }
    return [
      check('fabric-pack', 'fabric', /^Fabric Loader .*1\.21\.1/, ['appleskin', 'fabric-api'], (s) => s.minProtocol === 767),
      check('neo-pack', 'neoforge', /^NeoForge 21\.1/, ['jei'], (s) => s.minProtocol === 767),
      check('forge-pack', 'forge', /^Forge 47/, ['waystones', 'balm'], (s) => s.minProtocol === 763),
      check('paper-via', 'paper', /Paper/, [], (s) => s.minProtocol <= 47 && s.maxProtocol >= 769),
      check('paper-legacy', 'paper', /Paper/, [], (s) => s.minProtocol === 47 && s.maxProtocol === 47)
    ].join('; ')
  })

  await scenario('G1 guard: direct status ping and login to mc-paper-legacy:25565 are answered by the guard, not Paper', async () => {
    // Raw 1.8 handshake (protocol 47, "localhost", port 25565) sent from inside the container.
    // execFileSync passes the script as one argument, with no local shell quoting involved.
    const raw = (state, extra) => execFileSync(DOCKER, ['exec', container('mc-paper-legacy'), 'bash', '-c',
      `exec 3<>/dev/tcp/127.0.0.1/25565; printf '\\x0f\\x00\\x2f\\x09localhost\\x63\\xdd\\x${state}${extra}' >&3; ` +
      'timeout 3 cat <&3 | tr -d "\\000" | head -c 2000; true'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })
    const status = raw('01', '\\x01\\x00')
    expect(/vecta guard/.test(status) && /play\.test/.test(status), `direct status: ${JSON.stringify(status)}`)
    const login = raw('02', '')
    expect(/Join this server through play\.test/.test(login), `direct login: ${JSON.stringify(login)}`)
    const log = docker(`logs --since 30m ${container('mc-paper-legacy')} 2>&1`)
    expect(log.includes('guard listening on 0.0.0.0:25565'), 'guard did not start')
    return 'status and login refused with the join hint'
  })

  await scenario('S2 1.21.11 vanilla: ambiguous -> dialog -> pick paper-a -> transfer -> cookie -> spawn', async () => {
    const r = await join({ version: '1.21.11', username: 'dialog774', brand: 'vanilla', chooseDialog: (ids) => ids.includes('vecta:join/paper-a') && 'vecta:join/paper-a' })
    expect(JSON.stringify(r.hops[0].dialogIds.sort()) === JSON.stringify(['vecta:join/paper-a', 'vecta:join/paper-via']), `dialog ids ${r.hops[0].dialogIds}`)
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

  await scenario('S6 1.20.1 client: two candidates -> limbo menu -> /join vanilla-old -> reconnect -> spawn', async () => {
    const pick = await limboChoose({ version: '1.20.1', username: 'limbo763', choose: 'vanilla-old' })
    expect(pick.menu, `no limbo menu (${pick.reason} ${pick.error || ''})`)
    expect(pick.reason.includes('Reconnect now'), `disconnect: ${pick.reason}`)
    const r = await join({ version: '1.20.1', username: 'limbo763' })
    expect(r.final.outcome === 'spawned' && await backendSaw('mc-vanilla-old', 'limbo763'), `${describe(r)} ${r.final.reason}`)
    return `menu -> choose -> ${describe(r)}`
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

  await scenario('S9 26.1 client -> paper-via (ViaVersion list read by the jar); forced onto vanilla-old -> version message', async () => {
    const r = await join({ version: '26.1', username: 'newer775' })
    expect(r.final.outcome === 'spawned' && await backendSaw('mc-paper-via', 'newer775'), `plain join ${describe(r)} ${r.final.reason}`)
    const k = await join({ version: '26.1', username: 'toonew', host: 'vanilla-old.play.test' })
    expect(k.final.outcome === 'kicked' && /needs 1\.20/.test(k.final.reason), `subdomain ${k.final.outcome} ${k.final.reason}`)
    return `plain: ${describe(r)}; vanilla-old: kicked with explanation`
  })

  for (const [name, version, host, choose, svc] of [
    ['L1 1.8.8 two candidates -> limbo', '1.8.8', 'play.test', 'paper-legacy', 'mc-paper-legacy'],
    ['L2 1.12.2 lobby.play.test -> limbo', '1.12.2', 'lobby.play.test', 'paper-via', 'mc-paper-via'],
    ['L3 1.20.4 lobby.play.test -> config-phase limbo', '1.20.4', 'lobby.play.test', 'paper-via', 'mc-paper-via'],
    ['L4 1.16.5 lobby.play.test -> limbo', '1.16.5', 'lobby.play.test', 'paper-via', 'mc-paper-via'],
    ['L5 1.19.4 lobby.play.test -> limbo (signed chat commands)', '1.19.4', 'lobby.play.test', 'paper-via', 'mc-paper-via']
  ]) {
    await scenario(`${name} -> /join ${choose} -> reconnect -> spawn`, async () => {
      const user = 'limbo' + version.replace(/\./g, '')
      const pick = await limboChoose({ version, username: user, host, choose })
      expect(pick.menu, `no limbo menu (${pick.reason} ${pick.error || ''})`)
      expect(pick.reason.includes('Reconnect now'), `disconnect: ${pick.reason}`)
      const r = await join({ version, username: user })
      expect(r.final.outcome === 'spawned' && await backendSaw(svc, user), `${describe(r)} ${r.final.reason}`)
      return `menu -> choose -> ${describe(r)}`
    })
  }

  await scenario('P1 jar /server paper-via (1.21.4) -> cookie + transfer -> spawn on paper-via', async () => {
    await waitFor('paper-plugin registered', async () => (await api('/api/v1/servers')).length >= 0 &&
      /paper-plugin/.test(gatewayLog()), 180000)
    const user = 'plugsrv769'
    const r = await inGameCommand({ version: '1.21.4', username: user, host: 'paper-plugin.play.test', command: 'server paper-via' })
    expect(r.spawned && r.outcome === 'transfer' && r.cookies['vecta:route'], `on hidden server: ${r.outcome} ${r.reason} ${r.chat.slice(0, 200)}`)
    const hops = await followTransfers({ version: '1.21.4', username: user }, r.cookies, r.transfer)
    const last = hops[hops.length - 1]
    expect(hops[0].cookieRequested && last.outcome === 'spawned' && await backendSaw('mc-paper-via', user), `hops ${hops.map((h) => h.outcome)} ${last.reason}`)
    return `transfer -> ${hops.map((h) => h.outcome).join(' -> ')}`
  })

  await scenario('P2 jar /server vanilla-old (1.20.1 via ViaBackwards) -> reconnect -> spawn on vanilla-old', async () => {
    const user = 'plugsrv763'
    const r = await inGameCommand({ version: '1.20.1', username: user, host: 'paper-plugin.play.test', command: 'server vanilla-old' })
    expect(r.spawned && r.outcome === 'kicked' && r.reason.includes('Reconnect'), `on hidden server: ${r.outcome} ${r.reason} ${r.chat.slice(0, 200)}`)
    const j = await join({ version: '1.20.1', username: user })
    expect(j.final.outcome === 'spawned' && await backendSaw('mc-vanilla-old', user), `${describe(j)} ${j.final.reason}`)
    return `kicked "${JSON.parse(r.reason).text || r.reason}" -> ${describe(j)}`
  })

  await scenario('P3 jar /hub (1.21.4) -> lobby cookie -> lobby -> spawn on paper-via', async () => {
    const user = 'plughub769'
    const r = await inGameCommand({ version: '1.21.4', username: user, host: 'paper-plugin.play.test', command: 'hub' })
    expect(r.spawned && r.outcome === 'transfer' && r.cookies['vecta:route'], `on hidden server: ${r.outcome} ${r.reason} ${r.chat.slice(0, 200)}`)
    const hops = await followTransfers({ version: '1.21.4', username: user }, r.cookies, r.transfer)
    const last = hops[hops.length - 1]
    const log = gatewayLog()
    expect(hops[0].cookieRequested && !new RegExp(`route cookie rejected.*player=${user}`).test(log), 'lobby cookie rejected')
    expect(last.outcome === 'spawned' && await backendSaw('mc-paper-via', user), `hops ${hops.map((h) => h.outcome)} ${last.reason}`)
    return `transfer -> ${hops.map((h) => h.outcome).join(' -> ')}`
  })

  await scenario('S10 lobby.play.test forces lobby for 1.21.4 -> transfer -> paper-via', async () => {
    const r = await join({ version: '1.21.4', username: 'lobby769', host: 'lobby.play.test' })
    expect(r.hops[0].outcome === 'transfer' && r.final.outcome === 'spawned', `hops ${describe(r)} ${r.final.reason}`)
    expect(await backendSaw('mc-paper-via', 'lobby769'), 'paper-via did not see player')
    return describe(r)
  })

  await scenario('S11 forged route cookie is rejected', async () => {
    const r = await connectOnce({ version: '1.21.4', username: 'forger', intent: 3, cookies: { 'vecta:route': Buffer.from('v1|fabric-pack|forger|9999999999|AAAA') } })
    expect(r.cookieRequested, 'no cookie request')
    await sleep(2000)
    expect(!backendSawNow('mc-fabric', 'forger'), 'forged cookie reached fabric-pack')
    expect(gatewayLog().includes('bad route cookie signature'), 'gateway did not log rejection')
    return `first hop ${r.outcome}`
  })

  await scenario('S12 fabric server stop unregisters fabric-pack (jar shutdown hook); fabric client falls back; restart re-registers', async () => {
    docker(`stop ${container('mc-fabric')}`)
    let r1
    try {
      await waitFor('fabric-pack gone', async () => !(await onlineIds()).includes('fabric-pack'), 30000)
      expect(/msg="server unregistered" server=fabric-pack/.test(gatewayLog()), 'jar did not unregister on shutdown')
      r1 = await join({ version: '1.21.1', username: 'fabricNoPack', brand: 'fabric', channels: ['appleskin:sync'] })
      expect(r1.final.outcome === 'spawned' && await backendSaw('mc-paper-via', 'fabricNoPack'), `fallback ${describe(r1)} ${r1.final.reason}`)
    } finally {
      docker(`start ${container('mc-fabric')}`)
    }
    await waitFor('fabric-pack back', async () => (await onlineIds()).includes('fabric-pack'), 300000)
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
