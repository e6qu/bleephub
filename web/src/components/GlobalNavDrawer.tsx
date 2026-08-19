import { type ReactNode, useEffect, useRef } from "react";
import { NavLink } from "react-router";
import {
  Mark,
  SearchIcon,
  NotificationBellIcon,
  IssueOpenedIcon,
  PullRequestIcon,
  RepoIcon,
  GistIcon,
  PackageIcon,
  CodespaceIcon,
  MigrationIcon,
  OrganizationIcon,
  KeyIcon,
  ServerIcon,
  PeopleIcon,
  TeamIcon,
  GlobeIcon,
  AuditLogIcon,
  GraphIcon,
  CommentIcon,
} from "./octicons.js";

// ─── global-nav drawer (hamburger) ──────────────────────────────────────────
// Code-split out of the always-loaded AppHeader: it only mounts when the
// hamburger is clicked, so its two nav tables + drawer chrome load on demand
// rather than sitting in the entry bundle.

type DrawerItem = { label: string; to: string; icon: ReactNode; end?: boolean };

// Issues / Pull requests are viewer-scoped when signed in, matching
// github.com's global nav ("your" issues and PRs, via search qualifiers).
function githubNav(viewerLogin?: string): DrawerItem[] {
  const issuesQ = viewerLogin ? `is:issue author:${viewerLogin}` : "is:issue";
  const pullsQ = viewerLogin ? `is:pr author:${viewerLogin}` : "is:pr";
  return [
  { label: "Dashboard", to: "/ui/", icon: <RepoIcon size={16} />, end: true },
  { label: "Issues", to: `/ui/search?type=issues&q=${encodeURIComponent(issuesQ)}`, icon: <IssueOpenedIcon size={16} /> },
  { label: "Pull requests", to: `/ui/search?type=issues&q=${encodeURIComponent(pullsQ)}`, icon: <PullRequestIcon size={16} /> },
  { label: "Repositories", to: "/ui/repos", icon: <RepoIcon size={16} /> },
  { label: "Gists", to: "/ui/gists", icon: <GistIcon size={16} /> },
  { label: "Packages", to: "/ui/packages", icon: <PackageIcon size={16} /> },
  { label: "Marketplace", to: "/ui/marketplace", icon: <PackageIcon size={16} /> },
  { label: "Codespaces", to: "/ui/codespaces", icon: <CodespaceIcon size={16} /> },
  { label: "Copilot Spaces", to: "/ui/copilot/spaces", icon: <CommentIcon size={16} /> },
  { label: "Classroom", to: "/ui/classrooms", icon: <PeopleIcon size={16} /> },
  { label: "Migrations", to: "/ui/migrations", icon: <MigrationIcon size={16} /> },
  { label: "Notifications", to: "/ui/notifications", icon: <NotificationBellIcon size={16} /> },
  { label: "Explore", to: "/ui/search", icon: <SearchIcon size={16} /> },
  ];
}

// Bleephub service administration surfaces that map to public GitHub or GitHub
// Enterprise Server routes stay grouped away from the repository/product nav.
const OPS_NAV: DrawerItem[] = [
  { label: "System status", to: "/ui/operations", icon: <GraphIcon size={16} />, end: true },
  { label: "Workflow runs", to: "/ui/workflows", icon: <RepoIcon size={16} /> },
  { label: "Runners", to: "/ui/runners", icon: <ServerIcon size={16} /> },
  { label: "Metrics", to: "/ui/metrics", icon: <GraphIcon size={16} /> },
  { label: "GitHub Apps", to: "/ui/apps", icon: <KeyIcon size={16} /> },
  { label: "OAuth Apps", to: "/ui/oauth", icon: <KeyIcon size={16} /> },
  { label: "Users", to: "/ui/operations/users", icon: <PeopleIcon size={16} /> },
  { label: "Organizations", to: "/ui/operations/orgs", icon: <OrganizationIcon size={16} /> },
  { label: "Teams", to: "/ui/operations/teams", icon: <TeamIcon size={16} /> },
  { label: "Enterprise", to: "/ui/operations/enterprise", icon: <GlobeIcon size={16} /> },
  { label: "Audit log", to: "/ui/operations/audit-log", icon: <AuditLogIcon size={16} /> },
];

function DrawerSection({ title, items, onNavigate }: { title: string; items: DrawerItem[]; onNavigate: () => void }) {
  return (
    <div style={{ padding: "0.5rem 0" }}>
      <div
        style={{
          fontSize: "0.72rem",
          fontWeight: 600,
          textTransform: "uppercase",
          letterSpacing: "0.04em",
          color: "var(--color-fg-muted)",
          padding: "0.25rem 0.75rem",
        }}
      >
        {title}
      </div>
      {items.map((it) => (
        <NavLink
          key={it.to}
          to={it.to}
          end={it.end ?? false}
          onClick={onNavigate}
          style={{ textDecoration: "none" }}
        >
          {({ isActive }) => (
            <span
              className="flex items-center gap-2.5"
              style={{
                padding: "0.45rem 0.75rem",
                fontSize: "0.9rem",
                color: isActive ? "var(--color-fg)" : "var(--color-fg-muted)",
                fontWeight: isActive ? 600 : 500,
                borderLeft: `2px solid ${isActive ? "var(--color-accent)" : "transparent"}`,
                background: isActive ? "color-mix(in srgb, var(--color-fg-muted) 10%, transparent)" : "transparent",
              }}
            >
              {it.icon}
              {it.label}
            </span>
          )}
        </NavLink>
      ))}
    </div>
  );
}

export function GlobalNavDrawer({
  open,
  onClose,
  viewerLogin,
}: {
  open: boolean;
  onClose: () => void;
  viewerLogin?: string | undefined;
}) {
  const navRef = useRef<HTMLElement>(null);
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    document.addEventListener("keydown", onKey);
    // Move focus into the drawer on open and restore it to the trigger on
    // close, so a keyboard user is not left focused on the obscured page
    // behind the backdrop.
    const previouslyFocused = document.activeElement as HTMLElement | null;
    const first = navRef.current?.querySelector<HTMLElement>(
      'a[href], button:not([disabled]), [tabindex]:not([tabindex="-1"])',
    );
    (first ?? navRef.current)?.focus();
    return () => {
      document.removeEventListener("keydown", onKey);
      previouslyFocused?.focus();
    };
  }, [open, onClose]);
  if (!open) return null;
  return (
    <>
      <div
        onClick={onClose}
        style={{ position: "fixed", inset: 0, background: "rgba(0,0,0,0.35)", zIndex: 70 }}
      />
      <nav
        ref={navRef}
        tabIndex={-1}
        aria-label="Global"
        style={{
          position: "fixed",
          top: 0,
          left: 0,
          bottom: 0,
          width: 300,
          maxWidth: "85vw",
          background: "var(--color-bg)",
          borderRight: "1px solid var(--color-border)",
          boxShadow: "2px 0 16px rgba(0,0,0,0.18)",
          zIndex: 71,
          overflowY: "auto",
        }}
      >
        <div className="flex items-center gap-2" style={{ padding: "0.9rem 0.9rem 0.5rem" }}>
          <Mark size={22} />
          <span style={{ fontWeight: 600 }}>bleephub</span>
        </div>
        <DrawerSection title="GitHub" items={githubNav(viewerLogin)} onNavigate={onClose} />
        <div style={{ height: 1, background: "var(--color-border)" }} />
        <DrawerSection title="Operations" items={OPS_NAV} onNavigate={onClose} />
      </nav>
    </>
  );
}
