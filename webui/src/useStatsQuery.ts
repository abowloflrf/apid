import { useCallback, useRef, useState } from "react";
import { fetchJSON } from "./api";
import { toast } from "./components/Toast";
import type { ByModelRow, Options, RequestRow, Summary, TimeBucket } from "./types";

const RANGE_MS: Record<string, number> = {
  "1h": 3600e3, "6h": 6 * 3600e3, "24h": 864e5, "7d": 7 * 864e5, "30d": 30 * 864e5,
};
const BUCKET_LADDER = ["15min", "hour", "day"];
const BUCKET_MS: Record<string, number> = { "15min": 15 * 60e3, hour: 3600e3, day: 864e5 };
export const BUCKET_LABEL: Record<string, string> = { "15min": "15m", hour: "1h", day: "1d" };
const MAX_BUCKETS = 1500;

export const TZ_OFFSET = -Math.round(new Date().getTimezoneOffset() / 60);

export interface QueryState {
  range: string;
  customFrom: number | null;
  customTo: number | null;
  models: string[];
  protocols: string[];
  bucket: string;
  errorsOnly: boolean;
  limit: number;
  offset: number;
  auto: boolean;
}

export function rangeBounds(q: QueryState): { from: number | null; to: number | null } {
  if (q.range === "all") return { from: null, to: null };
  if (q.range === "custom") return { from: q.customFrom, to: q.customTo };
  const now = Date.now();
  if (q.range === "today") {
    const midnight = new Date();
    midnight.setHours(0, 0, 0, 0);
    return { from: midnight.getTime(), to: now };
  }
  return { from: now - RANGE_MS[q.range], to: now };
}

// Span of the active window in ms. For "all" we fall back to the known data span
// from /options so auto/clamp still have something to reason about (null if unknown).
function spanMs(q: QueryState, options: Options): number | null {
  const { from, to } = rangeBounds(q);
  if (from != null && to != null) return to - from;
  const lo = options.min_time ? Date.parse(options.min_time) : NaN;
  const hi = options.max_time ? Date.parse(options.max_time) : NaN;
  return lo < hi ? hi - lo : null;
}

function autoBucket(span: number | null): string {
  if (span == null) return "day";
  if (span <= 36 * 3600e3) return "15min";
  if (span <= 7 * 864e5) return "hour";
  return "day";
}

export function effectiveBucket(q: QueryState, options: Options): string {
  const span = spanMs(q, options);
  let unit = q.bucket === "auto" ? autoBucket(span) : q.bucket;
  if (span != null) {
    let i = Math.max(0, BUCKET_LADDER.indexOf(unit));
    while (i < BUCKET_LADDER.length - 1 && span / BUCKET_MS[BUCKET_LADDER[i]] > MAX_BUCKETS) i++;
    unit = BUCKET_LADDER[i];
  }
  return unit;
}

export interface StatsSnapshot {
  summary: Summary | null;
  ts: TimeBucket[];
  tsBucket: string;
  byModel: ByModelRow[];
  requests: RequestRow[];
  status: string;
  loading: boolean;
}

export interface StatsQuery extends QueryState, StatsSnapshot {
  options: Options;
  setRange: (r: string) => void;
  setCustom: (from: number | null, to: number | null) => void;
  setModels: (m: string[]) => void;
  setProtocols: (p: string[]) => void;
  setBucket: (b: string) => void;
  setErrorsOnly: (v: boolean) => void;
  setLimit: (v: number) => void;
  setOffset: (v: number) => void;
  setAuto: (v: boolean) => void;
  loadOptions: () => Promise<void>;
  loadAll: (overrides?: Partial<QueryState>) => Promise<void>;
  refresh: () => void;
}

export function useStatsQuery(): StatsQuery {
  const [range, setRange] = useState("24h");
  const [customFrom, setCustomFrom] = useState<number | null>(null);
  const [customTo, setCustomTo] = useState<number | null>(null);
  const [models, setModels] = useState<string[]>([]);
  const [protocols, setProtocols] = useState<string[]>([]);
  const [bucket, setBucket] = useState("auto");
  const [errorsOnly, setErrorsOnly] = useState(false);
  const [limit, setLimit] = useState(100);
  const [offset, setOffset] = useState(0);
  const [auto, setAuto] = useState(false);

  const [options, setOptions] = useState<Options>({ models: [], protocols: [] });
  const [summary, setSummary] = useState<Summary | null>(null);
  const [ts, setTs] = useState<TimeBucket[]>([]);
  const [tsBucket, setTsBucket] = useState("day");
  const [byModel, setByModel] = useState<ByModelRow[]>([]);
  const [requests, setRequests] = useState<RequestRow[]>([]);
  const [status, setStatus] = useState("loading…");
  const [loading, setLoading] = useState(false);
  const seqRef = useRef(0);
  // Mirror of the query state kept fresh across renders so loadAll can read the
  // latest values even when called right after a state update (filter changes).
  const stateRef = useRef<QueryState & { options: Options }>({
    range, customFrom, customTo, models, protocols, bucket, errorsOnly, limit, offset, auto, options,
  });
  stateRef.current = { range, customFrom, customTo, models, protocols, bucket, errorsOnly, limit, offset, auto, options };

  const loadOptions = useCallback(async () => {
    try {
      const o = await fetchJSON<Options>("options");
      setOptions({ models: o.models || [], protocols: o.protocols || [], min_time: o.min_time, max_time: o.max_time });
    } catch (e) {
      toast("options: " + (e as Error).message);
    }
  }, []);

  const loadAll = useCallback(async (overrides?: Partial<QueryState>) => {
    if (overrides && overrides.offset !== undefined) setOffset(overrides.offset);
    setLoading(true);
    setStatus("loading…");
    const seq = ++seqRef.current;
    const cur = { ...stateRef.current, ...overrides };
    const q: QueryState = cur;
    const b = effectiveBucket(q, cur.options);
    try {
      const query = {
        tz_offset: TZ_OFFSET,
        model: q.models.length ? q.models : undefined,
        protocol: q.protocols.length ? q.protocols : undefined,
      };
      const rb = rangeBounds(q);
      const [sum, timeSeries, byModelRows, reqRows] = await Promise.all([
        fetchJSON<Summary>("summary", { ...query, from: rb.from ?? undefined, to: rb.to ?? undefined }),
        fetchJSON<TimeBucket[]>("timeseries", { ...query, from: rb.from ?? undefined, to: rb.to ?? undefined, bucket: b }),
        fetchJSON<ByModelRow[]>("by_model", { ...query, from: rb.from ?? undefined, to: rb.to ?? undefined }),
        fetchJSON<RequestRow[]>("requests", {
          ...query, from: rb.from ?? undefined, to: rb.to ?? undefined,
          limit: cur.limit, offset: cur.offset, errors_only: cur.errorsOnly ? 1 : 0,
        }),
      ]);
      if (seq !== seqRef.current) return;
      setSummary(sum);
      setTs(timeSeries);
      setTsBucket(b);
      setByModel(byModelRows);
      setRequests(reqRows);
      setStatus(`updated ${new Date().toLocaleTimeString("en-GB", { hour12: false })}`);
    } catch (e) {
      if (seq === seqRef.current) setStatus("error");
      toast((e as Error).message);
    } finally {
      if (seq === seqRef.current) setLoading(false);
    }
  }, []);

  // The refresh button flashes a spin class briefly; expose as a simple no-op
  // refresh helper so the caller can trigger reloads without deep coupling.
  const refresh = useCallback(() => {
    void loadAll();
  }, [loadAll]);

  return {
    range, customFrom, customTo, models, protocols, bucket, errorsOnly, limit, offset, auto,
    options, summary, ts, tsBucket, byModel, requests, status, loading,
    setRange, setCustom: (from, to) => { setCustomFrom(from); setCustomTo(to); },
    setModels, setProtocols, setBucket, setErrorsOnly, setLimit, setOffset, setAuto,
    loadOptions, loadAll, refresh,
  };
}
