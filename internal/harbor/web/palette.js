// The page's one color source of truth: the custom properties in harbor.css,
// read once at load. Charts, SVG hatches, tooltips and legend swatches take
// their colors from here, so a reskin lives in :root and nowhere else. The
// fallbacks are the tokens' own values, used only if a property is missing.
const css = getComputedStyle(document.documentElement);
const v = (name, fallback) => {
  const s = css.getPropertyValue(name).trim();
  return s || fallback;
};

export const PAL = {
  // the accents, by the role the About tab names
  cyan: v("--cyan", "#7ad4f2"),
  amber: v("--amber", "#e8b56f"),
  rose: v("--magenta", "#fb7185"),
  green: v("--green", "#7ee8ac"),
  // surfaces and text tones the charts draw with
  ink: v("--ink", "#cbd8e8"),
  dim: v("--dim", "#8ba0b8"),
  grid: v("--chart-grid", "#182636"),
  tile: v("--tile", "#101a26"),
  // band and bar colors
  baseline: v("--seg-base", "#2e3f53"),
  cache: v("--seg-cache", "#466a89"),
  free: v("--free", "#3c5069"),
  squatterA: v("--squatter-a", "#8a3a4c"),
  squatterB: v("--squatter-b", "#3a1d26"),
  // rules, ticks and tints that are not ink
  rule: v("--rule-ink", "rgba(203,216,232,0.45)"),
  tick: v("--tick-ink", "rgba(203,216,232,0.28)"),
  glowRose: v("--glow-rose", "rgba(251,113,133,0.4)"),
  glowGreen: v("--glow-green", "rgba(126,232,172,0.55)"),
  // the tide's holder slots, in board order
  holders: [1, 2, 3, 4, 5, 6, 7].map((i) => v("--holder-" + i,
    ["#7ad4f2", "#e8b56f", "#7ee8ac", "#b5a1ff", "#5fb4c9", "#e0c078", "#c98bd4"][i - 1])),
};

// event tone -> the accent that event paints with: a grant is cyan, a drain
// amber, a released berth the muted slate of used memory, a stuck one rose.
export const TONE = {
  granted: () => PAL.cyan,
  draining: () => PAL.amber,
  freed: () => PAL.free,
  stuck: () => PAL.rose,
};
