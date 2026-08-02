import { useCallback, useEffect, useRef, useState } from "react";
import { fetchJSON } from "../api";
import { cacheCell, fmtCompact, fmtInt } from "../format";
import { toast } from "../components/Toast";
import { SessionModal, TOOLS, fmtTime } from "../components/SessionModal";
import type { AgentSession, SessionsResponse } from "../types";

const RANGES: [string, string][] = [
  ["24h", "24h"], ["7d", "7d"], ["30d", "30d"], ["", "all"],
];
const PAGE_SIZES = [20, 50, 100, 200];

const compactPath = (path: string): string => {
  const parts = path.split(/[\\/]+/).filter(Boolean);
  if (parts.length <= 3) return path;
  return `…/${parts.slice(-3).join("/")}`;
};

// Token-usage bucket for the weight/shade ladder in the tokens column:
// <20K, 20K–500K, 500K–1M, 1M–5M, ≥5M (10M+ tops out in the last bucket).
const tokLevel = (n: number): number => {
  if (n >= 5_000_000) return 4;
  if (n >= 1_000_000) return 3;
  if (n >= 500_000) return 2;
  if (n >= 20_000) return 1;
  return 0;
};

interface RowProps {
  s: AgentSession;
  onOpen: () => void;
}

function SessionRow({ s, onOpen }: RowProps) {
  const timeMs = s.created_at_ms;
  return (
    <tr className="sess-row" onClick={onOpen}>
      <td className="text sess-time">
        <time dateTime={new Date(timeMs).toISOString()} title={new Date(timeMs).toLocaleString()}>{fmtTime(timeMs)}</time>
      </td>
      <td className="sess-tool"><span className={`tool-badge t-${s.tool}`} title={s.tool}>{TOOLS.find((t) => t.id === s.tool)?.label ?? s.tool}</span></td>
      <td className="text sess-model">
        <span className="sess-ellipsis" title={s.model}>{s.model || "-"}</span>
      </td>
      <td className={`tnum sess-tokens tok-${tokLevel(s.tokens_used)}`} title={s.tokens_used > 0 ? fmtInt(s.tokens_used) : undefined}>{fmtCompact(s.tokens_used)}</td>
      <td className="sess-cache">{cacheCell(s.cache_hit_rate != null ? s.cache_hit_rate * 100 : null)}</td>
      <td className="text sess-cwd">
        <span className="sess-ellipsis" title={s.cwd}>{s.cwd ? compactPath(s.cwd) : "-"}</span>
      </td>
      <td className="text sess-title">
        <span className="sess-title-cell">
          <span className="title" title={s.title}>{s.title}</span>
          {s.archived ? <span className="pill archived" title="archived">archived</span> : null}
        </span>
      </td>
    </tr>
  );
}

interface Props {
  refreshKey: number;
}

export function SessionsView({ refreshKey }: Props) {
  const [tools, setTools] = useState<string[]>([]);
  const [query, setQuery] = useState("");
  const [range, setRange] = useState("");
  const [limit, setLimit] = useState(50);
  const [offset, setOffset] = useState(0);
  const [data, setData] = useState<SessionsResponse | null>(null);
  const [status, setStatus] = useState("loading…");
  const [loading, setLoading] = useState(false);
  const [modal, setModal] = useState<AgentSession | null>(null);
  const seqRef = useRef(0);
  const stateRef = useRef({ tools, query, range, limit, offset });
  stateRef.current = { tools, query, range, limit, offset };

  const load = useCallback(async (overrides?: Partial<typeof stateRef.current>) => {
    const cur = { ...stateRef.current, ...overrides };
    if (overrides?.offset !== undefined) setOffset(overrides.offset);
    setLoading(true);
    const seq = ++seqRef.current;
    try {
      const r = await fetchJSON<SessionsResponse>("sessions", {
        tool: cur.tools.length ? cur.tools : undefined,
        q: cur.query || undefined,
        since: cur.range || undefined,
        sort: "created",
        limit: cur.limit,
        offset: cur.offset,
        archived: "0",
        with_tokens: "1",
      });
      if (seq !== seqRef.current) return;
      setData(r);
      setStatus(`updated ${new Date().toLocaleTimeString("en-GB", { hour12: false })} · ${r.total} sessions`);
    } catch (e) {
      if (seq === seqRef.current) setStatus("error");
      toast((e as Error).message);
    } finally {
      if (seq === seqRef.current) setLoading(false);
    }
  }, []);

  // Initial load + refresh-key reloads (topbar refresh button).
  useEffect(() => {
    void load({ offset: 0 });
  }, [load, refreshKey]);

  // Debounced search box.
  useEffect(() => {
    const t = window.setTimeout(() => void load({ offset: 0 }), 300);
    return () => window.clearTimeout(t);
  }, [query]); // eslint-disable-line react-hooks/exhaustive-deps

  const toggleTool = (id: string) => {
    const next = tools.includes(id) ? tools.filter((x) => x !== id) : [...tools, id];
    setTools(next);
    void load({ tools: next, offset: 0 });
  };
  const applyRange = (r: string) => {
    setRange(r);
    void load({ range: r, offset: 0 });
  };
  const page = (delta: number) => {
    const next = Math.max(0, offset + delta * limit);
    void load({ offset: next });
  };

  const total = data?.total ?? 0;
  const first = data ? offset + 1 : 0;
  const last = Math.min(offset + limit, total);
  const sum = data?.summary;
  const cachePct = sum?.cache_pct != null ? sum.cache_pct * 100 : null;

  return (
    <div id="sessionsView">
      <section className="sess-bar">
        <div className="seg" id="sessTools" title="Agent tools">
          {TOOLS.map((t) => (
            <button
              key={t.id}
              className={`tool-btn t-${t.id}${tools.includes(t.id) ? " on" : ""}`}
              title={`Filter by ${t.label}`}
              onClick={() => toggleTool(t.id)}
            >
              {t.label}
            </button>
          ))}
        </div>
        <div className="seg" id="sessRange" title="Updated since">
          {RANGES.map(([val, label]) => (
            <button key={label} className={range === val ? "active" : ""} onClick={() => applyRange(val)}>{label}</button>
          ))}
        </div>
        <input
          className="sess-search"
          type="search"
          placeholder="search cwd / title…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <div className="spacer" />
        <span className="status" id="sessStatus">{status}</span>
      </section>

      <section className="sess-summary">
        {sum ? (
          <span className="ss-kpis">
            <span className="k">page</span><span className="v tnum">{sum.sessions}</span>
            <span className="k">tokens</span><span className="v tnum">{fmtCompact(sum.total_tokens)}</span>
            <span className="k">cache</span><span className="v">{cachePct != null ? cachePct.toFixed(1) + "%" : "—"}</span>
          </span>
        ) : null}
      </section>

      <section className="table-card">
        <div className="table-scroll">
          <table className="data-table sess-table">
            <colgroup>
              <col className="sess-col-time" />
              <col className="sess-col-tool" />
              <col className="sess-col-model" />
              <col className="sess-col-tokens" />
              <col className="sess-col-cache" />
              <col className="sess-col-cwd" />
              <col className="sess-col-title" />
            </colgroup>
            <thead>
              <tr>
                <th className="text">created</th>
                <th className="text" title="tool">tool</th>
                <th className="text">model</th>
                <th title="tokens used">tokens</th>
                <th title="cache hit rate">cache</th>
                <th className="text">cwd</th>
                <th className="text">title</th>
              </tr>
            </thead>
            <tbody>
              {data?.sessions.map((s) => (
                <SessionRow
                  key={s.tool + s.id}
                  s={s}
                  onOpen={() => setModal(s)}
                />
              ))}
              {data && !data.sessions.length ? (
                <tr><td colSpan={7} className="text muted sess-empty">No sessions match the current filters.</td></tr>
              ) : null}
            </tbody>
          </table>
        </div>
      </section>

      <section className="sess-pager">
        <button disabled={offset <= 0} onClick={() => page(-1)}>‹ prev</button>
        <span className="tnum">{total ? `${first}–${last} of ${total}` : "0"}</span>
        <button disabled={last >= total} onClick={() => page(1)}>next ›</button>
        <select
          value={limit}
          title="Page size"
          onChange={(e) => { const n = Number(e.target.value); setLimit(n); void load({ limit: n, offset: 0 }); }}
        >
          {PAGE_SIZES.map((n) => <option key={n} value={n}>{n}</option>)}
        </select>
        <span className={`loading-dot${loading ? " on" : ""}`} />
      </section>

      <SessionModal session={modal} onClose={() => setModal(null)} />
    </div>
  );
}
