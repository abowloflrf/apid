import { useCallback, useEffect, useRef, useState } from "react";
import { fetchJSON } from "../api";
import { upColorFor } from "../colors";
import { toast } from "../components/Toast";
import type { RouteInfo, RuleInfo, Topology, UpstreamInfo } from "../types";

const PROTO_SHORT: Record<string, string> = {
  openai_responses: "responses",
  openai_chat_completions: "chat",
  anthropic_messages: "anthropic",
};
const protoShort = (p: string): string => PROTO_SHORT[p] || p;

const AUTH_LABEL: Record<string, string> = {
  api_key: "own api_key",
  passthrough: "client credentials",
  stripped: "none — client key is stripped",
};

function Chip({ label, value, on }: { label: string; value: string; on: boolean }) {
  return (
    <span className={`topo-chip ${on ? "on" : "off"}`}>
      <span className="k">{label}</span>
      <span className="v">{value}</span>
    </span>
  );
}

function ModelTag({ r }: { r: RuleInfo }) {
  if (r.model_source === "passthrough") {
    return <span className="model-tag pass" title="client's model is forwarded unchanged">client model</span>;
  }
  const from = r.model_source === "rule" ? "rewritten by this rule" : "inherited from the upstream's model";
  return (
    <span className="model-tag" title={from}>
      → <b>{r.effective_model}</b>
    </span>
  );
}

interface RuleRowProps {
  r: RuleInfo;
  keyId: string;
  onHover: (sel: { key: string; up: string } | null) => void;
}

function RuleRow({ r, keyId, onHover }: RuleRowProps) {
  if (r.broken) {
    return (
      <li className="rule broken">
        <span className="match">{r.match || "*"}</span>
        <span className="spacer" />
        <span className="model-tag">undefined upstream <b>{r.upstream}</b></span>
      </li>
    );
  }
  const modeTitle = r.mode === "convert"
    ? `converted: ${r.upstream_protocol} spoken upstream`
    : r.via_responses
      ? "supports_responses: responses forwarded natively, no conversion"
      : "same protocol on both ends — bytes forwarded as-is";
  return (
    <li
      className="rule"
      data-key={keyId}
      data-up={r.upstream}
      onMouseEnter={() => onHover({ key: keyId, up: r.upstream })}
      onMouseLeave={() => onHover(null)}
    >
      <span className={`match ${r.match_kind === "catchall" ? "catchall" : ""}`}>{r.match || "*"}</span>
      <span className="kind">{r.match_kind}</span>
      <span className="spacer" />
      <ModelTag r={r} />
      <span className={`mode ${r.mode}`} title={modeTitle}>{r.mode}</span>
      <span className="up-name">{r.upstream}</span>
      <span className="anchor" style={{ background: upColorFor(r.upstream) }} />
    </li>
  );
}

function RouteCard({ rt, i, onHover }: { rt: RouteInfo; i: number; onHover: RuleRowProps["onHover"] }) {
  return (
    <article className="topo-card route">
      <div className="tc-head">
        <span className="method">POST</span>
        <span className="path">{rt.path}</span>
        <span className="pill proto" title={rt.input_protocol}>{protoShort(rt.input_protocol)}</span>
      </div>
      <ul className="rules">
        {rt.rules.map((r, j) => (
          <RuleRow key={`${i}-${j}`} r={r} keyId={`${i}-${j}`} onHover={onHover} />
        ))}
      </ul>
    </article>
  );
}

function UpstreamCard({ u, onHover }: { u: UpstreamInfo; onHover: (sel: { up: string } | null) => void }) {
  const auth = AUTH_LABEL[u.auth] || u.auth;
  const dualPill = u.supports_responses
    ? <span className="pill dual" title="same upstream also speaks openai_responses (supports_responses = true)">+responses</span>
    : null;
  return (
    <article
      className="topo-card upstream"
      data-up={u.name}
      onMouseEnter={() => onHover({ up: u.name })}
      onMouseLeave={() => onHover(null)}
    >
      <div className="tc-head">
        <span className="dot" style={{ background: upColorFor(u.name) }} />
        <span className="name">{u.name}</span>
        <span className="pill proto" title={u.protocol}>{protoShort(u.protocol)}</span>
        {dualPill}
      </div>
      <div className="tc-endpoint" title={u.endpoint}>{u.endpoint}</div>
      {u.supports_responses ? (
        <div className="tc-endpoint alt" title="OpenAI Responses endpoint; openai_responses rules forward here">
          responses: {u.responses_endpoint || ""}
        </div>
      ) : null}
      <div className="tc-meta">
        <span><span className="k">model</span> <code>{u.model || "passthrough"}</code></span>
        <span><span className="k">auth</span> <code title={u.auth_header}>{auth}</code></span>
        <span><span className="k">rules</span> <code>{u.ref_count}</code></span>
      </div>
    </article>
  );
}

interface AnchorPoint {
  key: string;
  up: string;
  x: number;
  y: number;
  tx: number;
  ty: number;
}

function topoAnchorPoints(graph: HTMLElement): AnchorPoint[] {
  const gb = graph.getBoundingClientRect();
  const rules = [...graph.querySelectorAll<HTMLElement>(".rule[data-up]")];
  const byUp = new Map<string, { el: HTMLElement; up: string; key: string; x: number; y: number }[]>();
  for (const li of rules) {
    const a = li.querySelector<HTMLElement>(".anchor");
    if (!a) continue;
    const ar = a.getBoundingClientRect();
    const item = { el: li, up: li.dataset.up || "", key: li.dataset.key || "", x: ar.right - gb.left, y: ar.top + ar.height / 2 - gb.top };
    if (!byUp.has(item.up)) byUp.set(item.up, []);
    byUp.get(item.up)!.push(item);
  }
  const out: AnchorPoint[] = [];
  for (const [up, items] of byUp) {
    const card = graph.querySelector<HTMLElement>(`.topo-card.upstream[data-up="${CSS.escape(up)}"]`);
    if (!card) continue;
    const cb = card.getBoundingClientRect();
    items.sort((a, b) => a.y - b.y);
    items.forEach((it, i) => {
      out.push({
        ...it,
        tx: cb.left - gb.left,
        ty: cb.top - gb.top + (cb.height * (i + 1)) / (items.length + 1),
      });
    });
  }
  return out;
}

function drawTopoLinks(graph: HTMLElement, svg: SVGSVGElement, topo: Topology) {
  if (getComputedStyle(svg).display === "none") {
    svg.innerHTML = "";
    return;
  }
  const gb = graph.getBoundingClientRect();
  svg.setAttribute("viewBox", `0 0 ${gb.width} ${gb.height}`);
  svg.setAttribute("width", String(gb.width));
  svg.setAttribute("height", String(gb.height));

  const modes: Record<string, string> = {};
  topo.routes.forEach((rt, i) => rt.rules.forEach((r, j) => { modes[`${i}-${j}`] = r.mode; }));
  svg.innerHTML = topoAnchorPoints(graph).map((p) => {
    const dx = Math.max(40, (p.tx - p.x) * 0.5);
    const d = `M ${p.x} ${p.y} C ${p.x + dx} ${p.y}, ${p.tx - dx} ${p.ty}, ${p.tx} ${p.ty}`;
    const convert = modes[p.key] === "convert";
    return `<path d="${d}" data-key="${p.key}" data-up="${p.up}" stroke="${upColorFor(p.up)}" stroke-width="1.5" opacity=".5" ${convert ? 'stroke-dasharray="5 4"' : ""} />`;
  }).join("");
}

// setTopoFocus dims everything unrelated to one rule or one upstream, so a
// single hover answers "what feeds this backend" / "where does this model go".
function setTopoFocus(graph: HTMLElement, sel: { key?: string; up: string } | null) {
  const paths = [...graph.querySelectorAll<SVGPathElement>("#topoLinks path")];
  const cards = [...graph.querySelectorAll<HTMLElement>(".topo-card.upstream")];
  const rules = [...graph.querySelectorAll<HTMLElement>(".rule[data-up]")];
  if (!sel) {
    paths.forEach((p) => p.classList.remove("hot", "dim"));
    cards.forEach((c) => c.classList.remove("hot", "dim"));
    rules.forEach((r) => r.classList.remove("dim"));
    return;
  }
  const hit = (el: HTMLElement | SVGPathElement): boolean =>
    sel.key ? el.dataset.key === sel.key : el.dataset.up === sel.up;
  paths.forEach((p) => { const on = hit(p); p.classList.toggle("hot", on); p.classList.toggle("dim", !on); });
  cards.forEach((c) => { const on = c.dataset.up === sel.up; c.classList.toggle("hot", on); c.classList.toggle("dim", !on); });
  rules.forEach((r) => r.classList.toggle("dim", !hit(r)));
}

export function TopologyView({ refreshKey }: { refreshKey: number }) {
  const [topo, setTopo] = useState<Topology | null>(null);
  const graphRef = useRef<HTMLElement>(null);
  const svgRef = useRef<SVGSVGElement>(null);
  const hoverRef = useRef<{ key?: string; up: string } | null>(null);

  const load = useCallback(async () => {
    try {
      const t = await fetchJSON<Topology>("topology");
      (t.upstreams || []).forEach((u) => upColorFor(u.name));
      setTopo(t);
    } catch (e) {
      toast("topology: " + (e as Error).message);
    }
  }, []);

  useEffect(() => { void load(); }, [load, refreshKey]);

  // Curve endpoints are measured from laid-out DOM, so anything that reflows the
  // cards (window resize, wrapping text, fonts settling) has to redraw them.
  useEffect(() => {
    if (!topo || !graphRef.current || !svgRef.current) return;
    const draw = () => {
      if (graphRef.current && svgRef.current) {
        drawTopoLinks(graphRef.current, svgRef.current, topo);
        setTopoFocus(graphRef.current, hoverRef.current);
      }
    };
    draw();
    const ro = new ResizeObserver(draw);
    ro.observe(graphRef.current);
    window.addEventListener("resize", draw);
    return () => {
      ro.disconnect();
      window.removeEventListener("resize", draw);
    };
  }, [topo, refreshKey]);

  if (!topo) return <div id="routesView" className="topo-loading">loading topology…</div>;

  const anyDual = (topo.upstreams || []).some((u) => u.supports_responses);
  const search = topo.search
    ? <Chip label="search" value={`${topo.search.provider} ${topo.search.path}`} on />
    : <Chip label="search" value="off" on={false} />;

  return (
    <div id="routesView">
      <section className="topo-bar" id="topoMeta">
        <Chip label="listen" value={topo.listen || "—"} on />
        <Chip label="routes" value={String((topo.routes || []).length)} on />
        <Chip label="upstreams" value={String((topo.upstreams || []).length)} on />
        <Chip label="client auth" value={topo.client_auth ? "on" : "off"} on={topo.client_auth} />
        <Chip label="trace" value={topo.trace ? "on" : "off"} on={topo.trace} />
        <Chip label="storage" value={topo.storage ? "on" : "off"} on={topo.storage} />
        {search}
      </section>
      <section className="topo-graph" id="topoGraph" ref={graphRef}>
        <div className="topo-col" id="topoRoutes">
          {(topo.routes || []).map((rt, i) => (
            <RouteCard key={rt.path} rt={rt} i={i} onHover={(sel) => {
              hoverRef.current = sel;
              if (graphRef.current) setTopoFocus(graphRef.current, sel);
            }} />
          ))}
        </div>
        <svg className="topo-links" id="topoLinks" aria-hidden="true" ref={svgRef} />
        <div className="topo-col" id="topoUpstreams">
          {(topo.upstreams || []).map((u) => (
            <UpstreamCard key={u.name} u={u} onHover={(sel) => {
              hoverRef.current = sel;
              if (graphRef.current) setTopoFocus(graphRef.current, sel);
            }} />
          ))}
        </div>
      </section>
      <section className="topo-legend" id="topoLegend">
        <span className="lg"><span className="swatch-line" /> forward — same protocol, raw bytes</span>
        <span className="lg"><span className="swatch-line dashed" /> convert — responses → chat</span>
        {anyDual ? <span className="lg"><span className="swatch-line" /> forward — responses passthrough (supports_responses)</span> : null}
        <span className="lg">match order: exact › glob › catch-all</span>
      </section>
    </div>
  );
}
