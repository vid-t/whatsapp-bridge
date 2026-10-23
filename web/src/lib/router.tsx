// A small history router: flat routes, no nesting.
import { useEffect, useSyncExternalStore, type AnchorHTMLAttributes, type MouseEvent } from "react";
import { PRODUCT } from "./product";

export type Route =
  | { name: "accounts" }
  | { name: "signin" }
  | { name: "link" }
  | { name: "account"; id: string }
  | { name: "assistants" }
  | { name: "keys" }
  | { name: "members" }
  | { name: "notfound" };

const ID_RE = /^[A-Za-z0-9_-]{1,64}$/;
const FIXED: Record<string, Route["name"]> = {
  "/": "accounts",
  "/signin": "signin",
  "/link": "link",
  "/assistants": "assistants",
  "/keys": "keys",
  "/members": "members",
};

export function matchRoute(pathname: string): Route {
  const p = pathname.replace(/\/+$/, "") || "/";
  const fixed = FIXED[p];
  if (fixed) return { name: fixed } as Route;
  const m = p.match(/^\/sessions\/([^/]+)$/);
  if (m) {
    let id = "";
    try {
      id = decodeURIComponent(m[1]);
    } catch {
      /* malformed escape: not found */
    }
    if (ID_RE.test(id)) return { name: "account", id };
  }
  return { name: "notfound" };
}

const listeners = new Set<() => void>();
const emit = () => listeners.forEach((l) => l());
window.addEventListener("popstate", emit);

function subscribe(l: () => void) {
  listeners.add(l);
  return () => listeners.delete(l);
}
const snapshot = () => location.pathname + location.search;

/** useLocation re-renders on navigation and returns the path plus query. */
export function useLocation() {
  const href = useSyncExternalStore(subscribe, snapshot);
  const url = new URL(href, location.origin);
  return { pathname: url.pathname, search: url.searchParams, href };
}

export function navigate(to: string, opts: { replace?: boolean } = {}) {
  if (to === snapshot()) return;
  if (opts.replace) history.replaceState(null, "", to);
  else history.pushState(null, "", to);
  emit();
}

/** A same-origin path to return to after sign-in; anything else becomes "/". */
export function safeNext(v: string | null | undefined) {
  if (!v || !v.startsWith("/") || v.startsWith("//") || v.startsWith("/\\")) return "/";
  if (v.startsWith("/signin")) return "/";
  return v;
}

/** Link is an <a> that navigates in-app on a plain left click. */
export function Link({ href, onClick, ...rest }: AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) {
  const handle = (e: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(e);
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    if (rest.target && rest.target !== "_self") return;
    if (!href.startsWith("/") || href.startsWith("/auth/")) return;
    e.preventDefault();
    navigate(href);
  };
  return <a href={href} onClick={handle} {...rest} />;
}

/** useTitle sets the document title for a page. */
export function useTitle(title: string) {
  useEffect(() => {
    document.title = title ? `${title} · ${PRODUCT}` : PRODUCT;
  }, [title]);
}
