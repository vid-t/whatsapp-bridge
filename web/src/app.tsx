import { useEffect, useRef, type ReactNode } from "react";
import { ConfirmProvider } from "@/components/confirm";
import { AppShell } from "@/components/shell";
import { AuthProvider, useAuth } from "@/lib/auth";
import { EventsProvider } from "@/lib/events";
import { NoticeProvider, useNotice } from "@/lib/notice";
import { matchRoute, navigate, useLocation } from "@/lib/router";
import { ThemeProvider } from "@/lib/theme";
import { AccountPage } from "@/pages/account";
import { AccountsPage } from "@/pages/accounts";
import { AssistantsPage } from "@/pages/assistants";
import { KeysPage } from "@/pages/keys";
import { LinkPage } from "@/pages/link";
import { MembersPage } from "@/pages/members";
import { Booting, NotFoundPage } from "@/pages/misc";
import { SignInPage } from "@/pages/signin";

function Pages() {
  const auth = useAuth();
  const { pathname, href } = useLocation();
  const route = matchRoute(pathname);
  const { clear } = useNotice();

  // A new page starts without the previous page's notice; focus moves to its
  // heading so screen readers announce it (not on the first load).
  const first = useRef(true);
  useEffect(() => {
    clear();
    if (first.current) {
      first.current = false;
      return;
    }
    window.scrollTo({ top: 0 });
    const t = window.setTimeout(() => {
      const h = document.querySelector<HTMLElement>("[data-page-title]");
      (h ?? document.getElementById("main"))?.focus({ preventScroll: true });
    }, 0);
    return () => clearTimeout(t);
  }, [pathname, clear]);

  // Signed out on a page that needs a session: the address becomes the sign-in page.
  const gated = auth.status === "ready" && !auth.authenticated && route.name !== "signin";
  useEffect(() => {
    if (gated) navigate(`/signin${href === "/" ? "" : "?next=" + encodeURIComponent(href)}`, { replace: true });
  }, [gated, href]);

  if (auth.status === "loading") return <Booting />;
  if (auth.status === "error") return <Booting error={auth.error} onRetry={() => void auth.refresh()} />;
  if (route.name === "signin" || gated) return <SignInPage />;

  switch (route.name) {
    case "accounts":
      return <AccountsPage />;
    case "link":
      return <LinkPage />;
    case "account":
      return <AccountPage key={route.id} id={route.id} />;
    case "assistants":
      return <AssistantsPage />;
    case "keys":
      return <KeysPage />;
    case "members":
      return <MembersPage />;
    default:
      return <NotFoundPage />;
  }
}

function Live({ children }: { children: ReactNode }) {
  const auth = useAuth();
  const { clearError } = useNotice();
  return (
    <EventsProvider enabled={auth.status === "ready" && auth.authenticated} onReconnect={clearError}>
      {children}
    </EventsProvider>
  );
}

export function App() {
  return (
    <ThemeProvider>
      <NoticeProvider>
        <AuthProvider>
          <ConfirmProvider>
            <Live>
              <AppShell>
                <Pages />
              </AppShell>
            </Live>
          </ConfirmProvider>
        </AuthProvider>
      </NoticeProvider>
    </ThemeProvider>
  );
}
