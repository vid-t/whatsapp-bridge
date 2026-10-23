import type { ReactNode } from "react";
import type { Tone } from "@/lib/format";
import { cn } from "@/lib/utils";

/** A status: a coloured dot and a word, never colour alone. `live` pulses the dot. */
export function Status({ tone, live, children, className }: { tone: Tone; live?: boolean; children: ReactNode; className?: string }) {
  return (
    <span className={cn("inline-flex items-center gap-2 whitespace-nowrap text-[13px] font-medium", className)} data-tone={tone}>
      <span className="status-dot" data-tone={tone} data-live={live || undefined} aria-hidden="true" />
      <span className={cn(tone === "off" && "text-muted-foreground")}>{children}</span>
    </span>
  );
}
