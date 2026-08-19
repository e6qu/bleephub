import { useEffect, useState, type CSSProperties } from "react";

/*
 * Syntax-highlighted code for blob views, diffs and gists. highlight.js is
 * loaded on demand via dynamic import() so it never rides in the entry
 * bundle: the component renders a plain <pre><code> immediately (same font,
 * padding and line-height as the highlighted result — no layout shift) and
 * swaps in highlighted markup once the library arrives.
 */

// Registered subset — the common languages a forge actually renders. Each
// import() below must stay a literal so the bundler can see it; the name
// list and the import list are consumed pairwise, keep them in sync.
const LANGUAGE_NAMES = [
  "go",
  "typescript",
  "javascript",
  "python",
  "rust",
  "java",
  "c",
  "cpp",
  "ruby",
  "php",
  "bash",
  "shell",
  "yaml",
  "json",
  "markdown",
  "xml",
  "css",
  "sql",
  "diff",
  "dockerfile",
  "makefile",
] as const;

type HLJSApi = (typeof import("highlight.js/lib/core"))["default"];

let hljsPromise: Promise<HLJSApi> | null = null;

function loadHljs(): Promise<HLJSApi> {
  hljsPromise ??= Promise.all([
    import("highlight.js/lib/core"),
    import("highlight.js/lib/languages/go"),
    import("highlight.js/lib/languages/typescript"),
    import("highlight.js/lib/languages/javascript"),
    import("highlight.js/lib/languages/python"),
    import("highlight.js/lib/languages/rust"),
    import("highlight.js/lib/languages/java"),
    import("highlight.js/lib/languages/c"),
    import("highlight.js/lib/languages/cpp"),
    import("highlight.js/lib/languages/ruby"),
    import("highlight.js/lib/languages/php"),
    import("highlight.js/lib/languages/bash"),
    import("highlight.js/lib/languages/shell"),
    import("highlight.js/lib/languages/yaml"),
    import("highlight.js/lib/languages/json"),
    import("highlight.js/lib/languages/markdown"),
    import("highlight.js/lib/languages/xml"),
    import("highlight.js/lib/languages/css"),
    import("highlight.js/lib/languages/sql"),
    import("highlight.js/lib/languages/diff"),
    import("highlight.js/lib/languages/dockerfile"),
    import("highlight.js/lib/languages/makefile"),
  ]).then(([core, ...languages]) => {
    const hljs = core.default;
    languages.forEach((mod, i) => hljs.registerLanguage(LANGUAGE_NAMES[i]!, mod.default));
    ensureHighlightStyles();
    return hljs;
  });
  return hljsPromise;
}

// Hand-rolled token palette keyed off the app's theme tokens (index.css
// defines every var for both :root and .dark), instead of a stock hljs theme
// CSS whose hard-coded colors would break dark mode. Injected once, only
// after the library actually loads, so pages that never highlight pay
// nothing.
const HIGHLIGHT_CSS = `
.code-highlight .hljs-comment,.code-highlight .hljs-quote{color:var(--color-fg-muted)}
.code-highlight .hljs-keyword,.code-highlight .hljs-selector-tag,.code-highlight .hljs-type,.code-highlight .hljs-doctag{color:var(--color-status-error)}
.code-highlight .hljs-string,.code-highlight .hljs-regexp,.code-highlight .hljs-char.escape_{color:var(--color-accent-emphasis)}
.code-highlight .hljs-title,.code-highlight .hljs-title.function_,.code-highlight .hljs-title.class_,.code-highlight .hljs-section{color:color-mix(in srgb, var(--color-brand-purple) 62%, var(--color-fg))}
.code-highlight .hljs-number,.code-highlight .hljs-literal,.code-highlight .hljs-attr,.code-highlight .hljs-attribute,.code-highlight .hljs-variable.constant_{color:var(--color-accent)}
.code-highlight .hljs-built_in,.code-highlight .hljs-tag,.code-highlight .hljs-name,.code-highlight .hljs-selector-class,.code-highlight .hljs-selector-id,.code-highlight .hljs-bullet{color:var(--color-success-text)}
.code-highlight .hljs-meta,.code-highlight .hljs-symbol,.code-highlight .hljs-template-variable{color:var(--color-brand-cyan-text)}
.code-highlight .hljs-addition{color:var(--color-success-text);background:var(--color-status-ok-soft)}
.code-highlight .hljs-deletion{color:var(--color-danger-text);background:var(--color-status-error-soft)}
.code-highlight .hljs-emphasis{font-style:italic}
.code-highlight .hljs-strong{font-weight:600}
`;

const STYLE_ID = "code-highlight-theme";

function ensureHighlightStyles(): void {
  if (typeof document === "undefined" || document.getElementById(STYLE_ID)) return;
  const style = document.createElement("style");
  style.id = STYLE_ID;
  style.textContent = HIGHLIGHT_CSS;
  document.head.appendChild(style);
}

// Extension → hljs language (or alias — aliases like tsx/jsx/html/sh resolve
// through hljs.getLanguage). Special-cased basenames (Dockerfile, Makefile)
// follow GitHub's linguist behavior.
const EXT_LANGUAGE: Record<string, string> = {
  go: "go",
  ts: "typescript",
  mts: "typescript",
  cts: "typescript",
  tsx: "tsx",
  js: "javascript",
  mjs: "javascript",
  cjs: "javascript",
  jsx: "jsx",
  py: "python",
  rs: "rust",
  java: "java",
  c: "c",
  h: "c",
  cc: "cpp",
  cpp: "cpp",
  cxx: "cpp",
  hpp: "cpp",
  hh: "cpp",
  rb: "ruby",
  php: "php",
  sh: "bash",
  bash: "bash",
  zsh: "bash",
  yml: "yaml",
  yaml: "yaml",
  json: "json",
  md: "markdown",
  markdown: "markdown",
  html: "html",
  htm: "html",
  xml: "xml",
  svg: "xml",
  css: "css",
  sql: "sql",
  diff: "diff",
  patch: "diff",
  dockerfile: "dockerfile",
  mk: "makefile",
  makefile: "makefile",
};

/** Infer an hljs language (or alias) from a file path; undefined when unknown. */
export function languageFromPath(path: string): string | undefined {
  const base = path.split("/").pop() ?? path;
  const lower = base.toLowerCase();
  if (lower === "dockerfile" || lower.startsWith("dockerfile.")) return "dockerfile";
  if (lower === "makefile" || lower === "gnumakefile") return "makefile";
  const dot = lower.lastIndexOf(".");
  if (dot < 0) return undefined;
  return EXT_LANGUAGE[lower.slice(dot + 1)];
}

function escapeHtml(text: string): string {
  return text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function highlightValue(hljs: HLJSApi, code: string, language: string | undefined): string | null {
  if (!language || !hljs.getLanguage(language)) return null;
  return hljs.highlight(code, { language, ignoreIllegals: true }).value;
}

// hljs spans can cross newlines (block comments, template strings), so a
// naive split would emit unbalanced HTML. Re-open the still-open spans at the
// start of each line and close them at its end, keeping every entry
// self-contained — what a per-line diff/blob renderer needs.
function splitHighlightedLines(html: string): string[] {
  const lines: string[] = [];
  const openStack: string[] = [];
  for (const raw of html.split("\n")) {
    const prefix = openStack.join("");
    const tagRe = /<span[^>]*>|<\/span>/g;
    let m: RegExpExecArray | null;
    while ((m = tagRe.exec(raw)) !== null) {
      if (m[0] === "</span>") openStack.pop();
      else openStack.push(m[0]);
    }
    lines.push(prefix + raw + "</span>".repeat(openStack.length));
  }
  return lines;
}

/**
 * Highlight a whole file and return one safe-HTML string per line (hljs
 * escapes source text; unknown languages fall back to plain escaping).
 * Render each entry inside an element with the `code-highlight` class.
 */
export async function highlightLines(code: string, language: string | undefined): Promise<string[]> {
  const hljs = await loadHljs();
  const value = highlightValue(hljs, code, language);
  if (value === null) return code.split("\n").map(escapeHtml);
  return splitHighlightedLines(value);
}

const PRE_STYLE: CSSProperties = {
  background: "var(--color-bg-subtle)",
  border: "1px solid var(--color-border)",
  borderRadius: "var(--radius-md)",
  padding: "0.7rem 0.85rem",
  fontFamily: "var(--font-mono)",
  fontSize: "0.78rem",
  lineHeight: 1.5,
  color: "var(--color-fg)",
  overflow: "auto",
  margin: 0,
};

/**
 * Blob/gist code block. Renders the source as plain text immediately and
 * upgrades in place to highlighted markup when highlight.js finishes
 * loading. `language` wins over `path`; with neither (or an unregistered
 * language) it stays a plain block.
 */
export function CodeHighlight({
  code,
  language,
  path,
  style,
  className = "",
}: {
  code: string;
  language?: string | undefined;
  path?: string | undefined;
  style?: CSSProperties;
  className?: string;
}) {
  const lang = language ?? (path !== undefined ? languageFromPath(path) : undefined);
  const [html, setHtml] = useState<string | null>(null);

  useEffect(() => {
    setHtml(null);
    if (!lang) return undefined;
    let cancelled = false;
    void loadHljs().then((hljs) => {
      if (cancelled) return;
      setHtml(highlightValue(hljs, code, lang));
    });
    return () => {
      cancelled = true;
    };
  }, [code, lang]);

  return (
    <pre className={`code-highlight ${className}`} style={{ ...PRE_STYLE, ...style }}>
      {html === null ? <code>{code}</code> : <code dangerouslySetInnerHTML={{ __html: html }} />}
    </pre>
  );
}
