import { useCallback, useEffect, useRef, useState } from "react";
import { api, errorText } from "./api";
import { useLater } from "./debounce";
import { useBridgeEvents } from "./events";
import type { SessionInfo } from "./types";

/**
 * useSessions keeps the account list current from the event stream. A failed
 * load is returned as `error`, for a one-line retry.
 */
export function useSessions() {
  const later = useLater();
  const [list, setList] = useState<SessionInfo[] | null>(null);
  const [error, setError] = useState("");
  const gen = useRef(0);
  const load = useCallback(async () => {
    const g = ++gen.current;
    try {
      const l = await api.get<SessionInfo[]>("/sessions");
      if (g !== gen.current) return;
      setList(l);
      setError("");
    } catch (err) {
      if (g === gen.current) setError("Could not load the accounts: " + errorText(err));
    }
  }, []);
  useEffect(() => {
    void load();
  }, [load]);
  useBridgeEvents(
    (ev) => {
      if (ev.type === "state") later("sessions", () => void load(), 100);
      else if (ev.type === "chats" || ev.type === "message") later("sessions", () => void load(), 1500);
    },
    () => void load(),
  );
  return { list, error, reload: load };
}
