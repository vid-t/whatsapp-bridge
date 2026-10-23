// Browser smoke test of the built UI against the mock bridge
// (scripts/mock-bridge.mjs), never a real account. It starts the mock and
// `vite preview` (which sends the bridge's CSP) on free ports, then walks every
// page at 1280 and 390 px wide, light and dark: sign in, accounts, link (the QR
// shows, then the mock links it), the account page (read a chat, untick and tick it),
// Connect AI, keys (create and revoke), members (approve), not found,
// sign out. It fails on console errors, CSP violations and axe WCAG 2.1 A/AA
// violations. Screenshots go to $SHOTS.
//
//   npm run build && npm run smoke
import { spawn } from "node:child_process";
import { mkdir, readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import net from "node:net";
import { chromium } from "playwright";
import { createMock } from "./mock-bridge.mjs";

const OUT = process.env.SHOTS ?? "/tmp/whatsapp-bridge-ui-v2";
const CODE = "test-code";
await mkdir(OUT, { recursive: true });
const axeSource = await readFile(createRequire(import.meta.url).resolve("axe-core/axe.min.js"), "utf8");

const failures = [];
let passed = 0;
const note = (ok, what, detail = "") => {
  if (ok) passed++;
  else failures.push(`${what}${detail ? ": " + detail : ""}`);
  console.log(`${ok ? "ok  " : "FAIL"} ${what}${detail ? " · " + detail : ""}`);
};

// ---- servers: the mock, and vite preview proxying to it (never to a real bridge)
const freePort = () =>
  new Promise((r) => {
    const s = net.createServer();
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => r(port));
    });
  });
const S = await createMock();
const port = await freePort();
const vite = spawn("npx", ["vite", "preview", "--host", "127.0.0.1", "--port", String(port), "--strictPort"], {
  env: { ...process.env, BRIDGE_URL: S.url },
  stdio: ["ignore", "pipe", "pipe"],
});
let viteLog = "";
vite.stdout.on("data", (d) => (viteLog += d));
vite.stderr.on("data", (d) => (viteLog += d));
const BASE = `http://127.0.0.1:${port}`;
for (let i = 0; ; i++) {
  if (await fetch(BASE + "/").then((r) => r.ok, () => false)) break;
  if (i > 80) throw new Error("vite preview did not start:\n" + viteLog);
  await new Promise((r) => setTimeout(r, 250));
}
console.log(`smoke: ${BASE} -> mock ${S.url}; screenshots in ${OUT}`);

// The theme is applied by a plain script in <head>, before the first paint
// (the CSP allows no inline script, so it is a hashed file under /static/).
{
  const shell = await fetch(BASE + "/").then((r) => r.text());
  const head = shell.slice(0, shell.indexOf("</head>"));
  const boot = head.match(/<script src="(\/static\/theme-[^"]+\.js)"><\/script>/);
  note(!!boot, "theme script runs in <head> before the first paint");
  if (boot) {
    const js = await fetch(BASE + boot[1]).then((r) => r.text());
    note(js.includes("bridge.theme") && js.includes("prefers-color-scheme"), "theme script reads the stored choice and the system");
  }
}

const now = new Date().toISOString();
const ago = (mins) => new Date(Date.now() - mins * 60000).toISOString();
const scenario = () => ({
  gated: true,
  name: "Home server",
  code: CODE,
  providers: ["code", "email", "google", "microsoft"],
  grants: [
    { id: "oa_0d8efbf4838d04f2", client_name: "Claude", client_id: "https://claude.ai/oauth/mcp-oauth-client-metadata", created_at: ago(60 * 24 * 9), last_used_at: ago(30) },
    { id: "oa_7c21e9a04b5d3f18", client_name: "Claude Code", client_id: "https://claude.ai/oauth/claude-code-client-metadata", created_at: ago(60 * 24 * 2), last_used_at: ago(180) },
    { id: "oa_3e9b0d6f1a2c4b77", client_name: "ChatGPT", client_id: "c_4f1d9a2b7e", created_at: ago(90), last_used_at: null },
  ],
  keys: [{ id: "5b1f0c2e", label: "Backup job", kind: "mcp", created_at: ago(60 * 24 * 12), last_used_at: ago(120) }],
  members: [
    { id: "m_alex", email: "alex@example.com", name: "Alex Morgan", provider: "email", status: "approved", created_at: ago(60 * 24 * 30), last_login_at: ago(15), approved_at: ago(60 * 24 * 30), approved_by: "session" },
    { id: "m_sam", email: "sam@example.com", name: "Sam Lee", provider: "google", status: "approved", created_at: ago(60 * 24 * 20), last_login_at: ago(60 * 26), approved_at: ago(60 * 24 * 20), approved_by: "alex@example.com" },
    { id: "m_jordan", email: "jordan@example.net", name: "Jordan Kim", provider: "", status: "approved", created_at: ago(60 * 24 * 3), last_login_at: null, approved_at: ago(60 * 24 * 3), approved_by: "alex@example.com" },
    { id: "m_robin", email: "robin@example.org", name: "", status: "pending", created_at: ago(12), requested_at: ago(12) },
  ],
});

// ---- one walk through every page, per variant
const EXPECTED = /Failed to load resource: the server responded with a status of (401|404|409)/;
const browser = await chromium.launch();

async function walk(scheme, width) {
  const v = `${scheme}-${width}`;
  S.reset(scenario());
  const context = await browser.newContext({ colorScheme: scheme, viewport: { width, height: 900 }, reducedMotion: "reduce" });
  await context.addInitScript(() => {
    window.__csp = [];
    document.addEventListener("securitypolicyviolation", (e) => window.__csp.push(`${e.violatedDirective} ${e.blockedURI || "inline"}`));
  });
  const page = await context.newPage();
  const errors = [];
  page.on("console", (m) => m.type() === "error" && !EXPECTED.test(m.text()) && errors.push(m.text()));
  page.on("pageerror", (e) => errors.push("pageerror: " + e.message));

  const h1 = (text) => page.locator("h1", { hasText: text }).waitFor({ timeout: 10000 });
  // Screenshots wait for entrance animations (dialogs, sheets, rows) to finish,
  // with the pointer parked off the controls so no hover state shows.
  const settle = async () => {
    await page.mouse.move(width - 2, 898);
    await page.waitForFunction(() =>
      document.getAnimations().every((a) => a.effect?.getTiming().iterations === Infinity || a.playState !== "running"),
    );
    await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
  };
  const shoot = async (name, fullPage = false) => {
    await settle();
    await page.screenshot({ path: `${OUT}/${name}-${v}.png`, fullPage });
  };
  const check = async (name, shot = true, fullPage = true) => {
    const csp = await page.evaluate(() => window.__csp.splice(0));
    note(errors.length === 0, `${v} ${name}: no console errors`, errors.splice(0).join(" | "));
    note(csp.length === 0, `${v} ${name}: no CSP violations`, csp.join(" | "));
    await page.evaluate(axeSource);
    const ax = await page.evaluate(async () =>
      (await window.axe.run(document, { runOnly: { type: "tag", values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"] } })).violations.map(
        (x) => `${x.id} ×${x.nodes.length}: ${x.nodes[0]?.target}`,
      ),
    );
    note(ax.length === 0, `${v} ${name}: axe WCAG 2.1 A/AA`, ax.join(" | "));
    if (shot) await shoot(name, fullPage);
  };
  const mobile = width < 600;
  // On a phone the tabs live in a menu sheet.
  const nav = async (label) => {
    if (mobile) {
      await page.getByRole("button", { name: "Menu" }).click();
      await page.getByRole("dialog").waitFor();
    }
    await page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: label }).click();
  };
  const isDark = () => page.evaluate(() => document.documentElement.classList.contains("dark"));
  const stored = () => page.evaluate(() => localStorage.getItem("bridge.theme"));
  // Picks a theme from the header menu (desktop) or the menu sheet (phone).
  const pickTheme = async (label) => {
    if (mobile) {
      await page.getByRole("button", { name: "Menu" }).click();
      await page.getByRole("radio", { name: label }).click();
      await page.keyboard.press("Escape");
      await page.getByRole("dialog").waitFor({ state: "detached" });
    } else {
      await page.getByRole("button", { name: "Theme" }).click();
      await page.getByRole("menuitemradio", { name: label }).click();
    }
  };

  try {
    // Sign in with the access code.
    await page.goto(BASE + "/");
    await h1("Sign in");
    note(page.url().endsWith("/signin"), `${v} signed out lands on /signin`);
    await check("signin");
    await page.getByLabel("Access code").fill(CODE);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await h1("Accounts");
    await page.getByRole("heading", { name: "No WhatsApp account linked yet" }).waitFor();
    await check("accounts-empty");

    // Link: the QR shows; the phone-number code shows; then the mock links it.
    await page.getByRole("link", { name: "Link a WhatsApp account" }).first().click();
    await h1("Link a WhatsApp account");
    await page.locator("img.qr").waitFor({ timeout: 10000 });
    await check("link");
    await page.getByRole("tab", { name: "Phone number" }).click();
    await page.getByLabel("Phone number with country code").fill("+1 555 010 0000");
    await page.getByRole("button", { name: "Get a code" }).click();
    await page.getByText("Your code", { exact: true }).waitFor();
    await check("link-code");
    S.link(S.pairingId());
    await page.getByText("Opening the account…").waitFor();
    await shoot("link-success");
    await h1("Alex Morgan");
    await page.getByRole("table").first().waitFor();
    await page.getByRole("checkbox", { name: "Family" }).waitFor();
    await check("account");

    // Read a chat's stored messages, then close the viewer.
    await page.getByRole("button", { name: "View messages in Family" }).click();
    await page.getByRole("heading", { name: "Messages in Family" }).waitFor();
    await page.getByText("Lunch on Sunday at ours?").waitFor();
    await check("account-messages", true, false);
    await page.getByRole("button", { name: "Close", exact: true }).click();
    await page.getByRole("dialog").waitFor({ state: "detached" });

    // Untick a chat (asks first), then tick it again. The box is controlled:
    // it changes once the bridge answers, so click rather than uncheck().
    const family = page.getByRole("checkbox", { name: "Family" });
    await family.click();
    await page.getByRole("alertdialog").waitFor();
    await shoot("untick-dialog");
    await page.getByRole("button", { name: "Untick and delete" }).click();
    await page.getByRole("row", { name: /Family/ }).getByText("Not shared").waitFor();
    note(!(await family.isChecked()), `${v} untick Family`);
    await family.click();
    await page.getByRole("row", { name: /Family/ }).getByText("Shared", { exact: true }).waitFor();
    note(await family.isChecked(), `${v} tick Family again`);

    // A second, offline account makes the list look like a real bridge.
    S.addWork();
    await nav("Accounts");
    await h1("Accounts");
    await page.getByRole("link", { name: "Alex Morgan" }).waitFor();
    await page.getByRole("link", { name: "Alex (work)" }).waitFor();
    await check("accounts");

    await nav("Connect AI");
    await h1("Connect AI");
    await page.getByRole("button", { name: "Revoke Claude", exact: true }).waitFor();
    await check("assistants");

    // Keys: create one (shown once), then revoke it.
    await nav("Keys");
    await h1("Keys");
    await page.getByRole("row", { name: /Backup job/ }).waitFor();
    await page.getByLabel("Name").fill("Laptop script");
    await page.getByRole("button", { name: "Create key" }).click();
    const secret = await page.locator("code.secret").textContent();
    note(/^wb_live_/.test(secret ?? ""), `${v} key shown once`, secret?.slice(0, 16));
    await page.getByRole("row", { name: /Laptop script/ }).waitFor();
    await check("keys");
    await page.getByRole("button", { name: "Revoke Laptop script" }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: "Revoke" }).click();
    await page.getByRole("row", { name: /Laptop script/ }).waitFor({ state: "detached" });
    note(S.keys.length === 1 && S.keys[0].label === "Backup job", `${v} key revoked`);

    // Members: approve the pending request.
    await nav("Members");
    await h1("Members");
    await page.getByRole("button", { name: "Approve robin@example.org" }).waitFor();
    await check("members");
    await page.getByRole("button", { name: "Approve robin@example.org" }).click();
    await page.getByText("No one is waiting.").waitFor();
    note(S.members.find((m) => m.email === "robin@example.org")?.status === "approved", `${v} member approved`);

    if (mobile) {
      await page.getByRole("button", { name: "Menu" }).click();
      await page.getByRole("dialog").getByRole("link", { name: "Members" }).waitFor();
      await shoot("menu-open");
      await page.keyboard.press("Escape");
      await page.getByRole("dialog").waitFor({ state: "detached" });
    }

    // Theme: pick the other one, keep it across a reload, then go back to the system's.
    const other = scheme === "light" ? "Dark" : "Light";
    await pickTheme(other);
    note((await isDark()) === (other === "Dark") && (await stored()) === other.toLowerCase(), `${v} theme switches to ${other}`);
    await page.reload();
    await h1("Members");
    note((await isDark()) === (other === "Dark"), `${v} theme kept after reload`);
    if (!mobile) await check(`theme-${other.toLowerCase()}`);
    await pickTheme("System");
    note((await isDark()) === (scheme === "dark") && (await stored()) === null, `${v} theme back to system`);

    await page.goto(BASE + "/no-such-page");
    await h1("Page not found");
    await check("not-found");

    if (mobile) {
      await page.getByRole("button", { name: "Menu" }).click();
      await page.getByRole("dialog").getByRole("link", { name: "Accounts" }).waitFor();
      await page.getByRole("button", { name: "Sign out" }).click();
    } else {
      await page.getByRole("button", { name: "Account" }).click();
      await page.getByRole("menu").waitFor();
      await shoot("account-menu");
      await page.getByRole("menuitem", { name: "Sign out" }).click();
    }

    // Signed out: the sign-in page again, and /security no longer exists.
    await h1("Sign in");
    await page.goto(BASE + "/security");
    await h1("Sign in");
    } catch (err) {
    note(false, `${v} walk`, String(err).split("\n").slice(0, process.env.ONLY ? 12 : 1).join(" / "));
    await page.screenshot({ path: `${OUT}/failed-${v}.png`, fullPage: true }).catch(() => {});
  }
  await context.close();
}

try {
  // ONLY=dark-390 runs one variant (for debugging).
  for (const scheme of ["light", "dark"])
    for (const width of [1280, 390]) if (!process.env.ONLY || process.env.ONLY === `${scheme}-${width}`) await walk(scheme, width);
} finally {
  await browser.close();
  vite.kill("SIGTERM");
  await S.close();
}
console.log(`\n${passed}/${passed + failures.length} checks passed; screenshots in ${OUT}`);
process.exit(failures.length ? 1 : 0);
