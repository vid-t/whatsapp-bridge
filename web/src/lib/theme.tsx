// Light, dark or the system's choice. The choice is kept in localStorage; a
// small script in <head> (vite.config.ts) applies it before the first paint,
// and this provider keeps <html> in step afterwards.
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { THEME_KEY } from "./theme-key";

export type Theme = "light" | "dark" | "system";

interface ThemeApi {
  theme: Theme;
  /** What is on screen now. */
  resolved: "light" | "dark";
  setTheme: (t: Theme) => void;
}

const Ctx = createContext<ThemeApi | null>(null);
const media = () => window.matchMedia("(prefers-color-scheme: dark)");
export const reducedMotion = () => window.matchMedia("(prefers-reduced-motion: reduce)").matches;

function stored(): Theme {
  try {
    const v = localStorage.getItem(THEME_KEY);
    return v === "light" || v === "dark" ? v : "system";
  } catch {
    return "system";
  }
}

function apply(dark: boolean) {
  const root = document.documentElement;
  root.classList.toggle("dark", dark);
  root.classList.toggle("light", !dark);
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setState] = useState<Theme>(stored);
  const [systemDark, setSystemDark] = useState(() => media().matches);
  const resolved = theme === "system" ? (systemDark ? "dark" : "light") : theme;

  useEffect(() => {
    const m = media();
    const on = () => setSystemDark(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);

  useEffect(() => apply(resolved === "dark"), [resolved]);

  const setTheme = useCallback((t: Theme) => {
    try {
      if (t === "system") localStorage.removeItem(THEME_KEY);
      else localStorage.setItem(THEME_KEY, t);
    } catch {
      /* private mode: the choice lasts for this page */
    }
    const dark = t === "dark" || (t === "system" && media().matches);
    const swap = () => {
      apply(dark);
      setState(t);
    };
    // A short cross-fade between the two palettes, where the browser can.
    const doc = document as Document & { startViewTransition?: (cb: () => void) => unknown };
    if (doc.startViewTransition && !reducedMotion() && document.documentElement.classList.contains("dark") !== dark) {
      doc.startViewTransition(swap);
    } else swap();
  }, []);

  const value = useMemo(() => ({ theme, resolved, setTheme }) as ThemeApi, [theme, resolved, setTheme]);
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useTheme() {
  const v = useContext(Ctx);
  if (!v) throw new Error("useTheme outside ThemeProvider");
  return v;
}
