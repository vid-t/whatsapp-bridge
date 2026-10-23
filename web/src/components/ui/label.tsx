import type { ComponentProps } from "react";
import { cn } from "@/lib/utils";

function Label({ className, ...props }: ComponentProps<"label">) {
  return (
    <label
      data-slot="label"
      className={cn("flex select-none items-center gap-2 text-sm font-medium leading-none peer-disabled:opacity-50", className)}
      {...props}
    />
  );
}

export { Label };
