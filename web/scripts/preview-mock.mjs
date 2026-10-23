// A click-through preview of the built UI against the mock bridge, without a
// real WhatsApp account. Linking completes by itself a few seconds after a QR
// or code shows, as if a phone had scanned it; on Connect AI, "Claude Code"
// connects by itself a little later; sign-in emails are printed here instead
// of sent.
//
//   npm run build && npm run preview:mock           (http://127.0.0.1:4174)
//   PORT=4180 CODE=my-code npm run preview:mock
//   LINKED=1 npm run preview:mock                   (start with two linked accounts)
//   NAME="Home server" npm run preview:mock         (a bridge display name)
//
// Sign in with the access code CODE (default "demo-code"), with Google or
// Microsoft (the mock signs you in at once), or by email: the link appears in
// this terminal. The mock is one shared state for everyone who opens it.
import { spawn } from "node:child_process";
import { createMock } from "./mock-bridge.mjs";

const PORT = process.env.PORT ?? "4174";
const HOST = process.env.HOST ?? "127.0.0.1";
const CODE = process.env.CODE ?? "demo-code";
const LINK_AFTER_MS = Number(process.env.LINK_AFTER_MS ?? 6000);
const GRANT_AFTER_MS = Number(process.env.GRANT_AFTER_MS ?? 12000);
const now = new Date().toISOString();

const S = await createMock({
  gated: true,
  code: CODE,
  name: process.env.NAME,
  providers: ["code", "email", "google", "microsoft"],
  allowedDomains: ["example.com"],
  oauth: true,
  grants: [],
  autoGrantAfterMs: GRANT_AFTER_MS,
  sessions: process.env.LINKED === "1" ? "linked" : undefined,
  work: process.env.LINKED === "1",
  members: [
    { id: "m_alex", email: "alex@example.com", name: "Alex Morgan", provider: "email", role: "member", status: "approved", created_at: now, last_login_at: now, requested_at: null, approved_at: now, approved_by: "session" },
    { id: "m_robin", email: "robin@example.org", name: "", provider: "", role: "member", status: "pending", created_at: now, last_login_at: null, requested_at: now, approved_at: null, approved_by: null },
  ],
});

// "Scan" every pairing a little after its QR or code appears.
const shown = new Map();
setInterval(() => {
  for (const s of S.sessions) {
    if (s.state !== "pairing") continue;
    const step = S.steps[s.id];
    if (!step || !["qr", "code"].includes(step.pair?.type)) continue;
    if (!shown.has(s.id)) shown.set(s.id, Date.now());
    if (Date.now() - shown.get(s.id) >= LINK_AFTER_MS) {
      shown.delete(s.id);
      S.link(s.id);
    }
  }
}, 250);

// Print every mail the bridge would send, with a link that works here.
let printed = 0;
setInterval(() => {
  for (; printed < S.mail.length; printed++) {
    const m = S.mail[printed];
    const where = m.link ? `http://${HOST}:${PORT}${m.link}` : "(no link: the request went to the approvers)";
    console.log(`preview-mock: mail "${m.kind}" to ${m.to}: ${where}`);
  }
}, 250);

// BRIDGE_URL always points at the mock, never at a real bridge.
const vite = spawn("npx", ["vite", "preview", "--host", HOST, "--port", PORT, "--strictPort"], {
  env: { ...process.env, BRIDGE_URL: S.url },
  stdio: "inherit",
});
vite.on("exit", (code) => {
  S.close().then(() => process.exit(code ?? 0));
});
for (const sig of ["SIGINT", "SIGTERM"]) process.on(sig, () => vite.kill(sig));

console.log(`preview-mock: UI on http://${HOST}:${PORT}, mock bridge on ${S.url}, access code "${CODE}"`);
