import type { ComponentProps } from "react";
import { cn } from "@/lib/utils";

function Input({ className, type, ...props }: ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "flex h-9 w-full min-w-0 rounded-md border border-input bg-background px-3 py-1 text-base shadow-xs transition-[color,box-shadow,border-color] duration-150 outline-none max-sm:h-10 sm:text-sm",
        "placeholder:text-muted-foreground selection:bg-ring/25 dark:bg-input/20",
        "hover:border-border-strong focus-visible:border-ring focus-visible:outline-none focus-visible:ring-[3px] focus-visible:ring-ring/20",
        "aria-invalid:border-destructive aria-invalid:ring-destructive/15 aria-invalid:focus-visible:ring-[3px]",
        "disabled:cursor-not-allowed disabled:opacity-50 file:border-0 file:bg-transparent",
        className,
      )}
      {...props}
    />
  );
}

export { Input };
