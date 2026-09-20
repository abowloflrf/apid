// Muted data colors on white surfaces; neutrals mirror the CSS theme.
export const C = {
  blue: "#566b80", green: "#607c6c", amber: "#947c55", teal: "#577e80",
  purple: "#80738f", rose: "#a06b68", muted: "#64748b",
  grid: "rgba(15,23,42,.06)", panel: "#ffffff",
};

export const PALETTE = [
  "#566b80", "#607c6c", "#80738f", "#947c55", "#577e80",
  "#986e83", "#a06b68", "#7c8da0", "#8ba294", "#b1a080",
  "#a095af", "#86a3a4", "#a78e7c", "#b49aaa", "#9ba7b5",
];

const modelColor: Record<string, string> = {};
export function colorFor(model: string): string {
  if (!(model in modelColor)) {
    modelColor[model] = PALETTE[Object.keys(modelColor).length % PALETTE.length];
  }
  return modelColor[model];
}

// Upstream colors live in their own scale: the model palette is keyed by model
// name and reusing it here would tint two unrelated things the same.
const upstreamColor: Record<string, string> = {};
export function upColorFor(name: string): string {
  if (!(name in upstreamColor)) {
    upstreamColor[name] = PALETTE[(Object.keys(upstreamColor).length * 3 + 1) % PALETTE.length];
  }
  return upstreamColor[name];
}
