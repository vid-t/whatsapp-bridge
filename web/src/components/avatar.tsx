import type { ReactNode } from "react";
import { cn, initials } from "@/lib/utils";

/** Initials in a neutral tile: round for a person, rounded square for a group. */
export function Avatar({
  name,
  group,
  size = "md",
  icon,
  className,
}: {
  name: string;
  group?: boolean;
  size?: "sm" | "md";
  icon?: ReactNode;
  className?: string;
}) {
  return (
    <span
      aria-hidden="true"
      className={cn(
        "grid shrink-0 select-none place-items-center bg-muted font-medium tracking-tight text-muted-foreground ring-1 ring-inset ring-border",
        size === "md" ? "size-8 text-[11px]" : "size-6 text-[10px]",
        group ? "rounded-lg" : "rounded-full",
        "[&_svg]:size-3.5",
        className,
      )}
    >
      {icon ?? initials(name)}
    </span>
  );
}
