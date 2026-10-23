import { EyeIcon, MessageSquareTextIcon, SearchIcon, SearchXIcon } from "lucide-react";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Avatar } from "@/components/avatar";
import { useConfirm } from "@/components/confirm";
import { Empty, LoadingRows } from "@/components/feedback";
import { SessionActions, SessionStatus } from "@/components/session-actions";
import { Page, PageHeader, Section } from "@/components/shell";
import { Status } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Segmented, SegmentedItem } from "@/components/ui/toggle-group";
import { ApiError, api, errorText } from "@/lib/api";
import { useLater } from "@/lib/debounce";
import { useBridgeEvents } from "@/lib/events";
import { ago, describeEvent, fmtDate, fmtPhone, fmtShort, fmtTime, platformName, plural, sessionTitle, stateNote } from "@/lib/format";
import { stagger, useFlash } from "@/lib/motion";
import { useNotice } from "@/lib/notice";
import { navigate, useTitle } from "@/lib/router";
import type { BridgeEvent, Chat, LogEntry, Message, SessionInfo } from "@/lib/types";
import { cn, enc } from "@/lib/utils";

interface LogRow {
  key: string;
  time: string;
  type: string;
  detail: string;
}

type Filter = "all" | "shared" | "not-shared";
const PAGE = 200;
const LOG_ROWS = 50;
const MSG_ROWS = 100;
let rowSeq = 0;

const byRecent = (a: Chat, b: Chat) => (Date.parse(b.last_message_at ?? "") || 0) - (Date.parse(a.last_message_at ?? "") || 0);
const chatName = (c: Chat) => c.name || c.jid.split("@")[0];
const fold = (s: string) => s.normalize("NFD").replace(/\p{Diacritic}/gu, "").toLowerCase();

function useWide() {
  const q = "(min-width: 640px)";
  const [wide, setWide] = useState(() => window.matchMedia(q).matches);
  useEffect(() => {
    const m = window.matchMedia(q);
    const on = () => setWide(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);
  return wide;
}

/* ---------------------------------------------------------------- details */

function Details({ session }: { session: SessionInfo }) {
  const rows: [string, ReactNode, boolean?][] = [
    ["Phone", fmtPhone(session.phone) || "–", true],
    ["WhatsApp name", session.push_name || session.business_name || "–"],
    ["Phone type", platformName(session.platform) || "–"],
    ["Linked", fmtDate(session.created_at) || "–"],
    ["Last message", ago(session.last_message_at) || "None yet"],
    ["Stored messages", session.message_count.toLocaleString()],
  ];
  return (
    <div className="overflow-hidden rounded-xl border bg-card shadow-xs">
      <dl className="grid grid-cols-2 gap-px bg-border sm:grid-cols-3 lg:grid-cols-6">
        {rows.map(([k, v, mono], i) => (
          <div key={k} className="row-enter grid content-start gap-1 bg-card px-4 py-3.5 sm:px-5 sm:py-4" style={stagger(i)}>
            <dt className="text-xs text-muted-foreground">{k}</dt>
            <dd className={cn("truncate text-sm font-medium", mono && "font-mono text-[13px]")}>{v}</dd>
          </div>
        ))}
      </dl>
      {session.last_error && (
        <div className="grid gap-1 border-t bg-destructive/5 px-4 py-3 sm:px-5">
          <p className="text-xs text-muted-foreground">Last error</p>
          <p className="text-sm text-destructive">{session.last_error}</p>
        </div>
      )}
    </div>
  );
}

function DetailsSkeleton() {
  return (
    <div role="status" className="grid grid-cols-2 gap-px overflow-hidden rounded-xl border bg-border sm:grid-cols-3 lg:grid-cols-6">
      <span className="sr-only">Loading…</span>
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className="grid gap-2 bg-card px-5 py-4">
          <Skeleton className="h-2.5 w-16 opacity-70" />
          <Skeleton className="h-3.5 w-24" />
        </div>
      ))}
    </div>
  );
}

/* ------------------------------------------------------------------ chats */

function ChatRow({
  c,
  i,
  wide,
  saving,
  onTick,
  onView,
}: {
  c: Chat;
  i: number;
  wide: boolean;
  saving: boolean;
  onTick: (on: boolean) => void;
  onView: () => void;
}) {
  const flash = useFlash<HTMLTableRowElement>(c.message_count);
  const cid = `chat-${c.jid}`;
  const name = chatName(c);
  const shared = c.ticked ? (
    <Status tone="ok" className="font-normal">
      Shared
    </Status>
  ) : (
    <Status tone="off" className="font-normal">
      Not shared
    </Status>
  );
  const count = c.ticked ? c.message_count.toLocaleString() : "–";
  const last = fmtTime(c.last_message_at) || "–";
  return (
    <TableRow ref={flash} data-off={!c.ticked || undefined} className="row-enter" style={stagger(i)}>
      <TableCell data-primary="" className="py-0">
        <div className="flex min-w-0 items-center gap-3">
          <Checkbox id={cid} checked={c.ticked} disabled={saving} onCheckedChange={(v) => onTick(v === true)} />
          <label htmlFor={cid} className="flex min-w-0 flex-1 cursor-pointer items-center gap-3 py-2">
            <Avatar
              name={name}
              group={c.is_group}
              className={cn("transition-colors duration-200", !c.ticked && "border border-dashed border-border-strong bg-transparent ring-0")}
            />
            <span className="grid min-w-0 flex-1">
              <span className="flex min-w-0 items-baseline gap-2">
                <span className={cn("truncate font-medium transition-colors duration-200", !c.ticked && "text-muted-foreground")}>{name}</span>
                {!wide && <span className="ml-auto shrink-0 text-xs text-muted-foreground">{fmtShort(c.last_message_at) || "–"}</span>}
              </span>
              {wide ? (
                <span className="text-xs text-muted-foreground">{c.is_group ? "Group" : "Direct"}</span>
              ) : (
                <span className="flex min-w-0 items-center gap-x-1.5 text-xs text-muted-foreground [&_[data-tone]]:text-xs">
                  {shared}
                  {c.ticked && (
                    <>
                      <span aria-hidden="true">·</span>
                      <span className="truncate">{plural(c.message_count, "message")}</span>
                    </>
                  )}
                </span>
              )}
            </span>
          </label>
        </div>
      </TableCell>
      {wide && (
        <>
          <TableCell>{shared}</TableCell>
          <TableCell className="text-right">{count}</TableCell>
          <TableCell className="text-muted-foreground">{last}</TableCell>
        </>
      )}
      <TableCell data-actions="" className="w-px whitespace-nowrap pr-3 text-right">
        {!wide && !(c.ticked && c.message_count > 0) && <span className="inline-block size-10 align-middle" aria-hidden="true" />}
        {c.ticked && c.message_count > 0 && (
          <Button type="button" variant="ghost" size={wide ? "sm" : "icon"} onClick={onView} className="text-muted-foreground hover:text-foreground">
            <EyeIcon aria-hidden="true" />
            <span className={wide ? undefined : "sr-only"}>View</span>
            <span className="sr-only"> messages in {name}</span>
          </Button>
        )}
      </TableCell>
    </TableRow>
  );
}

/* --------------------------------------------------------------- messages */

const KIND_LABEL: Record<string, string> = {
  image: "Photo",
  video: "Video",
  audio: "Voice message or audio",
  document: "Document",
  sticker: "Sticker",
  location: "Location",
  contact: "Contact",
  poll: "Poll",
  reaction: "Reaction",
};

/** A phone number for a phone JID; other ids (such as @lid) as they are. */
const senderNumber = (jid: string) => (jid.endsWith("@s.whatsapp.net") ? fmtPhone(jid.split("@")[0]) : jid.split("@")[0]);

function dayLabel(t: string) {
  const d = new Date(t);
  const today = new Date();
  const yesterday = new Date(Date.now() - 86400000);
  if (d.toDateString() === today.toDateString()) return "Today";
  if (d.toDateString() === yesterday.toDateString()) return "Yesterday";
  return d.toLocaleDateString([], { weekday: "long", day: "numeric", month: "long" });
}

/** The stored messages of one chat, oldest first, as the bridge (and an AI assistant) sees them. */
function MessagesSheet({
  chat,
  jid,
  msgs,
  owner,
  onClose,
}: {
  chat?: Chat;
  jid: string | null;
  msgs: Message[] | null;
  owner: string;
  onClose: () => void;
}) {
  const wide = useWide();
  const box = useRef<HTMLOListElement>(null);
  const shownFor = useRef<string | null>(null);
  // While the sheet slides away, it keeps showing the chat it had.
  const kept = useRef<{ chat?: Chat; jid: string }>({ jid: "" });
  if (jid) kept.current = { chat, jid };
  const name = kept.current.chat ? chatName(kept.current.chat) : kept.current.jid.split("@")[0];

  useEffect(() => {
    const el = box.current;
    if (!el || !msgs) return;
    const first = shownFor.current !== jid;
    shownFor.current = jid;
    const near = el.scrollHeight - el.scrollTop - el.clientHeight < 120;
    if (first || near) el.scrollTo({ top: el.scrollHeight, behavior: first ? "auto" : "smooth" });
  }, [msgs, jid]);

  let body: ReactNode;
  if (chat && !chat.ticked) body = <Empty className="m-5">This chat is not shared, so the bridge stores none of its messages.</Empty>;
  else if (msgs === null)
    body = (
      <div role="status" className="grid gap-5 px-5 py-6">
        <span className="sr-only">Loading messages…</span>
        {[56, 72, 40, 64].map((w, i) => (
          <div key={i} className="flex gap-3">
            <Skeleton className="size-6 rounded-full" />
            <div className="grid flex-1 gap-2">
              <Skeleton className="h-2.5 w-24 opacity-70" />
              <Skeleton className="h-3" style={{ width: `${w}%` }} />
            </div>
          </div>
        ))}
      </div>
    );
  else if (msgs.length === 0)
    body = (
      <Empty className="m-5" icon={<MessageSquareTextIcon />}>
        No stored messages in this chat yet. New ones appear here as they arrive.
      </Empty>
    );
  else {
    let lastDay = "";
    body = (
      <ol ref={box} className="min-h-0 flex-1 overflow-y-auto overscroll-contain px-5 pb-6 pt-2" tabIndex={0} aria-label={`Messages in ${name}`}>
        {msgs.map((m, i) => {
          const day = dayLabel(m.timestamp);
          const showDay = day !== lastDay;
          lastDay = day;
          const prev = msgs[i - 1];
          const sender = m.from_me ? "You" : m.sender_name || senderNumber(m.sender_jid);
          const grouped =
            !showDay && prev && prev.sender_jid === m.sender_jid && Date.parse(m.timestamp) - Date.parse(prev.timestamp) < 5 * 60000;
          return (
            <li key={m.id} className="row-enter" style={stagger(Math.max(0, msgs.length - 1 - i))}>
              {showDay && (
                <p className="sticky top-0 z-10 -mx-5 flex items-center gap-3 bg-background/95 px-5 py-3 text-xs font-medium text-muted-foreground">
                  <span className="h-px flex-1 bg-border" aria-hidden="true" />
                  {day}
                  <span className="h-px flex-1 bg-border" aria-hidden="true" />
                </p>
              )}
              <div className={cn("flex gap-3", grouped ? "pt-1" : "pt-3")}>
                {grouped ? (
                  <span className="w-6 shrink-0" aria-hidden="true" />
                ) : (
                  <Avatar name={m.from_me ? owner : sender} size="sm" className={cn(m.from_me && "bg-primary text-primary-foreground ring-0")} />
                )}
                <div className="min-w-0 flex-1">
                  <p className={cn("flex flex-wrap items-baseline gap-x-2 gap-y-0.5", grouped && "sr-only")}>
                    <span className="text-[13px] font-medium">{sender}</span>
                    <time dateTime={m.timestamp} className="text-xs text-muted-foreground">
                      {fmtTime(m.timestamp)}
                    </time>
                  </p>
                  {m.kind !== "text" && (
                    <Badge variant="outline" className="mt-1 rounded-md">
                      {KIND_LABEL[m.kind] ?? m.kind}
                    </Badge>
                  )}
                  {m.text && <p className="mt-0.5 whitespace-pre-wrap text-sm leading-relaxed [overflow-wrap:anywhere]">{m.text}</p>}
                </div>
              </div>
            </li>
          );
        })}
      </ol>
    );
  }

  return (
    <Sheet open={!!jid} onOpenChange={(o) => !o && onClose()}>
      <SheetContent side={wide ? "right" : "bottom"} className={cn(wide ? "w-[min(94vw,34rem)]" : "h-[88dvh]")}>
        <SheetHeader>
          <SheetTitle>Messages in {name}</SheetTitle>
          <SheetDescription>
            {msgs && msgs.length > 0
              ? `The latest ${plural(Math.min(msgs.length, MSG_ROWS), "stored message")}, oldest first. This is what AI assistants can read.`
              : "What the bridge stored for this chat."}
          </SheetDescription>
        </SheetHeader>
        {body}
      </SheetContent>
    </Sheet>
  );
}

/* --------------------------------------------------------------- activity */

function eventTone(type: string) {
  if (type === "state.connected" || type === "pair.success" || type === "chats.sync") return "ok";
  if (type.startsWith("mcp.") || type.startsWith("oauth.")) return "live";
  if (type === "state.banned" || type === "pair.error" || type === "auth.login_failed") return "bad";
  if (type.startsWith("state.") || type.startsWith("pair.")) return "warn";
  return "off";
}

function Activity({ logs }: { logs: LogRow[] }) {
  return (
    <ol className="divide-y overflow-hidden rounded-xl border bg-card shadow-xs">
      {logs.map((l, i) => {
        const e = describeEvent(l.type, l.detail);
        const tone = eventTone(l.type);
        return (
          <li
            key={l.key}
            className="row-enter grid grid-cols-[auto_minmax(0,1fr)] items-baseline gap-x-3 px-4 py-2.5 sm:grid-cols-[5.5rem_auto_minmax(0,1fr)] sm:gap-x-4 sm:px-5"
            style={stagger(i)}
          >
            <time dateTime={l.time} className="col-span-2 font-mono text-xs text-muted-foreground sm:col-span-1">
              {fmtTime(l.time)}
            </time>
            <span
              aria-hidden="true"
              className={cn(
                "size-1.5 translate-y-[-2px] rounded-full",
                tone === "ok" && "bg-success",
                tone === "warn" && "bg-warning",
                tone === "bad" && "bg-destructive",
                tone === "live" && "bg-live",
                tone === "off" && "bg-border-strong",
              )}
            />
            <p className="min-w-0 text-sm">
              <span className="font-medium" title={l.type}>
                {e.what}
              </span>
              {e.detail && <span className="text-muted-foreground [overflow-wrap:anywhere]"> · {e.detail}</span>}
            </p>
          </li>
        );
      })}
    </ol>
  );
}

/* ------------------------------------------------------------------- page */

export function AccountPage({ id }: { id: string }) {
  const wide = useWide();
  const { notify, clearError } = useNotice();
  const confirm = useConfirm();
  const later = useLater();
  const [session, setSession] = useState<SessionInfo | null>(null);
  const [gone, setGone] = useState(false);
  const [chats, setChats] = useState<Chat[] | null>(null);
  const [logs, setLogs] = useState<LogRow[] | null>(null);
  const [query, setQuery] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [limit, setLimit] = useState(PAGE);
  const [saving, setSaving] = useState<Set<string>>(new Set());
  const [open, setOpen] = useState<string | null>(null);
  const [msgs, setMsgs] = useState<Message[] | null>(null);
  const openRef = useRef<string | null>(null);
  openRef.current = open;

  useTitle(session ? sessionTitle(session) : "Account");
  const base = `/sessions/${enc(id)}`;

  const get = useCallback(
    async <T,>(path: string, render: (v: T) => void) => {
      try {
        render(await api.get<T>(base + path));
      } catch (err) {
        if (!(err instanceof ApiError && err.status === 404)) notify(errorText(err));
      }
    },
    [base, notify],
  );

  const loadSession = useCallback(async () => {
    try {
      setSession(await api.get<SessionInfo>(base));
      clearError();
      setGone(false);
    } catch (err) {
      if (!(err instanceof ApiError && err.status === 404)) return notify(errorText(err));
      setSession(null);
      setGone(true);
    }
  }, [base, clearError, notify]);

  const loadChats = useCallback(() => get<Chat[]>("/chats", (c) => setChats([...c].sort(byRecent))), [get]);
  const loadLogs = useCallback(
    () => get<LogEntry[]>(`/logs?limit=${LOG_ROWS}`, (l) => setLogs(l.map((x) => ({ key: "s" + x.seq, time: x.time, type: x.type, detail: x.detail })))),
    [get],
  );
  const loadMessages = useCallback(() => {
    const jid = openRef.current;
    if (!jid) return;
    return get<Message[]>(`/chats/${enc(jid)}/messages?limit=${MSG_ROWS}`, (m) => {
      if (openRef.current === jid) setMsgs(m);
    });
  }, [get]);
  const load = useCallback(() => {
    void loadSession();
    void loadChats();
    void loadLogs();
    void loadMessages();
  }, [loadSession, loadChats, loadLogs, loadMessages]);

  const showMessages = (jid: string) => {
    openRef.current = jid;
    setOpen(jid);
    setMsgs(null);
    void loadMessages();
  };
  const closeMessages = () => {
    openRef.current = null;
    setOpen(null);
  };

  useEffect(() => {
    load();
  }, [load]);

  const prependLog = (ev: BridgeEvent, type: string, detail?: string) =>
    setLogs((l) => [{ key: "e" + ++rowSeq, time: ev.time ?? new Date().toISOString(), type, detail: detail ?? "" }, ...(l ?? [])].slice(0, LOG_ROWS));

  useBridgeEvents((ev) => {
    if (ev.session_id !== id) return;
    switch (ev.type) {
      case "state":
        prependLog(ev, "state." + ev.state, ev.text);
        later("state", () => void loadSession(), 50);
        break;
      case "pair":
        if (ev.pair) prependLog(ev, "pair." + ev.pair.type, ev.pair.error);
        break;
      case "log":
        prependLog(ev, "log", ev.text);
        break;
      case "message":
        if (ev.message && ev.message.chat_jid === openRef.current) later("msgs", () => void loadMessages(), 500);
        later("chats", () => void loadChats(), 1000);
        later("session", () => void loadSession(), 1000);
        break;
      case "chats":
        later("chats", () => void loadChats(), 1000);
        later("session", () => void loadSession(), 1000);
        break;
    }
  }, load);

  /** Ticks or unticks chats; unticking chats with stored messages asks first. */
  const setTicked = async (list: Chat[], on: boolean) => {
    const changes = list.filter((c) => c.ticked !== on);
    if (changes.length === 0) return;
    const losing = on ? [] : changes.filter((c) => c.message_count > 0);
    if (losing.length > 0) {
      const n = losing.reduce((sum, c) => sum + c.message_count, 0);
      const ok = await confirm({
        title: changes.length === 1 ? `Stop sharing ${chatName(changes[0])}?` : `Stop sharing ${plural(changes.length, "chat")}?`,
        description: (
          <>
            <p>
              This deletes <strong className="font-medium text-foreground">{plural(n, "stored message")}</strong> from the bridge and stops
              storing new ones. AI assistants can no longer read them.
            </p>
            <p>Ticking a chat again stores new messages from then on. Deleted messages do not come back.</p>
          </>
        ),
        confirmLabel: "Untick and delete",
        destructive: true,
      });
      if (!ok) return;
    }
    setSaving((s) => new Set([...s, ...changes.map((c) => c.jid)]));
    const failed: string[] = [];
    for (const c of changes) {
      try {
        const updated = await api.patch<Chat>(`${base}/chats/${enc(c.jid)}`, { ticked: on });
        setChats((l) => (l ?? []).map((x) => (x.jid === c.jid ? { ...x, ...updated } : x)));
      } catch (err) {
        failed.push(`${chatName(c)}: ${errorText(err)}`);
      }
      setSaving((s) => {
        const next = new Set(s);
        next.delete(c.jid);
        return next;
      });
    }
    if (failed.length) notify("Some chats were not updated. " + failed.join("; "));
    if (openRef.current && changes.some((c) => c.jid === openRef.current)) void loadMessages();
    void loadSession();
  };

  if (gone) {
    return (
      <Page>
        <PageHeader back={{ href: "/", label: "Accounts" }} title="Account not found" lead="This account is no longer on the bridge." />
      </Page>
    );
  }

  const all = chats ?? [];
  const sharedCount = all.filter((c) => c.ticked).length;
  const byFilter = all.filter((c) => filter === "all" || (filter === "shared") === c.ticked);
  const shown = byFilter.filter((c) => !query || fold(chatName(c)).includes(fold(query)));
  const visible = shown.slice(0, limit);

  return (
    <Page>
      <PageHeader
        back={{ href: "/", label: "Accounts" }}
        title={session ? sessionTitle(session) : "Account"}
        badge={session ? <SessionStatus s={session} className="mt-1 rounded-full border bg-card px-2.5 py-0.5 shadow-xs" /> : undefined}
        lead={session ? stateNote(session) : undefined}
        actions={session ? <SessionActions s={session} changed={() => void loadSession()} removed={() => navigate("/")} /> : undefined}
      />

      {session ? <Details session={session} /> : <DetailsSkeleton />}

      <Section
        title="Chats"
        lead={
          <>
            <strong className="font-medium text-foreground">Only ticked chats are shared.</strong> The bridge stores their messages and AI
            assistants can read them. Unticked chats are not stored and no assistant can see them.
          </>
        }
        aside={
          chats && chats.length > 0 ? (
            <p className="text-sm text-muted-foreground" aria-live="polite">
              <span className="font-medium text-foreground tabular-nums">{sharedCount}</span> of {plural(chats.length, "chat")} shared
            </p>
          ) : undefined
        }
      >
        {chats === null ? (
          <LoadingRows label="Loading chats…" rows={5} />
        ) : chats.length === 0 ? (
          <Empty icon={<MessageSquareTextIcon />} title="No chats yet">
            WhatsApp sends the chat list a minute or two after linking.
          </Empty>
        ) : (
          <div className="grid gap-3">
            <div className="flex flex-col gap-2 lg:flex-row lg:items-center">
              <div className="relative lg:w-72">
                <Label htmlFor="chat-search" className="sr-only">
                  Search chats
                </Label>
                <SearchIcon className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" aria-hidden="true" />
                <Input
                  id="chat-search"
                  type="search"
                  placeholder="Search chats"
                  className="pl-9"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                />
              </div>
              <Segmented value={filter} onValueChange={(v) => setFilter(v as Filter)} aria-label="Show" className="max-sm:w-full max-sm:[&>button]:flex-1 max-sm:[&>button]:justify-center">
                <SegmentedItem value="all">
                  All <span className="tabular-nums text-muted-foreground">{all.length}</span>
                </SegmentedItem>
                <SegmentedItem value="shared">
                  Shared <span className="tabular-nums text-muted-foreground">{sharedCount}</span>
                </SegmentedItem>
                <SegmentedItem value="not-shared">
                  Not shared <span className="tabular-nums text-muted-foreground">{all.length - sharedCount}</span>
                </SegmentedItem>
              </Segmented>
              <div className="flex gap-2 lg:ml-auto">
                <Button type="button" variant="outline" size="sm" onClick={() => void setTicked(shown, true)} disabled={shown.every((c) => c.ticked)}>
                  Tick all shown
                </Button>
                <Button type="button" variant="outline" size="sm" onClick={() => void setTicked(shown, false)} disabled={shown.every((c) => !c.ticked)}>
                  Untick all shown
                </Button>
              </div>
            </div>
            {shown.length === 0 ? (
              <Empty icon={<SearchXIcon />} title="No chat matches">
                {query ? <>Nothing called “{query}” here. Check the spelling or show all chats.</> : "Nothing in this view."}
              </Empty>
            ) : (
              <Table>
                <caption className="sr-only">Chats of this account. Tick a chat to share it with AI assistants.</caption>
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead scope="col" className="pl-4">
                      Chat
                    </TableHead>
                    {wide && (
                      <>
                        <TableHead scope="col">Shared</TableHead>
                        <TableHead scope="col" className="text-right">
                          Messages
                        </TableHead>
                        <TableHead scope="col">Last message</TableHead>
                      </>
                    )}
                    <TableHead scope="col">
                      <span className="sr-only">Messages</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {visible.map((c, i) => (
                    <ChatRow
                      key={c.jid}
                      c={c}
                      i={i}
                      wide={wide}
                      saving={saving.has(c.jid)}
                      onTick={(on) => void setTicked([c], on)}
                      onView={() => (open === c.jid ? closeMessages() : showMessages(c.jid))}
                    />
                  ))}
                </TableBody>
              </Table>
            )}
            {shown.length > visible.length && (
              <div>
                <Button type="button" variant="outline" onClick={() => setLimit(shown.length)}>
                  Show all {shown.length.toLocaleString()} chats
                </Button>
              </div>
            )}
          </div>
        )}
      </Section>

      <MessagesSheet chat={all.find((c) => c.jid === open)} jid={open} msgs={msgs} owner={session ? sessionTitle(session) : "You"} onClose={closeMessages} />

      <Section title="Recent activity" lead="Linking, connection and AI assistant events for this account. Never message text.">
        {logs === null ? (
          <LoadingRows label="Loading activity…" rows={3} avatar={false} />
        ) : logs.length === 0 ? (
          <Empty title="No activity yet" />
        ) : (
          <Activity logs={logs} />
        )}
      </Section>
    </Page>
  );
}
