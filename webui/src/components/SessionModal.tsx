import { useEffect, useState } from "react";
import type { AgentSession } from "../types";
import { cacheCell, fmtCompact, fmtInt } from "../format";

const TOOLS: { id: string; label: string }[] = [
  { id: "codex", label: "Codex" },
  { id: "claude", label: "Claude Code" },
  { id: "pi", label: "pi" },
  { id: "opencode", label: "OpenCode" },
];

const fmtTime = (ms: number): string => {
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
};

// Copy-to-clipboard micro-button: shows ✓ briefly after a successful copy.
function CopyBtn({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false);
  if (!value) return null;
  const copy = () => {
    if (!navigator.clipboard?.writeText) return;
    navigator.clipboard.writeText(value).then(
      () => {
        setCopied(true);
        window.setTimeout(() => setCopied(false), 1200);
      },
      () => {},
    );
  };
  return (
    <button
      className={`copy-btn${copied ? " copied" : ""}`}
      title={`copy ${label}`}
      aria-label={`copy ${label}`}
      onClick={copy}
    >
      {copied ? "✓" : "⧉"}
    </button>
  );
}

const TOK_PARTS: { key: string; label: string; color: string }[] = [
  { key: "input", label: "input", color: "#5f6f96" },
  { key: "output", label: "output", color: "#6f9576" },
  { key: "cache read", label: "cache read", color: "#bb9a55" },
  { key: "cache write", label: "cache write", color: "#897fa8" },
];

// Compact numeric breakdown for the token usage details.
function TokenBreakdown({ s }: { s: AgentSession }) {
  const counts: Record<string, number> = {
    input: s.input_tokens,
    output: s.output_tokens,
    "cache read": s.cache_read_tokens,
    "cache write": s.cache_write_tokens,
  };
  return (
    <div className="sd-tokens">
      <div className="sd-tok-grid">
        {TOK_PARTS.map((p) => (
          <span key={p.key} className="sd-tok-item">
            <i style={{ background: p.color }} />
            <span className="lbl">{p.label}</span>
            <span className="cnt tnum">{fmtInt(counts[p.key])}</span>
          </span>
        ))}
      </div>
    </div>
  );
}

interface Props {
  session: AgentSession | null;
  onClose: () => void;
}

export function SessionModal({ session, onClose }: Props) {
  useEffect(() => {
    if (!session) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [session, onClose]);

  if (!session) return null;
  const s = session;

  return (
    <div id="sessModal" className="modal open" aria-hidden="false">
      <div className="modal-backdrop" onClick={onClose} />
      <div className="modal-dialog sess-modal-dialog" role="dialog" aria-modal="true" aria-label="Session detail">
        <div className="modal-head">
          <div className="sess-modal-title">
            <span className="sd-eyebrow">session details</span>
            <span className="sd-title" title={s.title}>{s.title || "untitled"}</span>
            <span className="sd-identity">
              <span className={`tool-badge t-${s.tool}`}>{TOOLS.find((t) => t.id === s.tool)?.label ?? s.tool}</span>
              {s.cli_version ? <span className="sd-identity-note">{s.cli_version}</span> : null}
              {s.reasoning_effort ? <span className="sd-identity-note">reasoning: {s.reasoning_effort}</span> : null}
              {s.archived ? <span className="pill archived">archived</span> : null}
            </span>
          </div>
          <button className="modal-close" type="button" aria-label="Close" onClick={onClose}>✕</button>
        </div>
        <div className="modal-body">
          <div className="sd-overview">
            <div className="sd-stat sd-stat-tokens">
              <span className="sd-stat-label">tokens used</span>
              <strong className="tnum">{fmtCompact(s.tokens_used)}</strong>
              <span className="sd-token-model-label">model</span>
              <span className="sd-token-model mono" title={s.model}>{s.model || "-"}</span>
            </div>
            <div className="sd-stat">
              <span className="sd-stat-label">cache hit</span>
              <strong className="sd-cache-value">{cacheCell(s.cache_hit_rate != null ? s.cache_hit_rate * 100 : null)}</strong>
            </div>
            <div className="sd-stat sd-stat-created">
              <span className="sd-stat-label">created</span>
              <strong className="sd-created-value mono">{fmtTime(s.created_at_ms)}</strong>
            </div>
          </div>

          <div className="sd-content">
            <section className="sd-section sd-section-session">
              <div className="sd-section-title">session</div>
              <div className="sd-fields">
                <div className="sd-field sd-field-wide">
                  <span className="k">session id</span>
                  <span className="v mono">{s.id}</span>
                  <CopyBtn value={s.id} label="session id" />
                </div>
                <div className="sd-field sd-field-time">
                  <span className="k">updated</span>
                  <span className="v mono">{fmtTime(s.updated_at_ms)}</span>
                </div>
              </div>
            </section>

            <section className="sd-section sd-section-usage">
              <div className="sd-section-title">token breakdown</div>
              <TokenBreakdown s={s} />
            </section>

            <section className="sd-section sd-section-paths">
              <div className="sd-section-title">technical details</div>
              <div className="sd-fields">
                <div className="sd-field sd-field-wide">
                  <span className="k">working directory</span>
                  <span className="v mono">{s.cwd || "-"}</span>
                </div>
                <div className="sd-field sd-field-wide">
                  <span className="k">rollout file</span>
                  <span className="v mono">{s.rollout_path || "-"}</span>
                </div>
              </div>
            </section>
          </div>
        </div>
      </div>
    </div>
  );
}

export { TOOLS, fmtTime };
