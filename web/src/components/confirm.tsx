import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "./ui/alert-dialog";
import { buttonVariants } from "./ui/button";

export interface ConfirmOptions {
  title: string;
  description: ReactNode;
  confirmLabel: string;
  destructive?: boolean;
}

type ConfirmFn = (o: ConfirmOptions) => Promise<boolean>;
const ConfirmCtx = createContext<ConfirmFn | null>(null);

/** confirm() opens an alert dialog and resolves true or false; focus goes back where it was. */
export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [opts, setOpts] = useState<ConfirmOptions | null>(null);
  const [open, setOpen] = useState(false);
  const resolver = useRef<((v: boolean) => void) | null>(null);
  const opener = useRef<HTMLElement | null>(null);

  const confirm = useCallback<ConfirmFn>((o) => {
    opener.current = document.activeElement as HTMLElement | null;
    resolver.current?.(false);
    setOpts(o);
    setOpen(true);
    return new Promise<boolean>((res) => {
      resolver.current = res;
    });
  }, []);

  const settle = (v: boolean) => {
    resolver.current?.(v);
    resolver.current = null;
    setOpen(false);
  };

  return (
    <ConfirmCtx.Provider value={confirm}>
      {children}
      <AlertDialog open={open} onOpenChange={(o) => !o && settle(false)}>
        {opts && (
          <AlertDialogContent
            onCloseAutoFocus={(e) => {
              e.preventDefault();
              opener.current?.focus?.({ preventScroll: true });
            }}
          >
            <AlertDialogHeader>
              <AlertDialogTitle>{opts.title}</AlertDialogTitle>
              <AlertDialogDescription asChild>
                <div>{opts.description}</div>
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel onClick={() => settle(false)}>Cancel</AlertDialogCancel>
              <AlertDialogAction
                className={buttonVariants({ variant: opts.destructive ? "destructive" : "default" })}
                onClick={() => settle(true)}
              >
                {opts.confirmLabel}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        )}
      </AlertDialog>
    </ConfirmCtx.Provider>
  );
}

export function useConfirm() {
  const v = useContext(ConfirmCtx);
  if (!v) throw new Error("useConfirm outside ConfirmProvider");
  return v;
}
