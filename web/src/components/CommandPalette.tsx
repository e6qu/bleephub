import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { searchRepositories, searchUsers } from "../api.js";
import {
  RepoIcon,
  PeopleIcon,
  SearchIcon,
  IssueOpenedIcon,
  PullRequestIcon,
  NotificationBellIcon,
  GistIcon,
  PackageIcon,
  KeyIcon,
  OrganizationIcon,
} from "./octicons.js";

/**
 * ⌘K / Ctrl-K "jump to" command palette, mirroring github.com's global
 * navigator. Static destinations are always available and filter by substring;
 * a non-empty query additionally searches repositories and users/orgs. The list
 * is a combobox+listbox: ↑/↓ move the active option, Enter navigates, Esc
 * closes. Focus opens on the input, is trapped in the dialog, and is restored
 * to the opener on close.
 */

type CmdItem = {
  id: string;
  label: string;
  sublabel?: string | undefined;
  icon: ReactNode;
  to: string;
  group: string;
};

function staticTargets(viewerLogin?: string): CmdItem[] {
  const items: CmdItem[] = [
    { id: "s-dashboard", label: "Dashboard", icon: <RepoIcon size={16} />, to: "/ui/", group: "Go to" },
  ];
  if (viewerLogin) {
    items.push({ id: "s-profile", label: "Your profile", sublabel: viewerLogin, icon: <PeopleIcon size={16} />, to: `/ui/${viewerLogin}`, group: "Go to" });
  }
  // Viewer-scoped like the header quick links: signed in, Issues / Pull
  // requests jump to "yours" via search qualifiers.
  const issuesQ = viewerLogin ? `is:issue author:${viewerLogin}` : "is:issue";
  const pullsQ = viewerLogin ? `is:pr author:${viewerLogin}` : "is:pr";
  items.push(
    { id: "s-repos", label: "Your repositories", icon: <RepoIcon size={16} />, to: "/ui/repos", group: "Go to" },
    { id: "s-issues", label: viewerLogin ? "Your issues" : "Issues", icon: <IssueOpenedIcon size={16} />, to: `/ui/search?type=issues&q=${encodeURIComponent(issuesQ)}`, group: "Go to" },
    { id: "s-pulls", label: viewerLogin ? "Your pull requests" : "Pull requests", icon: <PullRequestIcon size={16} />, to: `/ui/search?type=issues&q=${encodeURIComponent(pullsQ)}`, group: "Go to" },
    { id: "s-notifications", label: "Notifications", icon: <NotificationBellIcon size={16} />, to: "/ui/notifications", group: "Go to" },
    { id: "s-explore", label: "Explore", icon: <SearchIcon size={16} />, to: "/ui/search", group: "Go to" },
    { id: "s-marketplace", label: "Marketplace", icon: <PackageIcon size={16} />, to: "/ui/marketplace", group: "Go to" },
    { id: "s-gists", label: "Your gists", icon: <GistIcon size={16} />, to: "/ui/gists", group: "Go to" },
    { id: "s-settings", label: "Settings", icon: <KeyIcon size={16} />, to: "/ui/account", group: "Go to" },
  );
  return items;
}

export function CommandPalette({
  open,
  onClose,
  viewerLogin,
}: {
  open: boolean;
  onClose: () => void;
  viewerLogin?: string | undefined;
}) {
  const navigate = useNavigate();
  const [q, setQ] = useState("");
  const [debounced, setDebounced] = useState("");
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const listRef = useRef<HTMLDivElement>(null);

  // Reset query/selection each time the palette opens.
  useEffect(() => {
    if (open) {
      setQ("");
      setDebounced("");
      setActive(0);
    }
  }, [open]);

  // Debounce the query that drives the network searches.
  useEffect(() => {
    const t = setTimeout(() => setDebounced(q.trim()), 180);
    return () => clearTimeout(t);
  }, [q]);

  const hasQuery = debounced.length > 0;
  const reposQ = useQuery({
    queryKey: ["cmdk-repos", debounced],
    queryFn: () => searchRepositories(debounced),
    enabled: open && hasQuery,
  });
  const usersQ = useQuery({
    queryKey: ["cmdk-users", debounced],
    queryFn: () => searchUsers(debounced),
    enabled: open && hasQuery,
  });

  const items = useMemo<CmdItem[]>(() => {
    const needle = debounced.toLowerCase();
    const statics = staticTargets(viewerLogin).filter(
      (i) => !needle || i.label.toLowerCase().includes(needle) || (i.sublabel ?? "").toLowerCase().includes(needle),
    );
    const repos: CmdItem[] = hasQuery
      ? (reposQ.data?.items ?? []).slice(0, 5).map((r) => ({
          id: `r-${r.id}`,
          label: r.full_name,
          sublabel: r.description || undefined,
          icon: <RepoIcon size={16} />,
          to: `/ui/repos/${r.full_name}`,
          group: "Repositories",
        }))
      : [];
    const users: CmdItem[] = hasQuery
      ? (usersQ.data?.items ?? []).slice(0, 5).map((u) => ({
          id: `u-${u.id}`,
          label: u.login,
          sublabel: u.name || undefined,
          icon: u.type === "Organization" ? <OrganizationIcon size={16} /> : <PeopleIcon size={16} />,
          to: `/ui/${u.login}`,
          group: "Users & organizations",
        }))
      : [];
    return [...statics, ...repos, ...users];
  }, [debounced, hasQuery, reposQ.data, usersQ.data, viewerLogin]);

  // Keep the active index in range as results change.
  useEffect(() => {
    setActive((a) => (items.length === 0 ? 0 : Math.min(a, items.length - 1)));
  }, [items.length]);

  // Open: focus the input, trap Tab, restore focus on close.
  useEffect(() => {
    if (!open) return;
    const previouslyFocused = document.activeElement as HTMLElement | null;
    inputRef.current?.focus();
    return () => previouslyFocused?.focus();
  }, [open]);

  if (!open) return null;

  const go = (item?: CmdItem) => {
    if (!item) return;
    navigate(item.to);
    onClose();
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      onClose();
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => (items.length ? (a + 1) % items.length : 0));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => (items.length ? (a - 1 + items.length) % items.length : 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      go(items[active]);
    } else if (e.key === "Tab") {
      // Single-input dialog: keep focus trapped on the input.
      e.preventDefault();
    }
  };

  // Group the flat list for display while preserving the keyboard index.
  const groups: { name: string; items: { item: CmdItem; index: number }[] }[] = [];
  items.forEach((item, index) => {
    let g = groups.find((x) => x.name === item.group);
    if (!g) {
      g = { name: item.group, items: [] };
      groups.push(g);
    }
    g.items.push({ item, index });
  });

  const activeId = items[active]?.id;

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
        aria-label="Command palette"
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
        <div className="flex items-center gap-2" style={{ padding: "0.7rem 0.9rem", borderBottom: "1px solid var(--color-border)" }}>
          <span style={{ color: "var(--color-fg-muted)" }}>
            <SearchIcon size={16} />
          </span>
          <input
            ref={inputRef}
            role="combobox"
            aria-expanded="true"
            aria-controls="cmdk-listbox"
            aria-activedescendant={activeId}
            aria-label="Search repositories, users, and pages"
            value={q}
            onChange={(e) => {
              setQ(e.target.value);
              setActive(0);
            }}
            placeholder="Search or jump to…"
            style={{
              flex: 1,
              border: "none",
              outline: "none",
              background: "transparent",
              fontSize: "0.95rem",
              color: "var(--color-fg)",
            }}
          />
          <kbd
            style={{
              fontSize: "0.7rem",
              color: "var(--color-fg-muted)",
              border: "1px solid var(--color-border)",
              borderRadius: "0.35rem",
              padding: "0.05rem 0.35rem",
            }}
          >
            Esc
          </kbd>
        </div>

        <div id="cmdk-listbox" role="listbox" aria-label="Results" ref={listRef} style={{ overflowY: "auto", padding: "0.35rem" }}>
          {items.length === 0 ? (
            <div style={{ padding: "1rem", fontSize: "0.85rem", color: "var(--color-fg-muted)", textAlign: "center" }}>
              {hasQuery && (reposQ.isLoading || usersQ.isLoading) ? "Searching…" : "No results"}
            </div>
          ) : (
            groups.map((group) => (
              <div key={group.name}>
                <div
                  style={{
                    padding: "0.4rem 0.6rem 0.2rem",
                    fontSize: "0.68rem",
                    fontWeight: 600,
                    textTransform: "uppercase",
                    letterSpacing: "0.06em",
                    color: "var(--color-fg-muted)",
                  }}
                >
                  {group.name}
                </div>
                {group.items.map(({ item, index }) => {
                  const isActive = index === active;
                  return (
                    <div
                      key={item.id}
                      id={item.id}
                      role="option"
                      aria-selected={isActive}
                      onMouseEnter={() => setActive(index)}
                      onClick={() => go(item)}
                      className="flex items-center gap-2"
                      style={{
                        padding: "0.4rem 0.6rem",
                        borderRadius: "0.4rem",
                        cursor: "pointer",
                        background: isActive ? "var(--color-bg-subtle)" : "transparent",
                      }}
                    >
                      <span style={{ color: "var(--color-fg-muted)", flexShrink: 0 }}>{item.icon}</span>
                      <span style={{ fontSize: "0.88rem", color: "var(--color-fg)", whiteSpace: "nowrap" }}>
                        {item.label}
                      </span>
                      {item.sublabel && (
                        <span
                          style={{
                            fontSize: "0.76rem",
                            color: "var(--color-fg-muted)",
                            overflow: "hidden",
                            textOverflow: "ellipsis",
                            whiteSpace: "nowrap",
                          }}
                        >
                          {item.sublabel}
                        </span>
                      )}
                    </div>
                  );
                })}
              </div>
            ))
          )}
        </div>
      </div>
    </div>
  );
}
