// How AI assistants reach this bridge. The protected-resource document names
// the MCP endpoint (the bridge's public address) and, when the bridge runs its
// own OAuth, its authorization server: then assistants connect by signing in
// and need no key. Any failure means "keys only".
import { useEffect, useState } from "react";

export interface Access {
  /** null while checking. */
  oauth: boolean | null;
  /** The MCP endpoint as the bridge publishes it, or this origin's. */
  mcpUrl: string;
}

const local = (): Access => ({ oauth: null, mcpUrl: `${location.origin}/mcp` });

async function getJSON(path: string): Promise<Record<string, unknown> | null> {
  try {
    const res = await fetch(path, { headers: { Accept: "application/json" }, credentials: "same-origin" });
    // A dev server answers unknown paths with the HTML shell; only JSON counts.
    if (!res.ok || !(res.headers.get("Content-Type") ?? "").includes("json")) return null;
    return (await res.json()) as Record<string, unknown>;
  } catch {
    return null;
  }
}

/** https, or http on a loopback address. */
function usable(resource: unknown): resource is string {
  if (typeof resource !== "string") return false;
  try {
    const u = new URL(resource);
    if (u.pathname !== "/mcp") return false;
    if (u.protocol === "https:") return true;
    return u.protocol === "http:" && /^(127\.\d+\.\d+\.\d+|localhost|\[::1\])$/.test(u.hostname);
  } catch {
    return false;
  }
}

async function detect(): Promise<Access> {
  const base = local();
  const pr = await getJSON("/.well-known/oauth-protected-resource/mcp");
  if (!pr) return { ...base, oauth: false };
  const mcpUrl = usable(pr.resource) ? pr.resource : base.mcpUrl;
  const servers = Array.isArray(pr.authorization_servers) ? pr.authorization_servers : [];
  if (servers.length === 0) return { oauth: false, mcpUrl };
  const as = await getJSON("/.well-known/oauth-authorization-server");
  return { oauth: !!as && typeof as.authorization_endpoint === "string", mcpUrl };
}

let cached: Promise<Access> | null = null;

/** useAccess checks once per page load; the result is shared. */
export function useAccess(): Access {
  const [v, setV] = useState<Access>(local);
  useEffect(() => {
    let live = true;
    cached ??= detect();
    void cached.then((a) => live && setV(a));
    return () => {
      live = false;
    };
  }, []);
  return v;
}

/** Addresses that cloud assistants (claude.ai, ChatGPT) cannot reach: plain http, loopback, private ranges, tailnet names. */
export function notPublic(url: string) {
  try {
    const u = new URL(url);
    const h = u.hostname;
    return (
      u.protocol !== "https:" ||
      /^(localhost|\[::1\]|127\.|10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)/.test(h) ||
      h.endsWith(".ts.net") ||
      h.endsWith(".local")
    );
  } catch {
    return true;
  }
}
