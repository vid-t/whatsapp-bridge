import { ChevronLeftIcon, ChevronRightIcon, CircleAlertIcon, KeyRoundIcon, LogOutIcon, MenuIcon, MonitorIcon, MoonIcon, SunIcon, TriangleAlertIcon, XIcon } from "lucide-react";
import { useEffect, useId, useState, type ReactNode } from "react";
import { Avatar } from "./avatar";
import { Wordmark } from "./brand";
import { Alert } from "./ui/alert";
import { Button } from "./ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "./ui/dropdown-menu";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle, SheetTrigger } from "./ui/sheet";
import { Segmented, SegmentedItem } from "./ui/toggle-group";
import { useAuth } from "@/lib/auth";
import { useGlide } from "@/lib/glide";
import { stagger } from "@/lib/motion";
import { useNotice } from "@/lib/notice";
import { PRODUCT } from "@/lib/product";
import { Link, useLocation } from "@/lib/router";
import { useTheme, type Theme } from "@/lib/theme";
import { cn } from "@/lib/utils";

const NAV = [
  { href: "/", label: "Accounts", match: (p: string) => p === "/" || p === "/link" || p.startsWith("/sessions/") },
  { href: "/assistants", label: "Connect AI", match: (p: string) => p === "/assistants" },
  { href: "/keys", label: "Keys", match: (p: string) => p === "/keys" },
  { href: "/members", label: "Members", match: (p: string) => p === "/members" },
];

const THEMES: { value: Theme; label: string; icon: typeof SunIcon }[] = [
  { value: "light", label: "Light", icon: SunIcon },
  { value: "dark", label: "Dark", icon: MoonIcon },
  { value: "system", label: "System", icon: MonitorIcon },
];

/* ------------------------------------------------------------ theme */

function ThemeMenu() {
  const { theme, resolved, setTheme } = useTheme();
  return (
    <DropdownMenu modal={false}>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon-sm" aria-label="Theme" className="text-muted-foreground hover:text-foreground">
          <span className="relative grid size-4 place-items-center" aria-hidden="true">
            <SunIcon className="swap-icon absolute size-4" data-on={resolved === "light"} />
            <MoonIcon className="swap-icon absolute size-4" data-on={resolved === "dark"} />
          </span>
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-[9rem]">
        <DropdownMenuLabel className="text-xs font-medium text-muted-foreground">Theme</DropdownMenuLabel>
        <DropdownMenuRadioGroup value={theme} onValueChange={(v) => setTheme(v as Theme)}>
          {THEMES.map(({ value, label, icon: Icon }) => (
            <DropdownMenuRadioItem key={value} value={value}>
              <Icon aria-hidden="true" />
              {label}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function ThemeSwitch({ labelId }: { labelId: string }) {
  const { theme, setTheme } = useTheme();
  return (
    <Segmented value={theme} onValueChange={(v) => setTheme(v as Theme)} aria-labelledby={labelId}>
      {THEMES.map(({ value, label, icon: Icon }) => (
        <SegmentedItem key={value} value={value} aria-label={label} className="px-2.5">
          <Icon className="size-4" aria-hidden="true" />
        </SegmentedItem>
      ))}
    </Segmented>
  );
}

/* ---------------------------------------------------------- account */

function who(auth: ReturnType<typeof useAuth>) {
  return auth.email || "Signed in with the access code";
}

function AccountMenu() {
  const auth = useAuth();
  const { me } = auth;
  if (!auth.loginRequired) {
    return (
      <span className="rounded-full border px-2.5 py-0.5 text-xs font-medium text-muted-foreground" title="This bridge asks nobody to sign in">
        Open mode
      </span>
    );
  }
  return (
    <DropdownMenu modal={false}>
      <DropdownMenuTrigger asChild>
        <button
          type="button"
          className="cursor-pointer rounded-full transition-[box-shadow,transform] duration-150 hover:ring-4 hover:ring-accent active:scale-95"
        >
          <Avatar name={auth.email || "Access code"} icon={auth.email ? undefined : <KeyRoundIcon aria-hidden="true" />} />
          <span className="sr-only">Account</span>
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-64">
        <DropdownMenuLabel className="grid gap-0.5">
          {auth.email ? (
            <>
              <span className="text-xs text-muted-foreground">Signed in as</span>
              <span className="truncate font-medium">{auth.email}</span>
            </>
          ) : (
            <span className="font-medium">Signed in with the access code</span>
          )}
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuItem onSelect={() => void auth.signOut()}>
          <LogOutIcon aria-hidden="true" />
          Sign out
        </DropdownMenuItem>
        {me.version && (
          <>
            <DropdownMenuSeparator />
            <p className="px-2 py-1 font-mono text-[11px] text-muted-foreground">
              {PRODUCT} {me.version}
            </p>
          </>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/* --------------------------------------------------------- navigation */

function NavTabs() {
  const { pathname } = useLocation();
  const active = NAV.find((n) => n.match(pathname))?.href ?? "";
  const g = useGlide<HTMLUListElement>('[aria-current="page"]', active);
  const [hover, setHover] = useState<{ x: number; w: number; glide: boolean } | null>(null);

  return (
    <nav aria-label="Main" className="sticky top-0 z-40 hidden border-b bg-background md:block">
      <div className="container-page relative flex h-11 items-stretch">
        <div>
          <ul ref={g.ref} className="relative -ml-3 flex h-full" onMouseLeave={() => setHover(null)}>
            <span
              aria-hidden="true"
              data-glide={hover?.glide || undefined}
              className="pointer-events-none absolute top-1.5 bottom-1.5 left-0 rounded-md bg-accent transition-opacity duration-150 data-[glide]:transition-[transform,width,opacity] data-[glide]:duration-200 data-[glide]:ease-out-expo dark:bg-accent/70"
              style={{ transform: `translateX(${hover?.x ?? 0}px)`, width: hover?.w ?? 0, opacity: hover ? 1 : 0 }}
            />
            {NAV.map((n) => (
              <li key={n.href} className="flex">
                <Link
                  href={n.href}
                  aria-current={n.match(pathname) ? "page" : undefined}
                  onMouseEnter={(e) => {
                    const el = e.currentTarget;
                    setHover((h) => ({ x: el.offsetLeft, w: el.offsetWidth, glide: !!h }));
                  }}
                  className="relative z-10 flex items-center rounded-md px-3 text-sm text-muted-foreground transition-colors duration-150 hover:text-foreground aria-[current=page]:text-foreground"
                >
                  {n.label}
                </Link>
              </li>
            ))}
            <span
              aria-hidden="true"
              data-ready={g.ready || undefined}
              className="tab-glide pointer-events-none absolute bottom-0 left-0 h-[2px] rounded-full bg-foreground"
              style={g.box ? { transform: `translateX(${g.box.x + 8}px)`, width: Math.max(0, g.box.w - 16) } : { opacity: 0 }}
            />
          </ul>
        </div>
      </div>
    </nav>
  );
}

function MobileMenu({ instance }: { instance: string }) {
  const auth = useAuth();
  const { pathname } = useLocation();
  const [open, setOpen] = useState(false);
  const themeLabel = useId();
  useEffect(() => setOpen(false), [pathname]);
  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetTrigger asChild>
        <Button variant="ghost" size="icon" className="-mr-2 md:hidden">
          <MenuIcon className="size-5" aria-hidden="true" />
          <span className="sr-only">Menu</span>
        </Button>
      </SheetTrigger>
      <SheetContent side="right" className="w-[min(88vw,21rem)]" closeLabel="Close menu">
        <SheetHeader>
          <SheetTitle>
            <Wordmark />
          </SheetTitle>
          <SheetDescription>{instance || "Self-hosted bridge"}</SheetDescription>
        </SheetHeader>
        <nav aria-label="Main" className="grid gap-0.5 p-3">
          {NAV.map((n, i) => (
            <Link
              key={n.href}
              href={n.href}
              aria-current={n.match(pathname) ? "page" : undefined}
              style={stagger(i)}
              className="row-enter group flex h-11 items-center justify-between rounded-md px-3 text-[15px] font-medium text-muted-foreground transition-colors duration-150 hover:bg-accent hover:text-foreground aria-[current=page]:bg-accent aria-[current=page]:text-foreground"
            >
              {n.label}
              <ChevronRightIcon className="size-4 opacity-40 transition-transform duration-150 group-hover:translate-x-0.5" aria-hidden="true" />
            </Link>
          ))}
        </nav>
        <div className="mt-auto grid gap-4 border-t p-4">
          <div className="flex items-center justify-between">
            <span id={themeLabel} className="text-sm text-muted-foreground">
              Theme
            </span>
            <ThemeSwitch labelId={themeLabel} />
          </div>
          {auth.loginRequired ? (
            <div className="grid gap-3">
              <p className="flex min-w-0 items-center gap-2.5 text-sm">
                <Avatar name={auth.email || "Access code"} size="sm" icon={auth.email ? undefined : <KeyRoundIcon aria-hidden="true" />} />
                <span className="truncate">{who(auth)}</span>
              </p>
              <Button type="button" variant="outline" onClick={() => void auth.signOut()}>
                <LogOutIcon aria-hidden="true" />
                Sign out
              </Button>
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">Open mode: no sign-in</p>
          )}
          {auth.me.version && (
            <p className="font-mono text-[11px] text-muted-foreground">
              {PRODUCT} {auth.me.version}
            </p>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}

function Header() {
  const auth = useAuth();
  const instance = auth.name && auth.name !== PRODUCT ? auth.name : "";
  const signedIn = auth.status === "ready" && auth.authenticated;

  return (
    <>
      <header className="z-40 bg-background max-md:sticky max-md:top-0 max-md:border-b">
        <div className="container-page flex h-14 items-center justify-between gap-4">
          <div className="flex min-w-0 items-center gap-2 leading-none">
            <Link href="/" className="flex shrink-0 items-center rounded-md">
              <Wordmark />
            </Link>
            {instance && (
              <>
                <svg viewBox="0 0 16 24" className="h-6 w-4 shrink-0 text-border-strong" aria-hidden="true">
                  <path d="M11 3 5 21" stroke="currentColor" strokeWidth="1.25" strokeLinecap="round" />
                </svg>
                <span className="truncate text-[15px] font-medium text-muted-foreground">{instance}</span>
              </>
            )}
          </div>
          <div className="flex items-center gap-1.5">
            <div className={cn("flex items-center gap-2", signedIn && "max-md:hidden")}>
              <ThemeMenu />
              {signedIn && <AccountMenu />}
            </div>
            {signedIn && <MobileMenu instance={instance} />}
          </div>
        </div>
      </header>
      {signedIn && <NavTabs />}
    </>
  );
}

/* ------------------------------------------------------------- notice */

/** The page-level notice: an error that a later load clears, or a warning that stays. */
function NoticeBar() {
  const { notice, clear } = useNotice();
  if (!notice) return null;
  const err = notice.kind === "err";
  return (
    <Alert
      key={notice.text}
      variant={err ? "destructive" : "warning"}
      role={err ? "alert" : "status"}
      className="page-enter mb-6 flex items-start gap-3 pr-2"
    >
      {err ? (
        <CircleAlertIcon className="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden="true" />
      ) : (
        <TriangleAlertIcon className="mt-0.5 size-4 shrink-0 text-warning-foreground" aria-hidden="true" />
      )}
      <p className="flex-1">{notice.text}</p>
      <Button type="button" variant="ghost" size="icon-sm" className="-my-1 size-7" onClick={clear} aria-label="Dismiss">
        <XIcon aria-hidden="true" />
      </Button>
    </Alert>
  );
}

/* -------------------------------------------------------------- shell */

/** Header, tabs and <main>; the pages render inside. */
export function AppShell({ children }: { children: ReactNode }) {
  return (
    <>
      <a
        href="#main"
        className="fixed left-3 top-3 z-[60] -translate-y-20 rounded-md bg-primary px-3 py-2 text-sm font-medium text-primary-foreground shadow-lg transition-transform duration-200 focus:translate-y-0"
      >
        Skip to content
      </a>
      <Header />
      <main id="main" tabIndex={-1} className="container-page flex-1 pb-20 pt-8 outline-none sm:pt-10">
        <NoticeBar />
        {children}
      </main>
    </>
  );
}

/** A page's content; it rises in when the page opens. */
export function Page({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cn("page-enter", className)}>{children}</div>;
}

/** The page's h1 (focused on navigation), its lead, and actions beside it. */
export function PageHeader({
  title,
  lead,
  actions,
  back,
  badge,
}: {
  title: string;
  lead?: ReactNode;
  actions?: ReactNode;
  back?: { href: string; label: string };
  badge?: ReactNode;
}) {
  return (
    <div className="mb-8 grid gap-4 sm:mb-10 md:flex md:items-end md:justify-between md:gap-8">
      <div className="grid min-w-0 gap-2">
        {back && (
          <Link
            href={back.href}
            className="group -ml-0.5 inline-flex w-fit items-center gap-1 rounded-sm text-[13px] font-medium text-muted-foreground transition-colors duration-150 hover:text-foreground"
          >
            <ChevronLeftIcon className="size-3.5 transition-transform duration-150 group-hover:-translate-x-0.5" aria-hidden="true" />
            {back.label}
          </Link>
        )}
        <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
          <h1 tabIndex={-1} data-page-title="" className="min-w-0 text-2xl font-semibold leading-tight tracking-tight [overflow-wrap:anywhere]">
            {title}
          </h1>
          {badge}
        </div>
        {lead && <div className="max-w-2xl text-[15px] leading-relaxed text-muted-foreground">{lead}</div>}
      </div>
      {actions && <div className="flex shrink-0 flex-wrap gap-2">{actions}</div>}
    </div>
  );
}

/** A section inside a page: an h2, an optional lead, then the content. */
export function Section({
  title,
  lead,
  aside,
  children,
  className,
}: {
  title: string;
  lead?: ReactNode;
  aside?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  const id = useId();
  return (
    <section aria-labelledby={id} className={cn("mt-12 grid gap-4 first:mt-0", className)}>
      <div className="flex flex-wrap items-end justify-between gap-x-6 gap-y-2">
        <div className="grid max-w-2xl gap-1">
          <h2 id={id} className="text-base font-semibold tracking-tight">
            {title}
          </h2>
          {lead && <p className="text-sm leading-relaxed text-muted-foreground">{lead}</p>}
        </div>
        {aside}
      </div>
      {children}
    </section>
  );
}
