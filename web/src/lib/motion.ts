// Small motion helpers. Everything here is skipped under prefers-reduced-motion.
import { useEffect, useRef, type CSSProperties, type RefObject } from "react";
import { reducedMotion } from "./theme";

/**
 * useFlash makes an element glow in the accent and fade back when `value`
 * changes after the first render: a status flips, a new message lands. It
 * uses the Web Animations API, which the CSP allows (no style attribute).
 */
export function useFlash<T extends HTMLElement>(value: unknown): RefObject<T | null> {
  const ref = useRef<T>(null);
  const first = useRef(true);
  const last = useRef(value);
  useEffect(() => {
    if (first.current) {
      first.current = false;
      last.current = value;
      return;
    }
    if (Object.is(last.current, value)) return;
    last.current = value;
    const el = ref.current;
    if (!el || reducedMotion() || typeof el.animate !== "function") return;
    const live = getComputedStyle(document.documentElement).getPropertyValue("--live").trim() || "oklch(0.55 0.2 257)";
    el.animate(
      [
        { backgroundColor: `color-mix(in oklch, ${live} 14%, transparent)` },
        { backgroundColor: `color-mix(in oklch, ${live} 14%, transparent)`, offset: 0.35 },
        { backgroundColor: "transparent" },
      ],
      { duration: 2200, easing: "ease-out" },
    );
  }, [value]);
  return ref;
}

/** Index for a staggered entrance, as the CSS variable --i. */
export const stagger = (i: number) => ({ "--i": i }) as CSSProperties;
