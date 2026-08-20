import {
  useEffect,
  useId,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import Markdown from "./Markdown.js";
import { useComposerDraft } from "../hooks/useComposerDraft.js";

/*
 * GitHub-style comment box: Write/Preview tabs plus a markdown toolbar over a
 * controlled textarea. Drop-in upgrade for a bare <textarea> — same
 * value/placeholder/rows/disabled/id surface, but onChange receives the next
 * string (toolbar edits have no DOM event to forward).
 */

interface EditResult {
  value: string;
  selStart: number;
  selEnd: number;
}

// Wrap the selection in `marker` (bold/italic/inline code), or unwrap when
// the selection is already wrapped so the shortcut toggles like github.com.
function wrapInline(value: string, start: number, end: number, marker: string): EditResult {
  const len = marker.length;
  if (value.slice(start - len, start) === marker && value.slice(end, end + len) === marker) {
    return {
      value: value.slice(0, start - len) + value.slice(start, end) + value.slice(end + len),
      selStart: start - len,
      selEnd: end - len,
    };
  }
  const selected = value.slice(start, end);
  return {
    value: value.slice(0, start) + marker + selected + marker + value.slice(end),
    selStart: start + len,
    selEnd: end + len,
  };
}

// Prefix every line the selection touches ("### ", "> ", "- ", "- [ ] ",
// "1. " with incrementing numbers), keeping the whole block selected.
function prefixLines(value: string, start: number, end: number, prefix: string, numbered: boolean): EditResult {
  const lineStart = value.lastIndexOf("\n", start - 1) + 1;
  const lineEndIdx = value.indexOf("\n", end);
  const blockEnd = lineEndIdx < 0 ? value.length : lineEndIdx;
  const block = value.slice(lineStart, blockEnd);
  const next = block
    .split("\n")
    .map((line, i) => (numbered ? `${i + 1}. ${line}` : prefix + line))
    .join("\n");
  return {
    value: value.slice(0, lineStart) + next + value.slice(blockEnd),
    selStart: lineStart,
    selEnd: lineStart + next.length,
  };
}

function insertLink(value: string, start: number, end: number): EditResult {
  const selected = value.slice(start, end) || "text";
  const inserted = `[${selected}](url)`;
  // Select the "url" placeholder so typing replaces it.
  const urlStart = start + selected.length + 3;
  return {
    value: value.slice(0, start) + inserted + value.slice(end),
    selStart: urlStart,
    selEnd: urlStart + 3,
  };
}

function insertCode(value: string, start: number, end: number): EditResult {
  const selected = value.slice(start, end);
  if (!selected.includes("\n")) return wrapInline(value, start, end, "`");
  const inserted = "```\n" + selected + "\n```";
  return {
    value: value.slice(0, start) + inserted + value.slice(end),
    selStart: start + 4,
    selEnd: start + 4 + selected.length,
  };
}

function LinkIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" width={14} height={14} fill="currentColor">
      <path d="m7.775 3.275 1.25-1.25a3.5 3.5 0 1 1 4.95 4.95l-2.5 2.5a3.5 3.5 0 0 1-4.95 0 .751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018 1.998 1.998 0 0 0 2.83 0l2.5-2.5a2.002 2.002 0 0 0-2.83-2.83l-1.25 1.25a.751.751 0 0 1-1.042-.018.751.751 0 0 1-.018-1.042Zm-4.69 9.64a1.998 1.998 0 0 0 2.83 0l1.25-1.25a.751.751 0 0 1 1.042.018.751.751 0 0 1 .018 1.042l-1.25 1.25a3.5 3.5 0 1 1-4.95-4.95l2.5-2.5a3.5 3.5 0 0 1 4.95 0 .751.751 0 0 1-.018 1.042.751.751 0 0 1-1.042.018 1.998 1.998 0 0 0-2.83 0l-2.5 2.5a1.998 1.998 0 0 0 0 2.83Z" />
    </svg>
  );
}

interface ToolbarAction {
  label: string;
  glyph: ReactNode;
  glyphStyle?: CSSProperties;
  edit: (value: string, start: number, end: number) => EditResult;
}

const TOOLBAR: ToolbarAction[] = [
  { label: "Add heading", glyph: "H", glyphStyle: { fontWeight: 700 }, edit: (v, s, e) => prefixLines(v, s, e, "### ", false) },
  { label: "Bold", glyph: "B", glyphStyle: { fontWeight: 700 }, edit: (v, s, e) => wrapInline(v, s, e, "**") },
  { label: "Italic", glyph: "I", glyphStyle: { fontStyle: "italic", fontFamily: "serif" }, edit: (v, s, e) => wrapInline(v, s, e, "_") },
  { label: "Insert a quote", glyph: "❝", edit: (v, s, e) => prefixLines(v, s, e, "> ", false) },
  { label: "Insert code", glyph: "<>", glyphStyle: { fontFamily: "var(--font-mono)" }, edit: insertCode },
  { label: "Add a link", glyph: <LinkIcon />, edit: insertLink },
  { label: "Add a bulleted list", glyph: "•≡", edit: (v, s, e) => prefixLines(v, s, e, "- ", false) },
  { label: "Add a numbered list", glyph: "1.", edit: (v, s, e) => prefixLines(v, s, e, "", true) },
  { label: "Add a task list", glyph: "☑", edit: (v, s, e) => prefixLines(v, s, e, "- [ ] ", false) },
];

type ComposerTab = "write" | "preview";

export function MarkdownComposer({
  value,
  onChange,
  placeholder = "Leave a comment",
  label = "Comment body",
  rows = 4,
  disabled = false,
  id,
  draftKey,
}: {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  label?: string;
  rows?: number;
  disabled?: boolean;
  id?: string;
  /**
   * Stable identity for sessionStorage draft durability (github.com restores
   * half-typed comments after navigating away). null/undefined disables it —
   * the default, so existing callers and modal editors are unaffected.
   */
  draftKey?: string | null;
}) {
  useComposerDraft(draftKey ?? null, value, onChange);
  const [tab, setTab] = useState<ComposerTab>("write");
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const pendingSelection = useRef<{ start: number; end: number } | null>(null);
  const baseId = useId();
  const writeTabId = `${baseId}-write-tab`;
  const previewTabId = `${baseId}-preview-tab`;
  const writePanelId = `${baseId}-write`;
  const previewPanelId = `${baseId}-preview`;

  // Toolbar edits land through onChange, so the new selection can only be
  // applied after React re-renders the textarea with the new value.
  useEffect(() => {
    const sel = pendingSelection.current;
    const ta = textareaRef.current;
    if (!sel || !ta) return;
    pendingSelection.current = null;
    ta.focus();
    ta.setSelectionRange(sel.start, sel.end);
  }, [value]);

  const apply = (edit: ToolbarAction["edit"]) => {
    const ta = textareaRef.current;
    if (!ta || disabled) return;
    const result = edit(value, ta.selectionStart, ta.selectionEnd);
    pendingSelection.current = { start: result.selStart, end: result.selEnd };
    onChange(result.value);
  };

  const onTextareaKeyDown = (event: ReactKeyboardEvent<HTMLTextAreaElement>) => {
    if (!(event.metaKey || event.ctrlKey)) return;
    const key = event.key.toLowerCase();
    if (key === "b") apply((v, s, e) => wrapInline(v, s, e, "**"));
    else if (key === "i") apply((v, s, e) => wrapInline(v, s, e, "_"));
    else if (key === "k") apply(insertLink);
    else return;
    event.preventDefault();
  };

  // Same roving-tabindex pattern as ui.tsx Tabs, scoped to the two tabs.
  const tabs: { key: ComposerTab; label: string; id: string; panelId: string }[] = [
    { key: "write", label: "Write", id: writeTabId, panelId: writePanelId },
    { key: "preview", label: "Preview", id: previewTabId, panelId: previewPanelId },
  ];
  const moveTab = (event: ReactKeyboardEvent<HTMLButtonElement>, index: number) => {
    let next: number;
    if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
    else if (event.key === "ArrowLeft") next = (index - 1 + tabs.length) % tabs.length;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = tabs.length - 1;
    else return;
    event.preventDefault();
    setTab(tabs[next]!.key);
    event.currentTarget.parentElement
      ?.querySelectorAll<HTMLButtonElement>('[role="tab"]')
      .item(next)
      .focus();
  };

  return (
    <div
      style={{
        border: "1px solid var(--color-border)",
        borderRadius: "var(--radius-md)",
        background: "var(--color-surface)",
        overflow: "hidden",
      }}
    >
      <div
        className="flex flex-wrap items-center gap-1"
        style={{
          padding: "0.35rem 0.5rem",
          background: "var(--color-bg-subtle)",
          borderBottom: "1px solid var(--color-border)",
        }}
      >
        <div role="tablist" aria-label="Comment mode" className="flex gap-1" style={{ marginRight: "auto" }}>
          {tabs.map((t, index) => (
            <button
              key={t.key}
              id={t.id}
              type="button"
              role="tab"
              aria-selected={tab === t.key}
              aria-controls={t.panelId}
              tabIndex={tab === t.key ? 0 : -1}
              onClick={() => setTab(t.key)}
              onKeyDown={(event) => moveTab(event, index)}
              style={{
                padding: "0.3rem 0.7rem",
                fontSize: "0.82rem",
                fontWeight: tab === t.key ? 600 : 500,
                color: tab === t.key ? "var(--color-fg)" : "var(--color-fg-muted)",
                background: tab === t.key ? "var(--color-surface)" : "transparent",
                border: `1px solid ${tab === t.key ? "var(--color-border)" : "transparent"}`,
                borderRadius: "var(--radius-md)",
              }}
            >
              {t.label}
            </button>
          ))}
        </div>
        {tab === "write" && (
          <div className="flex items-center" role="toolbar" aria-label="Formatting">
            {TOOLBAR.map((action) => (
              <button
                key={action.label}
                type="button"
                aria-label={action.label}
                disabled={disabled}
                onClick={() => apply(action.edit)}
                className="inline-flex items-center justify-center"
                style={{
                  minWidth: "1.75rem",
                  minHeight: "1.75rem",
                  padding: "0.2rem 0.3rem",
                  fontSize: "0.8rem",
                  lineHeight: 1,
                  color: "var(--color-fg-muted)",
                  background: "transparent",
                  border: "none",
                  borderRadius: "var(--radius-sm)",
                  ...action.glyphStyle,
                }}
              >
                {action.glyph}
              </button>
            ))}
          </div>
        )}
      </div>
      <div
        id={writePanelId}
        role="tabpanel"
        aria-labelledby={writeTabId}
        hidden={tab !== "write"}
        style={{ padding: "0.5rem" }}
      >
        <textarea
          ref={textareaRef}
          id={id}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          onKeyDown={onTextareaKeyDown}
          placeholder={placeholder}
          aria-label={label}
          rows={rows}
          disabled={disabled}
          className="w-full"
          style={{ resize: "vertical", background: "transparent" }}
        />
      </div>
      <div
        id={previewPanelId}
        role="tabpanel"
        aria-labelledby={previewTabId}
        hidden={tab !== "preview"}
        className="markdown-body"
        style={{ padding: "0.75rem", minHeight: "4rem" }}
      >
        {tab === "preview" &&
          (value.trim() === "" ? (
            <p style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>Nothing to preview</p>
          ) : (
            <Markdown>{value}</Markdown>
          ))}
      </div>
    </div>
  );
}
