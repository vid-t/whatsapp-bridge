import { BotIcon, GlobeLockIcon, KeyRoundIcon } from "lucide-react";
import { useEffect, useState, type ReactNode } from "react";
import { Avatar } from "@/components/avatar";
import { CanSendBadge } from "@/components/can-send";
import { useConfirm } from "@/components/confirm";
import { CodeBlock, CopyField } from "@/components/copy";
import { Empty, LoadingRows, RetryLine } from "@/components/feedback";
import { Page, PageHeader, Section } from "@/components/shell";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { notPublic, useAccess } from "@/lib/access";
import { ApiError, api, errorText } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { ago, fmtDate } from "@/lib/format";
import { stagger } from "@/lib/motion";
import { useNotice } from "@/lib/notice";
import { useOptionalList } from "@/lib/optional";
import { PRODUCT } from "@/lib/product";
import { Link, useTitle } from "@/lib/router";
import type { Grant } from "@/lib/types";
import { enc } from "@/lib/utils";

/** The name the bridge gets in an assistant's config: "whatsapp-bridge". */
const SLUG = PRODUCT.toLowerCase().replace(/[^a-z0-9]+/g, "-");
const KEY_VAR = SLUG.toUpperCase().replace(/-/g, "_") + "_MCP_KEY";

/** Who published the client's identity, from a client-id URL; "" for a registered id. */
function publisher(clientId: string) {
  try {
    return new URL(clientId).hostname;
  } catch {
    return "";
  }
}

function Steps({ items }: { items: ReactNode[] }) {
  return (
    <ol className="grid gap-3.5">
      {items.map((t, i) => (
        <li key={i} className="row-enter flex gap-3.5" style={stagger(i)}>
          <span className="grid size-6 shrink-0 place-items-center rounded-full border bg-background font-mono text-[11px] font-medium text-muted-foreground" aria-hidden="true">
            {i + 1}
          </span>
          <span className="pt-0.5 leading-relaxed">{t}</span>
        </li>
      ))}
    </ol>
  );
}

const Note = ({ children }: { children: ReactNode }) => <p className="text-[13px] leading-relaxed text-muted-foreground">{children}</p>;

function Setup({ url, oauth }: { url: string; oauth: boolean }) {
  const key = "<your key>";
  const [tab, setTab] = useState("claude");
  return (
    <Tabs value={tab} onValueChange={setTab} className="overflow-hidden rounded-xl border bg-card shadow-xs">
      <div className="border-b bg-subtle px-3 py-3 sm:px-4">
        <TabsList value={tab} aria-label="Assistant" className="max-sm:grid max-sm:h-auto max-sm:w-full max-sm:grid-cols-2 max-sm:[&>button]:h-9">
          <TabsTrigger value="claude">Claude</TabsTrigger>
          <TabsTrigger value="claude-code">Claude Code</TabsTrigger>
          <TabsTrigger value="chatgpt">ChatGPT</TabsTrigger>
          <TabsTrigger value="codex">Codex CLI</TabsTrigger>
        </TabsList>
      </div>
      <div className="p-5 sm:p-6">
        <TabsContent value="claude" className="fade-enter grid gap-4">
          <h3 className="font-medium">Claude, on the web and in the desktop app</h3>
          <Steps
            items={[
              "Open Settings › Connectors (or Customize › Connectors) and choose Add custom connector.",
              <>
                Name it {PRODUCT} and paste the MCP server URL.
                {!oauth && <> Under Request headers, add Authorization with the value Bearer and your key.</>}
              </>,
              ...(oauth ? ["Click Add. Sign in to the bridge in the window that opens, then click Allow."] : []),
              `In a chat, open + › Connectors and switch ${PRODUCT} on.`,
            ]}
          />
        </TabsContent>
        <TabsContent value="claude-code" className="fade-enter grid gap-4">
          <h3 className="font-medium">Claude Code</h3>
          <CodeBlock
            label="Claude Code commands"
            code={
              oauth
                ? `claude mcp add --transport http ${SLUG} ${url}\nclaude mcp login ${SLUG}`
                : `claude mcp add --transport http ${SLUG} ${url} --header "Authorization: Bearer ${key}"`
            }
          />
          {oauth && <Note>The login opens the bridge's sign-in page in your browser. Click Allow. Inside Claude Code, /mcp does the same.</Note>}
        </TabsContent>
        <TabsContent value="chatgpt" className="fade-enter grid gap-4">
          <h3 className="font-medium">ChatGPT</h3>
          <Steps
            items={[
              "Open Settings › Apps (or Apps & Connectors) and turn on Developer mode under Advanced.",
              "Choose Create, paste the MCP server URL and pick OAuth as the authentication.",
              "Sign in to the bridge in the window that opens, then click Allow.",
              "In a chat, turn the app on from the tools menu.",
            ]}
          />
          {!oauth && <Note>ChatGPT needs assistant sign-in (OAuth), which this bridge does not offer.</Note>}
        </TabsContent>
        <TabsContent value="codex" className="fade-enter grid gap-4">
          <h3 className="font-medium">Codex CLI</h3>
          <CodeBlock
            label="Codex CLI commands"
            code={oauth ? `codex mcp add ${SLUG} --url ${url}` : `export ${KEY_VAR}=${key}\ncodex mcp add ${SLUG} --url ${url} --bearer-token-env-var ${KEY_VAR}`}
          />
          {oauth && <Note>Codex opens the bridge's sign-in page straight away. Click Allow. To sign in again later, run codex mcp login {SLUG}.</Note>}
        </TabsContent>
      </div>
    </Tabs>
  );
}

export function AssistantsPage() {
  useTitle("Connect AI");
  const access = useAccess();
  const { sendEnabled } = useAuth();
  const confirm = useConfirm();
  const { notify } = useNotice();
  const grants = useOptionalList<Grant>("/oauth/grants");
  const reload = grants.reload;
  const oauth = access.oauth !== false;

  // Pick up an assistant that signs in while the page is open.
  useEffect(() => {
    const t = window.setInterval(() => document.visibilityState === "visible" && void reload(), 10000);
    return () => clearInterval(t);
  }, [reload]);

  const revoke = async (g: Grant) => {
    const ok = await confirm({
      title: `Revoke ${g.client_name}?`,
      description: <p>{g.client_name} loses access at once. To use it again, connect it and sign in once more.</p>,
      confirmLabel: "Revoke",
      destructive: true,
    });
    if (!ok) return;
    try {
      await api.del(`/oauth/grants/${enc(g.id)}`);
      grants.set((l) => l.filter((x) => x.id !== g.id));
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) grants.set((l) => l.filter((x) => x.id !== g.id));
      else notify(errorText(err));
    }
  };

  const sorted = [...grants.list].sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at));

  return (
    <Page>
      <PageHeader
        title="Connect AI"
        lead={
          sendEnabled
            ? "AI assistants connect to the bridge over MCP. They read ticked chats only when you ask them something. Those you allowed to send can also send messages to ticked chats."
            : "AI assistants connect to the bridge over MCP. They can read ticked chats only, and only when you ask them something."
        }
      />

      <Section title="Address">
        <div className="grid gap-3 rounded-xl border bg-card p-5 shadow-xs sm:p-6">
          <CopyField label="MCP server URL" value={access.mcpUrl} />
          {oauth ? (
            <p className="flex items-center gap-2 text-[13px] text-muted-foreground">
              <GlobeLockIcon className="size-3.5 shrink-0" aria-hidden="true" />
              Assistants sign in to the bridge once. No key to copy.
            </p>
          ) : (
            <p className="flex items-start gap-2 text-[13px] text-muted-foreground">
              <KeyRoundIcon className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
              <span>
                This bridge has no assistant sign-in. Create a key under{" "}
                <Link href="/keys" className="link">
                  Keys
                </Link>{" "}
                and send it as the header <code className="text-[12px] text-foreground">Authorization: Bearer &lt;key&gt;</code>.
              </span>
            </p>
          )}
        </div>
        {notPublic(access.mcpUrl) && (
          <Alert variant="warning">
            <GlobeLockIcon aria-hidden="true" />
            <AlertTitle>Not reachable from the internet</AlertTitle>
            <AlertDescription>
              <p>
                Claude on the web and ChatGPT connect from their own servers, so they need a public https address. This address works with
                Claude Code and Codex CLI on a machine that can reach it.
              </p>
            </AlertDescription>
          </Alert>
        )}
      </Section>

      <Section title="Set up your assistant">
        <Setup url={access.mcpUrl} oauth={oauth} />
      </Section>

      <Section
        title="Connected assistants"
        lead={
          sendEnabled
            ? "Each assistant that signed in has its own grant. Those marked Can send may send; the others only read. To let one send, connect it again and click Allow. Revoke one and it loses access at once."
            : "Each assistant that signed in has its own grant. Revoke one and it loses access at once."
        }
      >
        {grants.status === "loading" ? (
          <LoadingRows label="Loading assistants…" rows={2} />
        ) : grants.status === "error" ? (
          <RetryLine text={"Could not load the assistants: " + grants.error} onRetry={() => void grants.reload()} />
        ) : grants.status === "off" ? (
          <Empty icon={<KeyRoundIcon />} title="No assistant sign-in">
            Assistants use keys instead; see{" "}
            <Link href="/keys" className="link">
              Keys
            </Link>
            .
          </Empty>
        ) : sorted.length === 0 ? (
          <Empty icon={<BotIcon />} title="No assistant has connected yet">
            Follow the steps above. The assistant appears here as soon as it signs in.
          </Empty>
        ) : (
          <Table className="[--meta-indent:2.75rem]">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead scope="col">Assistant</TableHead>
                <TableHead scope="col">Connected</TableHead>
                <TableHead scope="col">Last used</TableHead>
                <TableHead scope="col">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {sorted.map((g, i) => {
                const pub = publisher(g.client_id);
                return (
                  <TableRow key={g.id} className="row-enter" style={stagger(i)}>
                    <TableCell data-primary="">
                      <div className="flex min-w-0 items-center gap-3">
                        <Avatar name={g.client_name} group />
                        <div className="grid min-w-0">
                          <span className="flex min-w-0 items-center gap-2">
                            <span className="truncate font-medium">{g.client_name}</span>
                            {g.can_send && <CanSendBadge />}
                          </span>
                          <span className="truncate font-mono text-xs text-muted-foreground">{pub ? `${pub} · ${g.id}` : g.id}</span>
                        </div>
                      </div>
                    </TableCell>
                    <TableCell data-label="Connected" className="text-muted-foreground">
                      {fmtDate(g.created_at)}
                    </TableCell>
                    <TableCell data-label="Last used" className="text-muted-foreground">
                      {g.last_used_at ? ago(g.last_used_at) : "Not yet"}
                    </TableCell>
                    <TableCell data-actions="" className="w-px pr-3 text-right">
                      <Button type="button" variant="outline-destructive" size="sm" onClick={() => void revoke(g)} aria-label={`Revoke ${g.client_name}`}>
                        Revoke
                      </Button>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </Section>
    </Page>
  );
}
