import { ChevronDownIcon, CircleXIcon, Loader2Icon, QrCodeIcon, RotateCcwIcon, SmartphoneIcon, XIcon } from "lucide-react";
import { useEffect, useRef, useState, useSyncExternalStore, type CSSProperties, type FormEvent, type ReactNode } from "react";
import { FieldError, FieldHint, useSecondsLeft } from "./feedback";
import { Alert } from "./ui/alert";
import { Button } from "./ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "./ui/collapsible";
import { Input } from "./ui/input";
import { Label } from "./ui/label";
import { Skeleton } from "./ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "./ui/tabs";
import { useBridgeEvents } from "@/lib/events";
import { mmss } from "@/lib/format";
import { stagger } from "@/lib/motion";
import { useNotice } from "@/lib/notice";
import { NO_MD_MSG, PASSKEY_MSG, PairingController, type PairMode, type PairView } from "@/lib/pairing";
import { PRODUCT } from "@/lib/product";
import { cn } from "@/lib/utils";

/** A bar that drains to the deadline, and the time left beside a label. */
function Countdown({ until, label }: { until: number; label: string }) {
  const left = useSecondsLeft(until);
  const total = useRef({ until: 0, secs: 1 });
  if (total.current.until !== until) total.current = { until, secs: Math.max(1, Math.ceil((until - Date.now()) / 1000)) };
  const frac = Math.min(1, left / total.current.secs);
  return (
    <div className="grid w-full max-w-[232px] gap-2">
      <div className="h-1 overflow-hidden rounded-full bg-border">
        <div
          className="h-full origin-left rounded-full bg-live transition-transform duration-1000 ease-linear"
          style={{ transform: `scaleX(${frac})` }}
        />
      </div>
      <p className="flex justify-between text-xs text-muted-foreground">
        <span>{label}</span>
        <span className="font-mono tabular-nums">{mmss(left)}</span>
      </p>
    </div>
  );
}

/** A short code, one character per cell, the cells arriving one by one. */
function CodeCells({ code, label }: { code: string; label: string }) {
  const chars = code.replace(/-/g, "").split("");
  const half = Math.ceil(chars.length / 2);
  const cell = (c: string, i: number) => (
    <span
      key={i}
      style={stagger(i)}
      className="row-enter grid h-11 w-8 place-items-center rounded-md border bg-background font-mono text-lg font-medium shadow-xs"
    >
      {c}
    </span>
  );
  return (
    <p className="flex items-center gap-1" aria-label={`${label} ${code}`}>
      {chars.length === 8 ? (
        <>
          {chars.slice(0, half).map(cell)}
          <span className="mx-1.5 h-px w-2 bg-border-strong" aria-hidden="true" />
          {chars.slice(half).map((c, i) => cell(c, i + half))}
        </>
      ) : (
        chars.map(cell)
      )}
    </p>
  );
}

function CodeForm({ v, ctl }: { v: PairView; ctl: PairingController }) {
  const ref = useRef<HTMLInputElement>(null);
  const [phone, setPhone] = useState("");
  const wait = useSecondsLeft(v.retryAt);
  useEffect(() => {
    ref.current?.focus();
  }, []);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    void ctl.requestCode(phone, "");
  };
  const error = v.message?.kind === "err" ? v.message.text : "";
  return (
    <form onSubmit={submit} className="grid gap-4" noValidate>
      <div className="grid gap-2">
        <Label htmlFor="pair-phone">Phone number with country code</Label>
        <Input
          ref={ref}
          id="pair-phone"
          name="phone"
          type="tel"
          inputMode="tel"
          autoComplete="tel"
          placeholder="+1 555 010 0000"
          className="font-mono"
          value={phone}
          onChange={(e) => setPhone(e.target.value)}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? "pair-phone-error" : "pair-phone-hint"}
        />
        {error ? (
          <FieldError id="pair-phone-error">{error}</FieldError>
        ) : (
          <FieldHint id="pair-phone-hint">The number of the WhatsApp account to link. You then type a code on that phone.</FieldHint>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-3">
        <Button type="submit" disabled={v.requesting || wait > 0}>
          {v.requesting && <Loader2Icon className="animate-spin" aria-hidden="true" />}
          {wait > 0 ? `Try again in ${mmss(wait)}` : "Get a code"}
        </Button>
        {v.requesting && v.message && !error && <p className="fade-enter text-[13px] text-muted-foreground">{v.message.text}</p>}
      </div>
    </form>
  );
}

const QR_STEPS = [
  "Open WhatsApp on the phone.",
  "Go to Settings › Linked devices. On Android, open the three-dot menu › Linked devices.",
  "Tap Link a device and point the phone at this code.",
];
const CODE_STEPS = ["Open WhatsApp › Linked devices › Link a device.", "Tap Link with phone number instead.", "Type the code shown here."];

function Steps({ steps }: { steps: string[] }) {
  return (
    <ol className="grid gap-4">
      {steps.map((t, i) => (
        <li key={t} className="row-enter flex gap-3.5" style={stagger(i)}>
          <span
            className="grid size-6 shrink-0 place-items-center rounded-full border bg-background font-mono text-[11px] font-medium text-muted-foreground"
            aria-hidden="true"
          >
            {i + 1}
          </span>
          <span className="pt-0.5 leading-relaxed">{t}</span>
        </li>
      ))}
    </ol>
  );
}

/** What a screen reader hears; it changes only when the step does. */
function liveText(v: PairView) {
  if (v.done) return v.message?.text ?? "";
  if (v.screen === "qr") return v.qrPng ? "QR code ready. Scan it with the phone." : "Preparing a QR code.";
  if (v.screen === "code") return `Your code is ${v.code}.`;
  if (v.screen === "passkey") return `Check that your phone shows ${v.passkeyCode}.`;
  return "";
}

/** The right-hand stage: skeleton, QR, code, passkey, success or the end. */
function Stage({ v, ctl }: { v: PairView; ctl: PairingController }) {
  const linked = v.done && !!v.linkedId;
  const ended = v.done && !v.linkedId;
  let body: ReactNode;
  let key: string;

  if (linked) {
    key = "linked";
    body = (
      <div className="grid justify-items-center gap-4 text-center">
        <svg viewBox="0 0 56 56" className="pop-enter size-14 text-success" aria-hidden="true">
          <circle cx="28" cy="28" r="27" className="fill-success/12" />
          <circle cx="28" cy="28" r="27" fill="none" stroke="currentColor" strokeOpacity="0.35" strokeWidth="1.5" />
          <path
            d="M18 29l7 7 13-15"
            fill="none"
            stroke="currentColor"
            strokeWidth="3"
            strokeLinecap="round"
            strokeLinejoin="round"
            className="draw-path"
            style={{ "--len": 34 } as CSSProperties}
          />
        </svg>
        <div className="grid gap-1">
          <p className="text-base font-semibold">Linked</p>
          <p className="text-sm text-muted-foreground">Opening the account…</p>
        </div>
      </div>
    );
  } else if (ended) {
    key = "ended";
    body = (
      <div className="grid max-w-[260px] justify-items-center gap-4 text-center">
        <CircleXIcon className="pop-enter size-10 text-muted-foreground" strokeWidth={1.5} aria-hidden="true" />
        <p className="text-sm">{v.message?.text ?? "Linking ended."}</p>
        {v.retryVisible && <Retry v={v} ctl={ctl} />}
      </div>
    );
  } else if (v.screen === "qr") {
    key = v.qrPng ? "qr" : "qr-wait";
    body = v.qrPng ? (
      <div className="grid justify-items-center gap-5">
        <div className="rounded-xl bg-white p-3 shadow-sm ring-1 ring-black/8">
          <img key={v.qrPng} src={v.qrPng} alt="WhatsApp linking QR code" width={232} height={232} className="qr sharpen-enter block size-[232px] [image-rendering:pixelated]" />
        </div>
        {v.until > 0 && <Countdown until={v.until} label="New code in" />}
      </div>
    ) : (
      <div className="grid justify-items-center gap-5">
        <Skeleton className="size-[256px] rounded-xl" />
        <p className="text-xs text-muted-foreground">Preparing a code…</p>
      </div>
    );
  } else if (v.screen === "code") {
    key = "code:" + v.code;
    body = (
      <div className="grid justify-items-center gap-5">
        <p className="text-xs font-medium text-muted-foreground">Your code</p>
        <CodeCells code={v.code} label="Your code" />
        {v.until > 0 && <Countdown until={v.until} label="Expires in" />}
      </div>
    );
  } else if (v.screen === "passkey") {
    key = "passkey:" + v.passkeyCode;
    body = (
      <div className="grid justify-items-center gap-5 text-center">
        <p className="text-xs font-medium text-muted-foreground">Check that your phone shows</p>
        <CodeCells code={v.passkeyCode} label="Your phone should show" />
        <Button type="button" onClick={() => void ctl.confirmPasskey()} disabled={v.passkeyBusy}>
          {v.passkeyBusy && <Loader2Icon className="animate-spin" aria-hidden="true" />}I confirmed on my phone
        </Button>
      </div>
    );
  } else {
    key = "idle";
    body = (
      <div className="grid justify-items-center gap-3 text-center">
        <div className="grid size-[120px] place-items-center rounded-2xl border border-dashed bg-background text-muted-foreground">
          <SmartphoneIcon className="size-7" strokeWidth={1.5} aria-hidden="true" />
        </div>
        <p className="text-xs text-muted-foreground">The code appears here.</p>
      </div>
    );
  }
  return (
    <div key={key} className="fade-enter grid min-h-[320px] w-full place-items-center">
      {body}
    </div>
  );
}

/**
 * LinkFlow: QR and phone-code linking. It starts a QR pairing when it mounts
 * and cancels an unfinished one when it unmounts or the page is left.
 */
export function LinkFlow({ onLinked }: { onLinked: (sessionId: string) => void }) {
  const { notify, clearError } = useNotice();
  const linkedRef = useRef(onLinked);
  linkedRef.current = onLinked;
  const hooks = { notify, clearError, successText: "Linked.", onSuccess: (id: string) => linkedRef.current(id) };
  const [ctl] = useState(() => new PairingController(hooks));
  ctl.setHooks(hooks);
  const v = useSyncExternalStore(ctl.subscribe, ctl.getSnapshot);

  useEffect(() => {
    ctl.init();
    return () => ctl.dispose();
  }, [ctl]);
  useBridgeEvents(
    (ev) => ctl.onEvent(ev),
    () => void ctl.resync(),
  );

  const warn = !v.done && v.message?.kind === "warn" ? v.message.text : "";
  const linked = v.done && !!v.linkedId;
  const steps = (mode: PairMode) =>
    mode === "code" && v.screen === "code-form" && !v.done ? <CodeForm v={v} ctl={ctl} /> : <Steps steps={mode === "code" ? CODE_STEPS : QR_STEPS} />;

  return (
    <div className="overflow-hidden rounded-xl border bg-card shadow-xs md:grid md:grid-cols-[minmax(0,1fr)_minmax(320px,400px)]">
      <div className="grid content-start gap-6 p-5 sm:p-8">
        <Tabs value={v.mode} onValueChange={(m) => ctl.switchTo(m as PairMode)} activationMode="manual" className="grid gap-6">
          <TabsList value={v.mode} aria-label="How to link" className="max-sm:w-full">
            <TabsTrigger value="qr" disabled={linked}>
              <QrCodeIcon aria-hidden="true" />
              QR code
            </TabsTrigger>
            <TabsTrigger value="code" disabled={linked}>
              <SmartphoneIcon aria-hidden="true" />
              Phone number
            </TabsTrigger>
          </TabsList>
          <div className="grid gap-1.5">
            <h2 className="text-lg font-semibold tracking-tight">{v.mode === "qr" ? "Scan the QR code" : "Link with a phone number"}</h2>
            <p className="text-sm text-muted-foreground">
              {v.mode === "qr" ? "Use the phone that has the WhatsApp account." : "No camera needed: WhatsApp shows a prompt for a code."}
            </p>
          </div>
          <TabsContent value="qr">{v.mode === "qr" && steps("qr")}</TabsContent>
          <TabsContent value="code">{v.mode === "code" && steps("code")}</TabsContent>
        </Tabs>
        <p className="text-[13px] leading-relaxed text-muted-foreground">
          Next, you tick the chats to share. Nothing is stored until you do. Tick them within an hour to also keep the history WhatsApp
          sends at linking.
        </p>
        {warn && (
          <Alert variant="warning" role="status" className="page-enter">
            <p>{warn}</p>
          </Alert>
        )}
        {v.cancelVisible && !v.done && (
          <div>
            <Button type="button" variant="ghost" size="sm" className="-ml-2.5 text-muted-foreground" onClick={() => void ctl.cancel()} disabled={v.cancelBusy}>
              <XIcon aria-hidden="true" />
              Cancel linking
            </Button>
          </div>
        )}
      </div>
      <div className={cn("relative grid place-items-center border-t bg-subtle p-6 sm:p-8 md:border-l md:border-t-0", linked && "bg-success/5")}>
        <Stage v={v} ctl={ctl} />
      </div>
      <p className="sr-only" aria-live="polite">
        {liveText(v)}
      </p>
    </div>
  );
}

function Retry({ v, ctl }: { v: PairView; ctl: PairingController }) {
  const wait = useSecondsLeft(v.retryAt);
  return (
    <Button type="button" variant="outline" onClick={() => ctl.retry()} disabled={wait > 0}>
      <RotateCcwIcon aria-hidden="true" />
      {wait > 0 ? `Retry in ${mmss(wait)}` : "Start again"}
    </Button>
  );
}

/** Help under the linking steps: what happens, and the two messages a phone can show. */
export function LinkHelp() {
  const [open, setOpen] = useState(false);
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="rounded-xl border bg-card">
      <CollapsibleTrigger className="flex w-full cursor-pointer items-center justify-between gap-4 rounded-xl px-5 py-4 text-left text-sm font-medium transition-colors duration-150 hover:bg-muted/40 sm:px-6">
        Help
        <ChevronDownIcon
          className={cn("size-4 text-muted-foreground transition-transform duration-200 ease-out-expo", open && "rotate-180")}
          aria-hidden="true"
        />
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="grid max-w-3xl gap-3 px-5 pb-5 text-sm leading-relaxed text-muted-foreground sm:px-6">
          <p>
            The bridge appears on the phone as a linked device called “{PRODUCT}”, next to WhatsApp Web. Every chat starts unticked, and
            the bridge stores nothing from a chat until you tick it.
          </p>
          <p>
            <strong className="font-medium text-foreground">History.</strong> Right after linking, WhatsApp sends the recent history of
            your chats, once. The bridge holds it in memory, never on disk, for an hour. Tick chats in that hour and their history is
            stored with them. The rest is dropped after the hour, or sooner if the bridge restarts.
          </p>
          <p>Unlink it any time under Linked devices. WhatsApp also unlinks it if the phone stays offline for 14 days.</p>
          <p>
            <strong className="font-medium text-foreground">Passkey prompt.</strong> {PASSKEY_MSG}
          </p>
          <p>
            <strong className="font-medium text-foreground">Multi-device message.</strong> {NO_MD_MSG}
          </p>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}
