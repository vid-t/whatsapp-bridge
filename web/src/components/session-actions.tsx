import { LogOutIcon, RefreshCwIcon, Trash2Icon, XIcon } from "lucide-react";
import type { ReactNode } from "react";
import { useConfirm } from "./confirm";
import { Status } from "./status";
import { Button } from "./ui/button";
import { ApiError, api, errorText } from "@/lib/api";
import { busyStore, useBusyVersion } from "@/lib/busy";
import { sessionTitle, stateInfo } from "@/lib/format";
import { useNotice } from "@/lib/notice";
import { PRODUCT } from "@/lib/product";
import type { SessionInfo } from "@/lib/types";
import { enc } from "@/lib/utils";

export function SessionStatus({ s, className }: { s: Pick<SessionInfo, "state">; className?: string }) {
  const { label, tone } = stateInfo(s.state);
  return (
    <Status tone={tone} live={s.state === "connecting" || s.state === "pairing"} className={className}>
      {label}
    </Status>
  );
}

/**
 * The actions an account's state allows: Log out, Reconnect, Cancel linking,
 * Remove. `changed` runs after each one; `removed` after a successful Remove.
 */
export function SessionActions({ s, changed, removed }: { s: SessionInfo; changed: () => void; removed?: () => void }) {
  useBusyVersion();
  const confirm = useConfirm();
  const { notify, clearError } = useNotice();
  const path = "/sessions/" + enc(s.id);
  const busy = busyStore.isBusy(s.id);
  const title = sessionTitle(s);

  const run = (fn: () => Promise<void>) => async () => {
    busyStore.setBusy(s.id, true);
    clearError();
    try {
      await fn();
    } catch (err) {
      notify(errorText(err));
    }
    busyStore.setBusy(s.id, false);
    changed();
  };
  const ask = (verb: string, what: ReactNode) => confirm({ title: `${verb} ${title}?`, description: what, confirmLabel: verb, destructive: true });

  const logout = (
    <Button
      key="logout"
      type="button"
      variant="outline-destructive"
      disabled={busy}
      onClick={run(async () => {
        const ok = await ask(
          "Log out",
          <p>This unlinks the bridge on the phone and deletes this account's stored chats and messages. The activity log is kept.</p>,
        );
        if (!ok) return;
        const res = await api.post<{ warning?: string } | null>(path + "/logout");
        busyStore.setStillLinked(s.id, false);
        if (res && res.warning) notify(res.warning, "warn");
      })}
    >
      <LogOutIcon aria-hidden="true" />
      Log out
    </Button>
  );
  const remove = (
    <Button
      key="remove"
      type="button"
      variant="outline-destructive"
      disabled={busy}
      onClick={run(async () => {
        const ok = await ask(
          "Remove",
          <p>
            This deletes the account's stored chats and messages from the bridge. If the phone still lists “{PRODUCT}” under Linked
            devices, remove it there too.
          </p>,
        );
        if (!ok) return;
        try {
          await api.del(path);
        } catch (err) {
          if (err instanceof ApiError && err.status === 409) busyStore.setStillLinked(s.id, true);
          throw err;
        }
        removed?.();
      })}
    >
      <Trash2Icon aria-hidden="true" />
      Remove
    </Button>
  );

  const actions: ReactNode[] = [];
  if (s.state === "connected") actions.push(logout);
  else if (s.state === "pairing")
    actions.push(
      <Button key="cancel" type="button" variant="outline" disabled={busy} onClick={run(async () => void (await api.post(path + "/cancel")))}>
        <XIcon aria-hidden="true" />
        Cancel linking
      </Button>,
    );
  else {
    if (s.state === "disconnected")
      actions.push(
        <Button key="reconnect" type="button" variant="outline" disabled={busy} onClick={run(async () => void (await api.post(path + "/reconnect")))}>
          <RefreshCwIcon className={busy ? "animate-spin" : undefined} aria-hidden="true" />
          Reconnect
        </Button>,
      );
    // Remove answers 409 while the phone still lists the bridge; then Log out is offered too.
    if (busyStore.isStillLinked(s.id)) actions.push(logout);
    actions.push(remove);
  }
  return <>{actions}</>;
}
