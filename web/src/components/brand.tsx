import { PRODUCT } from "@/lib/product";
import { cn } from "@/lib/utils";

/** The product name, set in the header weight. */
export function Wordmark({ className }: { className?: string }) {
  return <span className={cn("inline-flex items-center text-[15px] font-semibold leading-none tracking-tight", className)}>{PRODUCT}</span>;
}
