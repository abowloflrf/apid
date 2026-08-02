// API base = the directory this page is served from, so the /stats/* JSON
// endpoints resolve whether apid runs at the root or under a reverse-proxy
// subpath (e.g. /apid/stats/). Depends on the trailing-slash page URL, which
// the server's /stats -> /stats/ redirect guarantees. In dev, Vite proxies
// /stats to a locally running apid.
const API_BASE = import.meta.env.DEV
  ? "/stats"
  : new URL(".", document.baseURI).href.replace(/\/+$/, "");

const KEY_STORAGE = "apid.stats_key";

export class AuthError extends Error {
  constructor() {
    super("invalid or missing stats API key");
    this.name = "AuthError";
  }
}

export function getStatsKey(): string {
  try {
    return localStorage.getItem(KEY_STORAGE) ?? "";
  } catch {
    return "";
  }
}

export function setStatsKey(key: string): void {
  try {
    if (key) localStorage.setItem(KEY_STORAGE, key);
    else localStorage.removeItem(KEY_STORAGE);
  } catch {
    // storage unavailable (private mode etc.); session works until reload
  }
}

type AuthListener = () => void;
let authFailure: AuthListener | null = null;
export function onAuthFailure(fn: AuthListener): void {
  authFailure = fn;
}

function authHeaders(): HeadersInit {
  const key = getStatsKey();
  return key ? { Authorization: `Bearer ${key}` } : {};
}

export type QueryValue = string | number | string[] | undefined;

export async function fetchJSON<T>(
  path: string,
  query?: Record<string, QueryValue>,
): Promise<T> {
  const p = new URLSearchParams();
  if (query) {
    for (const [k, v] of Object.entries(query)) {
      if (v === undefined) continue;
      if (Array.isArray(v)) v.forEach((x) => p.append(k, x));
      else p.set(k, String(v));
    }
  }
  const qs = p.toString();
  const r = await fetch(`${API_BASE}/${path}${qs ? "?" + qs : ""}`, { headers: authHeaders() });
  if (r.status === 401) {
    authFailure?.();
    throw new AuthError();
  }
  if (!r.ok) {
    let msg = `${r.status}`;
    try {
      msg = (await r.json()).error?.message || msg;
    } catch {
      // keep status-only message
    }
    throw new Error(`${path}: ${msg}`);
  }
  return r.json() as Promise<T>;
}
