import type { SessionInfo, SessionState } from "./types";

/** How a status reads: a colour tone plus a word (never colour alone). */
export type Tone = "ok" | "warn" | "bad" | "off";

const STATE: Record<SessionState, [string, Tone]> = {
  connected: ["Connected", "ok"],
  connecting: ["Connecting", "warn"],
  disconnected: ["Disconnected", "off"],
  pairing: ["Linking", "warn"],
  logged_out: ["Logged out", "off"],
  unpaired: ["Not linked", "off"],
  banned: ["Restricted by WhatsApp", "bad"],
};

export const stateInfo = (s: string): { label: string; tone: Tone } => {
  const v = STATE[s as SessionState];
  return v ? { label: v[0], tone: v[1] } : { label: s, tone: "off" };
};

/** What the state means, in one line. */
export function stateNote(s: Pick<SessionInfo, "state">) {
  switch (s.state) {
    case "connected":
      return "Receiving. New messages in ticked chats are stored.";
    case "connecting":
      return "Reconnecting to WhatsApp.";
    case "disconnected":
      return "Not receiving. Reconnect, or check that the phone is online.";
    case "logged_out":
      return "Unlinked on the phone. Nothing new arrives until you link it again.";
    case "banned":
      return "WhatsApp restricted this account. Nothing new arrives.";
    case "pairing":
      return "Waiting for the phone to scan the code.";
    default:
      return "";
  }
}

/** The name an account is known by. */
export const sessionTitle = (s: Pick<SessionInfo, "push_name" | "business_name" | "phone" | "label">) =>
  s.push_name || s.business_name || (s.phone ? fmtPhone(s.phone) : s.label || "New account");

const PLATFORMS: Record<string, string> = {
  iphone: "iPhone",
  ios: "iPhone",
  android: "Android",
  smba: "WhatsApp Business on Android",
  smbi: "WhatsApp Business on iPhone",
  web: "Web",
  macos: "Mac",
  windows: "Windows",
  ipad: "iPad",
};
export const platformName = (p?: string) => (p ? (PLATFORMS[p.toLowerCase()] ?? p) : "");

function validDate(t?: string | null): Date | null {
  if (!t) return null;
  const d = new Date(t);
  return Number.isNaN(d.getTime()) || d.getFullYear() < 2000 ? null : d;
}

/** Time of day for today, otherwise date and time. */
export function fmtTime(t?: string | null): string {
  const d = validDate(t);
  if (!d) return "";
  if (d.toDateString() === new Date().toDateString()) {
    return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  }
  return d.toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
}

/** Compact, for narrow lists: the time today, "Oct 20" this year, else "Oct 20, 2025". */
export function fmtShort(t?: string | null): string {
  const d = validDate(t);
  if (!d) return "";
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  if (d.getFullYear() === now.getFullYear()) return d.toLocaleDateString([], { month: "short", day: "numeric" });
  return d.toLocaleDateString([], { dateStyle: "medium" });
}

export function fmtDate(t?: string | null): string {
  const d = validDate(t);
  return d ? d.toLocaleDateString([], { dateStyle: "medium" }) : "";
}

export const plural = (n: number, one: string, many = one + "s") => `${n.toLocaleString()} ${n === 1 ? one : many}`;

export function fmtPairCode(code: string) {
  return code.length === 8 ? code.slice(0, 4) + "-" + code.slice(4) : code;
}

export function mmss(seconds: number) {
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
}

/** "just now", "4 min ago", "3 h ago", then the date. */
export function ago(t?: string | null): string {
  const d = validDate(t);
  if (!d) return "";
  const s = Math.max(0, (Date.now() - d.getTime()) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return fmtDate(t);
}

/** A phone number in international form: "+" and the digits. */
export function fmtPhone(digits?: string) {
  if (!digits) return "";
  return "+" + digits.replace(/\D/g, "");
}

const EVENT: Record<string, string> = {
  "state.connected": "Connected",
  "state.connecting": "Connecting",
  "state.disconnected": "Disconnected",
  "state.logged_out": "Logged out on the phone",
  "state.banned": "Restricted by WhatsApp",
  "state.pairing": "Linking started",
  "state.unpaired": "Not linked",
  "pair.success": "Linked",
  "pair.qr": "QR code shown",
  "pair.code": "Phone code shown",
  "pair.timeout": "Linking code expired",
  "pair.cancelled": "Linking cancelled",
  "pair.error": "Linking failed",
  "pair.passkey_request": "Passkey requested",
  "pair.passkey_code": "Passkey code shown",
  "pair.scanned_without_md": "Scanned without multi-device",
  "chats.sync": "Chats synced",
  "chats.ticked": "Chat ticked",
  "chats.unticked": "Chat unticked",
  "mcp.tool": "AI assistant read",
  "oauth.consent_granted": "AI assistant connected",
  "oauth.consent_denied": "AI assistant refused",
  "oauth.revoke": "AI assistant revoked",
  "auth.login": "Signed in",
  "auth.login_failed": "Sign-in refused",
  "member.approved": "Member approved",
  "member.invited": "Member invited",
  "member.removed": "Member removed",
  "member.requested": "Access requested",
  log: "Note",
};

const TOOL: Record<string, string> = {
  search_messages: "searched messages",
  read_chat: "read a chat",
  get_message_context: "read around a message",
  get_status: "checked the status",
  list_chats: "listed the chats",
  get_document: "opened a document",
};

/** A log line in plain words: what happened, then the detail. */
export function describeEvent(type: string, detail: string) {
  const what = EVENT[type] ?? type.replace(/[._]/g, " ").replace(/^./, (c) => c.toUpperCase());
  // "history sync: 9 chats", or key=value pairs whose values may hold spaces
  // ("device=My bridge"): a value runs to the next key= or the end.
  const first = detail.search(/(^|\s)\w+=/);
  const head = first < 0 ? detail : detail.slice(0, first);
  const tail = first < 0 ? "" : detail.slice(first);
  const kv = Object.fromEntries(
    [...tail.matchAll(/(\w+)=("[^"]*"|.*?)(?=\s+\w+=|$)/g)].map((m) => [m[1], m[2].trim().replace(/^"|"$/g, "")]),
  ) as Record<string, string>;
  const parts: string[] = [];
  if (kv.device) parts.push(`as ${kv.device}`);
  if (kv.tool) parts.push(TOOL[kv.tool] ?? kv.tool.replace(/_/g, " "));
  if (kv.rows) parts.push(`${kv.rows} ${kv.rows === "1" ? "row" : "rows"}`);
  if (kv.client) parts.push(kv.client);
  if (kv.email) parts.push(kv.email);
  if (kv.by) parts.push(`by ${kv.by}`);
  if (kv.via) parts.push(`with ${kv.via === "code" ? "the access code" : kv.via}`);
  if (kv.key) parts.push(kv.key.startsWith("oa_") ? `grant ${kv.key}` : `key ${kv.key}`);
  const plain = head.replace(/^history sync:\s*/, "").trim();
  return { what, detail: [plain, ...parts].filter(Boolean).join(" · ") };
}
