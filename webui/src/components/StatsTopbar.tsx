import { useEffect, useState } from "react";
import { toLocalInput } from "../format";
import { rangeBounds, type StatsQuery } from "../useStatsQuery";
import { Condensed } from "../views/StatsView";
import { Multiselect } from "./Multiselect";

const RANGES: [string, string][] = [
  ["1h", "1h"],
  ["6h", "6h"],
  ["24h", "24h"],
  ["today", "Today"],
  ["7d", "7d"],
  ["30d", "30d"],
  ["all", "All"],
];
const BUCKETS: [string, string][] = [
  ["auto", "auto"],
  ["15min", "15m"],
  ["hour", "1h"],
  ["day", "1d"],
];

interface DashboardTopbarProps {
  view: string;
  onView: (v: string) => void;
  liveCount: number;
}

export function DashboardTopbar({ view, onView, liveCount }: DashboardTopbarProps) {
  return (
    <header className="topbar">
      <div className="brand">
        <span className="logo">apid</span>
        <span className="sub" id="viewLabel">
          {view}
        </span>
      </div>
      <div className="seg views" id="viewTabs">
        <button
          type="button"
          className={view === "stats" ? "active" : ""}
          onClick={() => onView("stats")}
        >
          stats
        </button>
        <button
          type="button"
          className={view === "live" ? "active" : ""}
          onClick={() => onView("live")}
        >
          live
          <span className="tab-badge" hidden={!liveCount}>
            {liveCount}
          </span>
        </button>
        <button
          type="button"
          className={view === "routes" ? "active" : ""}
          onClick={() => onView("routes")}
        >
          routes
        </button>
        <button
          type="button"
          className={view === "sessions" ? "active" : ""}
          onClick={() => onView("sessions")}
        >
          sessions
        </button>
      </div>
    </header>
  );
}

export function StatsControls({ q }: { q: StatsQuery }) {
  const [fromInput, setFromInput] = useState("");
  const [toInput, setToInput] = useState("");
  const [spinning, setSpinning] = useState(false);
  const [filtersOpen, setFiltersOpen] = useState(false);
  const activeFilters =
    q.models.length + q.protocols.length + (q.bucket === "auto" ? 0 : 1);

  useEffect(() => {
    if (q.range === "custom" || q.range === "all") return;
    const { from, to } = rangeBounds(q);
    if (from != null && to != null) {
      setFromInput(toLocalInput(from));
      setToInput(toLocalInput(to));
    }
  }, [q.range, q.customFrom, q.customTo]);

  const applyRange = (r: string) => {
    q.setRange(r);
    if (r !== "custom" && r !== "all") {
      const { from, to } = rangeBounds({ ...q, range: r });
      if (from != null && to != null) {
        setFromInput(toLocalInput(from));
        setToInput(toLocalInput(to));
      }
    }
    void q.loadAll({ range: r, offset: 0 });
  };

  const applyCustom = () => {
    const from = fromInput ? new Date(fromInput).getTime() : null;
    const to = toInput ? new Date(toInput).getTime() : null;
    q.setRange("custom");
    q.setCustom(from, to);
    void q.loadAll({ range: "custom", customFrom: from, customTo: to, offset: 0 });
  };

  const handleRefresh = () => {
    setSpinning(true);
    window.setTimeout(() => setSpinning(false), 600);
    void q.loadAll();
  };

  return (
    <section className={`filters${filtersOpen ? "" : " collapsed"}`}>
      <button
        type="button"
        className="filters-toggle"
        onClick={() => setFiltersOpen((o) => !o)}
      >
        Filters
        {activeFilters > 0 ? <span className="tab-badge">{activeFilters}</span> : null}
      </button>
      <div className="ranges" id="rangePresets">
        {RANGES.map(([val, label]) => (
          <button
            type="button"
            key={val}
            className={q.range === val ? "active" : ""}
            onClick={() => applyRange(val)}
          >
            {label}
          </button>
        ))}
      </div>
      <div className="custom-range">
        <input
          type="datetime-local"
          title="From"
          value={fromInput}
          onChange={(e) => setFromInput(e.target.value)}
          onBlur={applyCustom}
        />
        <span className="dash">→</span>
        <input
          type="datetime-local"
          title="To"
          value={toInput}
          onChange={(e) => setToInput(e.target.value)}
          onBlur={applyCustom}
        />
      </div>
      <div className="filter" id="modelFilter">
        <Multiselect
          label="model"
          options={q.options.models}
          selected={q.models}
          onChange={(v) => {
            q.setModels(v);
            void q.loadAll({ models: v, offset: 0 });
          }}
          swatch
        />
      </div>
      <div className="filter" id="protocolFilter">
        <Multiselect
          label="protocol"
          options={q.options.protocols}
          selected={q.protocols}
          onChange={(v) => {
            q.setProtocols(v);
            void q.loadAll({ protocols: v, offset: 0 });
          }}
        />
      </div>
      <div className="seg" id="bucketSeg" title="Time-series granularity">
        {BUCKETS.map(([val, label]) => (
          <button
            type="button"
            key={val}
            className={q.bucket === val ? "active" : ""}
            onClick={() => {
              q.setBucket(val);
              void q.loadAll({ bucket: val });
            }}
          >
            {label}
          </button>
        ))}
      </div>
      <div className="spacer" />
      <div className="status-kpis" id="kpiCondensed">
        <Condensed s={q.summary} />
      </div>
      <span className="status" id="statusLine">
        {q.status}
      </span>
      <label className="auto" title="Auto refresh every 30s">
        <input
          type="checkbox"
          checked={q.auto}
          onChange={(e) => q.setAuto(e.target.checked)}
        />{" "}
        auto
      </label>
      <button
        type="button"
        className={`refresh${spinning ? " spin" : ""}`}
        title="Refresh stats"
        aria-label="Refresh stats"
        onClick={handleRefresh}
      >
        ⟳
      </button>
    </section>
  );
}
