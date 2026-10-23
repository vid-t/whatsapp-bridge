// JSON client for the bridge. Server text is only ever rendered as React text
// nodes; nothing from the server is inserted as HTML.

export class ApiError extends Error {
  status: number;
  /** Seconds from a Retry-After header (429 and 503), when present. */
  retryAfter: number | null;
  constructor(message: string, status: number, retryAfter: number | null) {
    super(message);
    this.status = status;
    this.retryAfter = retryAfter;
  }
}

export const UNAUTHORIZED_EVENT = "bridge:unauthorized";

function retryAfterSeconds(res: Response): number | null {
  const v = res.headers.get("Retry-After");
  if (!v) return null;
  const n = Number(v);
  if (Number.isFinite(n)) return Math.max(0, Math.ceil(n));
  const t = Date.parse(v);
  return Number.isNaN(t) ? null : Math.max(0, Math.ceil((t - Date.now()) / 1000));
}

/**
 * request calls the bridge. Paths starting with /auth/ are used as given;
 * everything else is under /api. A 401 outside the sign-in calls announces
 * that the browser session ended, so the app can show the sign-in screen.
 */
export async function request<T = unknown>(method: string, path: string, body?: unknown): Promise<T> {
  const url = path.startsWith("/auth/") ? path : "/api" + path;
  const init: RequestInit = { method, headers: { Accept: "application/json" }, credentials: "same-origin" };
  if (body !== undefined) {
    (init.headers as Record<string, string>)["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  let res: Response;
  try {
    res = await fetch(url, init);
  } catch {
    throw new ApiError("The bridge did not answer. Check your connection and try again.", 0, null);
  }
  if (res.status === 204 || res.status === 202) {
    await res.text().catch(() => "");
    return null as T;
  }
  const data = (await res.json().catch(() => null)) as { error?: string } | null;
  if (!res.ok) {
    if (res.status === 401 && !url.startsWith("/auth/") && url !== "/api/me") {
      window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    }
    const msg = (data && typeof data.error === "string" && data.error) || `Request failed (${res.status})`;
    throw new ApiError(msg, res.status, retryAfterSeconds(res));
  }
  return data as T;
}

export const api = {
  get: <T>(path: string) => request<T>("GET", path),
  post: <T>(path: string, body?: unknown) => request<T>("POST", path, body),
  patch: <T>(path: string, body?: unknown) => request<T>("PATCH", path, body),
  del: <T>(path: string) => request<T>("DELETE", path),
};

/** errorText adds the wait time to rate-limit errors. */
export function errorText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 429 && err.retryAfter) return `${err.message} (try again in ${waitText(err.retryAfter)})`;
    return err.message;
  }
  return err instanceof Error ? err.message : String(err);
}

export function waitText(seconds: number): string {
  if (seconds < 60) return `${seconds} second${seconds === 1 ? "" : "s"}`;
  const m = Math.ceil(seconds / 60);
  return `${m} minute${m === 1 ? "" : "s"}`;
}
