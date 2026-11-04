// Mako's service worker: shows pushes, and a tap opens the task on the board. A notification's buttons answer
// the agent right away; a push with close takes an answered request's notification away.
//
// On a phone paired end to end it is also the tunnel (docs/relay-e2e.md): the device's keys live in IndexedDB, and
// every request of the page to Mako goes sealed through one WebSocket to /e2e, so whatever carries it (tailscale
// serve, the relay) sees neither paths, nor cookies, nor bodies. The terminal's WebSocket rides inside it as a
// stream (makoSocket in board.html). Without keys (Mako's own window, a phone paired the old way) requests pass.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

const enc = new TextEncoder();
const dec = new TextDecoder();
const LABEL = "mako-relay-v1"; // relay.Version 1, as in go/internal/relay/seal.go
const BLOCK = 256;
const AHEAD = 1000; // counters reserved in IndexedDB at once, so a restart never reuses one
const P2M = 1;
const M2P = 2;
// What comes in the clear even with keys: the worker itself and what installing the app needs.
const clear = (p) => p === "/sw.js" || p === "/manifest.webmanifest" || p.startsWith("/icons/");
// The pairing page (pairPage in phone.go, put here when Mako serves this file): with keys the worker shows it itself,
// so it works with the tunnel down (the phone was disconnected on the Mac) and the relay cannot swap it.
const PAIR_PAGE = /*pair page*/ "";
const html = (body, status) => new Response(body, { status, headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer" } });
const unreachable = (why) => html(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mako</title><link rel="stylesheet" href="/style.css"></head><body style="display:block">
<div style="max-width:420px;margin:15vh auto;padding:0 20px;text-align:center"><div style="font-size:48px">🦈</div>
<h1 style="font-size:20px">Cannot reach Mako</h1><p style="color:var(--mute)">${why.replace(/[<&]/g, "")} Is the Mac awake, with Mako running?
If the phone was disconnected in Mako's settings, connect it again.</p>
<p><a class="button" href="/">Try again</a> <a class="button primary" href="/pair">Connect again</a></p></div></body></html>`, 502);

function b64(bytes) {
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
  return btoa(s);
}
function unb64(s) {
  if (!s) return new Uint8Array(0);
  s = s.replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(s + "=".repeat((4 - (s.length % 4)) % 4));
  const b = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) b[i] = raw.charCodeAt(i);
  return b;
}

// IndexedDB "mako-e2e", one store: device {id, mac, p2m, m2p} (non-extractable CryptoKeys), sendN (the highest
// counter reserved), recvN (the last one taken).
let dbP;
function db() {
  return (dbP ||= new Promise((ok, no) => {
    const r = indexedDB.open("mako-e2e", 1);
    r.onupgradeneeded = () => r.result.createObjectStore("kv");
    r.onsuccess = () => ok(r.result);
    r.onerror = () => { dbP = null; no(r.error); };
  }));
}
function idb(mode, body) {
  return db().then((d) => new Promise((ok, no) => {
    const t = d.transaction("kv", mode);
    let out;
    body(t.objectStore("kv"), (v) => (out = v));
    t.oncomplete = () => ok(out);
    t.onerror = t.onabort = () => no(t.error);
  }));
}
const idbGet = (k) => idb("readonly", (s, set) => { s.get(k).onsuccess = (e) => set(e.target.result); });

// The frame, byte for byte as relay.Sealed: nonce 0x00000000 ‖ n, AAD label ‖ v ‖ dir ‖ n ‖ mac ‖ d ‖ t ‖ i with
// 2-byte lengths, the plaintext padded to 256 bytes after a 4-byte length.
function putN(dv, at, n) {
  dv.setUint32(at, Math.floor(n / 2 ** 32));
  dv.setUint32(at + 4, n >>> 0);
}
function nonce(n) {
  const b = new Uint8Array(12);
  putN(new DataView(b.buffer), 4, n);
  return b;
}
function aad(f, mac, dir) {
  const head = enc.encode(LABEL);
  const parts = [mac, f.d || "", f.t, f.i || ""].map((s) => enc.encode(s));
  const out = new Uint8Array(head.length + 10 + parts.reduce((n, p) => n + 2 + p.length, 0));
  const dv = new DataView(out.buffer);
  out.set(head);
  let o = head.length;
  out[o++] = f.v;
  out[o++] = dir;
  putN(dv, o, f.n);
  o += 8;
  for (const p of parts) {
    if (p.length > 0xffff) throw new Error("a frame field is too long");
    dv.setUint16(o, p.length);
    out.set(p, o + 2);
    o += 2 + p.length;
  }
  return out;
}
async function seal(key, mac, dir, f, obj) {
  const payload = enc.encode(JSON.stringify(obj));
  const padded = new Uint8Array(Math.ceil((4 + payload.length) / BLOCK) * BLOCK);
  new DataView(padded.buffer).setUint32(0, payload.length);
  padded.set(payload, 4);
  const c = await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce(f.n), additionalData: aad(f, mac, dir) }, key, padded);
  return { ...f, c: b64(new Uint8Array(c)) };
}
async function unseal(key, mac, dir, f) {
  const b = new Uint8Array(await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce(f.n), additionalData: aad(f, mac, dir) }, key, unb64(f.c)));
  if (b.length < 4) throw new Error("the frame does not decrypt");
  const n = new DataView(b.buffer).getUint32(0);
  if (n > b.length - 4) throw new Error("the frame does not decrypt");
  return JSON.parse(dec.decode(b.subarray(4, 4 + n)));
}

// HKDF-SHA256 of the QR's secret, salted with the code's text: the same keys relay.DeriveKeys makes on the Mac.
async function deriveKeys(secret, code) {
  const raw = unb64(secret);
  if (raw.length < 32) throw new Error("The link is cut short: scan the QR code again.");
  const base = await crypto.subtle.importKey("raw", raw, "HKDF", false, ["deriveKey"]);
  const key = (info, usages) => crypto.subtle.deriveKey(
    { name: "HKDF", hash: "SHA-256", salt: enc.encode(code), info: enc.encode(LABEL + " " + info) },
    base, { name: "AES-GCM", length: 256 }, false, usages);
  return { pair: await key("pair", ["encrypt", "decrypt"]), p2m: await key("p2m", ["encrypt"]), m2p: await key("m2p", ["decrypt"]) };
}

let dev = null; // the device's keys, once paired end to end
let mode; // whether the tunnel is on; undefined till IndexedDB answers
let modeP = idbGet("device").then((d) => (mode = (dev = d || null) !== null), () => (mode = false));

// Pairing: one sealed frame under K_pair, counter 1. A retry sends the same bytes: sealing anew would reuse it.
async function pair(code, k) {
  const keys = await deriveKeys(k, code);
  const mac = self.location.host.toLowerCase();
  const sent = crypto.getRandomValues(new Uint8Array(16));
  const body = JSON.stringify(await seal(keys.pair, mac, P2M, { v: 1, t: "pair", i: code, n: 1 }, { nonce: b64(sent) }));
  let res;
  for (let tries = 0; ; tries++) {
    try {
      res = await fetch("/pair", { method: "POST", headers: { "Content-Type": "application/json" }, body });
      break;
    } catch {
      if (tries >= 2) throw new Error("Cannot reach Mako.");
      await new Promise((ok) => setTimeout(ok, 1000));
    }
  }
  if (!res.ok) throw new Error((await res.text()).trim() || "Mako did not pair the phone.");
  const f = await res.json();
  if (f.v !== 1 || f.t !== "paired" || f.i !== code || f.n !== 2 || f.d) throw new Error("Not Mako's answer: scan the QR code again.");
  let reply;
  try { reply = await unseal(keys.pair, mac, M2P, f); } catch { throw new Error("Not Mako's answer: scan the QR code again."); }
  // The nonce coming back proves the answer is from the Mac that knows the secret, not from the relay.
  if (!reply.id || reply.nonce !== b64(sent)) throw new Error("Not Mako's answer: scan the QR code again.");
  const d = { id: reply.id, mac, p2m: keys.p2m, m2p: keys.m2p };
  await idb("readwrite", (s) => { s.put(d, "device"); s.put(0, "sendN"); s.put(0, "recvN"); });
  sock?.ws.close();
  dev = d;
  sendN = recvN = null;
  mode = true;
  modeP = Promise.resolve(true);
}

// The counters. Sending: reserved ahead in one transaction, continuing above what any worker reserved, so no two
// frames ever share a nonce. Taking: only growing; written down now and then, since a replayed old frame carries an
// id nobody waits for any more.
let sendN = null;
let sendTop = 0;
async function nextN() {
  if (sendN === null || sendN >= sendTop) {
    [sendN, sendTop] = await idb("readwrite", (s, set) => {
      const g = s.get("sendN");
      g.onsuccess = () => {
        const from = Math.max(g.result || 0, sendN || 0);
        s.put(from + AHEAD, "sendN");
        set([from, from + AHEAD]);
      };
    });
    if (!Number.isSafeInteger(sendTop)) throw new Error("The counter ran out: pair the phone again.");
  }
  return ++sendN;
}
let recvN = null;
let recvSave = 0;
function recvSeen(n) {
  recvN = n;
  if (recvSave) return;
  recvSave = setTimeout(() => {
    recvSave = 0;
    idb("readwrite", (s) => s.put(recvN, "recvN")).catch(() => {});
  }, 1000);
}

// The tunnel: one WebSocket; frames are sealed and sent in one queue, and opened in the order they come.
let sock = null;
const pending = new Map(); // request id → its answer's promise
const streams = new Map(); // stream id → the page's MessagePort
function socket() {
  if (sock) return sock.ready;
  const ws = new WebSocket(`${self.location.protocol === "https:" ? "wss" : "ws"}://${self.location.host}/e2e?d=${encodeURIComponent(dev.id)}`);
  const me = (sock = { ws });
  me.ready = new Promise((ok, no) => {
    ws.onopen = () => ok(ws);
    ws.onerror = () => no(new Error("Cannot reach Mako."));
  });
  me.ready.catch(() => {});
  let q = Promise.resolve();
  ws.onmessage = (e) => { q = q.then(() => receive(e.data)).catch(() => {}); };
  ws.onclose = () => {
    if (sock === me) sock = null;
    for (const [i, p] of pending) { pending.delete(i); clearTimeout(p.timer); p.reject(new Error("Cannot reach Mako.")); }
    for (const [i, port] of streams) { streams.delete(i); port.postMessage({ t: "c" }); }
  };
  return me.ready;
}
let sendQ = Promise.resolve();
function send(t, i, obj) {
  const p = sendQ.then(async () => {
    const ws = await socket();
    const f = await seal(dev.p2m, dev.mac, P2M, { v: 1, d: dev.id, t, i, n: await nextN() }, obj);
    if (ws.readyState !== WebSocket.OPEN) throw new Error("Cannot reach Mako.");
    ws.send(JSON.stringify(f));
  });
  sendQ = p.catch(() => {});
  return p;
}
async function receive(text) {
  let f;
  try { f = JSON.parse(text); } catch { return; }
  if (recvN === null) recvN = (await idbGet("recvN")) || 0;
  // A counter that does not grow, or one past what a Number holds exactly, is not opened at all.
  if (!f || f.v !== 1 || f.d !== dev.id || !Number.isSafeInteger(f.n) || f.n <= recvN) return;
  let m;
  try { m = await unseal(dev.m2p, dev.mac, M2P, f); } catch { return; }
  recvSeen(f.n);
  if (f.t === "res") {
    const p = pending.get(f.i);
    if (p) { pending.delete(f.i); clearTimeout(p.timer); p.resolve(m); }
  } else if (f.t === "wsm") {
    streams.get(f.i)?.postMessage({ t: "m", d: dec.decode(unb64(m.ws)) });
  } else if (f.t === "wsc") {
    const port = streams.get(f.i);
    if (port) { streams.delete(f.i); port.postMessage({ t: "c" }); }
  }
}
const rid = () => b64(crypto.getRandomValues(new Uint8Array(16)));

async function roundTrip(req) {
  const i = rid();
  const answer = new Promise((resolve, reject) => {
    const timer = setTimeout(() => { pending.delete(i); reject(new Error("Mako did not answer.")); }, 60000);
    pending.set(i, { resolve, reject, timer });
  });
  answer.catch(() => {});
  try {
    await send("req", i, req);
  } catch (e) {
    clearTimeout(pending.get(i)?.timer);
    pending.delete(i);
    throw e;
  }
  return answer;
}

const sentHeaders = ["content-type", "accept", "accept-language", "if-none-match", "if-modified-since", "range", "x-mako"];
async function tunnelFetch(request, nav) {
  const u = new URL(request.url);
  const req = { m: request.method, u: u.pathname + u.search, h: {}, nav };
  for (const name of sentHeaders) {
    const v = request.headers.get(name);
    if (v) req.h[name === "x-mako" ? "X-Mako" : name] = v;
  }
  const idempotent = request.method === "GET" || request.method === "HEAD";
  if (!idempotent) {
    const body = new Uint8Array(await request.arrayBuffer());
    if (body.length) req.b = b64(body);
  }
  let m;
  for (let tries = 0; ; tries++) {
    try {
      m = await roundTrip(req);
      break;
    } catch (e) {
      // A GET is asked again once, on a new connection; anything else could do its work twice.
      if (tries > 0 || !idempotent) return new Response(e.message || "Cannot reach Mako.", { status: 502, headers: { "Content-Type": "text/plain; charset=utf-8", "X-Mako-Tunnel": "down" } });
    }
  }
  const status = m.s >= 200 && m.s <= 599 ? m.s : 502;
  const headers = new Headers();
  for (const [k, v] of Object.entries(m.h || {})) {
    try { headers.set(k, v); } catch {}
  }
  const to = headers.get("location");
  if (to && [301, 302, 303, 307, 308].includes(status)) return Response.redirect(new URL(to, self.location.origin).href, status);
  const body = status === 204 || status === 304 || request.method === "HEAD" ? null : unb64(m.b);
  return new Response(body, { status, headers });
}

self.addEventListener("fetch", (e) => {
  const req = e.request;
  const u = new URL(req.url);
  if (u.origin !== self.location.origin || clear(u.pathname) || mode === false) return;
  e.respondWith((async () => {
    if (!(await modeP)) return fetch(req);
    const nav = req.mode === "navigate";
    // Another site can send the browser here with a form: only a plain GET navigation goes on, as with a Lax cookie.
    if (nav && req.method !== "GET") return new Response("Forbidden", { status: 403 });
    if (nav && u.pathname === "/pair" && PAIR_PAGE) return html(PAIR_PAGE, 200);
    const res = await tunnelFetch(req, nav);
    if (nav && res.status === 502 && res.headers.get("X-Mako-Tunnel") === "down") return unreachable(await res.text());
    return res;
  })());
});

// A WebSocket of the page (makoSocket in board.html), as a stream of the tunnel: the page's MessagePort in, the
// stream's messages out. Its pings ("p") are answered, so the page sees when this worker is gone.
async function openStream(u, port) {
  if (!(await modeP)) return port.postMessage({ t: "c" });
  const i = rid();
  streams.set(i, port);
  port.onmessage = (ev) => {
    const m = ev.data || {};
    if (m.t === "p") port.postMessage({ t: "p" });
    else if (m.t === "s" && streams.has(i)) send("wsm", i, { ws: b64(enc.encode(String(m.d))) }).catch(() => {});
    else if (m.t === "c" && streams.delete(i)) send("wsc", i, {}).catch(() => {});
  };
  try {
    await send("wso", i, { u });
    port.postMessage({ t: "o" });
  } catch {
    streams.delete(i);
    port.postMessage({ t: "c" });
  }
}

self.addEventListener("message", (e) => {
  const m = e.data || {};
  const port = e.ports[0];
  if (m.t === "pair" && port) {
    e.waitUntil(pair(String(m.code || ""), String(m.k || "")).then(
      () => port.postMessage({ ok: true }),
      (err) => port.postMessage({ ok: false, error: err.message || String(err) })));
  } else if (m.t === "wso" && port) {
    e.waitUntil(openStream(String(m.u || ""), port));
  }
  // "alive": the page keeps a stream open; the message alone keeps this worker from being stopped as idle.
});

self.addEventListener("push", (e) => {
  let m = {};
  try { m = e.data ? e.data.json() : {}; } catch { m = { title: "Mako", body: e.data?.text() }; }
  if (m.close && m.tag) {
    // A push has to show a notification: the same tag, quietly, closed at once, takes the old one with it.
    e.waitUntil((async () => {
      await self.registration.showNotification(m.title || "Mako", { body: m.body || "", tag: m.tag, silent: true, icon: "/icons/icon-192.png" });
      for (const n of await self.registration.getNotifications({ tag: m.tag })) n.close();
    })());
    return;
  }
  const actions = m.actions || [];
  const max = self.Notification?.maxActions ?? 2;
  e.waitUntil(self.registration.showNotification(m.title || "Mako", {
    body: m.body || "",
    tag: m.tag || undefined,
    renotify: !!m.tag,
    icon: "/icons/icon-192.png",
    badge: "/icons/badge-96.png", // Android draws it as a white mask in the status bar
    actions: actions.slice(0, max).map((a, i) => ({ action: String(i), title: a.title })),
    data: { url: m.url || "/", actions },
  }));
});

async function openBoard(url) {
  const all = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  for (const c of all) {
    if (new URL(c.url).origin === self.location.origin) {
      await c.focus();
      return c.navigate(url);
    }
  }
  return self.clients.openWindow(url);
}

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = new URL(e.notification.data?.url || "/", self.location.origin).href;
  const action = e.action ? e.notification.data?.actions?.[Number(e.action)] : null;
  if (!action) {
    e.waitUntil(openBoard(url));
    return;
  }
  // A button: the answer goes to the agent; only when it cannot (already answered, agent gone) the board opens.
  // The worker's own fetch does not pass its fetch handler, so with keys it goes through the tunnel here.
  e.waitUntil((async () => {
    try {
      const req = new Request(new URL(action.url, self.location.origin).href, { method: "POST", credentials: "same-origin" });
      const res = (await modeP) ? await tunnelFetch(req, false) : await fetch(req);
      if ((await res.json()).ok) return;
    } catch {}
    return openBoard(url);
  })());
});
