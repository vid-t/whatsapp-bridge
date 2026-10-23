// Wire types of the bridge API (internal/wa/types.go and internal/web).

export type SessionState =
  | "unpaired"
  | "pairing"
  | "connecting"
  | "connected"
  | "disconnected"
  | "logged_out"
  | "banned";

export interface SessionInfo {
  id: string;
  label?: string;
  jid?: string;
  phone?: string;
  push_name?: string;
  platform?: string;
  business_name?: string;
  state: SessionState;
  last_error?: string;
  created_at: string;
  connected_at?: string;
  last_message_at?: string;
  message_count: number;
  chat_count: number;
}

export interface Chat {
  session_id: string;
  jid: string;
  name: string;
  is_group: boolean;
  last_message_at?: string;
  message_count: number;
  /** Ticked chats are stored and reach AI assistants. */
  ticked: boolean;
}

/** A stored message (internal/wa Message). Oldest first from the messages endpoint. */
export interface Message {
  session_id: string;
  chat_jid: string;
  id: string;
  sender_jid: string;
  sender_name?: string;
  timestamp: string;
  from_me: boolean;
  /** text, image, video, audio, document, sticker, location, contact, poll, reaction */
  kind: string;
  text: string;
}

export interface LogEntry {
  seq: number;
  time: string;
  session_id?: string;
  type: string;
  detail: string;
}

export type PairType =
  | "qr"
  | "code"
  | "timeout"
  | "success"
  | "error"
  | "passkey_request"
  | "passkey_code"
  | "scanned_without_md"
  | "cancelled";

export interface PairStep {
  type: PairType;
  code?: string;
  timeout_seconds?: number;
  error?: string;
  qr_png?: string;
}

export type EventType = "state" | "pair" | "message" | "chats" | "log";

export interface BridgeEvent {
  time?: string;
  session_id?: string;
  type: EventType;
  state?: SessionState;
  pair?: PairStep;
  text?: string;
  message?: Message;
}

export interface PairingState {
  session: SessionInfo;
  step: BridgeEvent | null;
  remaining_seconds: number;
}

export type Provider = "code" | "email" | "google" | "microsoft";

export interface Me {
  auth_required: boolean;
  authenticated: boolean;
  principal: "session" | "key" | "open" | "anonymous" | string;
  /** The bridge's display name (BRIDGE_NAME). */
  name?: string;
  version?: string;
  /** Enabled sign-in methods, in the bridge's order. */
  providers?: string[];
  /** Present for a member session (email, Google or Microsoft); absent for an access-code session. */
  user?: { email: string; name?: string; provider?: string };
}

export interface KeyInfo {
  id: string;
  label: string;
  kind?: string;
  created_at: string;
  last_used_at?: string | null;
}

export interface NewKey extends KeyInfo {
  /** The secret, shown once. Opaque. */
  key: string;
}

/** An OAuth grant: one AI assistant that signed in. */
export interface Grant {
  id: string;
  client_name: string;
  client_id: string;
  created_at: string;
  last_used_at?: string | null;
}

export interface Member {
  id: string;
  email: string;
  name?: string;
  provider?: string;
  role?: string;
  status?: "approved" | "pending";
  created_at?: string;
  last_login_at?: string | null;
  requested_at?: string | null;
  approved_at?: string | null;
  approved_by?: string | null;
}
