import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import type { ComponentProps } from "react";
import { cn } from "@/lib/utils";

const buttonVariants = cva(
  "relative inline-flex shrink-0 cursor-pointer select-none items-center justify-center gap-2 whitespace-nowrap rounded-md text-sm font-medium " +
    "transition-[color,background-color,border-color,box-shadow,transform,opacity] duration-150 ease-out active:scale-[0.98] " +
    "disabled:pointer-events-none disabled:opacity-50 aria-disabled:pointer-events-none aria-disabled:opacity-50 " +
    "[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  {
    variants: {
      variant: {
        default: "bg-primary text-primary-foreground shadow-xs hover:bg-primary/88",
        destructive: "bg-destructive-solid text-white shadow-xs hover:bg-destructive-solid/90",
        outline:
          "border border-input bg-background shadow-xs hover:border-border-strong hover:bg-accent hover:text-accent-foreground dark:bg-input/20 dark:hover:bg-input/40",
        "outline-destructive":
          "border border-input bg-background text-destructive shadow-xs hover:border-destructive/40 hover:bg-destructive/8 dark:bg-input/20 dark:hover:bg-destructive/15",
        secondary: "bg-secondary text-secondary-foreground hover:bg-secondary/75",
        ghost: "hover:bg-accent hover:text-accent-foreground dark:hover:bg-accent/60",
        link: "h-auto px-0 text-live underline-offset-4 hover:underline active:scale-100",
      },
      size: {
        default: "h-9 px-4 py-2 has-[>svg]:px-3 max-sm:h-10",
        sm: "h-8 gap-1.5 rounded-md px-3 text-[13px] has-[>svg]:px-2.5",
        lg: "h-10 rounded-md px-6 has-[>svg]:px-4",
        icon: "size-9 max-sm:size-10",
        "icon-sm": "size-8",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

function Button({
  className,
  variant,
  size,
  asChild = false,
  ...props
}: ComponentProps<"button"> & VariantProps<typeof buttonVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot : "button";
  return <Comp data-slot="button" className={cn(buttonVariants({ variant, size, className }))} {...props} />;
}

export { Button, buttonVariants };
