import type { ReactNode } from "react";

export const fmtInt = (v?: number | null): string =>
  (v ?? 0).toLocaleString("en-US");

export function fmtCompact(v?: number | null): string {
  v = v ?? 0;
  if (Math.abs(v) >= 1e6) return (v / 1e6).toFixed(2) + "M";
  if (Math.abs(v) >= 1e3) return (v / 1e3).toFixed(1) + "k";
  return String(v);
}

export function fmtDur(ms?: number | null): string {
  ms = ms ?? 0;
  if (ms >= 1000) return (ms / 1000).toFixed(2) + "s";
  return Math.round(ms) + "ms";
}

export const fmtPct = (v?: number | null): string => (v ?? 0).toFixed(2) + "%";
export const fmtRate = (v?: number | null): string => (v ?? 0).toFixed(1);

// Cache hit-rate heatmap. Hit rates cluster high, so the scale spends most of
// its resolution at the top: red -> amber -> green up to ~90%, then the elite
// band shifts hue green -> emerald -> teal AND deepens, with dense stops in the
// 99-100% range so a near-perfect rate visibly stands apart. Stops are
// [pct, hue, saturation, text-lightness]. Pass a percentage; null = dash.
const CACHE_STOPS: [number, number, number, number][] = [
  [0, 8, 42, 47],
  [50, 40, 42, 45],
  [75, 92, 38, 40],
  [90, 122, 40, 34],
  [97, 140, 44, 28],
  [99, 156, 48, 24],
  [99.7, 168, 52, 20],
  [100, 178, 56, 17],
];

const lerp = (a: number, b: number, u: number): number => a + (b - a) * u;

export function cacheStyle(pct: number): { h: number; s: number; l: number } {
  const p = Math.max(0, Math.min(100, pct));
  let i = 0;
  while (i < CACHE_STOPS.length - 1 && p > CACHE_STOPS[i + 1][0]) i++;
  const [p0, h0, s0, l0] = CACHE_STOPS[i];
  const [p1, h1, s1, l1] = CACHE_STOPS[Math.min(i + 1, CACHE_STOPS.length - 1)];
  const u = p1 === p0 ? 0 : (p - p0) / (p1 - p0);
  return { h: lerp(h0, h1, u), s: lerp(s0, s1, u), l: lerp(l0, l1, u) };
}

export function cacheCell(pct: number | null | undefined): ReactNode {
  if (pct == null) return <span className="muted">—</span>;
  const { h, s, l } = cacheStyle(pct);
  const lBg = 95 - (45 - l) * 0.22;
  // The >=99% "elite" tier gets a distinct ring so a near-perfect rate stands
  // apart at a glance. Kept as its own style property: React style objects do
  // not split properties on semicolons the way an HTML style string does.
  const elite = pct >= 99;
  return (
    <span
      className="heat"
      style={{
        color: `hsl(${h} ${s}% ${l}%)`,
        background: `hsl(${h} ${s}% ${lBg}%)`,
        boxShadow: elite ? `inset 0 0 0 1.5px hsl(${h} ${s}% ${l}% / .7)` : undefined,
      }}
    >
      {pct.toFixed(2)}%
    </span>
  );
}

// Per-request hit rate from raw tokens (server only aggregates it per model).
export const reqCachePct = (r: { input_tokens: number; cached_tokens: number }): number | null =>
  r.input_tokens > 0 ? (100 * r.cached_tokens) / r.input_tokens : null;

export function escapeHtml(s: unknown): string {
  return String(s).replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" })[c] as string);
}

export function escapeAttr(s: unknown): string {
  return escapeHtml(s).replace(/"/g, "&quot;");
}

export function toLocalInput(ms: number): string {
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function fmtElapsed(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 3600) return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  return `${Math.floor(s / 3600)}:${String(Math.floor((s % 3600) / 60)).padStart(2, "0")}:${String(s % 60).padStart(2, "0")}`;
}

export const fmtTok = (v: number, est?: boolean): string =>
  (est && v > 0 ? "~" : "") + fmtCompact(v);
