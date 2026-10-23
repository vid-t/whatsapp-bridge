// Measures the selected child of a container, so a single indicator element
// can glide between tabs or segments instead of jumping.
import { useLayoutEffect, useRef, useState, type CSSProperties } from "react";

export interface Glide {
  style: CSSProperties;
  ready: boolean;
}

/**
 * useGlide returns a ref for the container and the indicator's style. `selector`
 * picks the selected child; `dep` changes whenever the selection does.
 */
export function useGlide<T extends HTMLElement>(selector: string, dep: unknown) {
  const ref = useRef<T>(null);
  const [box, setBox] = useState<{ x: number; y: number; w: number; h: number } | null>(null);
  const [ready, setReady] = useState(false);

  useLayoutEffect(() => {
    const root = ref.current;
    if (!root) return;
    const measure = () => {
      const el = root.querySelector<HTMLElement>(selector);
      if (!el) return setBox(null);
      setBox({ x: el.offsetLeft, y: el.offsetTop, w: el.offsetWidth, h: el.offsetHeight });
    };
    measure();
    // Glide only after the first placement, so the indicator never flies in from 0.
    const t = requestAnimationFrame(() => setReady(true));
    const ro = new ResizeObserver(measure);
    ro.observe(root);
    return () => {
      cancelAnimationFrame(t);
      ro.disconnect();
    };
  }, [selector, dep]);

  const style: CSSProperties = box
    ? { transform: `translate(${box.x}px, ${box.y}px)`, width: box.w, height: box.h, opacity: 1 }
    : { opacity: 0, width: 0, height: 0 };
  return { ref, style, ready, box } as const;
}
