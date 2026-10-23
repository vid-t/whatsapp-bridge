import * as ToggleGroupPrimitive from "@radix-ui/react-toggle-group";
import type { ComponentProps } from "react";
import { useGlide } from "@/lib/glide";
import { cn } from "@/lib/utils";

/** A single-choice segmented control; the highlight glides to the chosen item. */
function Segmented({
  className,
  children,
  value,
  onValueChange,
  ...props
}: Omit<ComponentProps<typeof ToggleGroupPrimitive.Root>, "type" | "value" | "onValueChange" | "defaultValue"> & {
  value: string;
  onValueChange: (v: string) => void;
}) {
  const g = useGlide<HTMLDivElement>('[data-state="on"]', value);
  return (
    <ToggleGroupPrimitive.Root
      ref={g.ref}
      type="single"
      value={value}
      onValueChange={(v) => v && onValueChange(v)}
      data-slot="segmented"
      className={cn("relative inline-flex h-9 items-center rounded-lg bg-muted p-[3px] max-sm:h-10", className)}
      {...props}
    >
      <span
        aria-hidden="true"
        data-ready={g.ready || undefined}
        className="tab-glide pointer-events-none absolute left-0 top-0 rounded-md border border-border bg-background shadow-xs dark:border-input dark:bg-input/40"
        style={g.style}
      />
      {children}
    </ToggleGroupPrimitive.Root>
  );
}

function SegmentedItem({ className, ...props }: ComponentProps<typeof ToggleGroupPrimitive.Item>) {
  return (
    <ToggleGroupPrimitive.Item
      data-slot="segmented-item"
      className={cn(
        "relative z-10 inline-flex h-full cursor-pointer items-center gap-1.5 whitespace-nowrap rounded-md px-3 text-[13px] font-medium text-muted-foreground transition-colors duration-150",
        "hover:text-foreground data-[state=on]:text-foreground",
        className,
      )}
      {...props}
    />
  );
}

export { Segmented, SegmentedItem };
