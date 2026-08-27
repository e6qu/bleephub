import { useEffect, useRef, type ReactNode } from "react";

// Lists only shortcuts bleephub actually implements, so the sheet never
// advertises a dead binding.

interface Shortcut {
  keys: string[];
  description: string;
}
interface ShortcutGroup {
  name: string;
  shortcuts: Shortcut[];
}

const GROUPS: ShortcutGroup[] = [
  {
    name: "General",
    shortcuts: [
      { keys: ["?"], description: "Show this keyboard-shortcuts help" },
      { keys: ["s", "/"], description: "Focus the search bar (either key)" },
      { keys: ["⌘", "K"], description: "Open the command palette (Ctrl K on Windows/Linux)" },
      { keys: ["Esc"], description: "Close a dialog or menu" },
    ],
  },
  {
    name: "Site navigation",
    shortcuts: [
      { keys: ["g", "h"], description: "Go to your dashboard" },
      { keys: ["g", "n"], description: "Go to your notifications" },
      { keys: ["g", "e"], description: "Go to Explore / search" },
      { keys: ["g", "p"], description: "Go to your profile" },
    ],
  },
  {
    name: "Repositories",
    shortcuts: [{ keys: ["t"], description: "Activate the file finder (in a repository)" }],
  },
];

function Kbd({ children }: { children: ReactNode }) {
  return (
    <kbd
      style={{
        display: "inline-block",
        minWidth: "1.4rem",
        padding: "0.1rem 0.35rem",
        fontFamily: "var(--font-mono, monospace)",
        fontSize: "0.75rem",
        lineHeight: "1.2",
        textAlign: "center",
        color: "var(--color-fg)",
        background: "var(--color-surface-raised)",
        border: "1px solid var(--color-border)",
        borderRadius: "0.35rem",
        boxShadow: "0 1px 0 var(--color-border)",
      }}
    >
      {children}
    </kbd>
  );
}

export function KeyboardShortcuts({ open, onClose }: { open: boolean; onClose: () => void }) {
  const closeRef = useRef<HTMLButtonElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const previouslyFocused = document.activeElement as HTMLElement | null;
    closeRef.current?.focus();
    return () => previouslyFocused?.focus();
  }, [open]);

  if (!open) return null;

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      onClose();
    } else if (e.key === "Tab") {
      // The close button is the only focusable control — keep focus on it.
      e.preventDefault();
      closeRef.current?.focus();
    }
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-auto p-6"
      style={{ background: "color-mix(in srgb, #1f2328 50%, transparent)" }}
      onClick={onClose}
    >
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="kbd-shortcuts-title"
        className="w-full max-w-xl"
        style={{
          marginTop: "8vh",
          background: "var(--color-surface-raised)",
          border: "1px solid var(--color-border)",
          borderRadius: "0.75rem",
          boxShadow: "0 8px 24px rgba(31,35,40,0.2)",
          maxHeight: "70vh",
          display: "flex",
          flexDirection: "column",
          overflow: "hidden",
        }}
        onClick={(e) => e.stopPropagation()}
        onKeyDown={onKeyDown}
      >
        <div
          className="flex items-center justify-between"
          style={{ padding: "0.7rem 0.9rem", borderBottom: "1px solid var(--color-border)" }}
        >
          <h2 id="kbd-shortcuts-title" style={{ fontSize: "0.95rem", fontWeight: 600, margin: 0 }}>
            Keyboard shortcuts
          </h2>
          <button
            ref={closeRef}
            type="button"
            onClick={onClose}
            aria-label="Close keyboard shortcuts"
            style={{
              minWidth: "1.75rem",
              minHeight: "1.75rem",
              lineHeight: "1.75rem",
              padding: 0,
              background: "transparent",
              border: "1px solid var(--color-border)",
              borderRadius: "0.35rem",
              color: "var(--color-fg-muted)",
              cursor: "pointer",
            }}
          >
            ✕
          </button>
        </div>
        <div style={{ overflowY: "auto", padding: "0.9rem" }}>
          {GROUPS.map((group) => (
            <section key={group.name} style={{ marginBottom: "1rem" }}>
              <h3
                style={{
                  fontSize: "0.72rem",
                  textTransform: "uppercase",
                  letterSpacing: "0.03em",
                  color: "var(--color-fg-muted)",
                  margin: "0 0 0.4rem",
                }}
              >
                {group.name}
              </h3>
              <dl style={{ margin: 0 }}>
                {group.shortcuts.map((sc) => (
                  <div
                    key={sc.description}
                    className="flex items-center justify-between gap-3"
                    style={{ padding: "0.3rem 0", borderBottom: "1px solid var(--color-border)" }}
                  >
                    <dd style={{ margin: 0, fontSize: "0.85rem" }}>{sc.description}</dd>
                    <dt className="flex items-center gap-1" style={{ margin: 0, flexShrink: 0 }}>
                      {sc.keys.map((k, i) => (
                        <Kbd key={i}>{k}</Kbd>
                      ))}
                    </dt>
                  </div>
                ))}
              </dl>
            </section>
          ))}
        </div>
      </div>
    </div>
  );
}
