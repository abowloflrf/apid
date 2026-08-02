// Muted palette tuned for the warm-paper theme — desaturated, harmonious, never
// neon. Mirrors the CSS custom properties so chrome and data read as one piece.
export const C = {
  blue: "#5f6f96", green: "#6f9576", amber: "#bb9a55", teal: "#5f9499",
  purple: "#897fa8", rose: "#c07b73", muted: "#7a766c",
  grid: "rgba(40,36,28,.06)", panel: "#fdfcf9",
};

export const PALETTE = [
  "#5f6f96", "#6f9576", "#897fa8", "#bb9a55", "#5f9499",
  "#b98ca0", "#c07b73", "#7c88ad", "#86a98a", "#cbae6e",
  "#9a90bb", "#74a6ab", "#bf9a7e", "#c79bb0", "#8e9bbf",
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
