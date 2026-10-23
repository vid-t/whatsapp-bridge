// Page-level notice: errors are cleared by the next successful load;
// warnings stay until replaced or dismissed.
import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from "react";

export type NoticeKind = "err" | "warn";
export interface Notice {
  text: string;
  kind: NoticeKind;
}

interface NoticeApi {
  notice: Notice | null;
  notify: (text: string, kind?: NoticeKind) => void;
  clearError: () => void;
  clear: () => void;
}

const Ctx = createContext<NoticeApi | null>(null);

export function NoticeProvider({ children }: { children: ReactNode }) {
  const [notice, setState] = useState<Notice | null>(null);
  const ref = useRef<Notice | null>(null);
  const set = useCallback((n: Notice | null) => {
    ref.current = n;
    setState(n);
  }, []);
  const notify = useCallback((text: string, kind: NoticeKind = "err") => set(text ? { text, kind } : null), [set]);
  const clearError = useCallback(() => {
    if (ref.current?.kind === "err") set(null);
  }, [set]);
  const clear = useCallback(() => set(null), [set]);
  const value = useMemo(() => ({ notice, notify, clearError, clear }), [notice, notify, clearError, clear]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useNotice() {
  const v = useContext(Ctx);
  if (!v) throw new Error("useNotice outside NoticeProvider");
  return v;
}
