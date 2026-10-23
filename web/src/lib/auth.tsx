// Who the browser is and how it may sign in, from GET /api/me.
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { ApiError, UNAUTHORIZED_EVENT, api } from "./api";
import type { Me, Provider } from "./types";

export interface AuthState {
  status: "loading" | "ready" | "error";
  error: string;
  me: Me;
  /** Signed in, or no sign-in needed (open mode). */
  authenticated: boolean;
  /** The bridge asks for a sign-in (not open mode). */
  loginRequired: boolean;
  /** Sign-in methods, in the bridge's order. */
  providers: Provider[];
  /** The bridge's display name, "" when unset. */
  name: string;
  /** The signed-in member's email, "" for an access-code session. */
  email: string;
  refresh: () => Promise<void>;
  signIn: (code: string) => Promise<void>;
  /** Asks the bridge to email a sign-in link (202 whether or not the address is known). */
  emailLink: (email: string, next: string) => Promise<void>;
  signOut: () => Promise<void>;
}

const SIGNED_OUT: Me = { auth_required: true, authenticated: false, principal: "anonymous" };
const KNOWN: Provider[] = ["code", "email", "google", "microsoft"];
const Ctx = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthState["status"]>("loading");
  const [error, setError] = useState("");
  const [me, setMe] = useState<Me>(SIGNED_OUT);

  const refresh = useCallback(async () => {
    try {
      setMe(await api.get<Me>("/me"));
      setStatus("ready");
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setMe(SIGNED_OUT);
        setStatus("ready");
      } else {
        setError(err instanceof Error ? err.message : String(err));
        setStatus("error");
      }
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Any API call answered 401 means the browser session expired or was revoked.
  useEffect(() => {
    const lost = () => setMe((m) => (m.auth_required ? { ...m, authenticated: false, principal: "anonymous", user: undefined } : m));
    window.addEventListener(UNAUTHORIZED_EVENT, lost);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, lost);
  }, []);

  const signIn = useCallback(
    async (code: string) => {
      await api.post("/auth/login", { code });
      await refresh();
    },
    [refresh],
  );

  const emailLink = useCallback(async (email: string, next: string) => {
    await api.post("/auth/email/start", { email, next });
  }, []);

  const signOut = useCallback(async () => {
    try {
      await api.post("/auth/logout");
    } finally {
      await refresh();
    }
  }, [refresh]);

  const value = useMemo<AuthState>(() => {
    const listed = Array.isArray(me.providers) ? me.providers : [];
    return {
      status,
      error,
      me,
      authenticated: !me.auth_required || me.authenticated,
      loginRequired: me.auth_required,
      providers: listed.filter((p): p is Provider => (KNOWN as string[]).includes(p)),
      name: me.name?.trim() || "",
      email: me.user?.email ?? "",
      refresh,
      signIn,
      emailLink,
      signOut,
    };
  }, [status, error, me, refresh, signIn, emailLink, signOut]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAuth() {
  const v = useContext(Ctx);
  if (!v) throw new Error("useAuth outside AuthProvider");
  return v;
}
