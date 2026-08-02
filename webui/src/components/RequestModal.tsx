import { useEffect, type ReactNode } from "react";
import type { RequestRow } from "../types";
import { cacheCell, escapeHtml, fmtDur, fmtInt, reqCachePct } from "../format";
import { colorFor } from "../colors";

function dRow(k: string, v: ReactNode) {
  return (
    <div className="d-row">
      <span className="d-key">{k}</span>
      <span className="d-val">{v}</span>
    </div>
  );
}

export function statusPill(code?: number) {
  if (!code) return <span className="muted">—</span>;
  const ok = code >= 200 && code < 300;
  return <span className={`pill ${ok ? "ok" : "err"}`}>{code}</span>;
}

function modelDot(m?: string) {
  return m ? (
    <>
      <span className="dot" style={{ background: colorFor(m) }} />
      {m}
    </>
  ) : (
    <span className="muted">—</span>
  );
}

interface Props {
  request: RequestRow | null;
  onClose: () => void;
}

export function RequestModal({ request, onClose }: Props) {
  useEffect(() => {
    if (!request) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [request, onClose]);

  if (!request) return null;
  const r = request;

  return (
    <div id="reqModal" className="modal open" aria-hidden="false">
      <div className="modal-backdrop" onClick={onClose} />
      <div className="modal-dialog" role="dialog" aria-modal="true" aria-label="Request detail">
        <div className="modal-head">
          <h3>Request detail</h3>
          <button className="modal-close" type="button" aria-label="Close" onClick={onClose}>✕</button>
        </div>
        <div className="modal-body">
          <div className="d-sec">Request</div>
          {dRow("Time", new Date(r.time).toLocaleString("en-GB", { hour12: false }))}
          {dRow("Protocol", <span className="pill proto">{r.client_protocol || "—"}</span>)}
          {dRow("Stream", r.stream ? "SSE" : <span className="muted">no</span>)}
          {dRow("Client model", modelDot(r.client_model))}
          {dRow("Upstream model", modelDot(r.upstream_model))}
          {dRow("Upstream URL", r.upstream_url ? <span className="d-mono">{r.upstream_url}</span> : <span className="muted">—</span>)}
          {dRow("User-Agent", r.client_ua ? <span className="d-mono">{r.client_ua}</span> : <span className="muted">—</span>)}
          <div className="d-sec">Status &amp; timing</div>
          {dRow("Status", <>{statusPill(r.client_status)} <span className="d-arrow">→</span> {statusPill(r.upstream_status)}</>)}
          {dRow("Duration", fmtDur(r.duration_ms))}
          {dRow("TTFT", r.ttft_ms != null ? fmtDur(r.ttft_ms) : <span className="muted">—</span>)}
          {dRow("Throughput", <>{tokPerSec(r)} <span className="muted">tok/s</span></>)}
          <div className="d-sec">Tokens</div>
          {dRow("Input", fmtInt(r.input_tokens))}
          {dRow("Output", fmtInt(r.output_tokens))}
          {dRow("Cached", fmtInt(r.cached_tokens))}
          {dRow("Total", <span className="num-strong">{fmtInt(r.total_tokens)}</span>)}
          {dRow("Cache hit", cacheCell(reqCachePct(r)))}
          {r.error && (
            <>
              <div className="d-sec">Error</div>
              <div className="d-err">{escapeHtml(r.error)}</div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

export function tokPerSec(r: RequestRow): string {
  return r.duration_ms > 0 ? (1000 * r.output_tokens / r.duration_ms).toFixed(1) : "—";
}
