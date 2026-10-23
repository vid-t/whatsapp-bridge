import { useEffect, useRef } from "react";

/**
 * useLater returns later(key, fn, ms): run fn once after ms; calls for the
 * same key while one is pending are absorbed.
 */
export function useLater() {
  const timers = useRef(new Map<string, number>());
  useEffect(() => {
    const t = timers.current;
    return () => {
      t.forEach((id) => clearTimeout(id));
      t.clear();
    };
  }, []);
  return useRef((key: string, fn: () => void, ms: number) => {
    const t = timers.current;
    if (t.has(key)) return;
    t.set(
      key,
      window.setTimeout(() => {
        t.delete(key);
        fn();
      }, ms),
    );
  }).current;
}
