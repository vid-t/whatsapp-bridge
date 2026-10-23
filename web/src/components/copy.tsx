import { CheckIcon, CopyIcon } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";
import { Button } from "./ui/button";
import { cn } from "@/lib/utils";

async function writeClipboard(text: string) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Plain-http bridges have no async clipboard: copy through a selection.
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.className = "fixed -left-[9999px] top-0 opacity-0";
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch {
      ok = false;
    }
    ta.remove();
    return ok;
  }
}

type CopyState = "idle" | "copied" | "failed";

function useCopy(text: string) {
  const [state, setState] = useState<CopyState>("idle");
  const timer = useRef(0);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const copy = async () => {
    setState((await writeClipboard(text)) ? "copied" : "failed");
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setState("idle"), 1800);
  };
  return { state, copy };
}

/** Two icons stacked; the visible one cross-blurs into the other. */
function SwapIcon({ done }: { done: boolean }) {
  return (
    <span className="relative grid size-4 place-items-center" aria-hidden="true">
      <CopyIcon className="swap-icon absolute size-4" data-on={!done} />
      <CheckIcon className="swap-icon absolute size-4 text-success-foreground" data-on={done} />
    </span>
  );
}

export function CopyButton({
  text,
  label = "Copy",
  describedBy,
  variant = "outline",
  size = "sm",
  className,
}: {
  text: string;
  label?: string;
  describedBy?: string;
  variant?: "outline" | "ghost" | "default";
  size?: "sm" | "default";
  className?: string;
}) {
  const { state, copy } = useCopy(text);
  return (
    <Button type="button" variant={variant} size={size} className={className} onClick={() => void copy()} aria-describedby={describedBy}>
      <SwapIcon done={state === "copied"} />
      <span aria-live="polite">{state === "copied" ? "Copied" : state === "failed" ? "Select and copy" : label}</span>
    </Button>
  );
}

/** A labelled value on one line with Copy: an address or a key. */
export function CopyField({ label, value, className }: { label: string; value: string; className?: string }) {
  const id = useId();
  return (
    <div className={cn("grid gap-2", className)}>
      <span id={id} className="text-sm font-medium">
        {label}
      </span>
      <div className="flex min-w-0 items-center gap-2 rounded-lg border bg-subtle p-1.5 pl-3.5">
        <code aria-labelledby={id} className="min-w-0 flex-1 truncate text-[13px]">
          {value}
        </code>
        <CopyButton text={value} describedBy={id} />
      </div>
    </div>
  );
}

/** Terminal commands, with one Copy for all of them. */
export function CodeBlock({ code, label }: { code: string; label: string }) {
  return (
    <div className="group relative overflow-hidden rounded-lg border bg-subtle" role="group" aria-label={label}>
      <pre tabIndex={0} className="overflow-x-auto px-4 py-3.5 pr-32 text-[13px] leading-relaxed text-foreground">
        {code.split("\n").map((line, i) => (
          <span key={i} className="block">
            <span className="select-none text-muted-foreground" aria-hidden="true">
              ${" "}
            </span>
            {line}
          </span>
        ))}
      </pre>
      <div className="absolute right-2 top-2">
        <CopyButton text={code} label={code.includes("\n") ? "Copy commands" : "Copy command"} variant="outline" />
      </div>
    </div>
  );
}
