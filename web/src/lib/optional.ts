// Lists from endpoints that newer bridges add (OAuth grants, members). A 404
// means this bridge does not have the feature.
import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, api, errorText } from "./api";

export interface OptionalList<T> {
  /** loading (first answer pending); off (404 or disabled); ready; error (one-line retry). */
  status: "loading" | "off" | "ready" | "error";
  list: T[];
  error: string;
  reload: () => Promise<void>;
  set: (fn: (list: T[]) => T[]) => void;
}

export function useOptionalList<T>(path: string, enabled = true): OptionalList<T> {
  const [status, setStatus] = useState<OptionalList<T>["status"]>(enabled ? "loading" : "off");
  const [list, setList] = useState<T[]>([]);
  const [error, setError] = useState("");
  const gen = useRef(0);
  const reload = useCallback(async () => {
    if (!enabled) {
      setStatus("off");
      return;
    }
    const g = ++gen.current;
    try {
      const l = await api.get<T[]>(path);
      if (g !== gen.current) return;
      setList(Array.isArray(l) ? l : []);
      setStatus("ready");
      setError("");
    } catch (err) {
      if (g !== gen.current) return;
      if (err instanceof ApiError && err.status === 404) setStatus("off");
      else {
        setError(errorText(err));
        setStatus("error");
      }
    }
  }, [path, enabled]);
  useEffect(() => {
    void reload();
  }, [reload]);
  const set = useCallback((fn: (l: T[]) => T[]) => setList(fn), []);
  return { status, list, error, reload, set };
}
