// The linking state machine. One pairing at a time; generation counters drop
// stale responses (an orphaned pairing is cancelled); steps are ordered by
// server time only; countdowns run from now or from the server's
// remaining_seconds; a reload resumes through GET /pairing; leaving the page
// cancels via sendBeacon.
import { ApiError, api, errorText } from "./api";
import { fmtPairCode } from "./format";
import type { BridgeEvent, PairingState } from "./types";
import { enc } from "./utils";

export type PairMode = "qr" | "code";
export type Screen = "qr" | "code-form" | "code" | "passkey" | "none";
export type MsgKind = "" | "ok" | "warn" | "err";

export interface PairView {
  mode: PairMode;
  screen: Screen;
  qrPng: string;
  code: string;
  passkeyCode: string;
  passkeyBusy: boolean;
  /** Countdown deadline (ms epoch) for the QR or code on screen. */
  until: number;
  message: { text: string; kind: MsgKind } | null;
  cancelVisible: boolean;
  cancelBusy: boolean;
  retryVisible: boolean;
  /** After a 429, the earliest time (ms epoch) Retry is allowed. */
  retryAt: number;
  requesting: boolean;
  done: boolean;
  linkedId: string | null;
}

export const PAIR_KEY = "bridge.pairing";
export const PASSKEY_MSG =
  "Your account requires passkey linking, which the bridge cannot complete in this browser yet. " +
  "Try the phone-number code method or temporarily turn off passkeys in WhatsApp.";
export const NO_MD_MSG = "Your phone scanned the code, but multi-device is not enabled. Update WhatsApp, then scan again.";

export interface PairingHooks {
  notify: (text: string) => void;
  clearError: () => void;
  successText: string;
  onSuccess: (sessionId: string) => void;
}

const cancelPairing = (id: string) => api.post(`/sessions/${enc(id)}/cancel`);

const INITIAL: PairView = {
  mode: "qr",
  screen: "none",
  qrPng: "",
  code: "",
  passkeyCode: "",
  passkeyBusy: false,
  until: 0,
  message: null,
  cancelVisible: false,
  cancelBusy: false,
  retryVisible: false,
  retryAt: 0,
  requesting: false,
  done: false,
  linkedId: null,
};

export class PairingController {
  view: PairView = INITIAL;
  private id: string | null = null;
  private done = false;
  private last = 0;
  private gen = 0;
  private early: Record<string, BridgeEvent> = {};
  private listeners = new Set<() => void>();
  private disposed = false;
  private hooks: PairingHooks;

  constructor(hooks: PairingHooks) {
    this.hooks = hooks;
  }

  setHooks(hooks: PairingHooks) {
    this.hooks = hooks;
  }

  subscribe = (l: () => void) => {
    this.listeners.add(l);
    return () => void this.listeners.delete(l);
  };
  getSnapshot = () => this.view;

  private set(patch: Partial<PairView>) {
    if (this.disposed) return;
    this.view = { ...this.view, ...patch };
    this.listeners.forEach((l) => l());
  }

  private message(text: string, kind: MsgKind = "") {
    this.set({ message: text ? { text, kind } : null });
  }

  // ---- lifecycle -------------------------------------------------------------

  private onPageHide = () => {
    if (!this.id || this.done) return;
    navigator.sendBeacon(`/api/sessions/${enc(this.id)}/cancel`);
    sessionStorage.removeItem(PAIR_KEY);
  };
  private onPageShow = (e: PageTransitionEvent) => {
    if (e.persisted) location.reload();
  };

  init() {
    window.addEventListener("pagehide", this.onPageHide);
    window.addEventListener("pageshow", this.onPageShow);
    void this.resume();
  }

  /** dispose runs when the linking view unmounts: an unfinished pairing is cancelled. */
  dispose() {
    window.removeEventListener("pagehide", this.onPageHide);
    window.removeEventListener("pageshow", this.onPageShow);
    this.gen++; // in-flight starts now cancel their orphan
    if (this.id && !this.done) {
      cancelPairing(this.id).catch(() => {});
      sessionStorage.removeItem(PAIR_KEY);
    }
    this.disposed = true;
  }

  // Resume a pairing the page did not get to cancel, or start a fresh QR flow.
  private async resume() {
    let saved: { id?: string; mode?: string } | null = null;
    try {
      saved = JSON.parse(sessionStorage.getItem(PAIR_KEY) ?? "null");
    } catch {
      /* ignore */
    }
    if (saved && saved.id && (saved.mode === "qr" || saved.mode === "code")) {
      this.id = saved.id;
      this.set({ mode: saved.mode, screen: saved.mode === "qr" ? "qr" : "code", cancelVisible: true });
      if (await this.restore(saved.id)) return;
    }
    if (!this.disposed) void this.begin("qr");
  }

  // restore applies the server's view of a pairing; false once it is gone.
  private async restore(id: string): Promise<boolean> {
    try {
      const p = await api.get<PairingState>(`/sessions/${enc(id)}/pairing`);
      if (id !== this.id) return true;
      this.hooks.clearError();
      const st = p.session.state;
      if (st === "connecting" || st === "connected") this.success();
      else if (st !== "pairing") return false;
      else if (p.step) this.onStep(p.step, p.remaining_seconds);
      return true;
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) return false;
      this.hooks.notify(errorText(err));
      return true;
    }
  }

  /** resync runs after the event stream reconnects. */
  async resync() {
    if (!this.id || this.done) return;
    if (!(await this.restore(this.id)) && !this.done) this.end("Linking ended. Start again.");
  }

  // ---- user actions ------------------------------------------------------------

  switchTo(mode: PairMode) {
    if (mode === this.view.mode) return;
    if (this.id && !this.done) cancelPairing(this.id).catch(() => {});
    void this.begin(mode);
  }

  async begin(mode: PairMode) {
    this.id = null;
    this.done = false;
    this.last = 0;
    this.gen++;
    sessionStorage.removeItem(PAIR_KEY);
    this.set({
      ...INITIAL,
      mode,
      screen: mode === "code" ? "code-form" : "qr",
    });
    if (mode === "code") return;
    await this.start(
      () => api.post<{ id: string }>("/sessions/pair", {}),
      (err) => this.end("Could not start linking: " + errorText(err), err),
    );
  }

  async requestCode(rawPhone: string, label: string) {
    const phone = rawPhone.replace(/[\s+().-]/g, "");
    if (!/^\d{7,15}$/.test(phone)) {
      this.message("Enter the full number with country code, for example 41791234567.", "err");
      return;
    }
    this.set({ requesting: true });
    this.message("Asking WhatsApp for a code. This can take up to 20 seconds.");
    const res = await this.start(
      () => api.post<{ id: string; code: string }>("/sessions/pair-code", { phone, label: label.trim() }),
      (err) => {
        this.message(errorText(err), "err");
        if (err instanceof ApiError && err.status === 429 && err.retryAfter) {
          this.set({ retryAt: Date.now() + err.retryAfter * 1000 });
        }
      },
    );
    this.set({ requesting: false });
    if (res) this.onStep({ type: "pair", pair: { type: "code", code: res.code, timeout_seconds: 180 } });
  }

  async cancel() {
    if (!this.id || this.done) return;
    this.set({ cancelBusy: true });
    try {
      await cancelPairing(this.id);
      this.end("Linking cancelled.");
    } catch (err) {
      const gone = err instanceof ApiError && (err.status === 404 || err.status === 409);
      this.end(gone ? "Linking had already ended." : errorText(err));
    }
    this.set({ cancelBusy: false });
  }

  async confirmPasskey() {
    if (!this.id) return;
    this.set({ passkeyBusy: true });
    try {
      await api.post(`/sessions/${enc(this.id)}/passkey/confirm`);
      this.message("Confirmed. Waiting for WhatsApp to finish linking…");
    } catch (err) {
      this.message(errorText(err), "err");
      this.set({ passkeyBusy: false });
    }
  }

  retry() {
    void this.begin(this.view.mode);
  }

  // ---- server events -------------------------------------------------------------

  // start runs a pairing request and adopts the new session, unless the user
  // moved on meanwhile, in which case the orphan is cancelled.
  private async start<T extends { id: string }>(request: () => Promise<T>, onError: (err: unknown) => void) {
    const gen = this.gen;
    try {
      const res = await request();
      if (gen !== this.gen) {
        cancelPairing(res.id).catch(() => {});
        return null;
      }
      this.id = res.id;
      sessionStorage.setItem(PAIR_KEY, JSON.stringify({ id: res.id, mode: this.view.mode }));
      this.set({ cancelVisible: true });
      const early = this.early[res.id];
      this.early = {};
      if (early) this.onStep(early);
      return res;
    } catch (err) {
      if (gen === this.gen) onError(err);
      return null;
    }
  }

  onEvent(ev: BridgeEvent) {
    if (!this.id) {
      // Steps can arrive before the POST that started pairing has returned.
      if (ev.type === "pair" && ev.session_id) this.early[ev.session_id] = ev;
      return;
    }
    if (ev.session_id !== this.id || this.done) return;
    if (ev.type === "pair" && ev.pair) this.onStep(ev);
    if (ev.type === "state") {
      if (ev.state === "connecting" || ev.state === "connected") this.success();
      else if (ev.state !== "pairing") this.end(ev.text ? `Linking ended: ${ev.text}.` : "Linking ended.");
    }
  }

  // onStep renders a pairing step. Ordering uses server timestamps only;
  // timers count from now (or from the server's remaining seconds).
  private onStep(ev: BridgeEvent, remaining?: number) {
    if (this.done) return;
    if (ev.time) {
      const t = Date.parse(ev.time);
      if (t < this.last) return; // a stale step from restore()
      this.last = t;
    } else if (this.last) {
      return; // a step built from a POST response; the real one already arrived
    }
    const p = ev.pair;
    if (!p) return;
    const until = Date.now() + (remaining ?? p.timeout_seconds ?? 0) * 1000;
    switch (p.type) {
      case "qr":
        this.set({ screen: "qr", qrPng: p.qr_png || "", until });
        break;
      case "code":
        this.set({ screen: "code", message: null, code: fmtPairCode(p.code || ""), until });
        break;
      case "passkey_code":
        this.set({ screen: "passkey", message: null, passkeyCode: p.code || "", passkeyBusy: false, until: 0 });
        break;
      case "scanned_without_md":
        return this.message(NO_MD_MSG, "warn");
      case "passkey_request":
        return this.message(PASSKEY_MSG, "warn");
      case "success":
        return this.success();
      case "timeout":
        return this.end(
          this.view.mode === "qr" ? "The QR codes expired before a phone scanned one." : "The linking code expired.",
        );
      case "error":
        return this.end("Linking failed: " + (p.error || "unknown error"));
      case "cancelled":
        return this.end("Linking cancelled.");
    }
  }

  private finish(text: string, kind: MsgKind) {
    if (this.done) return false;
    this.done = true;
    sessionStorage.removeItem(PAIR_KEY);
    this.set({ screen: "none", cancelVisible: false, done: true, until: 0 });
    this.message(text, kind);
    return true;
  }

  private success() {
    const id = this.id;
    if (this.finish(this.hooks.successText, "ok") && id) {
      this.set({ linkedId: id });
      this.hooks.onSuccess(id);
    }
  }

  private end(text: string, err?: unknown) {
    if (this.finish(text, "err")) {
      const wait = err instanceof ApiError && err.status === 429 && err.retryAfter ? err.retryAfter : 0;
      this.set({ retryVisible: true, retryAt: wait ? Date.now() + wait * 1000 : 0 });
    }
  }
}
