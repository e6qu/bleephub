import {
  useEffect,
  useRef,
  useState,
  lazy,
  Suspense,
  type ReactNode,
  type CSSProperties,
  type FormEvent,
  type KeyboardEvent as ReactKeyboardEvent,
} from "react";
import { Link, useLocation, useNavigate } from "react-router";

// The command palette is behind a keyboard shortcut, so it is code-split out of
// the entry bundle and loaded on first ⌘K rather than at initial page load.
const CommandPalette = lazy(() =>
  import("./CommandPalette.js").then((m) => ({ default: m.CommandPalette })),
);
// The "?" shortcuts sheet and the global `g …` navigation sequences live in a
// lazily-loaded module so their code (and the modal) stay out of the entry
// bundle rather than weighing on first paint.
const GlobalShortcuts = lazy(() =>
  import("./GlobalShortcuts.js").then((m) => ({ default: m.GlobalShortcuts })),
);
// The global-nav drawer only mounts on a hamburger click, so it is code-split
// out of the always-loaded header too.
const GlobalNavDrawer = lazy(() =>
  import("./GlobalNavDrawer.js").then((m) => ({ default: m.GlobalNavDrawer })),
);
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTheme } from "@bleephub/ui-core/hooks";
import { useDismiss } from "../hooks/useDismiss.js";
import { useReportError, useToastQueryErrors } from "@bleephub/ui-core/components";
import {
  Mark,
  ThreeBarsIcon,
  SearchIcon,
  PlusIcon,
  TriangleDownIcon,
  NotificationBellIcon,
  IssueOpenedIcon,
  PullRequestIcon,
  SunIcon,
  MoonIcon,
  SignOutIcon,
  RepoIcon,
  GistIcon,
  PackageIcon,
  CodespaceIcon,
  MigrationIcon,
  OrganizationIcon,
  KeyIcon,
  PeopleIcon,
  GraphIcon,
  ProjectIcon,
  StarIcon,
} from "./octicons.js";
import { abortPendingRequests, clearToken, fetchCurrentUser, fetchNotifications, isRateLimited } from "../api.js";
import { loginPath, useSignedIn } from "../session.js";

/**
 * GitHub-faithful global header: hamburger → global-nav drawer, brand, a
 * search box, a "create" menu, Issues / Pull requests quick links, the
 * notifications bell, and an avatar dropdown. It mirrors github.com's chrome
 * so the app's information architecture matches a user's GitHub muscle memory;
 * only the visual styling is bleephub's own.
 */

// ─── click-outside dropdown menu ────────────────────────────────────────────

function HeaderMenu({
  label,
  trigger,
  align = "right",
  shauthUser,
  children,
}: {
  label: string;
  trigger: ReactNode;
  align?: "left" | "right";
  /** The signed-in username, published on the always-visible trigger so
   * post-deployment qualification can find the identity and open this menu to
   * reach the real sign-out control inside it. */
  shauthUser?: string | undefined;
  children: (close: () => void) => ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const close = () => setOpen(false);
  const ref = useDismiss(open, close);
  useEffect(() => {
    if (!open) return;
    menuRef.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus();
  }, [open]);
  const onMenuKeyDown = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    const items = Array.from(menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []);
    if (items.length === 0) return;
    const current = Math.max(items.indexOf(document.activeElement as HTMLElement), 0);
    let target: HTMLElement | undefined;
    switch (event.key) {
      case "ArrowDown":
        target = items[(current + 1) % items.length];
        break;
      case "ArrowUp":
        target = items[(current - 1 + items.length) % items.length];
        break;
      case "Home":
        target = items[0];
        break;
      case "End":
        target = items.at(-1);
        break;
      case "Escape":
        event.preventDefault();
        close();
        triggerRef.current?.focus();
        return;
      default:
        return;
    }
    event.preventDefault();
    target?.focus();
  };
  return (
    <div ref={ref} style={{ position: "relative" }}>
      <button
        ref={triggerRef}
        type="button"
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        data-shauth-user={shauthUser}
        onClick={() => setOpen((v) => !v)}
        className="app-header-control inline-flex items-center gap-1"
        style={{
          background: "transparent",
          color: "var(--color-fg-muted)",
          border: "1px solid var(--color-border)",
          borderRadius: "var(--radius-md)",
          padding: "0.3rem 0.5rem",
          cursor: "pointer",
        }}
      >
        {trigger}
      </button>
      {open && (
        <div
          ref={menuRef}
          role="menu"
          aria-label={label}
          onKeyDown={onMenuKeyDown}
          style={{
            position: "absolute",
            top: "calc(100% + 6px)",
            [align]: 0,
            minWidth: 220,
            background: "var(--color-bg)",
            border: "1px solid var(--color-border)",
            borderRadius: "var(--radius-md)",
            boxShadow: "0 8px 24px rgba(0,0,0,0.18)",
            zIndex: 60,
            padding: "0.35rem",
          }}
        >
          {children(close)}
        </div>
      )}
    </div>
  );
}

function MenuLink({ to, icon, children, onClick }: { to: string; icon?: ReactNode; children: ReactNode; onClick: () => void }) {
  return (
    <Link
      to={to}
      role="menuitem"
      onClick={onClick}
      className="flex items-center gap-2"
      style={{
        textDecoration: "none",
        color: "var(--color-fg)",
        fontSize: "0.85rem",
        padding: "0.4rem 0.5rem",
        borderRadius: "var(--radius-sm)",
      }}
    >
      {icon}
      {children}
    </Link>
  );
}

function MenuButton({ icon, children, onClick, type = "button", shauthSignOut }: { icon?: ReactNode; children: ReactNode; onClick?: () => void; type?: "button" | "submit"; shauthSignOut?: boolean }) {
  return (
    <button
      type={type}
      role="menuitem"
      onClick={onClick}
      data-shauth-sign-out={shauthSignOut ? "" : undefined}
      className="flex w-full items-center gap-2"
      style={{
        background: "transparent",
        border: "none",
        color: "var(--color-fg)",
        fontSize: "0.85rem",
        padding: "0.4rem 0.5rem",
        borderRadius: "var(--radius-sm)",
        cursor: "pointer",
        textAlign: "left",
      }}
    >
      {icon}
      {children}
    </button>
  );
}

function MenuSeparator() {
  return <div role="separator" style={{ height: 1, background: "var(--color-border)", margin: "0.35rem 0" }} />;
}

// ─── header ─────────────────────────────────────────────────────────────────

// Display/alignment intentionally live in the className (see iconButtonClass)
// rather than here: an inline `display` would override the responsive `hidden`
// utilities that collapse some of these controls at phone widths.
function iconButtonStyle(): CSSProperties {
  return {
    background: "transparent",
    color: "var(--color-fg-muted)",
    border: "1px solid var(--color-border)",
    borderRadius: "var(--radius-md)",
    height: 32,
    width: 32,
    position: "relative",
  };
}

const iconButtonClass = "app-header-control inline-flex items-center justify-center";
// Phone widths can't fit every header control; like github.com, the Issues /
// Pull requests quick links collapse below md (they remain reachable through
// the global-nav drawer and search).
const iconButtonMdUpClass = "app-header-control hidden items-center justify-center md:inline-flex";

function Avatar({ login, url, size = 24 }: { login: string; url?: string | undefined; size?: number }) {
  if (url) {
    return <img src={url} alt="" width={size} height={size} style={{ borderRadius: "50%", display: "block" }} />;
  }
  const initials = login.slice(0, 2).toUpperCase();
  return (
    <span
      aria-hidden
      className="inline-flex items-center justify-center"
      style={{
        width: size,
        height: size,
        borderRadius: "50%",
        background: "var(--color-accent)",
        color: "var(--color-accent-fg)",
        fontSize: size * 0.42,
        fontWeight: 600,
      }}
    >
      {initials}
    </span>
  );
}

export function AppHeader() {
  const navigate = useNavigate();
  const location = useLocation();
  // Inside a repo route the search box scopes to that repository, like
  // github.com's header search. Lightweight pathname parse — the header has no
  // route params of its own.
  const repoScope = location.pathname.match(/^\/ui\/repos\/([^/]+)\/([^/]+)/);
  const scopedRepo = repoScope ? `${decodeURIComponent(repoScope[1]!)}/${decodeURIComponent(repoScope[2]!)}` : null;
  const queryClient = useQueryClient();
  const reportError = useReportError();
  const { resolvedTheme, toggle } = useTheme("light");
  const isDark = resolvedTheme === "dark";
  const [drawer, setDrawer] = useState(false);
  const [q, setQ] = useState("");
  const [scope, setScope] = useState("repositories");
  const [paletteOpen, setPaletteOpen] = useState(false);

  // A signed-out visitor gets github.com's anonymous header: brand + search +
  // a Sign in button. No create menu, bell, avatar menu, drawer, palette or
  // shortcuts — and, critically, none of their viewer-scoped fetches/polls.
  const signedIn = useSignedIn();

  // Global ⌘K / Ctrl-K opens the command palette (github.com's "jump to").
  useEffect(() => {
    if (!signedIn) return;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && !e.altKey && (e.key === "k" || e.key === "K")) {
        e.preventDefault();
        setPaletteOpen((v) => !v);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [signedIn]);

  // A throttled 403 is final for the current window: retrying it only deepens
  // the exhaustion (same guard pattern as useMetricsData). Surface it instead
  // of spinning, so a rate-limited session fails visibly.
  const { data: user, error: userError } = useQuery({
    queryKey: ["current-user"],
    queryFn: ({ signal }) => fetchCurrentUser(signal),
    staleTime: 60_000,
    retry: (failureCount, err) => !isRateLimited(err) && failureCount < 1,
    enabled: signedIn,
  });
  useToastQueryErrors(isRateLimited(userError) ? userError : undefined, "API rate limit exceeded");
  // enabled:false also stops the 30s poll — an anonymous session must never
  // touch the notifications endpoint (it would 401 on every tick).
  const { data: notifications } = useQuery({
    queryKey: ["notifications", "header"],
    queryFn: ({ signal }) => fetchNotifications({}, signal),
    refetchInterval: (query) => (isRateLimited(query.state.error) ? false : 30_000),
    enabled: signedIn,
  });
  // Tolerate a non-array body: the header must keep rendering (and the
  // sign-out transition must keep working) even when this poll gets garbage.
  const unread = Array.isArray(notifications) ? notifications.filter((n) => n.unread).length : 0;
  const login = user?.login ?? "";

  const submitSearch = (e: FormEvent) => {
    e.preventDefault();
    let term = q.trim();
    // Inside a repository, an unqualified query searches that repository —
    // github.com's "In this repository" scope.
    if (scopedRepo && term && !/(^|\s)repo:/.test(term)) term = `repo:${scopedRepo} ${term}`;
    const typeParam = scope !== "repositories" ? `&type=${scope}` : "";
    navigate(term ? `/ui/search?q=${encodeURIComponent(term)}${typeParam}` : `/ui/search?type=${scope}`);
  };

  const submitLogout = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const form = event.currentTarget;
    // cancelQueries alone was not enough: nothing in the API layer honoured an
    // abort signal, so in-flight polls kept running, landed after the token was
    // gone, and 401'd. Abort them for real before navigating.
    try {
      await queryClient.cancelQueries();
    } catch (err) {
      reportError(err, "Sign-out could not cancel in-flight requests");
    }
    abortPendingRequests();
    // Do not clear the cache while this page is still mounted. Active query
    // observers immediately resubscribe after clear(), schedule fresh API
    // reads, and race the token removal below with unauthenticated 401s. The
    // native form navigation unloads this document and its cache moments later.
    clearToken();
    form.submit();
  };

  return (
    <>
      {signedIn && (
        <Suspense fallback={null}>
          <GlobalNavDrawer open={drawer} onClose={() => setDrawer(false)} viewerLogin={login || undefined} />
        </Suspense>
      )}
      <header className="app-header">
        <div className="mx-auto flex max-w-[1280px] items-center gap-3 px-4 py-2.5">
          {signedIn && (
            <button type="button" aria-label="Open global navigation" onClick={() => setDrawer(true)} className={iconButtonClass} style={iconButtonStyle()}>
              <ThreeBarsIcon size={16} />
            </button>
          )}

          <Link to="/ui/" className="inline-flex items-center gap-2" style={{ textDecoration: "none", color: "var(--color-fg)", minHeight: "1.625rem" }}>
            <Mark size={24} />
            <span style={{ fontWeight: 600, fontSize: "0.95rem" }} className="hidden sm:inline">
              bleephub
            </span>
          </Link>

          <form onSubmit={submitSearch} className="flex min-w-0 flex-1 items-center" style={{ maxWidth: 480 }}>
            <div
              className="app-header-search flex w-full min-w-0 items-center gap-2"
              style={{
                border: "1px solid var(--color-border)",
                borderRadius: "var(--radius-md)",
                background: "var(--color-bg)",
                padding: "0.3rem 0.55rem",
              }}
            >
              <SearchIcon size={14} style={{ color: "var(--color-fg-muted)" }} />
              <select
                aria-label="Search scope"
                value={scope}
                onChange={(e) => setScope(e.target.value)}
                className="hidden sm:block"
                style={{
                  border: "none",
                  outline: "none",
                  background: "transparent",
                  color: "var(--color-fg-muted)",
                  fontSize: "0.78rem",
                }}
              >
                <option value="repositories">Repos</option>
                <option value="code">Code</option>
                <option value="issues">Issues</option>
                <option value="users">Users &amp; orgs</option>
              </select>
              {scopedRepo && (
                <span
                  title={`Searches are scoped to ${scopedRepo}`}
                  className="hidden md:inline"
                  style={{ fontSize: "0.72rem", color: "var(--color-fg-muted)", whiteSpace: "nowrap" }}
                >
                  In this repository
                </span>
              )}
              <input
                id="global-search-input"
                type="search"
                value={q}
                onChange={(e) => setQ(e.target.value)}
                placeholder="Search or jump to…"
                aria-label={scopedRepo ? `Search in ${scopedRepo}` : "Search"}
                style={{
                  flex: 1,
                  minWidth: 0,
                  border: "none",
                  outline: "none",
                  background: "transparent",
                  color: "var(--color-fg)",
                  fontSize: "0.85rem",
                }}
              />
            </div>
          </form>

          {!signedIn && (
            // github.com's anonymous header ends in a Sign in affordance that
            // returns the visitor to the page they were reading.
            <Link
              to={loginPath(location)}
              className="app-header-control inline-flex items-center"
              style={{
                border: "1px solid var(--color-border)",
                borderRadius: "var(--radius-md)",
                padding: "0.3rem 0.75rem",
                color: "var(--color-fg)",
                textDecoration: "none",
                fontSize: "0.85rem",
                fontWeight: 600,
                whiteSpace: "nowrap",
                minHeight: "1.625rem",
              }}
            >
              Sign in
            </Link>
          )}
          {signedIn && (
          <div className="flex items-center gap-2">
            {/* create menu */}
            <HeaderMenu label="Create new…" trigger={<><PlusIcon size={14} /><TriangleDownIcon size={12} /></>}>
              {(close) => (
                <>
                  <MenuLink to="/ui/repos?new=1" icon={<RepoIcon size={16} />} onClick={close}>New repository</MenuLink>
                  <MenuLink to="/ui/migrations" icon={<MigrationIcon size={16} />} onClick={close}>Import repository</MenuLink>
                  <MenuLink to="/ui/gists?new=1" icon={<GistIcon size={16} />} onClick={close}>New gist</MenuLink>
                  <MenuLink to="/ui/operations/orgs?new=1" icon={<OrganizationIcon size={16} />} onClick={close}>New organization</MenuLink>
                  {login && <MenuLink to={`/ui/${login}?tab=projects`} icon={<ProjectIcon size={16} />} onClick={close}>New project</MenuLink>}
                  <MenuLink to="/ui/codespaces" icon={<CodespaceIcon size={16} />} onClick={close}>New codespace</MenuLink>
                </>
              )}
            </HeaderMenu>

            <Link
              to={`/ui/search?type=issues&q=${encodeURIComponent(login ? `is:issue author:${login}` : "is:issue")}`}
              aria-label={login ? "Your issues" : "Issues"}
              title={login ? "Your issues" : "Issues"}
              className={iconButtonMdUpClass}
              style={iconButtonStyle()}
            >
              <IssueOpenedIcon size={16} />
            </Link>
            <Link
              to={`/ui/search?type=issues&q=${encodeURIComponent(login ? `is:pr author:${login}` : "is:pr")}`}
              aria-label={login ? "Your pull requests" : "Pull requests"}
              title={login ? "Your pull requests" : "Pull requests"}
              className={iconButtonMdUpClass}
              style={iconButtonStyle()}
            >
              <PullRequestIcon size={16} />
            </Link>

            <Link to="/ui/notifications" aria-label={unread ? `Notifications (${unread} unread)` : "Notifications"} title="Notifications" className={iconButtonClass} style={iconButtonStyle()}>
              <NotificationBellIcon size={16} />
              {unread > 0 && (
                <span
                  aria-hidden
                  style={{
                    position: "absolute",
                    top: -4,
                    right: -4,
                    minWidth: 16,
                    height: 16,
                    padding: "0 4px",
                    borderRadius: 8,
                    background: "var(--color-accent)",
                    color: "var(--color-accent-fg)",
                    fontSize: "0.62rem",
                    fontWeight: 700,
                    display: "inline-flex",
                    alignItems: "center",
                    justifyContent: "center",
                  }}
                >
                  {unread > 99 ? "99+" : unread}
                </span>
              )}
            </Link>

            {/* avatar menu */}
            <HeaderMenu label="Open user menu" align="right" shauthUser={login || undefined} trigger={<><Avatar login={login} url={user?.avatar_url} /><TriangleDownIcon size={12} /></>}>
              {(close) => (
                <>
                  <div style={{ padding: "0.35rem 0.5rem", fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
                    Signed in as <strong style={{ color: "var(--color-fg)" }}>{login || "…"}</strong>
                  </div>
                  <MenuSeparator />
                  {login && <MenuLink to={`/ui/${login}`} icon={<PeopleIcon size={16} />} onClick={close}>My profile</MenuLink>}
                  <MenuLink to="/ui/repos" icon={<RepoIcon size={16} />} onClick={close}>My repositories</MenuLink>
                  <MenuLink to="/ui/gists" icon={<GistIcon size={16} />} onClick={close}>My gists</MenuLink>
                  <MenuLink to="/ui/packages" icon={<PackageIcon size={16} />} onClick={close}>My packages</MenuLink>
                  <MenuLink to="/ui/codespaces" icon={<CodespaceIcon size={16} />} onClick={close}>My codespaces</MenuLink>
                  <MenuLink to="/ui/settings/organizations" icon={<OrganizationIcon size={16} />} onClick={close}>Your organizations</MenuLink>
                  {login && <MenuLink to={`/ui/${login}?tab=projects`} icon={<ProjectIcon size={16} />} onClick={close}>Your projects</MenuLink>}
                  {login && <MenuLink to={`/ui/${login}?tab=stars`} icon={<StarIcon size={16} />} onClick={close}>Your stars</MenuLink>}
                  <MenuSeparator />
                  <MenuLink to="/ui/account" icon={<KeyIcon size={16} />} onClick={close}>Settings</MenuLink>
                  <MenuLink to="/ui/operations" icon={<GraphIcon size={16} />} onClick={close}>Operations</MenuLink>
                  <MenuSeparator />
                  <MenuButton icon={isDark ? <SunIcon size={16} /> : <MoonIcon size={16} />} onClick={() => { toggle(); close(); }}>
                    {isDark ? "Light theme" : "Dark theme"}
                  </MenuButton>
                  <form method="post" action="/auth/logout" onSubmit={(event) => void submitLogout(event)}>
                    <MenuButton type="submit" icon={<SignOutIcon size={16} />} shauthSignOut>Sign out</MenuButton>
                  </form>
                </>
              )}
            </HeaderMenu>
          </div>
          )}
        </div>
      </header>
      {signedIn && paletteOpen && (
        <Suspense fallback={null}>
          <CommandPalette open onClose={() => setPaletteOpen(false)} viewerLogin={login || undefined} />
        </Suspense>
      )}
      {signedIn && (
        <Suspense fallback={null}>
          <GlobalShortcuts login={login} />
        </Suspense>
      )}
    </>
  );
}
