// A stateful stand-in for the bridge, for browser tests of every path
// (sign-in by email, code, Google and Microsoft; linking; ticking; keys;
// grants; members) that must never run against a real account. It speaks the
// bridge's HTTP API closely enough for the web UI: JSON under /api, the /auth
// routes, a streaming /api/events, the OAuth discovery documents, and test
// hooks (link, grant, mail, ...) on the returned object. All data is made up.
import http from "node:http";
import { randomBytes } from "node:crypto";
import { deflateSync } from "node:zlib";

// ---- a QR-looking PNG (grayscale, no dependencies) ------------------------------
const CRC = new Int32Array(256).map((_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c;
});
const crc32 = (buf) => {
  let c = -1;
  for (const b of buf) c = CRC[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ -1) >>> 0;
};
const chunk = (type, data) => {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const td = Buffer.concat([Buffer.from(type), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(td));
  return Buffer.concat([len, td, crc]);
};
export function fakeQr(seed = 1) {
  const N = 29, S = 8, Q = 2;
  let x = seed * 9301 + 49297;
  const rnd = () => ((x = (x * 9301 + 49297) % 233280) / 233280);
  const m = Array.from({ length: N }, () => Array.from({ length: N }, () => rnd() > 0.52));
  const finder = (r0, c0) => {
    for (let r = 0; r < 7; r++)
      for (let c = 0; c < 7; c++) m[r0 + r][c0 + c] = r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4);
    for (let i = 0; i < 8; i++) {
      if (r0 + 7 < N) m[r0 + 7][Math.min(c0 + i, N - 1)] = false;
      if (c0 + 7 < N) m[Math.min(r0 + i, N - 1)][c0 + 7] = false;
    }
  };
  finder(0, 0);
  finder(0, N - 7);
  finder(N - 7, 0);
  const W = (N + 2 * Q) * S;
  const rows = [];
  for (let y = 0; y < W; y++) {
    const row = Buffer.alloc(W + 1);
    for (let px = 0; px < W; px++) {
      const r = Math.floor(y / S) - Q, c = Math.floor(px / S) - Q;
      row[px + 1] = r >= 0 && c >= 0 && r < N && c < N && m[r][c] ? 0 : 255;
    }
    rows.push(row);
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(W, 0);
  ihdr.writeUInt32BE(W, 4);
  ihdr[8] = 8;
  ihdr[9] = 0;
  const png = Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", deflateSync(Buffer.concat(rows))),
    chunk("IEND", Buffer.alloc(0)),
  ]);
  return "data:image/png;base64," + png.toString("base64");
}

// ---- fixtures ----------------------------------------------------------------------
const iso = (minsAgo) => new Date(Date.now() - minsAgo * 60000).toISOString();
export const NAME = "whatsapp-bridge";
export const LINKED_ID = "a1b2c3d4e5f60718";
export const OWN_JID = "15550100000:3@s.whatsapp.net";
const OWN = { phone: "15550100000", push_name: "Alex Morgan", platform: "iphone" };

/** A personal account's chats: family, friends, a project group; some not shared. */
export const CHATS = [
  { jid: "120363000000000001@g.us", name: "Family", is_group: true, mins: 4, count: 214 },
  { jid: "120363000000000002@g.us", name: "Project Atlas", is_group: true, mins: 38, count: 486 },
  { jid: "15550100001@s.whatsapp.net", name: "Sam Lee", is_group: false, mins: 95, count: 57 },
  { jid: "120363000000000006@g.us", name: "Weekend in Lisbon", is_group: true, mins: 140, count: 133 },
  { jid: "120363000000000003@g.us", name: "Book club", is_group: true, mins: 210, count: 92 },
  { jid: "15550100002@s.whatsapp.net", name: "Priya Shah", is_group: false, mins: 260, count: 41 },
  { jid: "120363000000000004@g.us", name: "Climbing Thursdays", is_group: true, mins: 300, count: 77 },
  { jid: "15550100005@s.whatsapp.net", name: "Mum", is_group: false, mins: 380, count: 128 },
  { jid: "15550100003@s.whatsapp.net", name: "Jordan Kim", is_group: false, mins: 420, count: 23 },
  { jid: "15550100006@s.whatsapp.net", name: "Ana García", is_group: false, mins: 900, count: 19 },
  { jid: "120363000000000005@g.us", name: "Neighbours", is_group: true, mins: 2000, count: 0, ticked: false },
  { jid: "120363000000000007@g.us", name: "School parents", is_group: true, mins: 2400, count: 0, ticked: false },
  { jid: "15550100004@s.whatsapp.net", name: "Dentist", is_group: false, mins: 2600, count: 0, ticked: false },
  { jid: "120363000000000008@g.us", name: "Five-a-side football", is_group: true, mins: 3100, count: 0, ticked: false },
  { jid: "15550100007@s.whatsapp.net", name: "Chris Nguyen", is_group: false, mins: 4300, count: 12 },
  { jid: "15550100008@s.whatsapp.net", name: "Taylor Brooks", is_group: false, mins: 7200, count: 0, ticked: false },
];

/** A second, older account (a work phone) for a fuller accounts list. */
export const WORK_CHATS = [
  { jid: "120363000000000101@g.us", name: "Design team", is_group: true, mins: 2950, count: 612 },
  { jid: "15550100101@s.whatsapp.net", name: "Riley Chen", is_group: false, mins: 3020, count: 88 },
  { jid: "120363000000000102@g.us", name: "Quarterly planning", is_group: true, mins: 3300, count: 240 },
  { jid: "120363000000000103@g.us", name: "Office", is_group: true, mins: 4100, count: 0, ticked: false },
  { jid: "15550100102@s.whatsapp.net", name: "Morgan Park", is_group: false, mins: 5000, count: 344 },
  { jid: "120363000000000104@g.us", name: "IT help", is_group: true, mins: 8000, count: 0, ticked: false },
];

const M = (who, mine, mins, text, kind = "text") => ({ who, mine, mins, text, kind });
const MESSAGES = {
  "120363000000000001@g.us": [
    M("Mum", false, 1630, "Who is around for a walk on Saturday morning?"),
    M("Dad", false, 1622, "Count me in. Ten at the park gate?"),
    M("", true, 1610, "Ten works. I'll bring the dog."),
    M("Mum", false, 1450, "[voice message 0:42]", "audio"),
    M("Leah", false, 1300, "[photo] Found these in the attic", "image"),
    M("Leah", false, 1299, "Grandpa's old slides, there are hundreds of them"),
    M("Dad", false, 1280, "Wonderful. Let's scan them over the holidays."),
    M("Mum", false, 190, "Lunch on Sunday at ours? 12:30."),
    M("", true, 182, "Yes, we'll be there. Shall I bring dessert?"),
    M("Dad", false, 150, "Dessert would be great. Your sister brings the salad."),
    M("Leah", false, 149, "Salad it is 🥗"),
    M("Mum", false, 61, "[photo] The garden this morning", "image"),
    M("Leah", false, 58, "👍", "reaction"),
    M("", true, 4, "Lovely. See you Sunday."),
  ],
  "120363000000000002@g.us": [
    M("Sam Lee", false, 1500, "Kick-off notes are in the shared folder. Please add your open questions by Wednesday."),
    M("Priya Shah", false, 1440, "Added mine. Biggest one: do we ship the beta to all three offices at once?"),
    M("", true, 1430, "I'd start with one office and widen after two weeks."),
    M("Sam Lee", false, 1420, "[poll] Beta rollout: one office first / all at once", "poll"),
    M("Sam Lee", false, 300, "Draft of the Atlas plan is in the shared folder."),
    M("Priya Shah", false, 290, "[document: atlas-plan-v3.pdf] Comments inline.", "document"),
    M("", true, 240, "Thanks both. Let's review on Thursday at 10."),
    M("Jordan Kim", false, 236, "I'll book the small meeting room."),
    M("Jordan Kim", false, 235, "[location] Meeting room 2, third floor", "location"),
    M("Sam Lee", false, 38, "Thursday 10 works. I'll send the invite."),
  ],
  "15550100001@s.whatsapp.net": [
    M("Sam Lee", false, 300, "Did you see the comments on section 2?"),
    M("Sam Lee", false, 120, "Can you look at section 2 before Thursday?"),
    M("", true, 95, "Will do this afternoon."),
  ],
  "120363000000000006@g.us": [
    M("Ana García", false, 900, "Flights are booked! Landing Friday 14:05."),
    M("Chris Nguyen", false, 640, "[document: lisbon-itinerary.pdf]", "document"),
    M("", true, 600, "Great list. Can we add the tram to Belém on Saturday?"),
    M("Ana García", false, 140, "Added. Dinner on Saturday is at 20:30."),
  ],
  "120363000000000003@g.us": [
    M("Jordan Kim", false, 400, "Next book: suggestions by Friday, please."),
    M("Priya Shah", false, 210, "I vote for the one about the lighthouse keeper."),
  ],
  "15550100002@s.whatsapp.net": [
    M("Priya Shah", false, 270, "Are you coming climbing on Thursday?"),
    M("", true, 260, "Yes, see you at seven."),
  ],
  "15550100005@s.whatsapp.net": [
    M("Mum", false, 400, "Don't forget Grandma's birthday on the 12th."),
    M("", true, 380, "Thanks, card is already in the post."),
  ],
};

let idSeq = 0;
const newId = (p = "") => p + (Date.now().toString(16) + (++idSeq).toString(16).padStart(4, "0")).slice(-16).padStart(16, "a");
const token = () => randomBytes(18).toString("base64url");

function fresh(opts) {
  const gated = opts.gated ?? true;
  return {
    gated,
    code: opts.code ?? "right-code",
    authed: opts.authed ?? !gated,
    providers: opts.providers ?? (gated ? ["code", "email", "google", "microsoft"] : []),
    user: opts.user ?? null,
    oauth: opts.oauth ?? true,
    grants: opts.grants === null ? null : [...(opts.grants ?? [])], // null: the endpoint answers 404
    members: opts.members === null ? null : [...(opts.members ?? [])],
    allowedDomains: opts.allowedDomains ?? [],
    approvers: opts.approvers ?? ["owner@example.com"],
    name: opts.name ?? NAME,
    cookies: {}, // the link and approve cookies: cookie value -> token
    sessions: [],
    chats: {},
    messages: {},
    logs: [],
    keys: opts.keys ? [...opts.keys] : [],
    log: [], // every non-GET request: {method, path, body}
    mail: [], // every mail the bridge would send: {kind, to, link, token}
    tokens: {}, // token -> {email, next, expires, used, purpose}
    pairs: 0,
    failSessions: opts.failSessions ?? 0,
    loginLimit: opts.loginLimit ?? 0,
    loginTries: 0,
    emailLimit: opts.emailLimit ?? 0,
    emailTries: 0,
    pairLimited: !!opts.pairLimited,
    steps: {},
    grantPolls: 0,
    autoGrantAfterMs: opts.autoGrantAfterMs ?? 0,
  };
}

/**
 * createMock starts the stand-in on 127.0.0.1 (opts.port, or a free port).
 * reset(opts) starts a new scenario on the same server. Options: gated,
 * code, authed, providers, user, oauth, grants, members, allowedDomains,
 * sessions ("linked" | "linked-old"), work (a second, offline account), keys, failSessions, loginLimit,
 * emailLimit, pairLimited, autoGrantAfterMs.
 */
export async function createMock(opts = {}) {
  const S = { clients: new Set() };
  S.reset = (o = {}) => {
    S.clients.forEach((c) => c.end());
    S.clients.clear();
    Object.assign(S, fresh(o));
    if (o.sessions === "linked") addLinked(S, LINKED_ID, 600);
    if (o.sessions === "linked-old") addLinked(S, LINKED_ID, 9000);
    if (o.work) addWork(S);
    return S;
  };
  S.reset(opts);
  const server = http.createServer((req, res) => {
    handle(S, req, res).catch((err) => {
      console.error("mock-bridge:", err);
      if (!res.headersSent) send(res, 500, { error: "mock failure" });
    });
  });
  await new Promise((r) => server.listen(opts.port ?? 0, "127.0.0.1", r));
  S.url = `http://127.0.0.1:${server.address().port}`;
  S.close = () =>
    new Promise((r) => {
      S.clients.forEach((c) => c.end());
      server.close(r);
      server.closeAllConnections?.();
    });
  S.emit = (ev) => emit(S, ev);
  S.link = (id) => link(S, id);
  S.addWork = (id) => addWork(S, id);
  S.pairingId = () => S.sessions.find((s) => s.state === "pairing")?.id;
  S.grant = (name = "Claude Code", clientId = "https://claude.ai/oauth/claude-code-client-metadata") => addGrant(S, name, clientId);
  S.lastMail = (to) => [...S.mail].reverse().find((m) => !to || m.to === to);
  S.expire = (tok) => {
    if (S.tokens[tok]) S.tokens[tok].expires = Date.now() - 1000;
  };
  return S;
}

function addGrant(S, name, clientId) {
  if (!S.grants) S.grants = [];
  const g = { id: "oa_" + randomBytes(8).toString("hex"), client_name: name, client_id: clientId, created_at: new Date().toISOString(), last_used_at: null };
  S.grants.push(g);
  S.logs.unshift({ seq: S.logs.length + 1, time: g.created_at, session_id: LINKED_ID, type: "oauth.consent_granted", detail: `client=${name}` });
  return g;
}

function chatsFor(id, list = CHATS) {
  return list.map((c) => ({
    session_id: id, jid: c.jid, name: c.name, is_group: c.is_group, last_message_at: iso(c.mins), message_count: c.count, ticked: c.ticked ?? true,
  }));
}
const storedCount = (list = CHATS) => list.reduce((n, c) => n + (c.ticked === false ? 0 : c.count), 0);

/** A stable made-up number per sender name. */
const senderJid = (name) => `1555020${String(1000 + ([...name].reduce((h, ch) => (h * 31 + ch.charCodeAt(0)) % 9000, 7))).padStart(4, "0")}@s.whatsapp.net`;

function messagesFor(id) {
  const out = {};
  for (const [jid, list] of Object.entries(MESSAGES)) {
    out[jid] = list.map((m, i) => ({
      session_id: id, chat_jid: jid, id: `${jid.slice(0, 6)}-${i}`, sender_jid: m.mine ? OWN_JID : senderJid(m.who),
      sender_name: m.mine ? undefined : m.who, timestamp: iso(m.mins), from_me: m.mine, kind: m.kind, text: m.text,
    }));
  }
  return out;
}

function addLinked(S, id, createdMinsAgo) {
  const s = {
    id, jid: OWN_JID, ...OWN,
    state: "connected", created_at: iso(createdMinsAgo), connected_at: iso(240), last_message_at: iso(4),
    message_count: storedCount(), chat_count: CHATS.length,
  };
  S.sessions.push(s);
  S.chats[id] = chatsFor(id);
  S.messages[id] = messagesFor(id);
  S.logs = [
    { seq: 9, time: iso(30), session_id: id, type: "mcp.tool", detail: "key=oa_0d8efbf4838d04f2 tool=search_messages rows=6" },
    { seq: 8, time: iso(31), session_id: id, type: "mcp.tool", detail: "key=oa_0d8efbf4838d04f2 tool=read_chat rows=40" },
    { seq: 7, time: iso(95), session_id: id, type: "chats.unticked", detail: "Taylor Brooks" },
    { seq: 6, time: iso(120), session_id: id, type: "oauth.consent_granted", detail: "client=Claude" },
    { seq: 5, time: iso(121), session_id: id, type: "auth.login", detail: "via=code" },
    { seq: 4, time: iso(240), session_id: id, type: "state.connected", detail: "" },
    { seq: 3, time: iso(241), session_id: id, type: "state.connecting", detail: "" },
    { seq: 2, time: iso(createdMinsAgo - 1), session_id: id, type: "chats.sync", detail: `history sync: ${CHATS.length} chats` },
    { seq: 1, time: iso(createdMinsAgo), session_id: id, type: "pair.success", detail: "device=whatsapp-bridge" },
  ];
  return s;
}

/** A second, older account: a work phone that is offline (disconnected). */
function addWork(S, id = "b7c8d9e0f1a20304") {
  const s = {
    id, jid: "15550100900:2@s.whatsapp.net", phone: "15550100900", push_name: "Alex (work)", platform: "android",
    state: "disconnected", last_error: "", created_at: iso(60 * 24 * 40), connected_at: iso(60 * 24 * 3), last_message_at: iso(2950),
    message_count: storedCount(WORK_CHATS), chat_count: WORK_CHATS.length,
  };
  S.sessions.push(s);
  S.chats[id] = chatsFor(id, WORK_CHATS);
  S.messages[id] = {};
  S.logs.push(
    { seq: 101, time: iso(2900), session_id: id, type: "state.disconnected", detail: "" },
    { seq: 100, time: iso(60 * 24 * 40), session_id: id, type: "pair.success", detail: "device=whatsapp-bridge" },
  );
  return s;
}

function emit(S, ev) {
  const e = { time: new Date().toISOString(), ...ev };
  const data = `event: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`;
  S.clients.forEach((c) => c.write(data));
  if (e.type === "pair" && e.session_id) S.steps[e.session_id] = e;
}

function link(S, id) {
  const s = S.sessions.find((x) => x.id === id);
  if (!s) throw new Error("no pairing " + id);
  Object.assign(s, {
    state: "connecting", jid: OWN_JID, ...OWN, connected_at: new Date().toISOString(),
  });
  S.chats[id] = chatsFor(id);
  S.messages[id] = messagesFor(id);
  s.chat_count = CHATS.length;
  s.message_count = storedCount();
  s.last_message_at = iso(4);
  const seq = S.logs.length;
  S.logs.unshift(
    { seq: seq + 3, time: new Date().toISOString(), session_id: id, type: "chats.sync", detail: `history sync: ${CHATS.length} chats` },
    { seq: seq + 2, time: new Date().toISOString(), session_id: id, type: "state.connected", detail: "" },
    { seq: seq + 1, time: new Date().toISOString(), session_id: id, type: "pair.success", detail: "device=whatsapp-bridge" },
  );
  emit(S, { type: "pair", session_id: id, pair: { type: "success" } });
  emit(S, { type: "state", session_id: id, state: "connecting" });
  setTimeout(() => {
    s.state = "connected";
    emit(S, { type: "state", session_id: id, state: "connected" });
    emit(S, { type: "chats", session_id: id });
  }, 300);
}

const send = (res, status, body, headers = {}) => {
  res.writeHead(status, { "Content-Type": "application/json; charset=utf-8", "Cache-Control": "no-store", ...headers });
  res.end(body === undefined ? "" : JSON.stringify(body));
};
const redirect = (res, status, location, headers = {}) => {
  res.writeHead(status, { Location: location, "Cache-Control": "no-store", ...headers });
  res.end();
};
const page = (res, status, title, text) => {
  res.writeHead(status, { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" });
  res.end(`<!doctype html><meta charset="utf-8"><title>${title}</title><h1>${title}</h1><p>${text}</p>`);
};

async function readBody(req) {
  const chunks = [];
  for await (const c of req) chunks.push(c);
  const raw = Buffer.concat(chunks).toString();
  if (!raw) return {};
  if ((req.headers["content-type"] ?? "").includes("application/x-www-form-urlencoded")) return Object.fromEntries(new URLSearchParams(raw));
  try {
    return JSON.parse(raw);
  } catch {
    return {};
  }
}

/** A server page with one same-origin POST button, as the bridge renders for emailed links. */
const buttonPage = (res, title, text, label, action) => {
  res.writeHead(200, { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" });
  res.end(`<!doctype html><meta charset="utf-8"><title>${title}</title><h1>${title}</h1><p>${text}</p><form method="post" action="${action}"><button type="submit">${label}</button></form>`);
};
const cookieOf = (req, name) => (req.headers.cookie ?? "").split(/;\s*/).map((c) => c.split("=")).find(([k]) => k === name)?.[1] ?? "";
const safeNext = (n) => (typeof n === "string" && n.startsWith("/") && !n.startsWith("//") ? n : "/");
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

function member(S, email) {
  return (S.members ?? []).find((m) => m.email.toLowerCase() === email.toLowerCase());
}

/** Issues a single-use token and "mails" its link to `to`; `about` is the address it signs in or approves. */
function issue(S, kind, to, purpose, next, ttlMs, about = to) {
  const t = token();
  S.tokens[t] = { email: about, next: safeNext(next), expires: Date.now() + ttlMs, used: false, purpose };
  const path = purpose === "approve" ? "/auth/email/approve" : "/auth/email/callback";
  S.mail.push({ kind, to, token: t, link: `${path}?token=${t}` });
  return t;
}

async function handle(S, req, res) {
  const u = new URL(req.url, "http://x");
  const p = u.pathname;
  const m = req.method;
  const body = m === "GET" ? {} : await readBody(req);
  if (m !== "GET") S.log.push({ method: m, path: p, body });
  const origin = `http://${req.headers.host}`;

  // Discovery (public).
  if (p === "/.well-known/oauth-protected-resource/mcp" || p === "/.well-known/oauth-protected-resource") {
    return send(res, 200, { resource: origin + "/mcp", authorization_servers: S.oauth ? [origin] : [], scopes_supported: ["mcp"], bearer_methods_supported: ["header"] });
  }
  if (p === "/.well-known/oauth-authorization-server" || p === "/.well-known/oauth-authorization-server/mcp") {
    if (!S.oauth) return send(res, 404, { error: "not found" });
    return send(res, 200, {
      issuer: origin, authorization_endpoint: origin + "/oauth/authorize", token_endpoint: origin + "/oauth/token",
      registration_endpoint: origin + "/oauth/register", response_types_supported: ["code"], code_challenge_methods_supported: ["S256"],
    });
  }
  if (p === "/healthz") return send(res, 200, { ok: true });

  // Who am I.
  if (p === "/api/me") {
    const me = {
      auth_required: S.gated, authenticated: S.authed, principal: S.gated ? (S.authed ? "session" : "anonymous") : "open",
      name: S.name, version: "mock-bridge", providers: S.providers,
    };
    if (S.user && S.authed) me.user = S.user;
    return send(res, 200, me);
  }

  // Access code.
  if (p === "/auth/login" && m === "POST") {
    if (!S.gated) return send(res, 409, { error: "open mode" });
    S.loginTries++;
    if (S.loginLimit && S.loginTries > S.loginLimit) return send(res, 429, { error: "too many attempts" }, { "Retry-After": "42" });
    if (body.code !== S.code) return send(res, 401, { error: "invalid access code" });
    S.authed = true;
    S.user = null;
    return send(res, 204);
  }
  if (p === "/auth/logout" && m === "POST") {
    S.authed = false;
    S.user = null;
    return send(res, 204);
  }

  // Email sign-in.
  if (p === "/auth/email/start" && m === "POST") {
    const ctype = req.headers["content-type"] ?? "";
    const form = ctype.includes("application/x-www-form-urlencoded");
    if (!form && !ctype.includes("application/json")) return send(res, 415, { error: "use JSON or a form" });
    if (!S.providers.includes("email")) return send(res, 404, { error: "email sign-in is not configured" });
    const email = String(body.email ?? "").trim();
    if (!EMAIL_RE.test(email)) return send(res, 400, { error: "email must be a valid address" });
    S.emailTries++;
    if (S.emailLimit && S.emailTries > S.emailLimit) return send(res, 429, { error: "too many sign-in requests; try again later" }, { "Retry-After": "300" });
    const known = member(S, email);
    const domainOk = S.allowedDomains.includes(email.split("@")[1]?.toLowerCase()) || S.approvers.includes(email.toLowerCase());
    if ((known && known.status === "approved") || (!known && domainOk)) {
      issue(S, "signin", email, "signin", body.next, 15 * 60000);
    } else {
      if (!S.members) S.members = [];
      if (!known) {
        S.members.push({
          id: newId("m_").slice(0, 10), email, name: "", provider: "", role: "member", status: "pending",
          created_at: new Date().toISOString(), last_login_at: null, requested_at: new Date().toISOString(), approved_at: null, approved_by: null,
        });
      }
      S.mail.push({ kind: "request", to: email, token: "", link: "" });
      for (const a of S.approvers) issue(S, "approve", a, "approve", "/", 7 * 24 * 3600000, email);
    }
    return form ? redirect(res, 303, "/signin?sent=1") : send(res, 202, { ok: true });
  }
  // Emailed links need a click: the GET moves the token into a cookie and
  // shows one button; only the same-origin POST acts (mail scanners open links).
  if (p === "/auth/email/callback" || p === "/auth/email/approve") {
    const purpose = p.endsWith("callback") ? "signin" : "approve";
    const cookieName = purpose === "signin" ? "bridge_link" : "bridge_approve";
    const fromQuery = u.searchParams.get("token");
    const fromCookie = S.cookies[cookieOf(req, cookieName)];
    const tok = fromQuery ?? fromCookie ?? "";
    const t = S.tokens[tok];
    const bad = (why) => (purpose === "signin" ? redirect(res, 303, `/signin?error=${why}`) : page(res, why === "link_invalid" ? 404 : 410, "This approval link cannot be used", "Open the console's Members page instead."));
    if (!t || t.purpose !== purpose) return bad("link_invalid");
    if (t.used) return bad("link_used");
    if (t.expires < Date.now()) return bad("link_expired");
    if (m === "GET" && fromQuery) {
      const c = token();
      S.cookies[c] = tok;
      return redirect(res, 303, p, { "Set-Cookie": `${cookieName}=${c}; Path=/auth/email; HttpOnly; SameSite=Lax; Max-Age=600` });
    }
    if (m === "GET") {
      return purpose === "signin"
        ? buttonPage(res, `Sign in to ${S.name}`, `Continue as ${t.email}.`, "Continue", p)
        : buttonPage(res, `Approve ${t.email}?`, `${t.email} asks for access to ${S.name}.`, "Approve", p);
    }
    if (m !== "POST") return send(res, 405, { error: "method not allowed" });
    if (purpose === "approve") {
      t.used = true;
      const mem = member(S, t.email);
      if (mem && mem.status === "approved") return page(res, 200, "Already approved", `${t.email} can already sign in.`);
      if (mem) Object.assign(mem, { status: "approved", approved_at: new Date().toISOString(), approved_by: S.approvers[0] ?? "approvers" });
      issue(S, "approved", t.email, "signin", "/", 3600000);
      return page(res, 200, "Approved", `${t.email} can now sign in.`);
    }
    t.used = true;
    const mem = member(S, t.email);
    if (mem && mem.status === "pending") return redirect(res, 303, "/signin?status=pending");
    if (!mem) {
      S.members?.push({
        id: newId("m_").slice(0, 10), email: t.email, name: "", provider: "email", role: "member", status: "approved",
        created_at: new Date().toISOString(), last_login_at: new Date().toISOString(), requested_at: null, approved_at: new Date().toISOString(), approved_by: "domain",
      });
    } else {
      mem.provider = "email";
      mem.last_login_at = new Date().toISOString();
    }
    S.authed = true;
    S.user = { email: t.email, name: mem?.name ?? "", provider: "email" };
    return redirect(res, 303, t.next);
  }

  // Google and Microsoft: the mock signs in at once and returns to `next`.
  const sso = p.match(/^\/auth\/(google|microsoft)\/start$/);
  if (sso && m === "GET") {
    if (!S.providers.includes(sso[1])) return send(res, 404, { error: "not configured" });
    const email = u.searchParams.get("as") ?? (sso[1] === "google" ? "alex@example.com" : "sam@example.com");
    const mem = member(S, email);
    if (mem && mem.status === "pending") return page(res, 403, "Waiting for approval", `${email} has asked for access to ${S.name}. You can sign in once a member approves.`);
    S.authed = true;
    S.user = { email, name: "", provider: sso[1] };
    return redirect(res, 302, safeNext(u.searchParams.get("next")));
  }

  if (!p.startsWith("/api/")) return send(res, 404, { error: "not found" });
  if (S.gated && !S.authed) return send(res, 401, { error: "Unauthorized" });

  // Events.
  if (p === "/api/events") {
    res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store", Connection: "keep-alive" });
    res.write(": connected\n\n");
    S.clients.add(res);
    req.on("close", () => S.clients.delete(res));
    return;
  }

  // Sessions and pairing.
  if (p === "/api/sessions" && m === "GET") {
    if (S.failSessions > 0) {
      S.failSessions--;
      return send(res, 500, { error: "database is locked" });
    }
    return send(res, 200, S.sessions);
  }
  if (p === "/api/sessions/pair" && m === "POST") {
    if (S.pairLimited) return send(res, 429, { error: "too many linking attempts; try again shortly" }, { "Retry-After": "42" });
    S.pairs++;
    const s = { id: newId(), state: "pairing", created_at: new Date().toISOString(), message_count: 0, chat_count: 0 };
    S.sessions.push(s);
    const n = S.pairs;
    setTimeout(() => emit(S, { type: "pair", session_id: s.id, pair: { type: "qr", qr_png: fakeQr(n), timeout_seconds: 60 } }), 150);
    return send(res, 200, { id: s.id });
  }
  if (p === "/api/sessions/pair-code" && m === "POST") {
    if (!/^\d{7,15}$/.test(String(body.phone ?? ""))) return send(res, 400, { error: "invalid phone number" });
    const s = { id: newId(), state: "pairing", label: body.label || undefined, created_at: new Date().toISOString(), message_count: 0, chat_count: 0 };
    S.sessions.push(s);
    setTimeout(() => emit(S, { type: "pair", session_id: s.id, pair: { type: "code", code: "K7Q2M9XD", timeout_seconds: 180 } }), 100);
    return send(res, 200, { id: s.id, code: "K7Q2M9XD" });
  }
  let r = p.match(/^\/api\/sessions\/([^/]+)(\/.*)?$/);
  if (r) {
    const id = decodeURIComponent(r[1]);
    const sub = r[2] ?? "";
    const s = S.sessions.find((x) => x.id === id);
    if (!s) return send(res, 404, { error: "session not found" });
    if (sub === "" && m === "GET") return send(res, 200, s);
    if (sub === "" && m === "DELETE") {
      if (s.state === "connected" || s.state === "connecting") return send(res, 409, { error: "still linked; log out first" });
      S.sessions = S.sessions.filter((x) => x.id !== id);
      return send(res, 204);
    }
    if (sub === "/pairing" && m === "GET") {
      const step = S.steps[id] ?? null;
      return send(res, 200, { session: s, step, remaining_seconds: step ? 45 : 0 });
    }
    if (sub === "/cancel" && m === "POST") {
      if (s.state !== "pairing") return send(res, 409, { error: "not pairing" });
      S.sessions = S.sessions.filter((x) => x.id !== id);
      emit(S, { type: "pair", session_id: id, pair: { type: "cancelled" } });
      return send(res, 204);
    }
    if (sub === "/logout" && m === "POST") {
      s.state = "logged_out";
      S.chats[id] = [];
      s.chat_count = 0;
      s.message_count = 0;
      emit(S, { type: "state", session_id: id, state: "logged_out" });
      return send(res, 200, {});
    }
    if (sub === "/reconnect" && m === "POST") {
      s.state = "connecting";
      emit(S, { type: "state", session_id: id, state: "connecting" });
      setTimeout(() => {
        s.state = "connected";
        emit(S, { type: "state", session_id: id, state: "connected" });
      }, 300);
      return send(res, 204);
    }
    if (sub === "/passkey/confirm" && m === "POST") return send(res, 204);
    if (sub === "/chats" && m === "GET") return send(res, 200, S.chats[id] ?? []);
    const c = sub.match(/^\/chats\/([^/]+)$/);
    if (c && m === "PATCH") {
      const jid = decodeURIComponent(c[1]);
      const chat = (S.chats[id] ?? []).find((x) => x.jid === jid);
      if (!chat) return send(res, 404, { error: "chat not found" });
      chat.ticked = !!body.ticked;
      if (!chat.ticked) {
        chat.message_count = 0;
        if (S.messages[id]) S.messages[id][jid] = [];
      }
      return send(res, 200, chat);
    }
    const msg = sub.match(/^\/chats\/([^/]+)\/messages$/);
    if (msg && m === "GET") {
      const jid = decodeURIComponent(msg[1]);
      const chat = (S.chats[id] ?? []).find((x) => x.jid === jid);
      if (!chat) return send(res, 404, { error: "chat not found" });
      if (chat.ticked === false) return send(res, 200, []);
      return send(res, 200, S.messages[id]?.[jid] ?? []);
    }
    const ctx = sub.match(/^\/chats\/([^/]+)\/messages\/([^/]+)\/context$/);
    if (ctx && m === "GET") {
      const list = S.messages[id]?.[decodeURIComponent(ctx[1])] ?? [];
      const i = list.findIndex((x) => x.id === decodeURIComponent(ctx[2]));
      if (i < 0) return send(res, 404, { error: "message not found" });
      return send(res, 200, list.slice(Math.max(0, i - 10), i + 11));
    }
    if (sub === "/logs") return send(res, 200, S.logs.filter((l) => l.session_id === id));
    return send(res, 404, { error: "not found" });
  }
  if (p === "/api/logs") return send(res, 200, S.logs);

  // Search, over ticked chats only.
  if (p === "/api/search" && m === "GET") {
    const q = (u.searchParams.get("q") ?? "").toLowerCase();
    const hits = [];
    for (const s of S.sessions)
      for (const c of S.chats[s.id] ?? []) {
        if (c.ticked === false) continue;
        for (const msg of S.messages[s.id]?.[c.jid] ?? []) if (q && msg.text.toLowerCase().includes(q)) hits.push({ message: msg, chat_name: c.name, session_id: s.id });
      }
    return send(res, 200, hits.reverse().slice(0, Number(u.searchParams.get("limit") ?? 50)));
  }

  // Keys.
  if (p === "/api/keys") {
    if (m === "GET") return send(res, 200, S.keys);
    if (m === "POST") {
      if (body.kind !== undefined && body.kind !== "mcp") return send(res, 400, { error: 'kind must be "mcp"' });
      const k = { id: randomBytes(4).toString("hex"), label: body.label || "My assistant", kind: "mcp", created_at: new Date().toISOString(), last_used_at: null };
      S.keys.push(k);
      return send(res, 201, { ...k, key: `wb_live_${k.id}_${randomBytes(24).toString("hex")}` });
    }
  }
  r = p.match(/^\/api\/keys\/([^/]+)$/);
  if (r && m === "DELETE") {
    if (!S.keys.some((k) => k.id === r[1])) return send(res, 404, { error: "key not found" });
    S.keys = S.keys.filter((k) => k.id !== r[1]);
    return send(res, 204);
  }

  // OAuth grants: 404 when the feature is off.
  if (p === "/api/oauth/grants" && m === "GET") {
    if (!S.grants) return send(res, 404, { error: "not found" });
    S.grantPolls++;
    if (S.autoGrantAfterMs && S.grants.length === 0 && !S.autoGrantTimer) {
      S.autoGrantTimer = setTimeout(() => {
        S.autoGrantTimer = null;
        if (S.grants && S.grants.length === 0) addGrant(S, "Claude Code", "https://claude.ai/oauth/claude-code-client-metadata");
      }, S.autoGrantAfterMs);
    }
    return send(res, 200, S.grants);
  }
  r = p.match(/^\/api\/oauth\/grants\/([^/]+)$/);
  if (r && m === "DELETE") {
    if (!S.grants || !S.grants.some((g) => g.id === r[1])) return send(res, 404, { error: "grant not found" });
    S.grants = S.grants.filter((g) => g.id !== r[1]);
    return send(res, 204);
  }

  // Members.
  if (p === "/api/members") {
    if (!S.members) return send(res, 404, { error: "not found" });
    if (m === "GET") return send(res, 200, S.members);
    if (m === "POST") {
      const email = String(body.email ?? "").trim();
      if (!EMAIL_RE.test(email)) return send(res, 400, { error: "invalid email" });
      const existing = member(S, email);
      if (existing && existing.status === "pending") {
        Object.assign(existing, { status: "approved", approved_at: new Date().toISOString(), approved_by: S.user?.email ?? "session", name: body.name || existing.name });
        if (S.providers.includes("email")) issue(S, "approved", email, "signin", "/", 3600000);
        return send(res, 201, existing);
      }
      if (existing) return send(res, 409, { error: "already a member" });
      const mem = {
        id: newId("m_").slice(0, 10), email, name: body.name ?? "", provider: "", role: "member", status: "approved",
        created_at: new Date().toISOString(), last_login_at: null, requested_at: null, approved_at: new Date().toISOString(), approved_by: S.user?.email ?? "session",
      };
      S.members.push(mem);
      if (S.providers.includes("email")) issue(S, "signin", email, "signin", "/", 15 * 60000);
      return send(res, 201, mem);
    }
  }
  r = p.match(/^\/api\/members\/([^/]+)\/approve$/);
  if (r && m === "POST") {
    const mem = (S.members ?? []).find((x) => x.id === r[1]);
    if (!mem) return send(res, 404, { error: "member not found" });
    if (mem.status === "approved") return send(res, 200, mem);
    Object.assign(mem, { status: "approved", approved_at: new Date().toISOString(), approved_by: S.user?.email ?? "session" });
    issue(S, "approved", mem.email, "signin", "/", 3600000);
    return send(res, 200, mem);
  }
  r = p.match(/^\/api\/members\/([^/]+)$/);
  if (r && m === "DELETE") {
    const gone = (S.members ?? []).find((x) => x.id === r[1]);
    if (!gone) return send(res, 404, { error: "member not found" });
    S.members = S.members.filter((x) => x.id !== r[1]);
    // Removing a member also invalidates its outstanding links.
    for (const t of Object.values(S.tokens)) if (t.email === gone.email) t.used = true;
    return send(res, 204);
  }
  return send(res, 404, { error: "not found" });
}
