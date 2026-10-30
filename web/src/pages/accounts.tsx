import { ChevronRightIcon, PlusIcon } from "lucide-react";
import { useEffect, useState } from "react";
import { Avatar } from "@/components/avatar";
import { LoadingRows, RetryLine } from "@/components/feedback";
import { SessionStatus } from "@/components/session-actions";
import { Page, PageHeader } from "@/components/shell";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { ago, fmtPhone, sessionTitle } from "@/lib/format";
import { stagger, useFlash } from "@/lib/motion";
import { Link, useTitle } from "@/lib/router";
import { useSessions } from "@/lib/sessions";
import type { Chat, SessionInfo } from "@/lib/types";
import { cn, enc } from "@/lib/utils";

/** Ticked chats per account; fetched again when an account's chat count changes. */
function useTickedCounts(list: SessionInfo[] | null) {
  const [counts, setCounts] = useState<Record<string, number>>({});
  const key = (list ?? [])
    .filter((s) => s.chat_count > 0)
    .map((s) => `${s.id}:${s.chat_count}`)
    .join(",");
  useEffect(() => {
    let live = true;
    for (const id of key ? key.split(",").map((k) => k.split(":")[0]) : []) {
      api
        .get<Chat[]>(`/sessions/${enc(id)}/chats`)
        .then((chats) => live && setCounts((c) => ({ ...c, [id]: chats.filter((x) => x.ticked).length })))
        .catch(() => {});
    }
    return () => {
      live = false;
    };
  }, [key]);
  return counts;
}

const LinkButton = ({ className }: { className?: string }) => (
  <Button asChild className={className}>
    <Link href="/link">
      <PlusIcon aria-hidden="true" />
      Link a WhatsApp account
    </Link>
  </Button>
);

/** First run: the three steps, the first one actionable. */
function GetStarted() {
  const steps = [
    { title: "Link an account.", text: "Scan a QR code with the phone, as you would for WhatsApp Web.", action: <LinkButton className="mt-3" /> },
    {
      title: "Tick the chats to share.",
      text: "Chats start unticked, and nothing is stored until you tick one. Tick within an hour of linking to keep their recent history.",
    },
    { title: "Connect your AI.", text: "Add the bridge to Claude, ChatGPT or Codex." },
  ];
  return (
    <section className="overflow-hidden rounded-xl border bg-card shadow-xs" aria-labelledby="empty-h">
      <div className="grid gap-1 border-b bg-subtle px-5 py-5 sm:px-8 sm:py-6">
        <h2 id="empty-h" className="text-base font-semibold tracking-tight">
          No WhatsApp account linked yet
        </h2>
        <p className="text-sm text-muted-foreground">Three steps, a couple of minutes.</p>
      </div>
      <ol className="px-5 py-6 sm:px-8 sm:py-8">
        {steps.map((s, i) => (
          <li key={s.title} className="row-enter relative flex gap-4 pb-8 last:pb-0" style={stagger(i)}>
            {i < steps.length - 1 && <span className="absolute bottom-0 left-[13px] top-8 w-px bg-border" aria-hidden="true" />}
            <span
              aria-hidden="true"
              className={cn(
                "relative grid size-[27px] shrink-0 place-items-center rounded-full font-mono text-xs font-medium",
                i === 0 ? "bg-primary text-primary-foreground" : "border bg-background text-muted-foreground",
              )}
            >
              {i + 1}
            </span>
            <div className="grid gap-0.5 pt-0.5">
              <p className={cn("font-medium", i > 0 && "text-foreground/80")}>{s.title}</p>
              <p className="text-sm text-muted-foreground">{s.text}</p>
              {s.action && <div>{s.action}</div>}
            </div>
          </li>
        ))}
      </ol>
    </section>
  );
}

function AccountRow({ s, i, ticked }: { s: SessionInfo; i: number; ticked?: number }) {
  const flash = useFlash<HTMLTableRowElement>(`${s.state}:${s.message_count}`);
  const title = sessionTitle(s);
  return (
    <TableRow ref={flash} className="row-enter group relative" style={stagger(i)}>
      <TableCell data-primary="">
        <div className="flex min-w-0 items-center gap-3">
          <Avatar name={title} />
          <div className="grid min-w-0">
            <Link
              href={`/sessions/${enc(s.id)}`}
              className="truncate font-medium after:absolute after:inset-0 after:content-[''] focus-visible:outline-none focus-visible:after:rounded-md focus-visible:after:outline-2 focus-visible:after:-outline-offset-2 focus-visible:after:outline-ring"
            >
              {title}
            </Link>
            <span className="truncate font-mono text-xs text-muted-foreground">{fmtPhone(s.phone) || "No number yet"}</span>
          </div>
        </div>
      </TableCell>
      <TableCell data-label="Status" data-bare="">
        <SessionStatus s={s} />
      </TableCell>
      <TableCell data-label="Shared chats" className="sm:text-right">
        {ticked !== undefined ? (
          <>
            <span className="font-medium text-foreground">{ticked.toLocaleString()}</span>
            <span className="text-muted-foreground"> of {s.chat_count.toLocaleString()}</span>
          </>
        ) : (
          s.chat_count.toLocaleString()
        )}
      </TableCell>
      <TableCell data-label="Messages" className="sm:text-right">
        {s.message_count.toLocaleString()}
      </TableCell>
      <TableCell data-label="Last message" className="text-muted-foreground">
        {ago(s.last_message_at) || "None yet"}
      </TableCell>
      <TableCell data-actions="" className="w-10 pr-3">
        <ChevronRightIcon
          className="size-4 text-muted-foreground/60 transition-transform duration-150 group-hover:translate-x-0.5 group-hover:text-foreground"
          aria-hidden="true"
        />
      </TableCell>
    </TableRow>
  );
}

export function AccountsPage() {
  useTitle("Accounts");
  const { sendEnabled } = useAuth();
  const { list, error, reload } = useSessions();
  const ticked = useTickedCounts(list);

  return (
    <Page>
      <PageHeader
        title="Accounts"
        lead={
          sendEnabled
            ? "Each account is one WhatsApp number, linked to the bridge as a device. AI assistants can send messages to its ticked chats."
            : "Each account is one WhatsApp number, linked to the bridge as a read-only device."
        }
        actions={list && list.length > 0 ? <LinkButton /> : undefined}
      />
      {error && <RetryLine text={error} onRetry={() => void reload()} />}
      {list === null ? (
        !error && <LoadingRows label="Loading accounts…" rows={2} />
      ) : list.length === 0 ? (
        <GetStarted />
      ) : (
        <Table>
          <caption className="sr-only">Linked WhatsApp accounts</caption>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead scope="col">Account</TableHead>
              <TableHead scope="col">Status</TableHead>
              <TableHead scope="col" className="text-right">
                Shared chats
              </TableHead>
              <TableHead scope="col" className="text-right">
                Messages
              </TableHead>
              <TableHead scope="col">Last message</TableHead>
              <TableHead scope="col" className="w-10">
                <span className="sr-only">Open</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {list.map((s, i) => (
              <AccountRow key={s.id} s={s} i={i} ticked={ticked[s.id]} />
            ))}
          </TableBody>
        </Table>
      )}
    </Page>
  );
}
