import * as TabsPrimitive from "@radix-ui/react-tabs";
import { type ComponentProps } from "react";
import { useGlide } from "@/lib/glide";
import { cn } from "@/lib/utils";

const Tabs = TabsPrimitive.Root;

/** A segmented tab list whose highlight glides to the active tab. */
function TabsList({ className, children, value, ...props }: ComponentProps<typeof TabsPrimitive.List> & { value: string }) {
  const g = useGlide<HTMLDivElement>('[data-state="active"]', value);
  return (
    <TabsPrimitive.List
      ref={g.ref}
      data-slot="tabs-list"
      className={cn("relative inline-flex h-9 w-fit items-center rounded-lg bg-muted p-[3px] text-muted-foreground", className)}
      {...props}
    >
      <span
        aria-hidden="true"
        data-ready={g.ready || undefined}
        className="tab-glide pointer-events-none absolute left-0 top-0 rounded-md border border-border bg-background shadow-xs dark:border-input dark:bg-input/40"
        style={g.style}
      />
      {children}
    </TabsPrimitive.List>
  );
}

function TabsTrigger({ className, ...props }: ComponentProps<typeof TabsPrimitive.Trigger>) {
  return (
    <TabsPrimitive.Trigger
      data-slot="tabs-trigger"
      className={cn(
        "relative z-10 inline-flex h-full flex-1 cursor-pointer items-center justify-center gap-1.5 whitespace-nowrap rounded-md px-3 text-sm font-medium transition-colors duration-150",
        "hover:text-foreground data-[state=active]:text-foreground disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0",
        className,
      )}
      {...props}
    />
  );
}

function TabsContent({ className, ...props }: ComponentProps<typeof TabsPrimitive.Content>) {
  return <TabsPrimitive.Content data-slot="tabs-content" className={cn("outline-none", className)} {...props} />;
}

export { Tabs, TabsContent, TabsList, TabsTrigger };
