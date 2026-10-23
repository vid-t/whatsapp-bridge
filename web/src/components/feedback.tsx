import { CircleAlertIcon, RotateCwIcon } from "lucide-react";
import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Alert } from "./ui/alert";
import { Button } from "./ui/button";
import { Skeleton } from "./ui/skeleton";
import { cn } from "@/lib/utils";

export function FieldError({ id, children }: { id?: string; children: ReactNode }) {
  return (
    <p id={id} className="fade-enter flex items-start gap-1.5 text-[13px] font-medium text-destructive" role="alert">
      <CircleAlertIcon className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
      <span>{children}</span>
    </p>
  );
}

export function FieldHint({ id, children }: { id?: string; children: ReactNode }) {
  return (
    <p id={id} className="text-[13px] text-muted-foreground">
      {children}
    </p>
  );
}

/** A failed load: one line and a Retry button. */
export function RetryLine({ text, onRetry }: { text: string; onRetry: () => void }) {
  return (
    <Alert variant="destructive" role="alert" className="fade-enter flex items-center justify-between gap-3">
      <span className="flex items-start gap-2.5">
        <CircleAlertIcon className="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden="true" />
        <span>{text}</span>
      </span>
      <Button type="button" variant="outline" size="sm" onClick={onRetry}>
        <RotateCwIcon aria-hidden="true" />
        Retry
      </Button>
    </Alert>
  );
}

/** Placeholder rows while a list loads; announced once as `label`. */
export function LoadingRows({ label, rows = 3, avatar = true, className }: { label: string; rows?: number; avatar?: boolean; className?: string }) {
  return (
    <div role="status" className={cn("overflow-hidden rounded-lg border bg-card", className)}>
      <span className="sr-only">{label}</span>
      <div className="h-9 border-b bg-subtle" />
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="flex h-14 items-center gap-3 border-b px-4 last:border-0">
          {avatar && <Skeleton className="size-8 rounded-full" />}
          <div className="grid flex-1 gap-2">
            <Skeleton className="h-3" style={{ width: `${[38, 26, 32, 22, 30][i % 5]}%` }} />
            <Skeleton className="h-2.5 opacity-70" style={{ width: `${[18, 24, 14, 20, 16][i % 5]}%` }} />
          </div>
          <Skeleton className="hidden h-3 w-16 sm:block" />
        </div>
      ))}
    </div>
  );
}

/** A quiet empty state inside a bordered panel. */
export function Empty({ title, children, icon, className }: { title?: string; children?: ReactNode; icon?: ReactNode; className?: string }) {
  return (
    <div className={cn("fade-enter grid justify-items-center gap-2 rounded-lg border border-dashed px-6 py-10 text-center", className)}>
      {icon && <div className="mb-1 grid size-10 place-items-center rounded-full bg-muted text-muted-foreground [&_svg]:size-5">{icon}</div>}
      {title && <p className="font-medium">{title}</p>}
      {children && <div className="max-w-sm text-sm text-muted-foreground">{children}</div>}
    </div>
  );
}

/** useSecondsLeft ticks down to a deadline (ms epoch); 0 when there is none. */
export function useSecondsLeft(until: number) {
  const calc = useCallback(() => Math.max(0, Math.ceil((until - Date.now()) / 1000)), [until]);
  const [s, setS] = useState(calc);
  useEffect(() => {
    setS(calc());
    if (!until) return;
    const t = window.setInterval(() => {
      const v = calc();
      setS(v);
      if (!v) clearInterval(t);
    }, 1000);
    return () => clearInterval(t);
  }, [until, calc]);
  return s;
}
