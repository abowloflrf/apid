import { useMemo, useState, type ReactNode } from "react";
import type { ChartConfiguration } from "chart.js/auto";
import { C, colorFor } from "../colors";
import { cacheCell, fmtCompact, fmtDur, fmtInt, fmtPct, fmtRate, reqCachePct } from "../format";
import { ChartCanvas } from "../components/Charts";
import { DataTable, type Column, type SortState } from "../components/DataTable";
import { RequestModal, statusPill, tokPerSec } from "../components/RequestModal";
import { BUCKET_LABEL, type StatsQuery } from "../useStatsQuery";
import type { ByModelRow, RequestRow, TimeBucket } from "../types";

// ---- sparkline / KPI cards ----

function Sparkline({ values, color }: { values?: (number | null | undefined)[]; color: string }) {
  const v = (values || []).map(Number).filter(Number.isFinite);
  if (v.length < 2) return <svg className="spark" aria-hidden="true" />;
  const W = 100, H = 30, pad = 3;
  const min = Math.min(...v), max = Math.max(...v), flat = max === min;
  const X = (i: number) => (i / (v.length - 1)) * W;
  const Y = (val: number) => (flat ? H / 2 : H - pad - ((val - min) / (max - min)) * (H - 2 * pad));
  const pts = v.map((val, i) => `${X(i).toFixed(1)},${Y(val).toFixed(1)}`).join(" ");
  return (
    <svg className="spark" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" aria-hidden="true">
      <polygon className="spark-area" points={`0,${H} ${pts} ${W},${H}`} fill={color} />
      <polyline className="spark-line" points={pts} stroke={color} />
    </svg>
  );
}

function Kpi(props: { cls: string; color: string; label: string; value: ReactNode; sub?: ReactNode; series?: (number | null | undefined)[] }) {
  const { cls, color, label, value, sub, series } = props;
  return (
    <div className={`kpi ${cls}`}>
      <div className="k-label">{label}</div>
      <div className="k-value">{value}</div>
      <div className="k-sub">{sub || ""}</div>
      <Sparkline values={series} color={color} />
    </div>
  );
}

function KpiRow({ s, ts }: { s: StatsQuery["summary"]; ts: TimeBucket[] }) {
  if (!s) {
    return (
      <section className="kpis">
        {Array.from({ length: 8 }, (_, i) => (
          <div className="kpi skel-kpi" key={i}>
            <div className="skel skel-line sm" />
            <div className="skel skel-line lg" />
            <div className="skel skel-line xs" />
            <div className="skel skel-line spark-skel" />
          </div>
        ))}
      </section>
    );
  }
  const nonStream = (s.requests || 0) - (s.stream_requests || 0);
  const col = (k: keyof TimeBucket) => ts.map((b) => b[k] as number);
  return (
    <section className="kpis">
      <Kpi cls="teal" color={C.teal} label="Requests" value={fmtInt(s.requests)}
        sub={`${fmtInt(s.stream_requests)} stream · ${fmtInt(nonStream)} sync`} series={col("requests")} />
      <Kpi cls="red" color={C.rose} label="Error rate" value={fmtPct(s.error_pct)}
        sub={`${fmtInt(s.errors)} failed`} series={col("error_pct")} />
      <Kpi cls="" color={C.blue} label="Total tokens" value={fmtCompact(s.total_tokens)}
        sub={fmtInt(s.total_tokens)} series={col("total_tokens")} />
      <Kpi cls="" color={C.blue} label="Input tokens" value={fmtCompact(s.input_tokens)}
        sub={`${fmtCompact(s.cached_tokens)} cached`} series={col("input_tokens")} />
      <Kpi cls="green" color={C.green} label="Output tokens" value={fmtCompact(s.output_tokens)}
        sub={fmtInt(s.output_tokens)} series={col("output_tokens")} />
      <Kpi cls="purple" color={C.purple} label="Cache hit" value={fmtPct(s.cache_pct)}
        sub={`${fmtCompact(s.cached_tokens)} cached`} series={col("cache_pct")} />
      <Kpi cls="amber" color={C.amber} label="Avg latency" value={fmtDur(s.avg_duration_ms)}
        sub={`TTFT ${fmtDur(s.avg_ttft_ms)} · max ${fmtDur(s.max_duration_ms)}`} series={col("avg_duration_ms")} />
      <Kpi cls="green" color={C.green} label="Throughput"
        value={<>{fmtRate(s.e2e_tok_per_sec)} <small>tok/s</small></>}
        sub={`post-TTFT ${fmtRate(s.post_ttft_tok_per_sec)}`} series={col("e2e_tok_per_sec")} />
    </section>
  );
}

function Condensed({ s }: { s: StatsQuery["summary"] }) {
  if (!s) return null;
  const items = [
    { dot: C.teal, label: "Reqs", value: fmtInt(s.requests) },
    { dot: C.rose, label: "Err", value: fmtPct(s.error_pct) },
    { dot: C.blue, label: "Total", value: fmtCompact(s.total_tokens) },
  ];
  return (
    <>
      {items.map((d, i) => (
        <div className="c-item" key={d.label}>
          {i > 0 ? <span className="c-sep" /> : null}
          <span className="c-dot" style={{ background: d.dot }} />
          <span className="c-label">{d.label}</span>
          <span className="c-value">{d.value}</span>
        </div>
      ))}
    </>
  );
}

// ---- charts ----

function makeBucketLabel(ts: TimeBucket[], unit: string) {
  if (unit === "day") return (b: TimeBucket) => b.bucket.slice(5);
  const dayOf = (b: TimeBucket) => b.bucket.slice(0, 10);
  const multiDay = ts.length > 1 && dayOf(ts[0]) !== dayOf(ts[ts.length - 1]);
  return (b: TimeBucket) => (multiDay ? `${b.bucket.slice(5, 10)} ${b.bucket.slice(11)}` : b.bucket.slice(11));
}

const baseScales = (stacked: boolean) => ({
  x: { grid: { color: C.grid }, ticks: { maxRotation: 0, autoSkip: true, maxTicksLimit: 12 }, stacked },
  y: { grid: { color: C.grid }, beginAtZero: true, stacked },
});

const baseOpts = (stacked: boolean) => ({
  responsive: true,
  interaction: { mode: "index" as const, intersect: false },
  plugins: {
    legend: { display: true, labels: { boxWidth: 10, boxHeight: 10, padding: 12 } },
    tooltip: {
      backgroundColor: "rgba(28, 26, 22, .92)",
      titleFont: { weight: 600 as const },
      bodyFont: { size: 12 },
      padding: 10,
      cornerRadius: 8,
      displayColors: true,
    },
  },
  scales: baseScales(stacked),
  animation: { duration: 350, easing: "easeOutQuart" as const },
});

function ChartCard(props: { title: string; hint?: string; children: ReactNode; span?: string }) {
  return (
    <div className={`card chart-card${props.span ? " " + props.span : ""}`}>
      <div className="card-head">
        <h3>{props.title}</h3>
        <span className="hint">{props.hint ?? ""}</span>
      </div>
      <div className="canvas-wrap">{props.children}</div>
    </div>
  );
}

// ---- tables ----

const byModelCols: Column<ByModelRow>[] = [
  { key: "upstream_model", label: "Model", text: true, cls: "model",
    render: (r) => (<><span className="dot" style={{ background: colorFor(r.upstream_model) }} />{r.upstream_model || "—"}</>) },
  { key: "requests", label: "Reqs", render: (r) => <span className="num-strong">{fmtInt(r.requests)}</span> },
  { key: "error_pct", label: "Err%", render: (r) => r.errors ? <span className="pill err">{fmtPct(r.error_pct)}</span> : <span className="muted">0%</span> },
  { key: "input_tokens", label: "Input", render: (r) => fmtInt(r.input_tokens) },
  { key: "output_tokens", label: "Output", render: (r) => fmtInt(r.output_tokens) },
  { key: "cached_tokens", label: "Cached", render: (r) => fmtInt(r.cached_tokens) },
  { key: "total_tokens", label: "Total", render: (r) => <span className="num-strong">{fmtInt(r.total_tokens)}</span> },
  { key: "cache_pct", label: "Cache%", render: (r) => cacheCell(r.input_tokens > 0 ? r.cache_pct : null) },
  { key: "avg_duration_ms", label: "Avg dur", render: (r) => fmtDur(r.avg_duration_ms) },
  { key: "avg_ttft_ms", label: "Avg TTFT", render: (r) => fmtDur(r.avg_ttft_ms) },
  { key: "e2e_tok_per_sec", label: "Tok/s", render: (r) => fmtRate(r.e2e_tok_per_sec) },
];

const reqCols: Column<RequestRow>[] = [
  { key: "time", label: "Time", text: true, sortVal: (r) => r.time,
    render: (r) => new Date(r.time).toLocaleString("en-GB", { hour12: false }) },
  { key: "client_protocol", label: "Proto", text: true,
    render: (r) => <span className="pill proto">{(r.client_protocol || "").replace(/^(openai_|anthropic_)/, "")}</span> },
  { key: "upstream_model", label: "Model", text: true, cls: "model",
    render: (r) => (<><span className="dot" style={{ background: colorFor(r.upstream_model) }} />{r.upstream_model || "—"}</>) },
  { key: "client_ua", label: "UA", text: true,
    render: (r) => r.client_ua ? <span className="ua" title={r.client_ua}>{r.client_ua}</span> : <span className="muted">—</span> },
  { key: "stream", label: "Stream", render: (r) => r.stream ? <span className="muted">SSE</span> : <span className="muted">—</span> },
  { key: "upstream_status", label: "Status", sortVal: (r) => r.upstream_status || r.client_status,
    render: (r) => statusPill(r.upstream_status || r.client_status) },
  { key: "duration_ms", label: "Dur", render: (r) => fmtDur(r.duration_ms) },
  { key: "ttft_ms", label: "TTFT", sortVal: (r) => r.ttft_ms ?? -1,
    render: (r) => r.ttft_ms != null ? fmtDur(r.ttft_ms) : <span className="muted">—</span> },
  { key: "input_tokens", label: "In", render: (r) => fmtInt(r.input_tokens) },
  { key: "output_tokens", label: "Out", render: (r) => fmtInt(r.output_tokens) },
  { key: "cached_tokens", label: "Cached", render: (r) => fmtInt(r.cached_tokens) },
  { key: "total_tokens", label: "Total", render: (r) => fmtInt(r.total_tokens) },
  { key: "cache_pct", label: "Cache%", sortVal: (r) => reqCachePct(r) ?? -1, render: (r) => cacheCell(reqCachePct(r)) },
  { key: "tps", label: "Tok/s", sortVal: (r) => (r.duration_ms > 0 ? 1000 * r.output_tokens / r.duration_ms : -1),
    render: (r) => tokPerSec(r) },
  { key: "error", label: "Error", text: true, sortable: false,
    render: (r) => r.error ? <span className="err-text" title={r.error}>{r.error}</span> : null },
];

// ---- view ----

export function StatsView({ q }: { q: StatsQuery }) {
  const [byModelSort, setByModelSort] = useState<SortState>({ key: "total_tokens", dir: "desc" });
  const [reqSort, setReqSort] = useState<SortState>({ key: "time", dir: "desc" });
  const [modal, setModal] = useState<RequestRow | null>(null);

  const toggleSort = (cur: SortState, key: string): SortState =>
    ({ key, dir: cur.key === key && cur.dir === "desc" ? "asc" : "desc" });

  const charts = useMemo(() => {
    const labels = q.ts.map(makeBucketLabel(q.ts, q.tsBucket));
    const auto = q.bucket === "auto" ? "auto · " : "";
    const hint = `${auto}${BUCKET_LABEL[q.tsBucket] || q.tsBucket} · ${q.ts.length} pts`;

    const tokOpts = baseOpts(true) as Record<string, unknown>;
    tokOpts.scales = { ...(tokOpts.scales as object), y1: {
      position: "right", beginAtZero: true, min: 0, max: 100,
      grid: { drawOnChartArea: false },
      ticks: { maxTicksLimit: 5, callback: (v: string | number) => v + "%" },
    } };
    (tokOpts.plugins as Record<string, unknown>).tooltip = {
      ...((tokOpts.plugins as Record<string, unknown>).tooltip as object),
      callbacks: {
        label: (c: { dataset: { label?: string }; raw?: number }) =>
          c.dataset.label === "hit-rate" ? ` hit-rate: ${(c.raw ?? 0).toFixed(1)}%` : ` ${c.dataset.label}: ${fmtInt(c.raw)}`,
      },
    };

    const shareOpts = {
      responsive: true,
      cutout: "58%",
      animation: { animateRotate: true, animateScale: true, duration: 500, easing: "easeOutQuart" },
      plugins: {
        legend: { position: "right", labels: { boxWidth: 10, boxHeight: 10, padding: 8, font: { size: 11 } } },
        tooltip: {
          backgroundColor: "rgba(28, 26, 22, .92)",
          callbacks: {
            label: (c: { label?: string; raw?: number; dataset: { data: number[] } }) => {
              const total = c.dataset.data.reduce((a, b) => a + b, 0);
              return ` ${c.label}: ${fmtInt(c.raw)} (${(((c.raw ?? 0) / (total || 1)) * 100 || 0).toFixed(1)}%)`;
            },
          },
        },
      },
    };

    const top = q.byModel.slice(0, 12);
    return {
      hint,
      requests: {
        type: "bar",
        data: {
          labels,
          datasets: [
            { label: "ok", data: q.ts.map((b) => b.requests - b.errors), backgroundColor: C.teal, borderRadius: 3, hoverBackgroundColor: C.teal },
            { label: "errors", data: q.ts.map((b) => b.errors), backgroundColor: C.rose, borderRadius: 3, hoverBackgroundColor: C.rose },
          ],
        },
        options: baseOpts(true),
      },
      tokens: {
        type: "bar",
        data: {
          labels,
          datasets: [
            { label: "cached", data: q.ts.map((b) => b.cached_tokens), backgroundColor: C.purple, borderRadius: 3, stack: "tok", hoverBackgroundColor: C.purple },
            { label: "uncached", data: q.ts.map((b) => Math.max(0, b.input_tokens - b.cached_tokens)), backgroundColor: C.blue, borderRadius: 3, stack: "tok", hoverBackgroundColor: C.blue },
            { label: "output", data: q.ts.map((b) => b.output_tokens), backgroundColor: C.green, borderRadius: 3, stack: "tok", hoverBackgroundColor: C.green },
            { type: "line", label: "hit-rate", data: q.ts.map((b) => (b.input_tokens > 0 ? (100 * b.cached_tokens) / b.input_tokens : null)), borderColor: C.amber, backgroundColor: "transparent", yAxisID: "y1", tension: 0.3, pointRadius: 0, borderWidth: 2, spanGaps: true, hoverBorderWidth: 3 },
          ],
        },
        options: tokOpts,
      },
      latency: {
        type: "line",
        data: {
          labels,
          datasets: [
            { label: "avg duration", data: q.ts.map((b) => b.avg_duration_ms), borderColor: C.amber, backgroundColor: "rgba(187,154,85,.12)", fill: true, tension: 0.3, pointRadius: 0, borderWidth: 2, hoverBorderWidth: 3 },
            { label: "avg TTFT", data: q.ts.map((b) => b.avg_ttft_ms), borderColor: C.blue, backgroundColor: "transparent", tension: 0.3, pointRadius: 0, borderWidth: 2, hoverBorderWidth: 3 },
          ],
        },
        options: baseOpts(false),
      },
      throughput: {
        type: "line",
        data: {
          labels,
          datasets: [
            { label: "e2e tok/s", data: q.ts.map((b) => b.e2e_tok_per_sec), borderColor: C.green, backgroundColor: "rgba(111,149,118,.12)", fill: true, tension: 0.3, pointRadius: 0, borderWidth: 2, hoverBorderWidth: 3 },
            { label: "post-TTFT tok/s", data: q.ts.map((b) => b.post_ttft_tok_per_sec), borderColor: C.purple, backgroundColor: "transparent", borderDash: [4, 3], tension: 0.3, pointRadius: 0, borderWidth: 2, hoverBorderWidth: 3 },
          ],
        },
        options: baseOpts(false),
      },
      share: {
        type: "doughnut",
        data: {
          labels: top.map((m) => m.upstream_model),
          datasets: [{
            data: top.map((m) => m.total_tokens),
            backgroundColor: top.map((m) => colorFor(m.upstream_model)),
            borderColor: C.panel,
            borderWidth: 2,
            hoverBorderWidth: 3,
            hoverBorderColor: "rgba(28,26,22,.12)",
          }],
        },
        options: shareOpts,
      },
    };
  }, [q.ts, q.tsBucket, q.bucket, q.byModel]);

  const hasData = q.ts.length > 0 || (q.byModel.length > 0);
  return (
    <div id="statsView">
      <KpiRow s={q.summary} ts={q.ts} />

      <section className="charts">
        <ChartCard title="Requests over time" hint={charts?.hint}>
          {hasData ? <ChartCanvas config={charts!.requests as ChartConfiguration} /> : <div className="skel skel-fill" />}
        </ChartCard>
        <ChartCard title="Tokens over time" hint="cached · uncached · output · hit-rate %">
          {hasData ? <ChartCanvas config={charts!.tokens as ChartConfiguration} /> : <div className="skel skel-fill" />}
        </ChartCard>
        <ChartCard title="Latency over time" hint="avg duration · avg TTFT (ms)">
          {hasData ? <ChartCanvas config={charts!.latency as ChartConfiguration} /> : <div className="skel skel-fill" />}
        </ChartCard>
        <ChartCard title="Throughput over time" hint="tokens / sec">
          {hasData ? <ChartCanvas config={charts!.throughput as ChartConfiguration} /> : <div className="skel skel-fill" />}
        </ChartCard>
        <ChartCard title="Token share by model" hint="total tokens" span="span2-doughnut">
          {hasData ? <ChartCanvas config={charts!.share as ChartConfiguration} /> : <div className="skel skel-fill" />}
        </ChartCard>
      </section>

      <section className="card table-card">
        <div className="card-head">
          <h3>By model</h3>
          <span className="hint">{q.byModel.length} models</span>
        </div>
        <div className="table-scroll">
          <DataTable
            columns={byModelCols}
            rows={q.byModel}
            sort={byModelSort}
            onSort={(k) => setByModelSort(toggleSort(byModelSort, k))}
          />
        </div>
      </section>

      <section className="card table-card">
        <div className="card-head">
          <h3>Requests</h3>
          <div className="head-controls">
            <label className="auto">
              <input
                type="checkbox"
                checked={q.errorsOnly}
                onChange={(e) => { q.setErrorsOnly(e.target.checked); q.loadAll({ errorsOnly: e.target.checked, offset: 0 }); }}
              /> errors only
            </label>
            <select
              title="Rows"
              value={q.limit}
              onChange={(e) => { q.setLimit(+e.target.value); q.loadAll({ limit: +e.target.value, offset: 0 }); }}
            >
              <option value={50}>50</option>
              <option value={100}>100</option>
              <option value={250}>250</option>
              <option value={500}>500</option>
            </select>
            <span className="pager">
              <button
                type="button"
                title="Previous page"
                disabled={q.offset <= 0}
                onClick={() => q.loadAll({ offset: Math.max(0, q.offset - q.limit) })}
              >‹</button>
              <span className="pager-page">Page {Math.floor(q.offset / q.limit) + 1}</span>
              <button
                type="button"
                title="Next page"
                disabled={q.requests.length < q.limit}
                onClick={() => q.loadAll({ offset: q.offset + q.limit })}
              >›</button>
            </span>
          </div>
        </div>
        <div className="table-scroll">
          <DataTable
            columns={reqCols}
            rows={q.requests}
            sort={reqSort}
            onSort={(k) => setReqSort(toggleSort(reqSort, k))}
            onRowClick={setModal}
          />
        </div>
      </section>

      <RequestModal request={modal} onClose={() => setModal(null)} />
    </div>
  );
}

export { Condensed };
