import { useEffect, useRef, useState } from "react";
import { colorFor } from "../colors";

interface Props {
  label: string;
  options: string[];
  selected: string[];
  onChange: (vals: string[]) => void;
  swatch?: boolean;
}

export function Multiselect({ label, options, selected, onChange, swatch }: Props) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  // Close on outside interaction, mirroring the original document-level click
  // handler that collapsed any open panel.
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", onDoc);
    return () => document.removeEventListener("mousedown", onDoc);
  }, [open]);

  const count = selected.length;
  return (
    <div className="ms" ref={rootRef}>
      <div
        className="ms-toggle"
        onClick={(e) => {
          e.stopPropagation();
          setOpen(!open);
        }}
      >
        <span className="label">{label}</span>
        <span className={`count ${count ? "" : "zero"}`}>{count || "all"}</span>
        <span className="caret">▼</span>
      </div>
      <div className={`ms-panel${open ? " open" : ""}`} onClick={(e) => e.stopPropagation()}>
        <div className="ms-actions">
          <button type="button" onClick={() => onChange(options.slice())}>all</button>
          <button type="button" onClick={() => onChange([])}>clear</button>
        </div>
        {options.length ? (
          options.map((o) => (
            <label className="ms-opt" key={o}>
              <input
                type="checkbox"
                checked={selected.includes(o)}
                onChange={(e) => {
                  onChange(e.target.checked ? [...selected, o] : selected.filter((x) => x !== o));
                }}
              />
              {swatch ? <span className="swatch" style={{ background: colorFor(o) }} /> : null}
              <span className="name" title={o}>{o}</span>
            </label>
          ))
        ) : (
          <div className="ms-empty">none</div>
        )}
      </div>
    </div>
  );
}
