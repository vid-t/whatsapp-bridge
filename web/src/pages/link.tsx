import { LinkFlow, LinkHelp } from "@/components/link-flow";
import { Page, PageHeader } from "@/components/shell";
import { navigate, useTitle } from "@/lib/router";
import { enc } from "@/lib/utils";

/** Link a WhatsApp account, then open it to tick its chats. */
export function LinkPage() {
  useTitle("Link a WhatsApp account");
  return (
    <Page>
      <PageHeader
        back={{ href: "/", label: "Accounts" }}
        title="Link a WhatsApp account"
        lead="The bridge joins the account as a linked device. It reads messages and never sends anything."
      />
      <div className="grid gap-4">
        <LinkFlow
          onLinked={(id) => window.setTimeout(() => location.pathname === "/link" && navigate(`/sessions/${enc(id)}`), 1200)}
        />
        <LinkHelp />
      </div>
    </Page>
  );
}
