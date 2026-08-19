/*
 * Colored unified-diff rendering for read-only patch surfaces (compare view,
 * commit detail). PRFilesView keeps its parse/style helpers module-private
 * (they are entangled with review-comment targeting), so this is a standalone
 * renderer for plain patches — same visual language, no comment affordances.
 */

/** Color a unified-diff line by its leading marker. */
export function diffLineStyle(line: string): { bg: string; fg: string } {
  if (line.startsWith("@@")) {
    return { bg: "color-mix(in srgb, var(--color-accent) 10%, transparent)", fg: "var(--color-accent)" };
  }
  if (line.startsWith("+")) {
    return { bg: "color-mix(in srgb, var(--gh-open) 14%, transparent)", fg: "var(--color-fg)" };
  }
  if (line.startsWith("-")) {
    return {
      bg: "color-mix(in srgb, var(--color-status-error) 14%, transparent)",
      fg: "var(--color-fg)",
    };
  }
  return { bg: "transparent", fg: "var(--color-fg)" };
}

export interface DiffTextLine {
  text: string;
  oldLine: number | null;
  newLine: number | null;
}

/** Walk a unified patch assigning old/new line numbers to each row. */
export function parseDiffText(patch: string): DiffTextLine[] {
  let oldLine = 0;
  let newLine = 0;
  return patch.split("\n").map((text) => {
    const header = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(text);
    if (header) {
      oldLine = Number(header[1]);
      newLine = Number(header[2]);
      return { text, oldLine: null, newLine: null };
    }
    // split() produces one empty sentinel when the patch ends in a newline;
    // "\ No newline" markers carry no source coordinates either.
    if (text === "" || text.startsWith("\\ No newline")) {
      return { text, oldLine: null, newLine: null };
    }
    if (text.startsWith("-") && !text.startsWith("---")) {
      return { text, oldLine: oldLine++, newLine: null };
    }
    if (text.startsWith("+") && !text.startsWith("+++")) {
      return { text, oldLine: null, newLine: newLine++ };
    }
    if (oldLine > 0 || newLine > 0) {
      return { text, oldLine: oldLine++, newLine: newLine++ };
    }
    return { text, oldLine: null, newLine: null };
  });
}

const GUTTER_STYLE = {
  width: "3rem",
  flexShrink: 0,
  paddingRight: "0.55rem",
  color: "var(--color-fg-subtle)",
} as const;

/** GitHub-style colored diff block for one file's patch. */
export function DiffText({ patch }: { patch: string }) {
  return (
    <div style={{ overflowX: "auto" }}>
      {parseDiffText(patch).map((line, i) => {
        const s = diffLineStyle(line.text);
        return (
          <div
            key={i}
            className="flex font-mono"
            style={{ margin: 0, fontSize: "0.76rem", lineHeight: 1.6, background: s.bg, color: s.fg }}
          >
            <span aria-hidden="true" className="select-none text-right tabular-nums" style={GUTTER_STYLE}>
              {line.oldLine ?? ""}
            </span>
            <span
              aria-hidden="true"
              className="select-none text-right tabular-nums"
              style={{ ...GUTTER_STYLE, borderRight: "1px solid var(--color-border-muted)" }}
            >
              {line.newLine ?? ""}
            </span>
            <span style={{ whiteSpace: "pre", paddingLeft: "0.6rem" }}>{line.text || " "}</span>
          </div>
        );
      })}
    </div>
  );
}
