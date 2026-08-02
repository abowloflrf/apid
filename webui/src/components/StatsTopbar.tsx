import { useEffect, useState } from "react";
import { toLocalInput } from "../format";
import { rangeBounds, type StatsQuery } from "../useStatsQuery";
import { Multiselect } from "./Multiselect";
import { Condensed } from "../views/StatsView";

const RANGES: [string, string][] = [
  ["1h", "1h"], ["6h", "6h"], ["24h", "24h"], ["today", "Today"],
  ["7d", "7d"], ["30d", "30d"], ["all", "All"],
];
const BUCKETS: [string, string][] = [["auto", "auto"], ["15min", "15m"], ["hour", "1h"], ["day", "1d"]];

interface Props {
  view: string;
  onView: (v: string) => void;
  q: StatsQuery;
  liveCount: number;
  onRefresh: () => void;
}

export function StatsTopbar({ view, onView, q, liveCount, onRefresh }: Props) {
  const [fromInput, setFromInput] = useState("");
  const [toInput, setToInput] = useState("");
  const [spinning, setSpinning] = useState(false);

  // Keep the custom-range inputs in sync with preset selections.
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
    q.loadAll({ range: r, offset: 0 });
  };

  const applyCustom = () => {
    const from = fromInput ? new Date(fromInput).getTime() : null;
    const to = toInput ? new Date(toInput).getTime() : null;
    q.setRange("custom");
    q.setCustom(from, to);
    q.loadAll({ range: "custom", customFrom: from, customTo: to, offset: 0 });
  };

  const handleRefresh = () => {
    setSpinning(true);
    window.setTimeout(() => setSpinning(false), 600);
    onRefresh();
  };

  return (
    <div className="topbars">
      <header className="topbar">
        <div className="brand">
          <span className="logo">apid</span>
          <span className="sub" id="viewLabel">{view}</span>
        </div>
        <div className="seg views" id="viewTabs">
          <button className={view === "stats" ? "active" : ""} onClick={() => onView("stats")}>stats</button>
          <button className={view === "live" ? "active" : ""} onClick={() => onView("live")}>
            live<span className="tab-badge" hidden={!liveCount}>{liveCount}</span>
          </button>
          <button className={view === "routes" ? "active" : ""} onClick={() => onView("routes")}>routes</button>
        </div>
        <div className="ranges" id="rangePresets">
          {RANGES.map(([val, label]) => (
            <button key={val} className={q.range === val ? "active" : ""} onClick={() => applyRange(val)}>{label}</button>
          ))}
        </div>
        <div className="custom-range">
          <input type="datetime-local" title="From" value={fromInput} onChange={(e) => setFromInput(e.target.value)} onBlur={applyCustom} />
          <span className="dash">→</span>
          <input type="datetime-local" title="To" value={toInput} onChange={(e) => setToInput(e.target.value)} onBlur={applyCustom} />
        </div>
        <div className="spacer" />
        <label className="auto" title="Auto refresh every 30s">
          <input type="checkbox" checked={q.auto} onChange={(e) => q.setAuto(e.target.checked)} /> auto
        </label>
        <button className={`refresh${spinning ? " spin" : ""}`} title="Refresh" onClick={handleRefresh}>⟳</button>
      </header>

      <section className="filters">
        <div className="filter" id="modelFilter">
          <Multiselect
            label="model"
            options={q.options.models}
            selected={q.models}
            onChange={(v) => { q.setModels(v); q.loadAll({ models: v, offset: 0 }); }}
            swatch
          />
        </div>
        <div className="filter" id="protocolFilter">
          <Multiselect
            label="protocol"
            options={q.options.protocols}
            selected={q.protocols}
            onChange={(v) => { q.setProtocols(v); q.loadAll({ protocols: v, offset: 0 }); }}
          />
        </div>
        <div className="seg" id="bucketSeg" title="Time-series granularity">
          {BUCKETS.map(([val, label]) => (
            <button key={val} className={q.bucket === val ? "active" : ""} onClick={() => { q.setBucket(val); q.loadAll({ bucket: val }); }}>{label}</button>
          ))}
        </div>
        <div className="spacer" />
        <div className="status-kpis" id="kpiCondensed">
          <Condensed s={q.summary} />
        </div>
        <span className="status" id="statusLine">{q.status}</span>
      </section>
    </div>
  );
}
