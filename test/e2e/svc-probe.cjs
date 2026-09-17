// Makeshift Simple Voice Chat client: joins through the gateway, requests the
// voice secret, then runs the UDP handshake (authenticate, connection check,
// keepalive) against the voice_host the server advertised.
//   GW_HOST=10.10.25.155 node test/e2e/svc-probe.cjs svc-test.play.test Alice Bob
'use strict'

const mc = require('minecraft-protocol')
const net = require('net')
const dgram = require('dgram')
const crypto = require('crypto')

const GW_HOST = process.env.GW_HOST || '10.10.25.155'
const GW_PORT = Number(process.env.GW_PORT || 35565)
const HOST = process.argv[2] || 'svc-test.play.test'
const NAMES = process.argv.slice(3).length ? process.argv.slice(3) : ['VoiceA']
const COMPAT = 20

function varint (n) {
  const out = []
  do {
    let b = n & 0x7f
    n >>>= 7
    if (n) b |= 0x80
    out.push(b)
  } while (n)
  return Buffer.from(out)
}

function readVarint (buf, off) {
  let n = 0; let shift = 0; let b
  do {
    b = buf[off++]
    n |= (b & 0x7f) << shift
    shift += 7
  } while (b & 0x80)
  return [n, off]
}

function parseSecret (d) {
  let o = 0
  const secret = d.subarray(o, o += 16)
  const port = d.readInt32BE(o); o += 4
  const uuid = d.subarray(o, o += 16)
  o += 1 // codec
  o += 4 // mtu
  o += 8 // distance
  const keepAlive = d.readInt32BE(o); o += 4
  o += 1 // groups
  const [len, o2] = readVarint(d, o)
  const voiceHost = d.subarray(o2, o2 + len).toString('utf8')
  return { secret, port, uuid, keepAlive, voiceHost }
}

function encrypt (key, plain) {
  const iv = crypto.randomBytes(12)
  const c = crypto.createCipheriv('aes-128-gcm', key, iv)
  const enc = Buffer.concat([c.update(plain), c.final()])
  return Buffer.concat([iv, enc, c.getAuthTag()])
}

function decrypt (key, payload) {
  const iv = payload.subarray(0, 12)
  const tag = payload.subarray(payload.length - 16)
  const d = crypto.createDecipheriv('aes-128-gcm', key, iv)
  d.setAuthTag(tag)
  return Buffer.concat([d.update(payload.subarray(12, payload.length - 16)), d.final()])
}

function clientDatagram (s, type, body) {
  const payload = encrypt(s.secret, Buffer.concat([Buffer.from([type]), body]))
  return Buffer.concat([Buffer.from([0xff]), s.uuid, varint(payload.length), payload])
}

const TYPES = { 6: 'AuthenticateAck', 8: 'KeepAlive', 10: 'ConnectionCheckAck', 7: 'Ping' }

function probe (name) {
  return new Promise((resolve) => {
    const log = (...a) => console.log(`[${name}]`, ...a)
    const result = { name, ok: false }
    const client = mc.createClient({
      version: '1.21.1', username: name, auth: 'offline', host: HOST, port: 25565, hideErrors: true,
      connect: (c) => c.setSocket(net.connect(GW_PORT, GW_HOST))
    })
    const timer = setTimeout(() => finish('timeout'), 90000)
    let sock
    function finish (why) {
      clearTimeout(timer)
      result.why = why
      try { sock && sock.close() } catch {}
      try { client.end() } catch {}
      resolve(result)
    }
    client.on('kick_disconnect', (p) => finish('kicked: ' + JSON.stringify(p.reason)))
    client.on('disconnect', (p) => finish('disconnected: ' + JSON.stringify(p.reason)))
    client.on('end', () => finish(result.ok ? 'done' : 'connection ended'))
    // Fabric API waits for the pong to its configuration-phase ping.
    client.on('ping', (p) => { if (client.state === 'configuration') client.write('pong', { id: p.id }) })
    client.on('system_chat',(p) => log('chat', JSON.stringify(p.content)))
    client.on('login', () => {
      log('spawned; requesting voice secret')
      client.write('custom_payload', { channel: 'minecraft:register', data: Buffer.from('voicechat:secret\0voicechat:request_secret') })
      const d = Buffer.alloc(4); d.writeInt32BE(COMPAT)
      client.write('custom_payload', { channel: 'voicechat:request_secret', data: d })
    })
    client.on('custom_payload', (p) => {
      if (p.channel !== 'voicechat:secret') return
      const s = parseSecret(p.data)
      result.voiceHost = s.voiceHost
      log(`secret received: serverPort=${s.port} voiceHost=${JSON.stringify(s.voiceHost)} keepAlive=${s.keepAlive}`)
      const m = /^(.*):(\d+)$/.exec(s.voiceHost)
      const host = m ? m[1].replace(/^\[|\]$/g, '') : GW_HOST
      const port = m ? Number(m[2]) : s.port
      sock = dgram.createSocket('udp4')
      const seen = {}
      let phase = 'auth'
      const send = () => {
        if (phase === 'auth') sock.send(clientDatagram(s, 5, Buffer.concat([s.uuid, s.secret])), port, host)
        else if (phase === 'check') sock.send(clientDatagram(s, 9, Buffer.alloc(0)), port, host)
      }
      const tick = setInterval(send, 1000)
      send()
      sock.on('message', (msg, rinfo) => {
        if (msg[0] !== 0xff) return log('non-voice datagram', msg.length)
        const [len, off] = readVarint(msg, 1)
        let plain
        try { plain = decrypt(s.secret, msg.subarray(off, off + len)) } catch (e) { return log('decrypt failed', e.message) }
        const type = plain[0]
        seen[type] = (seen[type] || 0) + 1
        if (seen[type] === 1) log(`udp from ${rinfo.address}:${rinfo.port}: ${TYPES[type] || type}`)
        if (type === 6 && phase === 'auth') { phase = 'check'; send() }
        if (type === 10) phase = 'connected'
        if (type === 8) sock.send(clientDatagram(s, 8, Buffer.alloc(0)), port, host) // echo keepalive
        if (phase === 'connected' && seen[8] >= 3) {
          clearInterval(tick)
          result.ok = true
          result.keepalives = seen[8]
          log(`voice connected via ${host}:${port}, ${seen[8]} keepalives exchanged`)
          finish('done')
        }
      })
    })
  })
}

Promise.all(NAMES.map(probe)).then((rs) => {
  for (const r of rs) console.log(`${r.ok ? 'PASS' : 'FAIL'} ${r.name}: ${r.why} voiceHost=${r.voiceHost}`)
  process.exit(rs.every((r) => r.ok) ? 0 : 1)
})
