import type { CSSProperties } from "react";

/** Convert a 6-hex label color to HSL; null for malformed input. */
function hexToHsl(hex: string): { h: number; s: number; l: number } | null {
  const m = /^([0-9a-fA-F]{6})$/.exec(hex.trim());
  if (!m) return null;
  const int = parseInt(m[1]!, 16);
  const r = ((int >> 16) & 0xff) / 255;
  const g = ((int >> 8) & 0xff) / 255;
  const b = (int & 0xff) / 255;
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const d = max - min;
  let h = 0;
  if (d !== 0) {
    if (max === r) h = ((g - b) / d) % 6;
    else if (max === g) h = (b - r) / d + 2;
    else h = (r - g) / d + 4;
    h *= 60;
    if (h < 0) h += 360;
  }
  const l = (max + min) / 2;
  const s = d === 0 ? 0 : d / (1 - Math.abs(2 * l - 1));
  return { h: Math.round(h), s: Math.round(s * 100), l: Math.round(l * 100) };
}

/**
 * WEB-068: the tinted background composites light in light mode and dark in
 * dark mode, so text must clear WCAG AA against both. Emit two hue-matched
 * foregrounds as CSS variables; `.label-pill` in index.css switches on theme.
 */
export function LabelPills({ labels }: { labels?: { name: string; color: string }[] }) {
  if (!labels || labels.length === 0) return null;
  return (
    <>
      {labels.map((l) => {
        const hsl = hexToHsl(l.color);
        const style: CSSProperties & Record<`--${string}`, string> = {
          padding: "0.1rem 0.55rem",
          borderRadius: "2rem",
          fontSize: "0.72rem",
          fontWeight: 500,
          background: `#${l.color}22`,
          border: `1px solid #${l.color}55`,
          whiteSpace: "nowrap",
          // Malformed-color fallback, overridden below.
          color: "var(--color-fg)",
        };
        if (hsl) {
          const sat = Math.max(hsl.s, 20);
          style["--label-fg-light"] = `hsl(${hsl.h} ${sat}% 26%)`;
          style["--label-fg-dark"] = `hsl(${hsl.h} ${Math.max(sat, 55)}% 80%)`;
        }
        return (
          <span key={l.name} className="label-pill" style={style}>
            {l.name}
          </span>
        );
      })}
    </>
  );
}
