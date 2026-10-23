import { CompassIcon } from "lucide-react";
import { useEffect } from "react";
import { RetryLine } from "@/components/feedback";
import { Page, PageHeader } from "@/components/shell";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PRODUCT } from "@/lib/product";
import { Link, useTitle } from "@/lib/router";

export function NotFoundPage() {
  useTitle("Not found");
  return (
    <Page className="grid min-h-[50vh] place-content-center justify-items-center text-center">
      <div className="mb-6 grid size-12 place-items-center rounded-full border bg-subtle text-muted-foreground">
        <CompassIcon className="size-5" aria-hidden="true" />
      </div>
      <h1 tabIndex={-1} data-page-title="" className="text-2xl font-semibold tracking-tight">
        Page not found
      </h1>
      <p className="mt-2 text-muted-foreground">The address may be old or mistyped.</p>
      <Button asChild variant="outline" className="mt-6">
        <Link href="/">Go to accounts</Link>
      </Button>
    </Page>
  );
}

/** While /api/me answers, and when it does not. */
export function Booting({ error, onRetry }: { error?: string; onRetry?: () => void }) {
  useEffect(() => {
    document.title = PRODUCT;
  }, []);
  if (error) {
    return (
      <Page>
        <PageHeader title="The bridge did not answer" lead="The page could not reach the bridge. It may be restarting." />
        <RetryLine text={error} onRetry={() => onRetry?.()} />
      </Page>
    );
  }
  return (
    <div role="status" className="fade-enter grid gap-3">
      <span className="sr-only">Loading…</span>
      <Skeleton className="h-8 w-48" />
      <Skeleton className="h-4 w-80 max-w-full opacity-70" />
      <Skeleton className="mt-8 h-48 w-full rounded-lg opacity-60" />
    </div>
  );
}
