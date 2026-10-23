// One EventSource for the whole app. The browser retries by itself, except
// after an HTTP error (a proxy's 502 during a restart, or a 503 when too many
// streams are open); then it gives up and we reconnect with backoff. Pages
// resync after a reconnect to catch missed events.
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, type ReactNode } from "react";
import type { BridgeEvent, EventType } from "./types";

type Listener = (ev: BridgeEvent) => void;

interface Bus {
  subscribe: (fn: Listener) => () => void;
  onResync: (fn: () => void) => () => void;
}

const TYPES: EventType[] = ["state", "pair", "message", "chats", "log"];
const Ctx = createContext<Bus | null>(null);

export function EventsProvider({ enabled, onReconnect, children }: {
  enabled: boolean;
  onReconnect?: () => void;
  children: ReactNode;
}) {
  const listeners = useRef(new Set<Listener>());
  const resyncs = useRef(new Set<() => void>());
  const reconnectRef = useRef(onReconnect);
  reconnectRef.current = onReconnect;

  useEffect(() => {
    if (!enabled) return;
    let es: EventSource | null = null;
    let timer = 0;
    let opened = false;
    let delay = 3000;
    let stopped = false;

    const connect = () => {
      if (stopped) return;
      es = new EventSource("/api/events");
      es.addEventListener("open", () => {
        delay = 3000;
        if (opened) {
          reconnectRef.current?.();
          resyncs.current.forEach((fn) => fn());
        }
        opened = true;
      });
      es.addEventListener("error", () => {
        if (!es || es.readyState !== EventSource.CLOSED) return;
        es.close();
        timer = window.setTimeout(connect, delay);
        delay = Math.min(delay * 2, 30000);
      });
      for (const type of TYPES) {
        es.addEventListener(type, (m) => {
          let ev: BridgeEvent;
          try {
            ev = JSON.parse((m as MessageEvent<string>).data) as BridgeEvent;
          } catch {
            return;
          }
          listeners.current.forEach((fn) => fn(ev));
        });
      }
    };
    connect();
    return () => {
      stopped = true;
      clearTimeout(timer);
      es?.close();
    };
  }, [enabled]);

  const subscribe = useCallback((fn: Listener) => {
    listeners.current.add(fn);
    return () => void listeners.current.delete(fn);
  }, []);
  const onResync = useCallback((fn: () => void) => {
    resyncs.current.add(fn);
    return () => void resyncs.current.delete(fn);
  }, []);
  const bus = useMemo<Bus>(() => ({ subscribe, onResync }), [subscribe, onResync]);
  return <Ctx.Provider value={bus}>{children}</Ctx.Provider>;
}

/**
 * useBridgeEvents calls onEvent for every server event and onResync after
 * the stream reconnects. The latest callbacks are used without resubscribing.
 */
export function useBridgeEvents(onEvent: Listener, onResync?: () => void) {
  const bus = useContext(Ctx);
  const ev = useRef(onEvent);
  const rs = useRef(onResync);
  ev.current = onEvent;
  rs.current = onResync;
  const subscribe = bus?.subscribe;
  const resync = bus?.onResync;
  useEffect(() => {
    if (!subscribe || !resync) return;
    const a = subscribe((e) => ev.current(e));
    const b = resync(() => rs.current?.());
    return () => {
      a();
      b();
    };
  }, [subscribe, resync]);
}
