import { useEffect, useMemo, useState } from "react";
import { useLocation, useNavigate } from "react-router";
import Markdown from "./Markdown.js";
import { highlightLines, languageFromPath } from "./CodeHighlight.js";

/** Decode base64 file content as UTF-8 text; null when the bytes are binary. */
export function decodeBlobText(b64: string): string | null {
  try {
    const bin = atob(b64.replace(/\s/g, ""));
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return text.includes("\u0000") ? null : text;
  } catch {
    return null;
  }
}

const IMAGE_MIME: Record<string, string> = {
  png: "image/png",
  jpg: "image/jpeg",
  jpeg: "image/jpeg",
  gif: "image/gif",
  svg: "image/svg+xml",
  webp: "image/webp",
  ico: "image/x-icon",
};

/** image/* MIME for an image path; undefined otherwise. */
export function blobImageMime(path: string): string | undefined {
  const dot = path.lastIndexOf(".");
  if (dot < 0) return undefined;
  return IMAGE_MIME[path.slice(dot + 1).toLowerCase()];
}

/** Parse a #L3 / #L3-L9 location hash into a 1-based line range. */
export function parseLineHash(hash: string): { start: number; end: number } | null {
  const m = /^#L(\d+)(?:-L(\d+))?$/.exec(hash);
  if (!m) return null;
  const start = Number(m[1]);
  const end = m[2] ? Number(m[2]) : start;
  return start <= end ? { start, end } : { start: end, end: start };
}

function escapeHtml(text: string): string {
  return text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function CodeLines({ text, path }: { text: string; path: string }) {
  const location = useLocation();
  const navigate = useNavigate();
  const plain = useMemo(() => text.split("\n").map(escapeHtml), [text]);
  const [highlighted, setHighlighted] = useState<string[] | null>(null);
  const range = parseLineHash(location.hash);

  useEffect(() => {
    let cancelled = false;
    setHighlighted(null);
    void highlightLines(text, languageFromPath(path)).then((lines) => {
      if (!cancelled) setHighlighted(lines);
    });
    return () => {
      cancelled = true;
    };
  }, [text, path]);

  useEffect(() => {
    if (!range) return;
    const el = document.getElementById(`L${range.start}`);
    // jsdom has no scrollIntoView; browsers always do.
    if (el && typeof el.scrollIntoView === "function") el.scrollIntoView({ block: "center" });
    // eslint-disable-next-line react-hooks/exhaustive-deps -- scroll once per hash value
  }, [location.hash]);

  const lines = highlighted ?? plain;
  const setHash = (hash: string) => navigate({ hash }, { replace: true });
  const onLineClick = (e: React.MouseEvent, n: number) => {
    e.preventDefault();
    // Shift-click extends the current anchor into a #L3-L9 range.
    if (e.shiftKey && range) {
      const [a, b] = n < range.start ? [n, range.end] : [range.start, n];
      setHash(`#L${a}-L${b}`);
    } else {
      setHash(`#L${n}`);
    }
  };

  return (
    <div
      className="code-highlight font-mono"
      style={{ overflowX: "auto", fontSize: ".8rem", lineHeight: 1.55, padding: ".6rem 0" }}
    >
      {lines.map((html, i) => {
        const n = i + 1;
        const targeted = range !== null && n >= range.start && n <= range.end;
        return (
          <div
            key={n}
            id={`L${n}`}
            className="flex"
            style={{
              background: targeted
                ? "color-mix(in srgb, var(--color-brand-gold) 18%, transparent)"
                : "transparent",
            }}
          >
            <a
              href={`#L${n}`}
              onClick={(e) => onLineClick(e, n)}
              className="select-none text-right tabular-nums"
              aria-label={`Line ${n}`}
              style={{
                width: "3.4rem",
                flexShrink: 0,
                paddingRight: ".8rem",
                color: "var(--color-fg-subtle)",
                textDecoration: "none",
              }}
            >
              {n}
            </a>
            <code style={{ whiteSpace: "pre", flex: 1 }} dangerouslySetInnerHTML={{ __html: html || "\n" }} />
          </div>
        );
      })}
    </div>
  );
}

export function BlobContent({
  path,
  name,
  base64,
  size,
}: {
  path: string;
  name: string;
  base64: string;
  size?: number | undefined;
}) {
  const imageMime = blobImageMime(path);
  const text = useMemo(() => (imageMime ? null : decodeBlobText(base64)), [imageMime, base64]);
  const isMarkdown = languageFromPath(path) === "markdown";
  // Markdown blobs open in Preview by default, matching github.com.
  const [mdView, setMdView] = useState<"preview" | "code">("preview");

  if (imageMime) {
    return (
      <div style={{ padding: "1.5rem", textAlign: "center", background: "var(--color-surface)" }}>
        <img
          src={`data:${imageMime};base64,${base64.replace(/\s/g, "")}`}
          alt={name}
          style={{ maxWidth: "100%" }}
        />
      </div>
    );
  }

  if (text === null) {
    const kb = ((size ?? Math.floor((base64.length * 3) / 4)) / 1024).toFixed(1);
    return (
      <div style={{ padding: "1.5rem", textAlign: "center", color: "var(--color-fg-muted)", fontSize: ".85rem" }}>
        <p style={{ margin: 0 }}>Binary file not shown ({kb} KB).</p>
        <a
          href={`data:application/octet-stream;base64,${base64.replace(/\s/g, "")}`}
          download={name}
          style={{ display: "inline-block", lineHeight: "1.625rem", color: "var(--color-accent)" }}
        >
          View raw
        </a>
      </div>
    );
  }

  if (isMarkdown) {
    return (
      <div>
        <div
          role="tablist"
          aria-label="Markdown view"
          className="flex gap-1"
          style={{ padding: "0.4rem 0.75rem 0", borderBottom: "1px solid var(--color-border)" }}
        >
          {(["preview", "code"] as const).map((v) => (
            <button
              key={v}
              type="button"
              role="tab"
              aria-selected={mdView === v}
              onClick={() => setMdView(v)}
              style={{
                padding: "0.3rem 0.7rem",
                marginBottom: "-1px",
                fontSize: "0.82rem",
                fontWeight: mdView === v ? 600 : 500,
                color: mdView === v ? "var(--color-fg)" : "var(--color-fg-muted)",
                background: "transparent",
                border: "none",
                borderBottom: `2px solid ${mdView === v ? "var(--color-accent)" : "transparent"}`,
              }}
            >
              {v === "preview" ? "Preview" : "Code"}
            </button>
          ))}
        </div>
        {mdView === "preview" ? (
          <div className="markdown-body" style={{ padding: "1.5rem", fontSize: "0.9rem" }}>
            <Markdown>{text}</Markdown>
          </div>
        ) : (
          <CodeLines text={text} path={path} />
        )}
      </div>
    );
  }

  return <CodeLines text={text} path={path} />;
}
