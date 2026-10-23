import { ChevronDownIcon, EyeIcon, EyeOffIcon, HourglassIcon, Loader2Icon, MailCheckIcon, TriangleAlertIcon } from "lucide-react";
import { Fragment, useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { FieldError, useSecondsLeft } from "@/components/feedback";
import { Page } from "@/components/shell";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ApiError, errorText, waitText } from "@/lib/api";
import { useAuth } from "@/lib/auth";
import { mmss } from "@/lib/format";
import { PRODUCT } from "@/lib/product";
import { Link, navigate, safeNext, useLocation, useTitle } from "@/lib/router";
import { cn, enc } from "@/lib/utils";

const EMAIL_KEY = "bridge.signin.email";
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

type LinkError = "link_expired" | "link_used" | "link_invalid";
const LINK_ERRORS: Record<LinkError, { title: string; lead: string }> = {
  link_expired: { title: "This link has expired", lead: "Sign-in links work for 15 minutes. Ask for a new one below." },
  link_used: { title: "This link was already used", lead: "Each link signs in once. Ask for a new one below." },
  link_invalid: { title: "This link does not work", lead: "It may be cut off in the email. Ask for a new one below." },
};

/* The providers' own marks, on their own sign-in buttons only. */
function GoogleLogo() {
  return (
    <svg viewBox="0 0 18 18" className="size-[18px]" aria-hidden="true" focusable="false">
      <path fill="#4285F4" d="M17.64 9.2c0-.64-.06-1.25-.16-1.84H9v3.48h4.84a4.14 4.14 0 0 1-1.8 2.72v2.26h2.92c1.7-1.57 2.68-3.88 2.68-6.62Z" />
      <path fill="#34A853" d="M9 18c2.43 0 4.47-.8 5.96-2.18l-2.92-2.26c-.8.54-1.84.86-3.04.86-2.34 0-4.32-1.58-5.03-3.7H.96v2.33A9 9 0 0 0 9 18Z" />
      <path fill="#FBBC05" d="M3.97 10.72A5.4 5.4 0 0 1 3.68 9c0-.6.1-1.18.29-1.72V4.95H.96A9 9 0 0 0 0 9c0 1.45.35 2.83.96 4.05l3.01-2.33Z" />
      <path fill="#EA4335" d="M9 3.58c1.32 0 2.5.45 3.44 1.35l2.58-2.58A9 9 0 0 0 .96 4.95l3.01 2.33C4.68 5.16 6.66 3.58 9 3.58Z" />
    </svg>
  );
}

function MicrosoftLogo() {
  return (
    <svg viewBox="0 0 18 18" className="size-[18px]" aria-hidden="true" focusable="false">
      <path fill="#F25022" d="M0 0h8.5v8.5H0z" />
      <path fill="#7FBA00" d="M9.5 0H18v8.5H9.5z" />
      <path fill="#00A4EF" d="M0 9.5h8.5V18H0z" />
      <path fill="#FFB900" d="M9.5 9.5H18V18H9.5z" />
    </svg>
  );
}

function EmailForm({ next, onSent }: { next: string; onSent: (email: string) => void }) {
  const { emailLink } = useAuth();
  const [email, setEmail] = useState(() => sessionStorage.getItem(EMAIL_KEY) ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [retryAt, setRetryAt] = useState(0);
  const wait = useSecondsLeft(retryAt);
  const input = useRef<HTMLInputElement>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const v = email.trim();
    if (!EMAIL_RE.test(v)) {
      setError("Enter an email address, like you@example.com.");
      input.current?.focus();
      return;
    }
    setBusy(true);
    setError("");
    try {
      await emailLink(v, next);
      sessionStorage.setItem(EMAIL_KEY, v);
      onSent(v);
    } catch (err) {
      if (err instanceof ApiError && err.status === 400) setError("That does not look like an email address.");
      else if (err instanceof ApiError && err.status === 429) {
        setError(`Too many sign-in requests. Try again in ${waitText(err.retryAfter ?? 60)}.`);
        setRetryAt(Date.now() + (err.retryAfter ?? 60) * 1000);
      } else setError(errorText(err));
      input.current?.focus();
    }
    setBusy(false);
  };

  return (
    <form onSubmit={submit} method="post" action="/auth/email/start" className="grid gap-3" noValidate>
      <input type="hidden" name="next" value={next} />
      <div className="grid gap-2">
        <Label htmlFor="signin-email">Email</Label>
        <Input
          ref={input}
          id="signin-email"
          name="email"
          type="email"
          inputMode="email"
          autoComplete="email"
          autoCapitalize="off"
          spellCheck={false}
          placeholder="you@example.com"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          aria-invalid={error ? true : undefined}
          aria-describedby={error ? "signin-email-error" : undefined}
        />
        {error && <FieldError id="signin-email-error">{error}</FieldError>}
      </div>
      <Button type="submit" disabled={busy || wait > 0} className="w-full">
        {busy && <Loader2Icon className="animate-spin" aria-hidden="true" />}
        {wait > 0 ? `Try again in ${mmss(wait)}` : busy ? "Sending…" : "Email me a sign-in link"}
      </Button>
    </form>
  );
}

function CodeForm({ next, primary }: { next: string; primary: boolean }) {
  const { signIn } = useAuth();
  const [code, setCode] = useState("");
  const [show, setShow] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [retryAt, setRetryAt] = useState(0);
  const wait = useSecondsLeft(retryAt);
  const input = useRef<HTMLInputElement>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!code.trim()) {
      setError("Enter the access code.");
      input.current?.focus();
      return;
    }
    setBusy(true);
    setError("");
    try {
      await signIn(code.trim());
      setCode("");
      navigate(next, { replace: true });
      return;
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) setError("That code is not right. Check it and try again.");
      else if (err instanceof ApiError && err.status === 409) setError("This bridge runs in open mode and needs no sign-in.");
      else if (err instanceof ApiError && err.status === 429) {
        setError("Too many tries. Wait a minute, then try again.");
        setRetryAt(Date.now() + (err.retryAfter ?? 60) * 1000);
      } else setError(errorText(err));
      input.current?.focus();
    }
    setBusy(false);
  };

  return (
    <form onSubmit={submit} className="grid gap-3" noValidate>
      {/* A username field lets password managers file the code under this bridge. */}
      <input type="text" name="username" autoComplete="username" value={PRODUCT} readOnly hidden />
      <div className="grid gap-2">
        <Label htmlFor="access-code">Access code</Label>
        <div className="relative">
          <Input
            ref={input}
            id="access-code"
            name="code"
            type={show ? "text" : "password"}
            autoComplete="current-password"
            spellCheck={false}
            autoCapitalize="off"
            className="pr-11 font-mono"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? "access-code-error" : undefined}
          />
          <button
            type="button"
            onClick={() => setShow((v) => !v)}
            aria-pressed={show}
            aria-label={show ? "Hide the code" : "Show the code"}
            className="absolute right-1 top-1/2 grid size-8 -translate-y-1/2 cursor-pointer place-items-center rounded-md text-muted-foreground transition-colors duration-150 hover:bg-accent hover:text-foreground"
          >
            <span className="relative grid size-4 place-items-center" aria-hidden="true">
              <EyeIcon className="swap-icon absolute size-4" data-on={!show} />
              <EyeOffIcon className="swap-icon absolute size-4" data-on={show} />
            </span>
          </button>
        </div>
        {error && <FieldError id="access-code-error">{error}</FieldError>}
      </div>
      <Button type="submit" variant={primary ? "default" : "outline"} disabled={busy || wait > 0} className="w-full">
        {busy && <Loader2Icon className="animate-spin" aria-hidden="true" />}
        {wait > 0 ? `Try again in ${mmss(wait)}` : busy ? "Signing in…" : "Sign in"}
      </Button>
    </form>
  );
}

function Or() {
  return (
    <p className="flex items-center gap-3 text-xs text-muted-foreground" aria-hidden="true">
      <span className="h-px flex-1 bg-border" />
      or
      <span className="h-px flex-1 bg-border" />
    </p>
  );
}

function Shell({ title, lead, icon, children }: { title: string; lead?: ReactNode; icon?: ReactNode; children?: ReactNode }) {
  return (
    <Page className="mx-auto grid w-full max-w-[25rem] gap-7 pt-2 sm:pt-10">
      <div className="grid justify-items-center gap-3 text-center">
        {icon}
        <h1 tabIndex={-1} data-page-title="" className={cn("text-2xl font-semibold tracking-tight [overflow-wrap:anywhere]", icon && "mt-2")}>
          {title}
        </h1>
        {lead && <div className="text-[15px] leading-relaxed text-muted-foreground">{lead}</div>}
      </div>
      {children}
    </Page>
  );
}

const Card = ({ children, className }: { children: ReactNode; className?: string }) => (
  <div className={cn("grid gap-5 rounded-xl border bg-card p-5 shadow-sm shadow-black/[0.03] sm:p-6", className)}>{children}</div>
);

const IconTile = ({ children }: { children: ReactNode }) => (
  <div className="pop-enter grid size-12 place-items-center rounded-full border bg-subtle text-foreground [&_svg]:size-5">{children}</div>
);

export function SignInPage() {
  const auth = useAuth();
  const { search } = useLocation();
  const next = safeNext(search.get("next"));
  const sent = search.get("sent") === "1";
  const pending = search.get("status") === "pending";
  const linkError = search.get("error") as LinkError | null;
  const [sentTo, setSentTo] = useState(() => (sent ? (sessionStorage.getItem(EMAIL_KEY) ?? "") : ""));
  const [helpOpen, setHelpOpen] = useState(false);
  const where = auth.name || "this bridge";

  useTitle(sent ? "Check your email" : pending ? "Waiting for approval" : "Sign in");

  // Signed in already (or no sign-in needed): go where the browser was headed.
  useEffect(() => {
    if (auth.status === "ready" && auth.authenticated) navigate(next, { replace: true });
  }, [auth.status, auth.authenticated, next]);

  const back = () => {
    const p = new URLSearchParams();
    if (next !== "/") p.set("next", next);
    const s = p.toString();
    return "/signin" + (s ? "?" + s : "");
  };

  if (sent) {
    return (
      <Shell
        icon={
          <IconTile>
            <MailCheckIcon aria-hidden="true" />
          </IconTile>
        }
        title="Check your email"
        lead={
          sentTo ? (
            <>
              If <strong className="font-medium text-foreground">{sentTo}</strong> may sign in to {where}, a sign-in link is on its way. Open it
              in this browser. It works once, for 15 minutes.
            </>
          ) : (
            <>If your address may sign in to {where}, a sign-in link is on its way. Open it in this browser. It works once, for 15 minutes.</>
          )
        }
      >
        <Card className="gap-0 p-0 sm:p-0">
          <Collapsible open={helpOpen} onOpenChange={setHelpOpen}>
            <CollapsibleTrigger className="flex w-full cursor-pointer items-center justify-between rounded-xl px-5 py-4 text-left text-sm font-medium transition-colors duration-150 hover:bg-muted/40">
              No email yet?
              <ChevronDownIcon
                className={cn("size-4 text-muted-foreground transition-transform duration-200 ease-out-expo", helpOpen && "rotate-180")}
                aria-hidden="true"
              />
            </CollapsibleTrigger>
            <CollapsibleContent>
              <div className="grid gap-2 px-5 pb-5 text-sm leading-relaxed text-muted-foreground">
                <p>Check the spam folder. The link opens a page with one button, Continue, which signs this browser in.</p>
                <p>If you are new here, a member approves your request first. You get the link once they do.</p>
              </div>
            </CollapsibleContent>
          </Collapsible>
        </Card>
        <Button asChild variant="outline" className="w-full">
          <Link href={back()} onClick={() => setSentTo("")}>
            Use a different email
          </Link>
        </Button>
      </Shell>
    );
  }

  if (pending) {
    return (
      <Shell
        icon={
          <IconTile>
            <HourglassIcon aria-hidden="true" />
          </IconTile>
        }
        title="Waiting for approval"
        lead="A member of this bridge has your request. You get an email as soon as you can sign in."
      >
        <Button asChild variant="outline" className="w-full">
          <Link href={back()}>Back to sign in</Link>
        </Button>
      </Shell>
    );
  }

  const onSent = (email: string) => {
    setSentTo(email);
    navigate("/signin?sent=1" + (next !== "/" ? "&next=" + enc(next) : ""));
  };
  const err = linkError && LINK_ERRORS[linkError] ? LINK_ERRORS[linkError] : null;
  const hasEmail = auth.providers.includes("email");
  const methods: ReactNode[] = [];
  for (const p of auth.providers) {
    if (p === "code") methods.push(<CodeForm key="code" next={next} primary={!hasEmail} />);
    if (p === "email") methods.push(<EmailForm key="email" next={next} onSent={onSent} />);
  }
  const sso = auth.providers.filter((p) => p === "google" || p === "microsoft");
  if (sso.length) {
    methods.push(
      <div key="sso" className="grid gap-2">
        {sso.map((p) => (
          <Button key={p} asChild variant="outline" className="w-full">
            <a href={`/auth/${p}/start?next=${enc(next)}`}>
              {p === "google" ? <GoogleLogo /> : <MicrosoftLogo />}
              Continue with {p === "google" ? "Google" : "Microsoft"}
            </a>
          </Button>
        ))}
      </div>,
    );
  }

  return (
    <Shell
      icon={
        err ? (
          <IconTile>
            <TriangleAlertIcon aria-hidden="true" />
          </IconTile>
        ) : undefined
      }
      title={err ? err.title : auth.name ? `Sign in to ${auth.name}` : "Sign in"}
      lead={err?.lead}
    >
      <Card>
        {methods.length === 0 ? (
          <p className="text-center text-sm text-muted-foreground">This bridge has no sign-in method switched on. Ask whoever runs it.</p>
        ) : (
          methods.map((m, i) => (
            <Fragment key={i}>
              {i > 0 && <Or />}
              {m}
            </Fragment>
          ))
        )}
      </Card>
    </Shell>
  );
}
