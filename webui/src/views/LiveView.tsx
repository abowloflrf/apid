import { useEffect, useMemo, useRef, useState } from "react";
import { fetchJSON } from "../api";
import { fmtDur, fmtElapsed, fmtTok } from "../format";
import type { LiveRequest, LiveSnapshot } from "../types";

const LIVE_POLL_MS = 1000;

function livePhase(r: LiveRequest): "sync" | "waiting" | "streaming" {
  if (!r.stream) return "sync";
  return r.ttft_ms == null ? "waiting" : "streaming";
}

function liveProtoLabel(r: LiveRequest): string {
  const short = (p: string) =>
    p === "openai_responses" ? "responses" :
    p === "openai_chat_completions" ? "chat" :
    p === "anthropic_messages" ? "anthropic" : p;
  const a = short(r.client_protocol), b = short(r.upstream_protocol);
  return a === b ? a : `${a}→${b}`;
}

interface CardData {
  r: LiveRequest;
  ema: number;
}

function LiveCard({ data, now }: { data: CardData; now: number }) {
  const { r, ema } = data;
  const phase = livePhase(r);
  const outText = fmtTok(r.output_tokens, r.output_est);
  const [entered, setEntered] = useState(false);
  const outRef = useRef<HTMLSpanElement>(null);
  const prevOutRef = useRef(outText);

  // One-shot entrance; removing the class arms the transition end state.
  useEffect(() => {
    const raf = requestAnimationFrame(() => setEntered(true));
    return () => cancelAnimationFrame(raf);
  }, []);

  // Flash the output counter when the value changes.
  useEffect(() => {
    const el = outRef.current;
    if (el && prevOutRef.current !== outText) {
      el.classList.remove("tick");
      void el.offsetWidth;
      el.classList.add("tick");
      prevOutRef.current = outText;
    }
  }, [outText]);

  const client = r.client_model || "(no model)";
  const modelHtml = r.upstream_model && r.upstream_model !== r.client_model
    ? <>{client} <span className="lc-arrow">→</span> <b>{r.upstream_model}</b></>
    : <b>{client}</b>;

  const rateV = phase === "streaming"
    ? (ema >= 100 ? Math.round(ema) : ema.toFixed(1))
    : phase === "waiting" ? "TTFT…" : "…";
  const pct = phase === "streaming" ? Math.min(100, Math.sqrt(ema / 200) * 100) : 0;

  return (
    <article className={`live-card${entered ? "" : " enter"}`} data-id={r.id} data-phase={phase}>
      <div className="lc-head">
        <span className="lc-status" />
        <span className="lc-model" title={`${r.client_model || ""} → ${r.upstream_model || ""}`}>{modelHtml}</span>
        <span className="spacer" />
        <span className="lc-elapsed tnum">{fmtElapsed(now - r.start)}</span>
      </div>
      <div className="lc-tags">
        <span className="pill proto" title={`${r.client_protocol} → ${r.upstream_protocol}`}>{liveProtoLabel(r)}</span>
        <span className={`mode ${r.mode}`}>{r.mode}</span>
        {r.stream ? <span className="lc-sse">SSE</span> : <span className="lc-sync">sync</span>}
        <span className="lc-up" title="upstream">{r.upstream || ""}</span>
      </div>
      <div className="lc-body">
        <div className="lc-tok"><span className="lc-k">in</span><span className="lc-v lc-in tnum">{fmtTok(r.input_tokens, r.input_est)}</span></div>
        <div className="lc-tok"><span className="lc-k">out</span><span className="lc-v lc-out tnum" ref={outRef}>{outText}</span></div>
        <div className="lc-rate"><span className="lc-rate-v tnum">{rateV}</span><span className="lc-rate-u">{phase === "streaming" ? "tok/s" : ""}</span></div>
      </div>
      <div className="lc-meter"><div className="lc-meter-fill" style={{ width: pct + "%" }} /></div>
      <div className="lc-foot">
        <span className="lc-path" title={r.path || ""}>{r.path || ""}</span>
        <span className="lc-ttft tnum">{r.ttft_ms != null ? `TTFT ${fmtDur(r.ttft_ms)}` : ""}</span>
        <span className="spacer" />
        <span className="lc-ua" title={r.client_ua || ""}>{r.client_ua || ""}</span>
      </div>
    </article>
  );
}

export function LiveView({ onLiveCount }: { onLiveCount: (n: number) => void }) {
  const [snap, setSnap] = useState<LiveSnapshot | null>(null);
  const [status, setStatus] = useState("loading…");
  const ratesRef = useRef<Map<number, { out: number; t: number; ema: number }>>(new Map());
  const tickRef = useRef(0);

  useEffect(() => {
    let stopped = false;
    const load = async () => {
      const seq = ++tickRef.current;
      try {
        const r = await fetchJSON<LiveSnapshot>("active");
        if (stopped || seq !== tickRef.current) return;
        setSnap(r);
        setStatus(`polling 1s · ${new Date().toLocaleTimeString("en-GB", { hour12: false })}`);
      } catch (e) {
        if (!stopped && seq === tickRef.current) setStatus("error: " + (e as Error).message);
      }
    };
    void load();
    const t = setInterval(load, LIVE_POLL_MS);
    return () => {
      stopped = true;
      clearInterval(t);
    };
  }, []);

  useEffect(() => {
    onLiveCount(snap?.requests.length ?? 0);
  }, [snap, onLiveCount]);

  const view = useMemo(() => {
    if (!snap) return null;
    const nowMs = performance.now();
    const seen = new Set<number>();
    const cards: CardData[] = [];
    let sumRate = 0;
    let streaming = 0;
    for (const r of snap.requests) {
      seen.add(r.id);
      const prev = ratesRef.current.get(r.id);
      let ema = 0;
      if (prev && nowMs > prev.t) {
        const inst = Math.max(0, (r.output_tokens - prev.out) / ((nowMs - prev.t) / 1000));
        ema = prev.ema > 0 ? prev.ema * 0.55 + inst * 0.45 : inst;
      }
      ratesRef.current.set(r.id, { out: r.output_tokens, t: nowMs, ema });
      if (livePhase(r) === "streaming") { sumRate += ema; streaming++; }
      cards.push({ r, ema });
    }
    for (const id of [...ratesRef.current.keys()]) {
      if (!seen.has(id)) ratesRef.current.delete(id);
    }
    return { cards, sumRate, streaming };
  }, [snap]);

  const count = snap?.requests.length ?? 0;
  return (
    <div id="liveView">
      <section className="live-bar">
        <div className="live-count-wrap">
          <span className={`live-beacon${count > 0 ? " on" : ""}`} />
          <span className="live-count">{count}</span>
          <span className="live-count-label">active</span>
        </div>
        <div className="live-agg">
          {view && view.streaming ? (
            <><span className="la-v">{view.sumRate >= 100 ? Math.round(view.sumRate) : view.sumRate.toFixed(1)}</span> tok/s · {view.streaming} streaming</>
          ) : null}
        </div>
        <div className="spacer" />
        <span className="status" id="liveStatus">{status}</span>
      </section>
      <div className="live-grid" id="liveGrid">
        {view?.cards.map((c) => (
          <LiveCard key={c.r.id} data={c} now={snap?.now ?? Date.now()} />
        ))}
        {!count && <div className="live-empty" id="liveEmpty">No requests in flight — waiting for traffic…</div>}
      </div>
    </div>
  );
}
