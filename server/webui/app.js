"use strict";

// apid stats dashboard. Talks to the /stats/* JSON API, renders KPIs, Chart.js
// charts and sortable tables. All aggregation happens server-side; this file is
// purely presentation + filter state.

const API = "/stats";
// Muted palette tuned for the warm-paper theme — desaturated, harmonious, never
// neon. Mirrors the CSS custom properties so chrome and data read as one piece.
const C = {
  blue: "#5f6f96", green: "#6f9576", amber: "#bb9a55", teal: "#5f9499",
  purple: "#897fa8", rose: "#c07b73", muted: "#7a766c",
  grid: "rgba(40,36,28,.06)", panel: "#fdfcf9",
};
const PALETTE = [
  "#5f6f96", "#6f9576", "#897fa8", "#bb9a55", "#5f9499",
  "#b98ca0", "#c07b73", "#7c88ad", "#86a98a", "#cbae6e",
  "#9a90bb", "#74a6ab", "#bf9a7e", "#c79bb0", "#8e9bbf",
];
// Day boundaries follow the viewer's local zone. Server param is an integer.
const TZ_OFFSET = -Math.round(new Date().getTimezoneOffset() / 60);

const state = {
  range: "24h",
  from: null, // ms, only when range === "custom"
  to: null,
  models: [],
  protocols: [],
  bucket: "auto",
  errorsOnly: false,
  limit: 100,
  auto: false,
};

const modelColor = {}; // model name -> stable color
function colorFor(model) {
  if (!(model in modelColor)) {
    modelColor[model] = PALETTE[Object.keys(modelColor).length % PALETTE.length];
  }
  return modelColor[model];
}

// ---- formatting ----
const fmtInt = (v) => (v ?? 0).toLocaleString("en-US");
function fmtCompact(v) {
  v = v ?? 0;
  if (Math.abs(v) >= 1e6) return (v / 1e6).toFixed(2) + "M";
  if (Math.abs(v) >= 1e3) return (v / 1e3).toFixed(1) + "k";
  return String(v);
}
function fmtDur(ms) {
  ms = ms ?? 0;
  if (ms >= 1000) return (ms / 1000).toFixed(2) + "s";
  return Math.round(ms) + "ms";
}
const fmtPct = (v) => (v ?? 0).toFixed(2) + "%";
const fmtRate = (v) => (v ?? 0).toFixed(1);

// Cache hit-rate heatmap. Hit rates cluster high, so the scale spends most of
// its resolution at the top: red -> amber -> green up to ~90%, then the elite
// band shifts hue green -> emerald -> teal AND deepens, with dense stops in the
// 99-100% range so a near-perfect rate visibly stands apart. Stops are
// [pct, hue, saturation, text-lightness]. Pass a percentage; null = dash.
const CACHE_STOPS = [
  [0, 8, 42, 47],
  [50, 40, 42, 45],
  [75, 92, 38, 40],
  [90, 122, 40, 34],
  [97, 140, 44, 28],
  [99, 156, 48, 24],
  [99.7, 168, 52, 20],
  [100, 178, 56, 17],
];
const lerp = (a, b, u) => a + (b - a) * u;
function cacheStyle(pct) {
  const p = Math.max(0, Math.min(100, pct));
  let i = 0;
  while (i < CACHE_STOPS.length - 1 && p > CACHE_STOPS[i + 1][0]) i++;
  const [p0, h0, s0, l0] = CACHE_STOPS[i];
  const [p1, h1, s1, l1] = CACHE_STOPS[Math.min(i + 1, CACHE_STOPS.length - 1)];
  const u = p1 === p0 ? 0 : (p - p0) / (p1 - p0);
  return { h: lerp(h0, h1, u), s: lerp(s0, s1, u), l: lerp(l0, l1, u) };
}
function cacheCell(pct) {
  if (pct == null) return `<span class="muted">—</span>`;
  const { h, s, l } = cacheStyle(pct);
  const lBg = 95 - (45 - l) * 0.22;
  // A hairline ring marks the >=99% "elite" tier as a distinct class at a glance.
  const ring = pct >= 99 ? `;box-shadow:inset 0 0 0 1px hsl(${h} ${s}% ${l}% / .5)` : "";
  return `<span class="heat" style="color:hsl(${h} ${s}% ${l}%);background:hsl(${h} ${s}% ${lBg}%)${ring}">${pct.toFixed(2)}%</span>`;
}
// Per-request hit rate from raw tokens (server only aggregates it per model).
const reqCachePct = (r) => (r.input_tokens > 0 ? (100 * r.cached_tokens) / r.input_tokens : null);

function toLocalInput(ms) {
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

// ---- query state ----
const RANGE_MS = { "1h": 3600e3, "6h": 6 * 3600e3, "24h": 864e5, "7d": 7 * 864e5, "30d": 30 * 864e5 };
function rangeBounds() {
  if (state.range === "all") return { from: null, to: null };
  if (state.range === "custom") return { from: state.from, to: state.to };
  const now = Date.now();
  if (state.range === "today") {
    const midnight = new Date();
    midnight.setHours(0, 0, 0, 0); // local midnight in the viewer's zone
    return { from: midnight.getTime(), to: now };
  }
  return { from: now - RANGE_MS[state.range], to: now };
}
// Bucket ladder, coarsest-to-finest. BUCKET_MS is the grid width each unit floors
// onto ("day" is calendar-based). BUCKET_LABEL is the compact UI/hint label.
const BUCKET_LADDER = ["15min", "hour", "day"];
const BUCKET_MS = { "15min": 15 * 60e3, hour: 3600e3, day: 864e5 };
const BUCKET_LABEL = { "15min": "15m", hour: "1h", day: "1d" };
const MAX_BUCKETS = 1500; // cap points so Chart.js stays smooth and payloads stay small

// Span of the active window in ms. For "all" we fall back to the known data span
// from /options so auto/clamp still have something to reason about (null if unknown).
function spanMs(from, to) {
  if (from != null && to != null) return to - from;
  const lo = optionsCache.minTime ? Date.parse(optionsCache.minTime) : NaN;
  const hi = optionsCache.maxTime ? Date.parse(optionsCache.maxTime) : NaN;
  return lo < hi ? hi - lo : null;
}
// Pick the finest grid that keeps the window readable (~100 points or fewer).
function autoBucket(span) {
  if (span == null) return "day";
  if (span <= 36 * 3600e3) return "15min"; // <=1.5d
  if (span <= 7 * 864e5) return "hour";    // <=7d
  return "day";
}
function effectiveBucket(from, to) {
  const span = spanMs(from, to);
  let unit = state.bucket === "auto" ? autoBucket(span) : state.bucket;
  // Coarsen the chosen/auto unit until the window yields at most MAX_BUCKETS points.
  if (span != null) {
    let i = Math.max(0, BUCKET_LADDER.indexOf(unit));
    while (i < BUCKET_LADDER.length - 1 && span / BUCKET_MS[BUCKET_LADDER[i]] > MAX_BUCKETS) i++;
    unit = BUCKET_LADDER[i];
  }
  return unit;
}
function qs(extra) {
  const { from, to } = rangeBounds();
  const p = new URLSearchParams();
  if (from != null) p.set("from", Math.round(from));
  if (to != null) p.set("to", Math.round(to));
  p.set("tz_offset", TZ_OFFSET);
  state.models.forEach((m) => p.append("model", m));
  state.protocols.forEach((pr) => p.append("protocol", pr));
  for (const k in extra) p.set(k, extra[k]);
  return p.toString();
}
async function api(path, extra) {
  const r = await fetch(`${API}/${path}?${qs(extra)}`);
  if (!r.ok) {
    let msg = `${r.status}`;
    try { msg = (await r.json()).error?.message || msg; } catch {}
    throw new Error(`${path}: ${msg}`);
  }
  return r.json();
}

// ---- toast ----
let toastTimer;
function toast(msg) {
  const el = document.getElementById("toast");
  el.textContent = msg;
  el.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), 4000);
}

// ---- KPIs ----
// Inline SVG sparkline: a translucent area under a crisp line, stretched to the
// card width (non-scaling-stroke keeps the line even). Trend context only — the
// current value is the KPI number itself, so no axes or end marker are needed.
function sparkline(values, color) {
  const v = (values || []).map(Number).filter(Number.isFinite);
  if (v.length < 2) return `<svg class="spark" aria-hidden="true"></svg>`;
  const W = 100, H = 30, pad = 3;
  const min = Math.min(...v), max = Math.max(...v), flat = max === min;
  const X = (i) => (i / (v.length - 1)) * W;
  const Y = (val) => (flat ? H / 2 : H - pad - ((val - min) / (max - min)) * (H - 2 * pad));
  const pts = v.map((val, i) => `${X(i).toFixed(1)},${Y(val).toFixed(1)}`).join(" ");
  return `<svg class="spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" aria-hidden="true">
    <polygon class="spark-area" points="0,${H} ${pts} ${W},${H}" fill="${color}" />
    <polyline class="spark-line" points="${pts}" stroke="${color}" /></svg>`;
}
function kpi(cls, color, label, value, sub, series) {
  return `<div class="kpi ${cls}"><div class="k-label">${label}</div>
    <div class="k-value">${value}</div><div class="k-sub">${sub || ""}</div>
    ${sparkline(series, color)}</div>`;
}
// Build the condensed KPI chips shown inline in the status bar — same summary
// data as the full KPI row, in a compact dot + label + value form.
function buildCondensedItems(s) {
  // Status-bar essentials only — volume / health / usage. Output, throughput and
  // latency stay in the full KPI cards and charts below, not duplicated up here.
  return [
    { dot: C.teal, label: "Reqs", value: fmtInt(s.requests) },
    { dot: C.rose, label: "Err", value: fmtPct(s.error_pct) },
    { dot: C.blue, label: "Total", value: fmtCompact(s.total_tokens) },
  ];
}
function renderCondensed(s) {
  const bar = document.getElementById("kpiCondensed");
  if (!bar) return;
  bar.innerHTML = buildCondensedItems(s).map((d, i) => {
    const sep = i > 0 ? '<span class="c-sep"></span>' : '';
    return `${sep}<div class="c-item"><span class="c-dot" style="background:${d.dot}"></span><span class="c-label">${d.label}</span><span class="c-value">${d.value}</span></div>`;
  }).join("");
}
function renderKPIs(s, ts) {
  ts = ts || [];
  const nonStream = (s.requests || 0) - (s.stream_requests || 0);
  const col = (k) => ts.map((b) => b[k]);
  document.getElementById("kpis").innerHTML = [
    kpi("teal", C.teal, "Requests", fmtInt(s.requests), `${fmtInt(s.stream_requests)} stream · ${fmtInt(nonStream)} sync`, col("requests")),
    kpi("red", C.rose, "Error rate", fmtPct(s.error_pct), `${fmtInt(s.errors)} failed`, col("error_pct")),
    kpi("", C.blue, "Total tokens", fmtCompact(s.total_tokens), `${fmtInt(s.total_tokens)}`, col("total_tokens")),
    kpi("", C.blue, "Input tokens", fmtCompact(s.input_tokens), `${fmtCompact(s.cached_tokens)} cached`, col("input_tokens")),
    kpi("green", C.green, "Output tokens", fmtCompact(s.output_tokens), `${fmtInt(s.output_tokens)}`, col("output_tokens")),
    kpi("purple", C.purple, "Cache hit", fmtPct(s.cache_pct), `${fmtCompact(s.cached_tokens)} cached`, col("cache_pct")),
    kpi("amber", C.amber, "Avg latency", fmtDur(s.avg_duration_ms), `TTFT ${fmtDur(s.avg_ttft_ms)} · max ${fmtDur(s.max_duration_ms)}`, col("avg_duration_ms")),
    kpi("green", C.green, "Throughput", `${fmtRate(s.e2e_tok_per_sec)} <small>tok/s</small>`, `post-TTFT ${fmtRate(s.post_ttft_tok_per_sec)}`, col("e2e_tok_per_sec")),
  ].join("");
  renderCondensed(s);
}
// Placeholder KPI cards shown until the first load resolves.
function renderKPISkeletons() {
  document.getElementById("kpis").innerHTML = Array.from({ length: 8 }, () =>
    `<div class="kpi skel-kpi"><div class="skel skel-line sm"></div>` +
    `<div class="skel skel-line lg"></div><div class="skel skel-line xs"></div>` +
    `<div class="skel skel-line spark-skel"></div></div>`).join("");
}

// ---- charts ----
Chart.defaults.color = C.muted;
Chart.defaults.font.family = "ui-monospace, Menlo, Consolas, monospace";
Chart.defaults.font.size = 11;
Chart.defaults.maintainAspectRatio = false;
const GRID = C.grid;
const charts = {};

// Build an axis labeler for the current series. Day buckets show MM-DD; finer
// buckets ("YYYY-MM-DDTHH:MM") show HH:MM, prefixed with MM-DD only when the
// window spans more than one day so same-day windows stay uncluttered.
function makeBucketLabel(ts, unit) {
  if (unit === "day") return (b) => b.bucket.slice(5);
  const dayOf = (b) => b.bucket.slice(0, 10);
  const multiDay = ts.length > 1 && dayOf(ts[0]) !== dayOf(ts[ts.length - 1]);
  return (b) => (multiDay ? `${b.bucket.slice(5, 10)} ${b.bucket.slice(11)}` : b.bucket.slice(11));
}
const baseScales = (stacked) => ({
  x: { grid: { color: GRID }, ticks: { maxRotation: 0, autoSkip: true, maxTicksLimit: 12 }, stacked },
  y: { grid: { color: GRID }, beginAtZero: true, stacked },
});
const baseOpts = (stacked) => ({
  responsive: true,
  interaction: { mode: "index", intersect: false },
  plugins: {
    legend: { display: true, labels: { boxWidth: 10, boxHeight: 10, padding: 12 } },
    tooltip: {
      // Warm ink tooltip to sit with the paper ground.
      backgroundColor: "rgba(28, 26, 22, .92)",
      titleFont: { weight: 600 },
      bodyFont: { size: 12 },
      padding: 10,
      cornerRadius: 8,
      displayColors: true,
    },
  },
  scales: baseScales(stacked),
  // Subtle animation on data refresh: transitions feel alive but finish fast.
  animation: { duration: 350, easing: "easeOutQuart" },
});

function upsert(id, config) {
  if (charts[id]) {
    charts[id].data = config.data;
    charts[id].options = config.options;
    charts[id].update();
  } else {
    charts[id] = new Chart(document.getElementById(id), config);
  }
}

function renderTimeCharts(ts, unit) {
  const labels = ts.map(makeBucketLabel(ts, unit));
  const auto = state.bucket === "auto" ? "auto · " : "";
  document.getElementById("reqHint").textContent = `${auto}${BUCKET_LABEL[unit] || unit} · ${ts.length} pts`;

  upsert("chartRequests", {
    type: "bar",
    data: {
      labels,
      datasets: [
        { label: "ok", data: ts.map((b) => b.requests - b.errors), backgroundColor: C.teal, borderRadius: 3, hoverBackgroundColor: C.teal },
        { label: "errors", data: ts.map((b) => b.errors), backgroundColor: C.rose, borderRadius: 3, hoverBackgroundColor: C.rose },
      ],
    },
    options: baseOpts(true),
  });

  // Input is split into its cached vs uncached parts and stacked under output,
  // so the purple cached segment's share of each bar reads as the hit rate at a
  // glance; the exact cache hit-rate (%) rides a right-hand 0–100 axis as a line.
  // Cached reuses the "Cache hit" KPI's purple to tie the two together.
  const tokOpts = baseOpts(true);
  tokOpts.scales.y1 = {
    position: "right", beginAtZero: true, min: 0, max: 100,
    grid: { drawOnChartArea: false },
    ticks: { maxTicksLimit: 5, callback: (v) => v + "%" },
  };
  tokOpts.plugins.tooltip.callbacks = {
    label: (c) => c.dataset.label === "hit-rate"
      ? ` hit-rate: ${(c.raw ?? 0).toFixed(1)}%`
      : ` ${c.dataset.label}: ${fmtInt(c.raw)}`,
  };
  upsert("chartTokens", {
    type: "bar",
    data: {
      labels,
      datasets: [
        { label: "cached", data: ts.map((b) => b.cached_tokens), backgroundColor: C.purple, borderRadius: 3, stack: "tok", hoverBackgroundColor: C.purple },
        { label: "uncached", data: ts.map((b) => Math.max(0, b.input_tokens - b.cached_tokens)), backgroundColor: C.blue, borderRadius: 3, stack: "tok", hoverBackgroundColor: C.blue },
        { label: "output", data: ts.map((b) => b.output_tokens), backgroundColor: C.green, borderRadius: 3, stack: "tok", hoverBackgroundColor: C.green },
        { type: "line", label: "hit-rate", data: ts.map((b) => (b.input_tokens > 0 ? (100 * b.cached_tokens) / b.input_tokens : null)), borderColor: C.amber, backgroundColor: "transparent", yAxisID: "y1", tension: 0.3, pointRadius: 0, borderWidth: 2, spanGaps: true, hoverBorderWidth: 3 },
      ],
    },
    options: tokOpts,
  });

  upsert("chartLatency", {
    type: "line",
    data: {
      labels,
      datasets: [
        { label: "avg duration", data: ts.map((b) => b.avg_duration_ms), borderColor: C.amber, backgroundColor: "rgba(187,154,85,.12)", fill: true, tension: 0.3, pointRadius: 0, borderWidth: 2, hoverBorderWidth: 3 },
        { label: "avg TTFT", data: ts.map((b) => b.avg_ttft_ms), borderColor: C.blue, backgroundColor: "transparent", tension: 0.3, pointRadius: 0, borderWidth: 2, hoverBorderWidth: 3 },
      ],
    },
    options: baseOpts(false),
  });

  upsert("chartThroughput", {
    type: "line",
    data: {
      labels,
      datasets: [
        { label: "e2e tok/s", data: ts.map((b) => b.e2e_tok_per_sec), borderColor: C.green, backgroundColor: "rgba(111,149,118,.12)", fill: true, tension: 0.3, pointRadius: 0, borderWidth: 2, hoverBorderWidth: 3 },
        { label: "post-TTFT tok/s", data: ts.map((b) => b.post_ttft_tok_per_sec), borderColor: C.purple, backgroundColor: "transparent", borderDash: [4, 3], tension: 0.3, pointRadius: 0, borderWidth: 2, hoverBorderWidth: 3 },
      ],
    },
    options: baseOpts(false),
  });
}

function renderShare(byModel) {
  const top = byModel.slice(0, 12);
  upsert("chartShare", {
    type: "doughnut",
    data: {
      labels: top.map((m) => m.upstream_model),
      datasets: [{
        data: top.map((m) => m.total_tokens),
        backgroundColor: top.map((m) => colorFor(m.upstream_model)),
        borderColor: C.panel,
        borderWidth: 2,
        // Slight outward shift on hover for each segment.
        hoverBorderWidth: 3,
        hoverBorderColor: "rgba(28,26,22,.12)",
      }],
    },
    options: {
      responsive: true,
      cutout: "58%",
      animation: { animateRotate: true, animateScale: true, duration: 500, easing: "easeOutQuart" },
      plugins: {
        legend: { position: "right", labels: { boxWidth: 10, boxHeight: 10, padding: 8, font: { size: 11 } } },
        tooltip: {
          backgroundColor: "rgba(28, 26, 22, .92)",
          callbacks: { label: (c) => ` ${c.label}: ${fmtInt(c.raw)} (${((c.raw / c.dataset.data.reduce((a, b) => a + b, 0)) * 100 || 0).toFixed(1)}%)` },
        },
      },
    },
  });
}

// ---- sortable tables ----
// onRowClick (optional) makes each data row clickable, called with the row datum.
function renderTable(tableId, columns, rows, sortKey, sortDir, onSort, onRowClick) {
  const table = document.getElementById(tableId);
  const thead = table.tHead || table.createTHead();
  thead.innerHTML = "";
  const tr = thead.insertRow();
  columns.forEach((c) => {
    const th = document.createElement("th");
    th.className = c.text ? "text" : "";
    const arrow = c.key === sortKey ? `<span class="arrow">${sortDir === "asc" ? "▲" : "▼"}</span>` : "";
    th.innerHTML = `${c.label} ${arrow}`;
    if (c.sortable !== false) th.onclick = () => onSort(c.key);
    tr.appendChild(th);
  });

  const tbody = table.tBodies[0] || table.createTBody();
  if (!rows.length) {
    tbody.innerHTML = `<tr class="empty-row"><td colspan="${columns.length}">No data for this filter.</td></tr>`;
    return;
  }
  const sorted = [...rows];
  if (sortKey) {
    const col = columns.find((c) => c.key === sortKey);
    const get = (r) => (col && col.sortVal ? col.sortVal(r) : r[sortKey]);
    sorted.sort((a, b) => {
      const x = get(a), y = get(b);
      if (typeof x === "number" && typeof y === "number") return sortDir === "asc" ? x - y : y - x;
      return sortDir === "asc" ? String(x).localeCompare(String(y)) : String(y).localeCompare(String(x));
    });
  }
  tbody.innerHTML = sorted.map((r, i) => {
    const open = onRowClick ? ` class="clickable" data-i="${i}"` : "";
    return `<tr${open}>` + columns.map((c) => {
      const cls = c.text ? "text" : c.cls || "";
      return `<td class="${cls}">${c.render(r)}</td>`;
    }).join("") + "</tr>";
  }).join("");
  if (onRowClick) {
    tbody.querySelectorAll("tr.clickable").forEach((tr) => {
      tr.onclick = () => {
        // Don't hijack a click the user made to select cell text.
        if (window.getSelection && String(window.getSelection())) return;
        onRowClick(sorted[+tr.dataset.i]);
      };
    });
  }
}

// by-model table
const byModelSort = { key: "total_tokens", dir: "desc" };
let byModelData = [];
const byModelCols = [
  { key: "upstream_model", label: "Model", text: true, cls: "model", render: (r) => `<span class="dot" style="background:${colorFor(r.upstream_model)}"></span>${r.upstream_model ? escapeHtml(r.upstream_model) : "—"}` },
  { key: "requests", label: "Reqs", render: (r) => `<span class="num-strong">${fmtInt(r.requests)}</span>` },
  { key: "error_pct", label: "Err%", render: (r) => r.errors ? `<span class="pill err">${fmtPct(r.error_pct)}</span>` : `<span class="muted">0%</span>` },
  { key: "input_tokens", label: "Input", render: (r) => fmtInt(r.input_tokens) },
  { key: "output_tokens", label: "Output", render: (r) => fmtInt(r.output_tokens) },
  { key: "cached_tokens", label: "Cached", render: (r) => fmtInt(r.cached_tokens) },
  { key: "total_tokens", label: "Total", render: (r) => `<span class="num-strong">${fmtInt(r.total_tokens)}</span>` },
  { key: "cache_pct", label: "Cache%", render: (r) => cacheCell(r.input_tokens > 0 ? r.cache_pct : null) },
  { key: "avg_duration_ms", label: "Avg dur", render: (r) => fmtDur(r.avg_duration_ms) },
  { key: "avg_ttft_ms", label: "Avg TTFT", render: (r) => fmtDur(r.avg_ttft_ms) },
  { key: "e2e_tok_per_sec", label: "Tok/s", render: (r) => fmtRate(r.e2e_tok_per_sec) },
];
function renderByModel(rows) {
  byModelData = rows;
  document.getElementById("byModelHint").textContent = `${rows.length} models`;
  drawByModel();
}
function drawByModel() {
  renderTable("byModelTable", byModelCols, byModelData, byModelSort.key, byModelSort.dir, (k) => {
    byModelSort.dir = byModelSort.key === k && byModelSort.dir === "desc" ? "asc" : "desc";
    byModelSort.key = k;
    drawByModel();
  });
}

// requests table
const reqSort = { key: "time", dir: "desc" };
let reqData = [];
function statusPill(code) {
  if (!code) return `<span class="muted">—</span>`;
  return `<span class="pill ${code >= 200 && code < 300 ? "ok" : "err"}">${code}</span>`;
}
function tokPerSec(r) {
  return r.duration_ms > 0 ? (1000 * r.output_tokens / r.duration_ms).toFixed(1) : "—";
}
const reqCols = [
  { key: "time", label: "Time", text: true, render: (r) => new Date(r.time).toLocaleString("en-GB", { hour12: false }), sortVal: (r) => r.time },
  { key: "client_protocol", label: "Proto", text: true, render: (r) => `<span class="pill proto">${(r.client_protocol || "").replace(/^(openai_|anthropic_)/, "")}</span>` },
  { key: "upstream_model", label: "Model", text: true, cls: "model", render: (r) => `<span class="dot" style="background:${colorFor(r.upstream_model)}"></span>${r.upstream_model ? escapeHtml(r.upstream_model) : "—"}` },
  { key: "client_ua", label: "UA", text: true, render: (r) => r.client_ua ? `<span class="ua" title="${escapeAttr(r.client_ua)}">${escapeHtml(r.client_ua)}</span>` : `<span class="muted">—</span>` },
  { key: "stream", label: "Stream", render: (r) => r.stream ? `<span class="muted">SSE</span>` : `<span class="muted">—</span>` },
  { key: "upstream_status", label: "Status", render: (r) => statusPill(r.upstream_status || r.client_status), sortVal: (r) => r.upstream_status || r.client_status },
  { key: "duration_ms", label: "Dur", render: (r) => fmtDur(r.duration_ms) },
  { key: "ttft_ms", label: "TTFT", render: (r) => r.ttft_ms != null ? fmtDur(r.ttft_ms) : `<span class="muted">—</span>`, sortVal: (r) => r.ttft_ms ?? -1 },
  { key: "input_tokens", label: "In", render: (r) => fmtInt(r.input_tokens) },
  { key: "output_tokens", label: "Out", render: (r) => fmtInt(r.output_tokens) },
  { key: "cached_tokens", label: "Cached", render: (r) => fmtInt(r.cached_tokens) },
  { key: "total_tokens", label: "Total", render: (r) => fmtInt(r.total_tokens) },
  { key: "cache_pct", label: "Cache%", render: (r) => cacheCell(reqCachePct(r)), sortVal: (r) => reqCachePct(r) ?? -1 },
  { key: "tps", label: "Tok/s", render: (r) => tokPerSec(r), sortVal: (r) => r.duration_ms > 0 ? 1000 * r.output_tokens / r.duration_ms : -1 },
  { key: "error", label: "Error", text: true, sortable: false, render: (r) => r.error ? `<span class="err-text" title="${escapeAttr(r.error)}">${escapeHtml(r.error)}</span>` : "" },
];
function renderRequests(rows) {
  reqData = rows;
  drawRequests();
}
function drawRequests() {
  renderTable("requestsTable", reqCols, reqData, reqSort.key, reqSort.dir, (k) => {
    reqSort.dir = reqSort.key === k && reqSort.dir === "desc" ? "asc" : "desc";
    reqSort.key = k;
    drawRequests();
  }, openReqModal);
}

// ---- request detail modal ----
// Clicking a request row opens a read-only detail panel with every captured
// field, grouped into sections and reusing the table's formatters/pills.
function dRow(k, v) { return `<div class="d-row"><span class="d-key">${k}</span><span class="d-val">${v}</span></div>`; }
function modelDot(m) { return m ? `<span class="dot" style="background:${colorFor(m)}"></span>${escapeHtml(m)}` : `<span class="muted">—</span>`; }
function openReqModal(r) {
  const rows = [
    `<div class="d-sec">Request</div>`,
    dRow("Time", escapeHtml(new Date(r.time).toLocaleString("en-GB", { hour12: false }))),
    dRow("Protocol", `<span class="pill proto">${escapeHtml(r.client_protocol || "—")}</span>`),
    dRow("Stream", r.stream ? "SSE" : `<span class="muted">no</span>`),
    dRow("Client model", modelDot(r.client_model)),
    dRow("Upstream model", modelDot(r.upstream_model)),
    dRow("Upstream URL", r.upstream_url ? `<span class="d-mono">${escapeHtml(r.upstream_url)}</span>` : `<span class="muted">—</span>`),
    dRow("User-Agent", r.client_ua ? `<span class="d-mono">${escapeHtml(r.client_ua)}</span>` : `<span class="muted">—</span>`),
    `<div class="d-sec">Status &amp; timing</div>`,
    dRow("Status", `${statusPill(r.client_status)} <span class="d-arrow">→</span> ${statusPill(r.upstream_status)}`),
    dRow("Duration", fmtDur(r.duration_ms)),
    dRow("TTFT", r.ttft_ms != null ? fmtDur(r.ttft_ms) : `<span class="muted">—</span>`),
    dRow("Throughput", `${tokPerSec(r)} <span class="muted">tok/s</span>`),
    `<div class="d-sec">Tokens</div>`,
    dRow("Input", fmtInt(r.input_tokens)),
    dRow("Output", fmtInt(r.output_tokens)),
    dRow("Cached", fmtInt(r.cached_tokens)),
    dRow("Total", `<span class="num-strong">${fmtInt(r.total_tokens)}</span>`),
    dRow("Cache hit", cacheCell(reqCachePct(r))),
  ];
  if (r.error) rows.push(`<div class="d-sec">Error</div>`, `<div class="d-err">${escapeHtml(r.error)}</div>`);
  document.getElementById("reqModalBody").innerHTML = rows.join("");
  const m = document.getElementById("reqModal");
  m.classList.add("open");
  m.setAttribute("aria-hidden", "false");
}
function closeReqModal() {
  const m = document.getElementById("reqModal");
  m.classList.remove("open");
  m.setAttribute("aria-hidden", "true");
}
function escapeHtml(s) { return String(s).replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c])); }
function escapeAttr(s) { return escapeHtml(s).replace(/"/g, "&quot;"); }

// ---- multiselect filter ----
function buildMultiselect(containerId, label, options, selected, onChange, swatch) {
  const el = document.getElementById(containerId);
  el.classList.add("ms");
  const count = selected.length;
  el.innerHTML = `
    <div class="ms-toggle">
      <span class="label">${label}</span>
      <span class="count ${count ? "" : "zero"}">${count || "all"}</span>
      <span class="caret">▼</span>
    </div>
    <div class="ms-panel">
      <div class="ms-actions"><button data-act="all">all</button><button data-act="none">clear</button></div>
      ${options.length ? options.map((o) => `
        <label class="ms-opt">
          <input type="checkbox" value="${escapeAttr(o)}" ${selected.includes(o) ? "checked" : ""} />
          ${swatch ? `<span class="swatch" style="background:${colorFor(o)}"></span>` : ""}
          <span class="name" title="${escapeAttr(o)}">${escapeHtml(o)}</span>
        </label>`).join("") : `<div class="ms-empty">none</div>`}
    </div>`;

  const panel = el.querySelector(".ms-panel");
  el.querySelector(".ms-toggle").onclick = (e) => {
    e.stopPropagation();
    document.querySelectorAll(".ms-panel.open").forEach((p) => { if (p !== panel) p.classList.remove("open"); });
    panel.classList.toggle("open");
  };
  panel.onclick = (e) => e.stopPropagation();
  panel.querySelectorAll("input").forEach((cb) => {
    cb.onchange = () => {
      const vals = [...panel.querySelectorAll("input:checked")].map((c) => c.value);
      onChange(vals);
    };
  });
  panel.querySelector('[data-act="all"]').onclick = () => { onChange(options.slice()); };
  panel.querySelector('[data-act="none"]').onclick = () => { onChange([]); };
}

document.addEventListener("click", () => {
  document.querySelectorAll(".ms-panel.open").forEach((p) => p.classList.remove("open"));
});

// ---- load ----
let optionsCache = { models: [], protocols: [], minTime: null, maxTime: null };
function rebuildFilters() {
  buildMultiselect("modelFilter", "model", optionsCache.models, state.models, (vals) => {
    state.models = vals; rebuildFilters(); loadAll();
  }, true);
  buildMultiselect("protocolFilter", "protocol", optionsCache.protocols, state.protocols, (vals) => {
    state.protocols = vals; rebuildFilters(); loadAll();
  }, false);
}

async function loadOptions() {
  try {
    const o = await fetch(`${API}/options`).then((r) => r.json());
    optionsCache = { models: o.models || [], protocols: o.protocols || [], minTime: o.min_time || null, maxTime: o.max_time || null };
    o.models?.forEach(colorFor); // assign stable colors by global order
    rebuildFilters();
  } catch (e) {
    toast("options: " + e.message);
  }
}

let inFlight = 0;
async function loadAll() {
  const btn = document.getElementById("refreshBtn");
  btn.classList.add("spin");
  setTimeout(() => btn.classList.remove("spin"), 600);
  const status = document.getElementById("statusLine");
  status.textContent = "loading…";
  const seq = ++inFlight;

  const { from, to } = rangeBounds();
  const bucket = effectiveBucket(from, to);
  try {
    const [summary, ts, byModel, requests] = await Promise.all([
      api("summary"),
      api("timeseries", { bucket }),
      api("by_model"),
      api("requests", { limit: state.limit, errors_only: state.errorsOnly ? 1 : 0 }),
    ]);
    if (seq !== inFlight) return; // a newer load superseded this one
    renderKPIs(summary, ts);
    renderTimeCharts(ts, bucket);
    renderByModel(byModel);
    renderShare(byModel);
    renderRequests(requests);
    status.textContent = `updated ${new Date().toLocaleTimeString("en-GB", { hour12: false })}`;
  } catch (e) {
    if (seq === inFlight) status.textContent = "error";
    toast(e.message);
  }
}

// ---- wiring ----
function setRange(range) {
  state.range = range;
  document.querySelectorAll("#rangePresets button").forEach((b) => b.classList.toggle("active", b.dataset.range === range));
  if (range !== "custom" && range !== "all") {
    const { from, to } = rangeBounds();
    document.getElementById("fromInput").value = toLocalInput(from);
    document.getElementById("toInput").value = toLocalInput(to);
  }
}

let autoTimer = null;
function setupEvents() {
  document.querySelectorAll("#rangePresets button").forEach((b) => {
    b.onclick = () => { setRange(b.dataset.range); loadAll(); };
  });
  const fromI = document.getElementById("fromInput");
  const toI = document.getElementById("toInput");
  const onCustom = () => {
    state.range = "custom";
    document.querySelectorAll("#rangePresets button").forEach((b) => b.classList.remove("active"));
    state.from = fromI.value ? new Date(fromI.value).getTime() : null;
    state.to = toI.value ? new Date(toI.value).getTime() : null;
    loadAll();
  };
  fromI.onchange = onCustom;
  toI.onchange = onCustom;

  document.querySelectorAll("#bucketSeg button").forEach((b) => {
    b.onclick = () => {
      state.bucket = b.dataset.bucket;
      document.querySelectorAll("#bucketSeg button").forEach((x) => x.classList.toggle("active", x === b));
      loadAll();
    };
  });

  document.getElementById("errorsOnly").onchange = (e) => { state.errorsOnly = e.target.checked; loadAll(); };
  document.getElementById("limitSelect").onchange = (e) => { state.limit = +e.target.value; loadAll(); };
  document.getElementById("refreshBtn").onclick = () => loadAll();

  document.querySelector("#reqModal .modal-close").onclick = closeReqModal;
  document.querySelector("#reqModal .modal-backdrop").onclick = closeReqModal;
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") closeReqModal(); });

  document.getElementById("autoRefresh").onchange = (e) => {
    state.auto = e.target.checked;
    if (autoTimer) { clearInterval(autoTimer); autoTimer = null; }
    if (state.auto) autoTimer = setInterval(loadAll, 30000);
  };
}

// ---- Init ----
async function init() {
  renderKPISkeletons();
  setRange("24h");
  setupEvents();

  await loadOptions();
  await loadAll();
  // First load done: drop the skeletons, then let the one-shot entrance settle.
  document.body.classList.remove("loading");
  setTimeout(() => document.body.classList.remove("intro"), 1200);
}
init();
