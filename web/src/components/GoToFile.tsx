import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { Spinner } from "@bleephub/ui-core/components";
import { fetchRepoTreeRecursive } from "../api.js";

/**
 * GitHub's repo "Go to file" fuzzy finder (the `t` shortcut / "Go to file"
 * button). Loads the full recursive tree at the current ref, subsequence-matches
 * the query against file paths (preferring basename hits), and navigates to the
 * chosen blob. Dialog + combobox/listbox with ↑/↓/Enter/Esc, focus trapped in
 * the dialog and restored to the opener on close.
 */

function subsequenceMatch(path: string, needle: string): boolean {
  if (!needle) return true;
  let i = 0;
  for (let j = 0; j < path.length && i < needle.length; j++) {
    if (path[j] === needle[i]) i++;
  }
  return i === needle.length;
}

export function GoToFile({
  owner,
  repo,
  gitRef,
  onClose,
}: {
  owner: string;
  repo: string;
  gitRef: string;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const [q, setQ] = useState("");
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);

  const treeQ = useQuery({
    queryKey: ["repo-tree-recursive", owner, repo, gitRef],
    queryFn: () => fetchRepoTreeRecursive(owner, repo, gitRef),
  });

  const files = useMemo(
    () => (treeQ.data?.tree ?? []).filter((e) => e.type === "blob").map((e) => e.path),
    [treeQ.data],
  );

  const results = useMemo(() => {
    const needle = q.trim().toLowerCase();
    if (!needle) return files.slice(0, 50);
    const matched = files.filter((p) => subsequenceMatch(p.toLowerCase(), needle));
    // Prefer a basename substring hit, then shorter paths.
    matched.sort((a, b) => {
      const ab = a.slice(a.lastIndexOf("/") + 1).toLowerCase().includes(needle);
      const bb = b.slice(b.lastIndexOf("/") + 1).toLowerCase().includes(needle);
      if (ab !== bb) return ab ? -1 : 1;
      return a.length - b.length;
    });
    return matched.slice(0, 50);
  }, [files, q]);

  useEffect(() => setActive(0), [q]);
  useEffect(() => {
    const previouslyFocused = document.activeElement as HTMLElement | null;
    inputRef.current?.focus();
    return () => previouslyFocused?.focus();
  }, []);

  const go = (path?: string) => {
    if (!path) return;
    navigate(`/ui/repos/${owner}/${repo}/blob/${encodeURIComponent(gitRef)}/${path}`);
    onClose();
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      onClose();
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => (results.length ? (a + 1) % results.length : 0));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => (results.length ? (a - 1 + results.length) % results.length : 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      go(results[active]);
    } else if (e.key === "Tab") {
      e.preventDefault();
    }
  };

  const activeId = results[active] ? `gtf-${active}` : undefined;

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-auto p-6"
      style={{ background: "color-mix(in srgb, #1f2328 50%, transparent)" }}
      onClick={onClose}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Go to file"
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
        <div style={{ padding: "0.7rem 0.9rem", borderBottom: "1px solid var(--color-border)" }}>
          <input
            ref={inputRef}
            role="combobox"
            aria-expanded="true"
            aria-controls="gtf-listbox"
            aria-activedescendant={activeId}
            aria-label="Search files by path"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Go to file…"
            style={{ width: "100%", border: "none", outline: "none", background: "transparent", fontSize: "0.95rem", color: "var(--color-fg)" }}
          />
        </div>
        <div id="gtf-listbox" role="listbox" aria-label="Files" style={{ overflowY: "auto", padding: "0.35rem" }}>
          {treeQ.isLoading ? (
            <div style={{ padding: "1rem" }}>
              <Spinner label="loading files" />
            </div>
          ) : results.length === 0 ? (
            <div style={{ padding: "1rem", fontSize: "0.85rem", color: "var(--color-fg-muted)", textAlign: "center" }}>
              No matching files
            </div>
          ) : (
            results.map((path, index) => {
              const isActive = index === active;
              const slash = path.lastIndexOf("/");
              const dir = slash >= 0 ? path.slice(0, slash + 1) : "";
              const name = slash >= 0 ? path.slice(slash + 1) : path;
              return (
                <div
                  key={path}
                  id={`gtf-${index}`}
                  role="option"
                  aria-selected={isActive}
                  onMouseEnter={() => setActive(index)}
                  onClick={() => go(path)}
                  className="font-mono"
                  style={{
                    padding: "0.35rem 0.6rem",
                    borderRadius: "0.4rem",
                    cursor: "pointer",
                    fontSize: "0.82rem",
                    whiteSpace: "nowrap",
                    overflow: "hidden",
                    textOverflow: "ellipsis",
                    background: isActive ? "var(--color-bg-subtle)" : "transparent",
                  }}
                >
                  <span style={{ color: "var(--color-fg-muted)" }}>{dir}</span>
                  <span style={{ color: "var(--color-fg)" }}>{name}</span>
                </div>
              );
            })
          )}
        </div>
      </div>
    </div>
  );
}
