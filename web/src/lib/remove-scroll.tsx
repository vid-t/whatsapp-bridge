// A stand-in for react-remove-scroll, which Radix dialogs and menus use to
// stop the page scrolling behind them. The original injects a <style>
// element at runtime, which the bridge's CSP (style-src 'self') refuses.
// This one sets data-scroll-locked on <html>; styles.css turns that into
// overflow: hidden. vite.config.ts aliases the package to this file.
import { forwardRef, useEffect, type ElementType, type ReactNode } from "react";

let locks = 0;

function lock() {
  locks++;
  if (locks === 1) document.documentElement.setAttribute("data-scroll-locked", "");
}
function unlock() {
  locks = Math.max(0, locks - 1);
  if (locks === 0) document.documentElement.removeAttribute("data-scroll-locked");
}

interface Props {
  as?: ElementType;
  enabled?: boolean;
  children?: ReactNode;
  // Accepted for compatibility; the lock does not need them.
  allowPinchZoom?: boolean;
  shards?: unknown[];
  noIsolation?: boolean;
  inert?: boolean;
  removeScrollBar?: boolean;
  gapMode?: string;
  noRelative?: boolean;
  forwardProps?: boolean;
  [key: string]: unknown;
}

const Lock = forwardRef<HTMLElement, Props>(function RemoveScroll(
  {
    as: As = "div",
    enabled = true,
    allowPinchZoom: _a,
    shards: _s,
    noIsolation: _n,
    inert: _i,
    removeScrollBar: _r,
    gapMode: _g,
    noRelative: _nr,
    forwardProps: _f,
    ...rest
  },
  ref,
) {
  useEffect(() => {
    if (!enabled) return;
    lock();
    return unlock;
  }, [enabled]);
  const Comp = As as ElementType;
  return <Comp ref={ref} {...rest} />;
});

export const RemoveScroll = Object.assign(Lock, {
  classNames: { fullWidth: "width-before-scroll-bar", zeroRight: "right-scroll-bar-position" },
});
export default RemoveScroll;
