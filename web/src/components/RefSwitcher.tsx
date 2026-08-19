import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { fetchRepoTags } from "../api.js";
import { useDismiss } from "../hooks/useDismiss.js";
import { BranchIcon, TagIcon, ChevronDownIcon, CheckIcon } from "./octicons.js";

/**
 * GitHub's branch/tag switcher: a trigger button showing the current ref and a
 * filterable popover with Branches/Tags tabs (combobox + listbox keyboard
 * pattern, same as GoToFile). Tags load lazily on first open; branches come
 * from the caller (every code view already has them).
 */
export function RefSwitcher({
  owner,
  repo,
  current,
  branches,
  defaultBranch,
  onSelect,
}: {
  owner: string;
  repo: string;
  current: string;
  branches: string[];
  defaultBranch?: string | undefined;
  onSelect: (ref: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [tab, setTab] = useState<"branches" | "tags">("branches");
  const [q, setQ] = useState("");
  const [active, setActive] = useState(0);
  const wrapRef = useDismiss<HTMLDivElement>(open, () => setOpen(false));
  const inputRef = useRef<HTMLInputElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  // Same query key as the repo Tags tab so the cache is shared.
  const tagsQ = useQuery({
    queryKey: ["repo-tags", owner, repo],
    queryFn: () => fetchRepoTags(owner, repo),
    enabled: open,
  });

  const names = useMemo(
    () => (tab === "branches" ? branches : (tagsQ.data ?? []).map((t) => t.name)),
    [tab, branches, tagsQ.data],
  );
  const results = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return names.filter((n) => n.toLowerCase().includes(needle)).slice(0, 50);
  }, [names, q]);

  useEffect(() => setActive(0), [q, tab]);
  useEffect(() => {
    if (open) inputRef.current?.focus();
  }, [open]);

  const close = () => {
    setOpen(false);
    setQ("");
    triggerRef.current?.focus();
  };
  const choose = (name?: string) => {
    if (!name) return;
    close();
    onSelect(name);
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      close();
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => (results.length ? (a + 1) % results.length : 0));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => (results.length ? (a - 1 + results.length) % results.length : 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      choose(results[active]);
    }
  };

  const activeId = results[active] ? `refsw-${active}` : undefined;
  const isBranch = branches.includes(current);

  return (
    <div ref={wrapRef} style={{ position: "relative" }}>
      <button
        ref={triggerRef}
        type="button"
        aria-label="Switch branches or tags"
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
        className="inline-flex max-w-64 items-center gap-1.5"
        style={{
          background: "var(--color-bg-subtle)",
          color: "var(--color-fg)",
          border: "1px solid var(--color-border)",
          borderRadius: "var(--radius-md)",
          padding: "0.3rem 0.7rem",
          fontSize: "0.82rem",
          fontWeight: 600,
        }}
      >
        {isBranch ? <BranchIcon size={14} /> : <TagIcon size={14} />}
        <span className="truncate">{current}</span>
        <ChevronDownIcon size={13} />
      </button>
      {open && (
        <div
          role="dialog"
          aria-label="Switch branches or tags"
          onKeyDown={onKeyDown}
          style={{
            position: "absolute",
            top: "calc(100% + 6px)",
            left: 0,
            zIndex: 30,
            width: 320,
            background: "var(--color-surface-raised)",
            border: "1px solid var(--color-border)",
            borderRadius: "var(--radius-md)",
            boxShadow: "0 8px 24px rgba(31,35,40,0.2)",
            display: "flex",
            flexDirection: "column",
            maxHeight: "24rem",
            overflow: "hidden",
          }}
        >
          <div style={{ padding: "0.6rem 0.7rem 0.4rem" }}>
            <input
              ref={inputRef}
              role="combobox"
              aria-expanded="true"
              aria-controls="refsw-listbox"
              aria-activedescendant={activeId}
              aria-label={`Find a ${tab === "branches" ? "branch" : "tag"}`}
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder={tab === "branches" ? "Find a branch…" : "Find a tag…"}
              className="w-full"
              style={{ fontSize: "0.84rem", padding: "0.3rem 0.5rem" }}
            />
          </div>
          <div
            role="tablist"
            aria-label="Ref types"
            className="flex gap-1"
            style={{ padding: "0 0.7rem", borderBottom: "1px solid var(--color-border)" }}
          >
            {(["branches", "tags"] as const).map((t) => (
              <button
                key={t}
                type="button"
                role="tab"
                aria-selected={tab === t}
                onClick={() => setTab(t)}
                style={{
                  padding: "0.3rem 0.6rem",
                  marginBottom: "-1px",
                  fontSize: "0.8rem",
                  fontWeight: tab === t ? 600 : 500,
                  color: tab === t ? "var(--color-fg)" : "var(--color-fg-muted)",
                  background: "transparent",
                  border: "none",
                  borderBottom: `2px solid ${tab === t ? "var(--color-accent)" : "transparent"}`,
                }}
              >
                {t === "branches" ? "Branches" : "Tags"}
              </button>
            ))}
          </div>
          <div id="refsw-listbox" role="listbox" aria-label={tab === "branches" ? "Branches" : "Tags"} style={{ overflowY: "auto", padding: "0.3rem" }}>
            {tab === "tags" && tagsQ.isLoading ? (
              <div style={{ padding: "0.6rem", fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>Loading tags…</div>
            ) : tab === "tags" && tagsQ.isError ? (
              <div role="alert" style={{ padding: "0.6rem", fontSize: "0.8rem", color: "var(--color-status-error)" }}>
                Failed to load tags
              </div>
            ) : results.length === 0 ? (
              <div style={{ padding: "0.6rem", fontSize: "0.8rem", color: "var(--color-fg-muted)", textAlign: "center" }}>
                Nothing to show
              </div>
            ) : (
              results.map((name, index) => {
                const isActive = index === active;
                return (
                  <div
                    key={name}
                    id={`refsw-${index}`}
                    role="option"
                    aria-selected={name === current}
                    onMouseEnter={() => setActive(index)}
                    onClick={() => choose(name)}
                    className="flex items-center gap-1.5"
                    style={{
                      padding: "0.32rem 0.55rem",
                      borderRadius: "0.35rem",
                      cursor: "pointer",
                      fontSize: "0.82rem",
                      background: isActive ? "var(--color-bg-subtle)" : "transparent",
                    }}
                  >
                    <span style={{ width: 14, flexShrink: 0, display: "inline-flex" }}>
                      {name === current ? <CheckIcon size={13} /> : null}
                    </span>
                    <span className="truncate" style={{ flex: 1 }}>{name}</span>
                    {defaultBranch !== undefined && tab === "branches" && name === defaultBranch && (
                      <span
                        style={{
                          fontSize: "0.68rem",
                          fontWeight: 600,
                          color: "var(--color-fg-muted)",
                          border: "1px solid var(--color-border)",
                          borderRadius: "2rem",
                          padding: "0 0.45rem",
                        }}
                      >
                        default
                      </span>
                    )}
                  </div>
                );
              })
            )}
          </div>
        </div>
      )}
    </div>
  );
}
