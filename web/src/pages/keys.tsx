import { KeyRoundIcon, Loader2Icon, PlusIcon, ShieldAlertIcon } from "lucide-react";
import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Avatar } from "@/components/avatar";
import { CanSendBadge } from "@/components/can-send";
import { useConfirm } from "@/components/confirm";
import { CopyButton } from "@/components/copy";
import { Empty, FieldError, FieldHint, LoadingRows, RetryLine } from "@/components/feedback";
import { Page, PageHeader, Section } from "@/components/shell";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useAccess } from "@/lib/access";
import { ApiError, api, errorText } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { ago, fmtDate } from "@/lib/format";
import { stagger } from "@/lib/motion";
import { useNotice } from "@/lib/notice";
import { Link, useTitle } from "@/lib/router";
import type { KeyInfo, NewKey } from "@/lib/types";
import { enc } from "@/lib/utils";

export function KeysPage() {
  useTitle("Keys");
  const access = useAccess();
  const { sendEnabled } = useAuth();
  const confirm = useConfirm();
  const { notify } = useNotice();
  const [keys, setKeys] = useState<KeyInfo[] | null>(null);
  const [error, setError] = useState("");
  const [label, setLabel] = useState("");
  const [canSend, setCanSend] = useState(false);
  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState("");
  const [created, setCreated] = useState<NewKey | null>(null);

  const load = useCallback(async () => {
    try {
      setKeys(await api.get<KeyInfo[]>("/keys"));
      setError("");
    } catch (err) {
      setError("Could not load the keys: " + errorText(err));
    }
  }, []);
  useEffect(() => {
    void load();
  }, [load]);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    const l = label.trim();
    if (!l) {
      setFormError("Name the key after what uses it, so you can revoke the right one later.");
      return;
    }
    setBusy(true);
    setFormError("");
    try {
      setCreated(await api.post<NewKey>("/keys", { label: l, can_send: sendEnabled && canSend }));
      setLabel("");
      setCanSend(false);
      void load();
    } catch (err) {
      setFormError(errorText(err));
    }
    setBusy(false);
  };

  const revoke = async (k: KeyInfo) => {
    const ok = await confirm({
      title: `Revoke ${k.label}?`,
      description: <p>Whatever uses this key loses access at once. This cannot be undone.</p>,
      confirmLabel: "Revoke",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/keys/${enc(k.id)}`);
    } catch (err) {
      if (!(err instanceof ApiError && err.status === 404)) return notify(errorText(err));
    }
    setKeys((l) => (l ?? []).filter((x) => x.id !== k.id));
    if (created?.id === k.id) setCreated(null);
  };

  return (
    <Page>
      <PageHeader
        title="Keys"
        lead={
          <>
            A key lets a script, or an AI assistant that cannot sign in, read ticked chats over MCP at{" "}
            <code className="rounded bg-muted px-1.5 py-0.5 text-[13px] text-foreground">{access.mcpUrl}</code>. Assistants that sign in
            need no key; see{" "}
            <Link href="/assistants" className="link">
              Connect AI
            </Link>
            .
          </>
        }
      />

      <Section title="Create a key">
        <form onSubmit={create} className="grid gap-4 rounded-xl border bg-card p-5 shadow-xs sm:p-6" noValidate>
          <div className="grid max-w-xl gap-2">
            <Label htmlFor="key-label">Name</Label>
            <div className="flex flex-col gap-2 sm:flex-row">
              <Input
                id="key-label"
                placeholder="Laptop script"
                value={label}
                onChange={(e) => setLabel(e.target.value)}
                maxLength={80}
                aria-invalid={formError ? true : undefined}
                aria-describedby={formError ? "key-label-error" : "key-label-hint"}
              />
              <Button type="submit" disabled={busy} className="shrink-0">
                {busy ? <Loader2Icon className="animate-spin" aria-hidden="true" /> : <PlusIcon aria-hidden="true" />}
                {busy ? "Creating…" : "Create key"}
              </Button>
            </div>
            {formError ? (
              <FieldError id="key-label-error">{formError}</FieldError>
            ) : (
              <FieldHint id="key-label-hint">What uses the key, so you can revoke the right one later.</FieldHint>
            )}
          </div>
          {sendEnabled && (
            <div className="flex max-w-xl items-start gap-3">
              <Checkbox
                id="key-send"
                checked={canSend}
                disabled={busy}
                onCheckedChange={(v) => setCanSend(v === true)}
                aria-describedby="key-send-hint"
                className="mt-px"
              />
              <div className="grid gap-1.5">
                <Label htmlFor="key-send" className="cursor-pointer leading-snug">
                  Let this key send messages to ticked chats
                </Label>
                <FieldHint id="key-send-hint">
                  Without this the key can only read. You cannot add sending to a key later; create a new key instead.
                </FieldHint>
              </div>
            </div>
          )}
        </form>
        {created && (
          <div key={created.id} className="page-enter grid gap-3 rounded-xl border bg-subtle p-5 sm:p-6" role="status">
            <p className="flex items-start gap-2.5 text-sm">
              <ShieldAlertIcon className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
              <span>
                <strong className="font-medium">Copy the key for {created.label} now.</strong>{" "}
                <span className="text-muted-foreground">
                  It is shown only once; the bridge keeps only a hash.{" "}
                  {created.can_send ? "It can send messages to ticked chats." : "It can only read."}
                </span>
              </span>
            </p>
            <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:items-center">
              <div
                role="region"
                aria-label={`New key for ${created.label}`}
                tabIndex={0}
                className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap rounded-md border bg-background px-3 py-2"
              >
                <code className="secret select-all text-[13px]">{created.key}</code>
              </div>
              <CopyButton text={created.key} label="Copy key" size="default" />
            </div>
            <p className="text-[13px] text-muted-foreground">
              Send it as the header <code className="text-[12px] text-foreground">Authorization: Bearer &lt;key&gt;</code>.
            </p>
          </div>
        )}
      </Section>

      <Section
        title="Active keys"
        lead={
          sendEnabled
            ? "Keys marked Can send may send messages to ticked chats; the others only read. Revoke a key and whatever uses it stops at once."
            : "Revoke a key and whatever uses it stops at once."
        }
      >
        {error ? (
          <RetryLine text={error} onRetry={() => void load()} />
        ) : keys === null ? (
          <LoadingRows label="Loading keys…" rows={2} />
        ) : keys.length === 0 ? (
          <Empty icon={<KeyRoundIcon />} title="No keys yet">
            Create one above when a script or an assistant needs it.
          </Empty>
        ) : (
          <Table className="[--meta-indent:2.75rem]">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead scope="col">Name</TableHead>
                <TableHead scope="col">Created</TableHead>
                <TableHead scope="col">Last used</TableHead>
                <TableHead scope="col">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {keys.map((k, i) => (
                <TableRow key={k.id} className="row-enter" style={stagger(i)}>
                  <TableCell data-primary="">
                    <div className="flex min-w-0 items-center gap-3">
                      <Avatar name={k.label} group />
                      <div className="grid min-w-0">
                        <span className="flex min-w-0 items-center gap-2">
                          <span className="truncate font-medium">{k.label}</span>
                          {k.can_send && <CanSendBadge />}
                        </span>
                        <span className="truncate font-mono text-xs text-muted-foreground">{k.id}</span>
                      </div>
                    </div>
                  </TableCell>
                  <TableCell data-label="Created" className="text-muted-foreground">
                    {fmtDate(k.created_at)}
                  </TableCell>
                  <TableCell data-label="Last used" className="text-muted-foreground">
                    {k.last_used_at ? ago(k.last_used_at) : "Never"}
                  </TableCell>
                  <TableCell data-actions="" className="w-px pr-3 text-right">
                    <Button type="button" variant="outline-destructive" size="sm" onClick={() => void revoke(k)} aria-label={`Revoke ${k.label}`}>
                      Revoke
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>
    </Page>
  );
}
