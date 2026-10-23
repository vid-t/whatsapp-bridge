import { CheckIcon, InboxIcon, Loader2Icon, UserPlusIcon, UsersIcon } from "lucide-react";
import { useState, type FormEvent } from "react";
import { Avatar } from "@/components/avatar";
import { useConfirm } from "@/components/confirm";
import { Empty, FieldError, LoadingRows, RetryLine } from "@/components/feedback";
import { Page, PageHeader, Section } from "@/components/shell";
import { Status } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ApiError, api, errorText } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { ago, fmtDate } from "@/lib/format";
import { stagger } from "@/lib/motion";
import { useNotice } from "@/lib/notice";
import { useOptionalList } from "@/lib/optional";
import { useTitle } from "@/lib/router";
import type { Member } from "@/lib/types";
import { enc } from "@/lib/utils";

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const PROVIDER: Record<string, string> = { email: "Email link", google: "Google", microsoft: "Microsoft" };

/** Who let a member in, from approved_by. */
function admittedBy(by?: string | null) {
  if (!by) return "Added";
  if (by.includes("@")) return `Approved by ${by}`;
  return (
    ({
      session: "Approved with the access code",
      open: "Added in open mode",
      admin: "Added by the admin command",
      domain: "Allowed by email domain",
      approvers: "Approver",
    }) as Record<string, string>
  )[by] ?? "Added";
}

function Person({ m, you }: { m: Member; you?: boolean }) {
  return (
    <div className="flex min-w-0 items-center gap-3">
      <Avatar name={m.name || m.email} />
      <div className="grid min-w-0">
        <span className="flex min-w-0 items-center gap-2">
          <span className="truncate font-medium">{m.name || m.email}</span>
          {you && <Badge variant="outline">You</Badge>}
        </span>
        {m.name && <span className="truncate text-xs text-muted-foreground">{m.email}</span>}
      </div>
    </div>
  );
}

export function MembersPage() {
  useTitle("Members");
  const auth = useAuth();
  const confirm = useConfirm();
  const { notify } = useNotice();
  const members = useOptionalList<Member>("/members", auth.loginRequired);
  const [busy, setBusy] = useState<Set<string>>(new Set());

  const pending = members.list
    .filter((m) => m.status === "pending")
    .sort((a, b) => Date.parse(b.requested_at ?? b.created_at ?? "") - Date.parse(a.requested_at ?? a.created_at ?? ""));
  const approved = members.list.filter((m) => m.status !== "pending").sort((a, b) => a.email.localeCompare(b.email));

  const withBusy = async (id: string, fn: () => Promise<void>) => {
    setBusy((b) => new Set(b).add(id));
    try {
      await fn();
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) void members.reload();
      else notify(errorText(err));
    }
    setBusy((b) => {
      const n = new Set(b);
      n.delete(id);
      return n;
    });
  };

  const approve = (m: Member) =>
    withBusy(m.id, async () => {
      const updated = await api.post<Member>(`/members/${enc(m.id)}/approve`);
      members.set((l) => l.map((x) => (x.id === m.id ? { ...x, ...(updated ?? {}), status: "approved" } : x)));
    });

  const remove = async (m: Member) => {
    const isPending = m.status === "pending";
    const ok = await confirm({
      title: isPending ? `Turn down ${m.email}?` : `Remove ${m.email}?`,
      description: isPending ? (
        <p>The request is deleted. They can ask again from the sign-in page.</p>
      ) : (
        <p>They are signed out everywhere and can no longer sign in. AI assistants they connected keep working until you revoke them under Connect AI.</p>
      ),
      confirmLabel: isPending ? "Turn down" : "Remove",
      destructive: true,
    });
    if (!ok) return;
    await withBusy(m.id, async () => {
      await api.del(`/members/${enc(m.id)}`);
      members.set((l) => l.filter((x) => x.id !== m.id));
    });
  };

  return (
    <Page>
      <PageHeader
        title="Members"
        lead="People with access sign in with their email, Google or Microsoft. New people ask first, and any member can approve them."
      />

      {!auth.loginRequired ? (
        <Empty icon={<UsersIcon />} title="Open mode">
          This bridge asks nobody to sign in, so it has no members. Set an access code to turn sign-in on.
        </Empty>
      ) : members.status === "loading" ? (
        <LoadingRows label="Loading members…" rows={3} />
      ) : members.status === "error" ? (
        <RetryLine text={"Could not load the members: " + members.error} onRetry={() => void members.reload()} />
      ) : members.status === "off" ? (
        <Empty icon={<UsersIcon />} title="No member sign-in">
          Everyone signs in with the access code.
        </Empty>
      ) : (
        <>
          <Section
            title="Waiting for approval"
            lead="They asked to sign in. Approve the people you know; they get a sign-in link by email."
            aside={pending.length > 0 ? <Badge variant="warning">{pending.length} waiting</Badge> : undefined}
          >
            {pending.length === 0 ? (
              <Empty icon={<InboxIcon />} className="py-8">
                No one is waiting.
              </Empty>
            ) : (
              <Table className="[--meta-indent:2.75rem]">
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead scope="col">Email</TableHead>
                    <TableHead scope="col">Status</TableHead>
                    <TableHead scope="col">
                      <span className="sr-only">Actions</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {pending.map((m, i) => (
                    <TableRow key={m.id} className="row-enter" style={stagger(i)}>
                      <TableCell data-primary="">
                        <Person m={m} />
                      </TableCell>
                      <TableCell data-label="Status" data-bare="">
                        <Status tone="warn" live>
                          Asked {ago(m.requested_at ?? m.created_at) || "recently"}
                        </Status>
                      </TableCell>
                      <TableCell data-actions="end" className="w-px whitespace-nowrap pr-3 text-right">
                        <div className="flex justify-end gap-2 max-sm:justify-start">
                          <Button type="button" size="sm" onClick={() => void approve(m)} disabled={busy.has(m.id)} aria-label={`Approve ${m.email}`}>
                            {busy.has(m.id) ? <Loader2Icon className="animate-spin" aria-hidden="true" /> : <CheckIcon aria-hidden="true" />}
                            Approve
                          </Button>
                          <Button type="button" variant="outline" size="sm" onClick={() => void remove(m)} disabled={busy.has(m.id)} aria-label={`Turn down ${m.email}`}>
                            Turn down
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </Section>

          <Section title="Members" lead="Everyone here can see the linked accounts, change which chats are shared and approve others.">
            {approved.length === 0 ? (
              <Empty icon={<UsersIcon />}>No members yet.</Empty>
            ) : (
              <Table className="[--meta-indent:2.75rem]">
                <TableHeader>
                  <TableRow className="hover:bg-transparent">
                    <TableHead scope="col">Member</TableHead>
                    <TableHead scope="col">Sign-in</TableHead>
                    <TableHead scope="col">
                      <span className="sr-only">Actions</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {approved.map((m, i) => (
                    <TableRow key={m.id} className="row-enter" style={stagger(i)}>
                      <TableCell data-primary="">
                        <Person m={m} you={m.email === auth.email} />
                      </TableCell>
                      <TableCell data-label="Sign-in" data-bare="" className="text-muted-foreground">
                        {m.last_login_at
                          ? `${PROVIDER[m.provider ?? ""] ?? "Signed in"}, last ${ago(m.last_login_at)}`
                          : `${admittedBy(m.approved_by)}${fmtDate(m.approved_at ?? m.created_at) ? " on " + fmtDate(m.approved_at ?? m.created_at) : ""}. Not signed in yet.`}
                      </TableCell>
                      <TableCell data-actions="" className="w-px pr-3 text-right">
                        <Button type="button" variant="outline-destructive" size="sm" onClick={() => void remove(m)} disabled={busy.has(m.id)} aria-label={`Remove ${m.email}`}>
                          Remove
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </Section>

          <Section title="Invite someone" lead="They are approved at once and can sign in with this email.">
            <InviteForm onInvited={(m) => members.set((l) => [...l.filter((x) => x.id !== m.id), m])} />
          </Section>
        </>
      )}
    </Page>
  );
}

function InviteForm({ onInvited }: { onInvited: (m: Member) => void }) {
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [done, setDone] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const v = email.trim();
    setDone("");
    if (!EMAIL_RE.test(v)) {
      setError("Enter their email address, like name@example.com.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const m = await api.post<Member>("/members", name.trim() ? { email: v, name: name.trim() } : { email: v });
      onInvited({ status: "approved", ...m });
      setDone(`${v} can sign in now. If the bridge sends mail, a sign-in link is on its way.`);
      setEmail("");
      setName("");
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) setError(`${v} is already a member.`);
      else if (err instanceof ApiError && err.status === 400) setError("That does not look like an email address.");
      else setError(errorText(err));
    }
    setBusy(false);
  };

  return (
    <form onSubmit={submit} className="grid gap-4 rounded-xl border bg-card p-5 shadow-xs sm:p-6" noValidate>
      <div className="grid gap-4 sm:grid-cols-2 lg:max-w-2xl">
        <div className="grid gap-2">
          <Label htmlFor="invite-email">Email</Label>
          <Input
            id="invite-email"
            type="email"
            inputMode="email"
            autoComplete="off"
            autoCapitalize="off"
            spellCheck={false}
            placeholder="name@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? "invite-error" : undefined}
          />
        </div>
        <div className="grid gap-2">
          <Label htmlFor="invite-name">
            Name <span className="font-normal text-muted-foreground">(optional)</span>
          </Label>
          <Input id="invite-name" autoComplete="off" value={name} onChange={(e) => setName(e.target.value)} />
        </div>
      </div>
      {error && <FieldError id="invite-error">{error}</FieldError>}
      <div className="flex flex-wrap items-center gap-3">
        <Button type="submit" disabled={busy}>
          {busy ? <Loader2Icon className="animate-spin" aria-hidden="true" /> : <UserPlusIcon aria-hidden="true" />}
          {busy ? "Inviting…" : "Invite"}
        </Button>
        <span role="status" className="text-[13px] text-success-foreground">
          {done && (
            <span className="fade-enter inline-flex items-center gap-1.5">
              <CheckIcon className="size-3.5" aria-hidden="true" />
              {done}
            </span>
          )}
        </span>
      </div>
    </form>
  );
}
