// Sessions with a request in flight, and sessions whose Remove was refused
// because they are still linked (so Log out is offered too). Module-level, so
// the state survives re-renders and page changes.
import { useSyncExternalStore } from "react";

const busy = new Set<string>();
const stillLinked = new Set<string>();
const listeners = new Set<() => void>();
let version = 0;
const emit = () => {
  version++;
  listeners.forEach((l) => l());
};

export const busyStore = {
  isBusy: (id: string) => busy.has(id),
  setBusy(id: string, on: boolean) {
    if (on) busy.add(id);
    else busy.delete(id);
    emit();
  },
  isStillLinked: (id: string) => stillLinked.has(id),
  setStillLinked(id: string, on: boolean) {
    if (on) stillLinked.add(id);
    else stillLinked.delete(id);
    emit();
  },
};

export function useBusyVersion() {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    () => version,
  );
}
