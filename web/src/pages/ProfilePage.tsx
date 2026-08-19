import { useMemo, useState } from "react";
import { useParams, Link, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import {
  fetchUserProfile,
  fetchUserReposByLoginPage,
  fetchUserOrgsByLogin,
  fetchUserProjectsV2,
  fetchUserEvents,
  fetchRepoReadme,
  fetchPackages,
  fetchAuthenticatedUser,
  fetchPinnedRepos,
  setPinnedRepos,
  checkFollowing,
  followUser,
  unfollowUser,
  ghFetch,
} from "../api.js";
import { decodeContentsBase64 } from "../utils/contents.js";
import type {
  BleephubRepo,
  GithubOrgSummary,
  GithubUserProfile,
  GithubProjectV2,
  GithubPackage,
  GithubUserEvent,
} from "../types.js";
import { Avatar } from "../components/Avatar.js";
import { SectionLabel, Blankslate, Box, Button } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import Markdown from "../components/Markdown.js";
import { ContributionGraph } from "../components/ContributionGraph.js";
import { RelativeTime } from "../components/RelativeTime.js";
import {
  RepoStatsLine,
  ForkedFromLine,
  LocationIcon,
  MailIcon,
} from "../components/RepoCardMeta.js";
import { walkRepoPages, walkNumberedPages, limitedGhFetch } from "../utils/uiFetch.js";
import {
  RepoIcon,
  BranchIcon,
  PeopleIcon,
  GlobeIcon,
  OrganizationIcon,
  LockIcon,
  StarIcon,
  PackageIcon,
  ProjectIcon,
  ClockIcon,
} from "../components/octicons.js";

type NavTab = "overview" | "repositories" | "projects" | "packages" | "stars";
// followers/following are reachable from the header counts, not the tab row.
type ProfileTab = NavTab | "followers" | "following";
const TABS: { key: NavTab; label: string; icon: React.ReactNode }[] = [
  { key: "overview", label: "Overview", icon: <RepoIcon size={15} /> },
  { key: "repositories", label: "Repositories", icon: <RepoIcon size={15} /> },
  { key: "projects", label: "Projects", icon: <ProjectIcon size={15} /> },
  { key: "packages", label: "Packages", icon: <PackageIcon size={15} /> },
  { key: "stars", label: "Stars", icon: <StarIcon size={15} /> },
];

export function ProfilePage() {
  const { login = "" } = useParams<{ login: string }>();
  const [params] = useSearchParams();
  const rawTab = params.get("tab");
  const tab: ProfileTab =
    TABS.some((t) => t.key === rawTab) || rawTab === "followers" || rawTab === "following"
      ? (rawTab as ProfileTab)
      : "overview";

  const profile = useQuery({
    queryKey: ["user-profile", login],
    queryFn: () => fetchUserProfile(login),
  });
  const orgs = useQuery({
    queryKey: ["user-orgs", login],
    queryFn: () => fetchUserOrgsByLogin(login),
  });

  if (profile.isLoading) return <Spinner label="loading profile" />;
  if (profile.isError || !profile.data) {
    return <InlineError title="Failed to load profile" detail={String(profile.error)} />;
  }

  return (
    <div className="grid gap-6 md:grid-cols-[296px_1fr]">
      <ProfileSidebar profile={profile.data} orgs={orgs.data} />
      <div>
        <ProfileTabs login={login} active={tab} repoCount={profile.data.public_repos} />
        <div className="mt-4">
          {tab === "overview" && <ProfileOverview login={login} />}
          {tab === "repositories" && <ProfileRepos login={login} />}
          {tab === "projects" && <ProfileProjects login={login} />}
          {tab === "packages" && <ProfilePackages login={login} />}
          {tab === "stars" && <ProfileStars login={login} />}
          {tab === "followers" && <ProfileFollows login={login} kind="followers" />}
          {tab === "following" && <ProfileFollows login={login} kind="following" />}
        </div>
      </div>
    </div>
  );
}

/** Underline tab nav mirroring github.com/{login} (Overview/Repositories/Projects/Packages/Stars). */
function ProfileTabs({
  login,
  active,
  repoCount,
}: {
  login: string;
  active: ProfileTab;
  repoCount?: number;
}) {
  return (
    <nav
      aria-label="Profile"
      className="flex items-center gap-1 overflow-x-auto"
      style={{ borderBottom: "1px solid var(--color-border)" }}
    >
      {TABS.map((t) => {
        const isActive = t.key === active;
        const to = t.key === "overview" ? `/ui/${login}` : `/ui/${login}?tab=${t.key}`;
        return (
          <Link
            key={t.key}
            to={to}
            aria-current={isActive ? "page" : undefined}
            className="inline-flex items-center gap-2"
            style={{
              padding: "0.5rem 0.75rem",
              fontSize: "0.85rem",
              fontWeight: isActive ? 600 : 400,
              color: isActive ? "var(--color-fg)" : "var(--color-fg-muted)",
              borderBottom: isActive ? "2px solid var(--color-accent-emphasis, var(--color-accent))" : "2px solid transparent",
              textDecoration: "none",
              whiteSpace: "nowrap",
            }}
          >
            <span style={{ color: "var(--color-fg-muted)" }}>{t.icon}</span>
            {t.label}
            {t.key === "repositories" && typeof repoCount === "number" && (
              <Counter>{repoCount}</Counter>
            )}
          </Link>
        );
      })}
    </nav>
  );
}

function Counter({ children }: { children: React.ReactNode }) {
  return (
    <span
      style={{
        fontSize: "0.72rem",
        fontWeight: 500,
        color: "var(--color-fg-muted)",
        background: "var(--color-bg-subtle)",
        border: "1px solid var(--color-border)",
        borderRadius: "2rem",
        padding: "0.02rem 0.5rem",
      }}
    >
      {children}
    </span>
  );
}

/**
 * Follow/Unfollow control for another user's profile. Renders nothing on the
 * viewer's own profile. The follow state comes from GET user/following/{login}
 * (204/404); the button toggles it via PUT/DELETE and refreshes both.
 */
function FollowButton({ login }: { login: string }) {
  const qc = useQueryClient();
  const viewer = useQuery({ queryKey: ["viewer"], queryFn: fetchAuthenticatedUser });
  const viewerLogin = typeof viewer.data?.login === "string" ? viewer.data.login : null;
  const isSelf = viewerLogin !== null && viewerLogin === login;
  const following = useQuery({
    queryKey: ["following", login],
    queryFn: () => checkFollowing(login),
    enabled: viewerLogin !== null && !isSelf,
  });
  const isFollowing = following.data === true;
  const toggle = useMutation({
    mutationFn: () => (isFollowing ? unfollowUser(login) : followUser(login)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["following", login] });
      qc.invalidateQueries({ queryKey: ["user-profile", login] });
    },
  });

  if (isSelf || viewerLogin === null) return null;
  return (
    <div>
      <Button size="sm" disabled={toggle.isPending} onClick={() => toggle.mutate()}>
        {isFollowing ? "Unfollow" : "Follow"}
      </Button>
      <MutationError of={toggle} />
    </div>
  );
}

function ProfileSidebar({
  profile,
  orgs,
}: {
  profile: GithubUserProfile;
  orgs?: GithubOrgSummary[] | undefined;
}) {
  const p = profile;
  return (
    <aside className="flex flex-col gap-3">
      <Avatar login={p.login} src={p.avatar_url} size={200} />
      <div>
        <div style={{ fontSize: "1.5rem", fontWeight: 600, lineHeight: 1.2 }}>{p.name || p.login}</div>
        <div style={{ fontSize: "1.15rem", color: "var(--color-fg-muted)", fontWeight: 300 }}>{p.login}</div>
      </div>
      <FollowButton login={p.login} />
      {p.bio && <p style={{ fontSize: "0.9rem", color: "var(--color-fg)" }}>{p.bio}</p>}
      <div className="flex items-center gap-2" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
        <PeopleIcon size={15} />
        <span>
          <Link to={`/ui/${p.login}?tab=followers`} style={{ color: "var(--color-fg-muted)", textDecoration: "none" }}>
            <strong style={{ color: "var(--color-fg)" }}>{p.followers}</strong> followers
          </Link>
        </span>
        <span>·</span>
        <span>
          <Link to={`/ui/${p.login}?tab=following`} style={{ color: "var(--color-fg-muted)", textDecoration: "none" }}>
            <strong style={{ color: "var(--color-fg)" }}>{p.following}</strong> following
          </Link>
        </span>
      </div>
      <ul className="flex flex-col gap-1.5" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", listStyle: "none", margin: 0, padding: 0 }}>
        {p.company && <MetaRow icon={<OrganizationIcon size={15} />}>{p.company}</MetaRow>}
        {p.location && <MetaRow icon={<LocationIcon size={15} />}>{p.location}</MetaRow>}
        {p.email && <MetaRow icon={<MailIcon size={15} />}>{p.email}</MetaRow>}
        {p.blog && (
          <MetaRow icon={<GlobeIcon size={15} />}>
            <a href={p.blog} style={{ color: "var(--color-accent)", textDecoration: "none" }} rel="noreferrer">
              {p.blog}
            </a>
          </MetaRow>
        )}
        {p.twitter_username && <MetaRow>@{p.twitter_username}</MetaRow>}
      </ul>
      {orgs && orgs.length > 0 && (
        <div>
          <SectionLabel>Organizations</SectionLabel>
          <div className="flex flex-wrap gap-2">
            {orgs.map((o) => (
              <Link key={o.id} to={`/ui/orgs/${o.login}`} title={o.login}>
                <Avatar login={o.login} src={o.avatar_url} size={32} square />
              </Link>
            ))}
          </div>
        </div>
      )}
      <div style={{ fontSize: "0.78rem", color: "var(--color-fg-subtle)" }}>
        Joined {new Date(p.created_at).toLocaleDateString()}
      </div>
    </aside>
  );
}

function MetaRow({ icon, children }: { icon?: React.ReactNode; children: React.ReactNode }) {
  return (
    <li className="flex items-center gap-2">
      {icon && <span style={{ color: "var(--color-fg-subtle)" }}>{icon}</span>}
      <span className="min-w-0 break-words">{children}</span>
    </li>
  );
}

// ─── Overview tab: profile README + contribution graph + recent activity ────────

function ProfileOverview({ login }: { login: string }) {
  const readme = useQuery({
    queryKey: ["profile-readme", login],
    // The profile README is the README of the <login>/<login> repo. A 404 (no
    // such repo / no README) is the common, non-error case — treat it as "none".
    queryFn: async () => {
      try {
        const file = await fetchRepoReadme(login, login);
        return decodeContentsBase64(file.content);
      } catch {
        return null;
      }
    },
    retry: false,
  });
  const events = useQuery({
    queryKey: ["user-events", login],
    queryFn: () => fetchUserEvents(login),
  });

  return (
    <div className="flex flex-col gap-5">
      <PinnedSection login={login} />

      {readme.data && (
        <Box header={<span className="inline-flex items-center gap-2"><RepoIcon size={14} />{login} / README.md</span>}>
          <div style={{ padding: "1rem 1.25rem" }} className="markdown-body">
            <Markdown>{readme.data}</Markdown>
          </div>
        </Box>
      )}

      <section>
        <SectionLabel>Contribution activity</SectionLabel>
        {events.isLoading && <Spinner label="loading activity" />}
        {events.isError && (
          <InlineError title="Failed to load activity" detail={String(events.error)} />
        )}
        {events.data && <ContributionGraph events={events.data} />}
      </section>

      {events.data && events.data.length > 0 && (
        <section>
          <SectionLabel>Recent activity</SectionLabel>
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {events.data.slice(0, 15).map((e, i) => (
              <ActivityRow key={e.id ?? i} event={e} />
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

// ─── Pinned repositories (profile Overview) ─────────────────────────────────────

function PinnedSection({ login }: { login: string }) {
  const qc = useQueryClient();
  const viewer = useQuery({ queryKey: ["viewer"], queryFn: fetchAuthenticatedUser });
  const isSelf = typeof viewer.data?.login === "string" && viewer.data.login === login;
  const pinnedQ = useQuery({ queryKey: ["pinned", login], queryFn: () => fetchPinnedRepos(login) });
  const [editing, setEditing] = useState(false);
  const muted = { fontSize: "0.85rem", color: "var(--color-fg-muted)" } as const;

  const pinned = pinnedQ.data ?? [];
  // On another user's profile with no pins, GitHub hides the section entirely.
  if (pinnedQ.isLoading) return null;
  if (!isSelf && pinned.length === 0) return null;

  return (
    <section>
      <div className="mb-2 flex items-center justify-between">
        <SectionLabel>Pinned</SectionLabel>
        {isSelf && (
          <Button size="sm" onClick={() => setEditing((v) => !v)}>
            {editing ? "Done" : "Customize your pins"}
          </Button>
        )}
      </div>
      {editing ? (
        <PinnedEditor
          login={login}
          current={pinned.map((r) => r.full_name)}
          onSaved={() => {
            qc.invalidateQueries({ queryKey: ["pinned", login] });
            setEditing(false);
          }}
        />
      ) : pinned.length === 0 ? (
        <p style={muted}>You have no pinned repositories yet. Use “Customize your pins”.</p>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2">
          {pinned.map((repo) => (
            <PinnedCard key={repo.id} repo={repo} />
          ))}
        </div>
      )}
    </section>
  );
}

function PinnedCard({ repo }: { repo: BleephubRepo }) {
  const [owner, name] = repo.full_name.split("/");
  return (
    <div style={{ border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)", padding: "0.85rem 1rem" }}>
      <Link
        to={`/ui/repos/${owner}/${name}`}
        className="inline-flex items-center gap-1"
        style={{ color: "var(--color-accent)", fontWeight: 600, fontSize: "0.9rem", textDecoration: "none" }}
      >
        <RepoIcon size={14} /> {repo.name}
      </Link>
      {repo.description && (
        <p className="mt-1" style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", maxWidth: "30rem" }}>
          {repo.description}
        </p>
      )}
      <div className="mt-2">
        <RepoStatsLine repo={repo} showUpdated={false} />
      </div>
    </div>
  );
}

function PinnedEditor({
  login,
  current,
  onSaved,
}: {
  login: string;
  current: string[];
  onSaved: () => void;
}) {
  const [selected, setSelected] = useState<string[]>(current);
  const reposQ = useQuery({
    queryKey: ["own-repos-for-pins", login],
    queryFn: () => fetchUserReposByLoginPage(login, { sort: "updated" }),
  });
  const saveMut = useMutation({
    mutationFn: () => setPinnedRepos(login, selected),
    onSuccess: onSaved,
  });
  const toggle = (fullName: string) =>
    setSelected((s) =>
      s.includes(fullName) ? s.filter((x) => x !== fullName) : s.length >= 6 ? s : [...s, fullName],
    );
  const repos = reposQ.data?.items ?? [];

  return (
    <Box>
      <div style={{ padding: "0.75rem 1rem" }}>
        <div style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", marginBottom: "0.5rem" }}>
          Select up to 6 repositories ({selected.length}/6)
        </div>
        {reposQ.isLoading ? (
          <Spinner label="loading repositories" />
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0, maxHeight: "16rem", overflowY: "auto" }}>
            {repos.map((r) => {
              const checked = selected.includes(r.full_name);
              return (
                <li key={r.id}>
                  <label className="flex items-center gap-2" style={{ padding: "0.25rem 0", fontSize: "0.85rem" }}>
                    <input
                      type="checkbox"
                      checked={checked}
                      disabled={!checked && selected.length >= 6}
                      onChange={() => toggle(r.full_name)}
                    />
                    {r.name}
                  </label>
                </li>
              );
            })}
          </ul>
        )}
        <MutationError of={saveMut} />
        <div className="mt-2 flex justify-end">
          <Button variant="primary" size="sm" disabled={saveMut.isPending} onClick={() => saveMut.mutate()}>
            {saveMut.isPending ? "Saving…" : "Save pins"}
          </Button>
        </div>
      </div>
    </Box>
  );
}

function describeEvent(e: GithubUserEvent): string {
  const repo = e.repo?.name ?? "a repository";
  switch (e.type) {
    case "PushEvent":
      return `Pushed ${e.payload?.size ?? ""} commit${e.payload?.size === 1 ? "" : "s"} to ${repo}`.replace("  ", " ");
    case "CreateEvent":
      return `Created ${e.payload?.ref_type ?? "a ref"} in ${repo}`;
    case "DeleteEvent":
      return `Deleted ${e.payload?.ref_type ?? "a ref"} in ${repo}`;
    case "IssuesEvent":
      return `${cap(e.payload?.action ?? "updated")} an issue in ${repo}`;
    case "IssueCommentEvent":
      return `Commented on an issue in ${repo}`;
    case "PullRequestEvent":
      return `${cap(e.payload?.action ?? "updated")} a pull request in ${repo}`;
    default:
      return `Activity in ${repo}`;
  }
}

function cap(s: string): string {
  return s.length ? s.charAt(0).toUpperCase() + s.slice(1) : s;
}

function ActivityRow({ event }: { event: GithubUserEvent }) {
  const repo = event.repo?.name;
  return (
    <li
      className="flex items-center gap-2"
      style={{ padding: "0.5rem 0", borderBottom: "1px solid var(--color-border)", fontSize: "0.85rem" }}
    >
      <span style={{ color: "var(--color-fg-subtle)" }}>
        <ClockIcon size={14} />
      </span>
      <span style={{ color: "var(--color-fg)" }}>{describeEvent(event)}</span>
      {repo && (
        <Link
          to={`/ui/repos/${repo}`}
          style={{ color: "var(--color-accent)", textDecoration: "none", marginLeft: "auto" }}
        >
          {repo}
        </Link>
      )}
      <span style={{ color: "var(--color-fg-muted)", fontSize: "0.76rem", marginLeft: repo ? 0 : "auto" }}>
        <RelativeTime iso={event.created_at} />
      </span>
    </li>
  );
}

// ─── Repositories tab (paginated owned repos) ───────────────────────────────────

type ProfileRepoType = "all" | "sources" | "forks" | "archived";
type ProfileRepoSort = "updated" | "name" | "stars";
const LOCAL_PAGE_SIZE = 30;

function ProfileRepos({ login }: { login: string }) {
  const [filter, setFilter] = useState("");
  const [type, setType] = useState<ProfileRepoType>("all");
  const [sortKey, setSortKey] = useState<ProfileRepoSort>("updated");
  const [pageUrl, setPageUrl] = useState<string | undefined>(undefined);
  const [pageStack, setPageStack] = useState<string[]>([]);
  const [localPage, setLocalPage] = useState(1);

  // Server-side portion of the controls: type sources/forks and the
  // updated/name sorts map straight onto the repos list API's query params.
  const serverFilters = useMemo(
    () => ({
      type: type === "sources" ? "sources" : type === "forks" ? "forks" : undefined,
      sort: (sortKey === "name" ? "full_name" : "updated") as "full_name" | "updated",
    }),
    [type, sortKey],
  );
  // Search text, the Archived type and the Stars sort have no server-side
  // query (as on real GitHub's REST list) — walk every page (capped) so
  // matches on later pages are still found, then finish client-side.
  const needsWalk = filter.trim() !== "" || type === "archived" || sortKey === "stars";

  const resetPaging = () => {
    setPageUrl(undefined);
    setPageStack([]);
    setLocalPage(1);
  };

  const pageQ = useQuery({
    queryKey: ["user-profile-repos", login, serverFilters, pageUrl ?? "first"],
    queryFn: () => fetchUserReposByLoginPage(login, serverFilters, pageUrl),
    enabled: !needsWalk,
  });
  const walkQ = useQuery({
    queryKey: ["user-profile-repos-walk", login, serverFilters, type],
    queryFn: () =>
      walkRepoPages(
        (f, u) => fetchUserReposByLoginPage(login, f, u),
        // The archived filter needs the unfiltered set (archived repos can be
        // sources or forks); other walks keep the server-side type narrowing.
        type === "archived" ? { sort: serverFilters.sort } : serverFilters,
      ),
    enabled: needsWalk,
  });

  const walked = useMemo(() => {
    if (!walkQ.data) return [];
    const q = filter.trim().toLowerCase();
    let repos = walkQ.data.items;
    if (type === "archived") repos = repos.filter((r) => r.archived);
    if (q) {
      repos = repos.filter(
        (r) => r.name.toLowerCase().includes(q) || (r.description ?? "").toLowerCase().includes(q),
      );
    }
    if (sortKey === "stars") {
      repos = [...repos].sort((a, b) => (b.stargazers_count ?? 0) - (a.stargazers_count ?? 0));
    }
    return repos;
  }, [walkQ.data, filter, type, sortKey]);

  const isLoading = needsWalk ? walkQ.isLoading : pageQ.isLoading;
  const isError = needsWalk ? walkQ.isError : pageQ.isError;
  const error = needsWalk ? walkQ.error : pageQ.error;

  const localPageCount = Math.max(1, Math.ceil(walked.length / LOCAL_PAGE_SIZE));
  const shown = needsWalk
    ? walked.slice((localPage - 1) * LOCAL_PAGE_SIZE, localPage * LOCAL_PAGE_SIZE)
    : pageQ.data?.items ?? [];

  const goNext = () => {
    if (needsWalk) {
      setLocalPage((p) => Math.min(localPageCount, p + 1));
      return;
    }
    if (!pageQ.data?.nextUrl) return;
    setPageStack((s) => [...s, pageUrl ?? ""]);
    setPageUrl(pageQ.data.nextUrl);
  };
  const goPrev = () => {
    if (needsWalk) {
      setLocalPage((p) => Math.max(1, p - 1));
      return;
    }
    setPageStack((s) => {
      const prev = s[s.length - 1];
      setPageUrl(prev || undefined);
      return s.slice(0, -1);
    });
  };
  const hasPrev = needsWalk ? localPage > 1 : pageStack.length > 0;
  const hasNext = needsWalk ? localPage < localPageCount : !!pageQ.data?.nextUrl;

  return (
    <section>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
        <SectionLabel>Repositories</SectionLabel>
        <div className="flex flex-wrap items-center gap-2">
          <input
            type="search"
            value={filter}
            onChange={(e) => {
              setFilter(e.target.value);
              setLocalPage(1);
            }}
            placeholder="Find a repository…"
            aria-label="Find a repository"
            style={{ fontSize: "0.82rem", minWidth: "14rem" }}
          />
          <select
            aria-label="Type"
            value={type}
            onChange={(e) => {
              setType(e.target.value as ProfileRepoType);
              resetPaging();
            }}
            style={{ fontSize: "0.82rem" }}
          >
            <option value="all">All</option>
            <option value="sources">Sources</option>
            <option value="forks">Forks</option>
            <option value="archived">Archived</option>
          </select>
          <select
            aria-label="Sort"
            value={sortKey}
            onChange={(e) => {
              setSortKey(e.target.value as ProfileRepoSort);
              resetPaging();
            }}
            style={{ fontSize: "0.82rem" }}
          >
            <option value="updated">Last updated</option>
            <option value="name">Name</option>
            <option value="stars">Stars</option>
          </select>
        </div>
      </div>

      {isLoading && <Spinner label="loading repositories" />}
      {isError && <InlineError title="Failed to load repositories" detail={String(error)} />}
      {walkQ.data?.truncated && needsWalk && (
        <p style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
          Searched the first {walkQ.data.items.length} repositories only.
        </p>
      )}
      {!isLoading &&
        !isError &&
        (needsWalk || pageQ.data) &&
        (shown.length === 0 ? (
          filter.trim() || type !== "all" ? (
            <Blankslate icon={<RepoIcon size={28} />} title="No matches">
              No repository matches the current filters.
            </Blankslate>
          ) : (
            <Blankslate icon={<RepoIcon size={28} />} title="No repositories">
              This user has no repositories.
            </Blankslate>
          )
        ) : (
          <ul style={{ borderTop: "1px solid var(--color-border)" }}>
            {shown.map((repo) => (
              <ProfileRepoRow key={repo.id} repo={repo} />
            ))}
          </ul>
        ))}

      {(hasPrev || hasNext) && (
        <div className="mt-4 flex items-center gap-2">
          <Button onClick={goPrev} disabled={!hasPrev}>
            Previous
          </Button>
          <Button onClick={goNext} disabled={!hasNext}>
            Next
          </Button>
        </div>
      )}
    </section>
  );
}

function ProfileRepoRow({ repo }: { repo: BleephubRepo }) {
  const [owner, name] = repo.full_name.split("/");
  return (
    <li style={{ padding: "0.9rem 0", borderBottom: "1px solid var(--color-border)" }}>
      <div className="flex items-center gap-2">
        <Link
          to={`/ui/repos/${owner}/${name}`}
          style={{ color: "var(--color-accent)", fontWeight: 600, fontSize: "1rem", textDecoration: "none" }}
        >
          {repo.name}
        </Link>
        <span
          className="inline-flex items-center gap-1"
          style={{
            fontSize: "0.68rem",
            fontWeight: 500,
            color: "var(--color-fg-muted)",
            border: "1px solid var(--color-border)",
            borderRadius: "2rem",
            padding: "0.05rem 0.5rem",
            textTransform: "capitalize",
          }}
        >
          {repo.private && <LockIcon size={11} />}
          {repo.private ? "Private" : repo.visibility || "Public"}
        </span>
        {repo.organization && (
          <span className="inline-flex items-center gap-1" style={{ fontSize: "0.72rem", color: "var(--color-fg-subtle)" }}>
            <OrganizationIcon size={12} /> {repo.organization.login}
          </span>
        )}
      </div>
      {repo.description && (
        <p className="mt-1" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", maxWidth: "44rem" }}>
          {repo.description}
        </p>
      )}
      <ForkedFromLine repo={repo} />
      <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1">
        <span
          className="inline-flex items-center gap-1"
          style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}
        >
          <BranchIcon size={13} /> {repo.default_branch}
        </span>
        <RepoStatsLine repo={repo} />
      </div>
    </li>
  );
}

// ─── Stars tab ──────────────────────────────────────────────────────────────────

type StarsSort = "recent" | "stars" | "name";

function ProfileStars({ login }: { login: string }) {
  const [sortKey, setSortKey] = useState<StarsSort>("recent");
  const [page, setPage] = useState(1);
  // Walk every page (capped) so a profile with >30 stars isn't silently
  // truncated to the server's first page; sort and paginate locally.
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["user-starred", login],
    queryFn: () => walkNumberedPages<BleephubRepo>(`/api/v3/users/${encodeURIComponent(login)}/starred`),
  });

  const sorted = useMemo(() => {
    const items = data?.items ?? [];
    if (sortKey === "stars") return [...items].sort((a, b) => (b.stargazers_count ?? 0) - (a.stargazers_count ?? 0));
    if (sortKey === "name") return [...items].sort((a, b) => a.full_name.localeCompare(b.full_name));
    return items;
  }, [data, sortKey]);

  const pageCount = Math.max(1, Math.ceil(sorted.length / LOCAL_PAGE_SIZE));
  const shown = sorted.slice((page - 1) * LOCAL_PAGE_SIZE, page * LOCAL_PAGE_SIZE);

  return (
    <section>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
        <SectionLabel>Starred repositories{data ? ` · ${sorted.length}` : ""}</SectionLabel>
        <select
          aria-label="Sort stars"
          value={sortKey}
          onChange={(e) => {
            setSortKey(e.target.value as StarsSort);
            setPage(1);
          }}
          style={{ fontSize: "0.82rem" }}
        >
          <option value="recent">Recently starred</option>
          <option value="stars">Most stars</option>
          <option value="name">Name</option>
        </select>
      </div>
      {isLoading && <Spinner label="loading stars" />}
      {isError && <InlineError title="Failed to load stars" detail={String(error)} />}
      {data &&
        (sorted.length === 0 ? (
          <Blankslate icon={<StarIcon size={28} />} title="No starred repositories">
            This user hasn’t starred any repositories yet.
          </Blankslate>
        ) : (
          <>
            <ul style={{ borderTop: "1px solid var(--color-border)" }}>
              {shown.map((repo) => (
                <ProfileRepoRow key={repo.id} repo={repo} />
              ))}
            </ul>
            {pageCount > 1 && (
              <div className="mt-4 flex items-center gap-2">
                <Button onClick={() => setPage((p) => Math.max(1, p - 1))} disabled={page === 1}>
                  Previous
                </Button>
                <span style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
                  Page {page} of {pageCount}
                </span>
                <Button onClick={() => setPage((p) => Math.min(pageCount, p + 1))} disabled={page === pageCount}>
                  Next
                </Button>
              </div>
            )}
          </>
        ))}
    </section>
  );
}

// ─── Packages tab ───────────────────────────────────────────────────────────────

function ProfilePackages({ login }: { login: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["user-packages", login],
    queryFn: () => fetchPackages({ kind: "user", username: login }) as Promise<GithubPackage[]>,
  });
  return (
    <section>
      <SectionLabel>Packages</SectionLabel>
      {isLoading && <Spinner label="loading packages" />}
      {isError && <InlineError title="Failed to load packages" detail={String(error)} />}
      {data &&
        (data.length === 0 ? (
          <Blankslate icon={<PackageIcon size={28} />} title="No packages">
            This user hasn’t published any packages.
          </Blankslate>
        ) : (
          <ul style={{ borderTop: "1px solid var(--color-border)" }}>
            {data.map((pkg) => (
              <li
                key={pkg.id}
                className="flex flex-wrap items-center gap-2"
                style={{ padding: "0.9rem 0", borderBottom: "1px solid var(--color-border)" }}
              >
                <PackageIcon size={16} />
                <span style={{ fontWeight: 600, fontSize: "0.95rem" }}>{pkg.name}</span>
                <span style={{ fontSize: "0.72rem", color: "var(--color-fg-muted)", textTransform: "capitalize" }}>
                  {pkg.package_type}
                </span>
                <span style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)", marginLeft: "auto" }}>
                  {pkg.version_count} version{pkg.version_count === 1 ? "" : "s"} · {pkg.visibility}
                </span>
              </li>
            ))}
          </ul>
        ))}
    </section>
  );
}

// ─── Projects tab (ProjectsV2) ──────────────────────────────────────────────────

function ProfileProjects({ login }: { login: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["user-projects-v2", login],
    queryFn: () => fetchUserProjectsV2(login),
  });
  return (
    <section>
      <SectionLabel>Projects</SectionLabel>
      {isLoading && <Spinner label="loading projects" />}
      {isError && <InlineError title="Failed to load projects" detail={String(error)} />}
      {data &&
        (data.length === 0 ? (
          <Blankslate icon={<ProjectIcon size={28} />} title="No projects">
            This user has no projects.
          </Blankslate>
        ) : (
          <ul style={{ borderTop: "1px solid var(--color-border)" }}>
            {data.map((proj: GithubProjectV2) => (
              <li
                key={proj.id}
                style={{ padding: "0.9rem 0", borderBottom: "1px solid var(--color-border)" }}
              >
                <div className="flex items-center gap-2">
                  <ProjectIcon size={16} />
                  <span style={{ fontWeight: 600, fontSize: "0.95rem" }}>{proj.title}</span>
                  <span style={{ fontSize: "0.72rem", color: "var(--color-fg-muted)" }}>#{proj.number}</span>
                </div>
                {proj.short_description && (
                  <p className="mt-1" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", maxWidth: "44rem" }}>
                    {proj.short_description}
                  </p>
                )}
              </li>
            ))}
          </ul>
        ))}
    </section>
  );
}

// ─── Followers / Following lists (reached from the header counts) ───────────────

interface FollowAccount {
  login: string;
  avatar_url: string;
}

function ProfileFollows({ login, kind }: { login: string; kind: "followers" | "following" }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["user-follows", login, kind],
    queryFn: () => ghFetch<FollowAccount[]>(`/api/v3/users/${encodeURIComponent(login)}/${kind}`),
  });
  return (
    <section>
      <SectionLabel>{kind === "followers" ? "Followers" : "Following"}</SectionLabel>
      {isLoading && <Spinner label={`loading ${kind}`} />}
      {isError && <InlineError title={`Failed to load ${kind}`} detail={String(error)} />}
      {data &&
        (data.length === 0 ? (
          <Blankslate icon={<PeopleIcon size={28} />} title={kind === "followers" ? "No followers yet" : "Not following anyone"}>
            {kind === "followers"
              ? "When people follow this user, they'll show up here."
              : "This user isn't following anyone yet."}
          </Blankslate>
        ) : (
          <ul className="grid gap-3 sm:grid-cols-2" style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {data.map((account) => (
              <li key={account.login}>
                <FollowAccountCard account={account} />
              </li>
            ))}
          </ul>
        ))}
    </section>
  );
}

/**
 * A follower/following row hydrated with the account's name/bio/location —
 * fetched lazily per row (concurrency-capped, cached under the same key the
 * profile page uses) — plus the Follow/Unfollow control.
 */
function FollowAccountCard({ account }: { account: FollowAccount }) {
  const { data } = useQuery({
    queryKey: ["user-profile", account.login],
    queryFn: () => limitedGhFetch<GithubUserProfile>(`/api/v3/users/${encodeURIComponent(account.login)}`),
    staleTime: 60_000,
    retry: false,
  });
  return (
    <Box style={{ padding: "0.75rem 1rem" }}>
      <div className="flex items-start gap-3">
        <Link to={`/ui/${account.login}`} style={{ flexShrink: 0 }}>
          <Avatar login={account.login} src={account.avatar_url} size={40} />
        </Link>
        <div className="min-w-0 flex-1">
          <Link
            to={`/ui/${account.login}`}
            style={{
              color: "var(--color-fg)",
              textDecoration: "none",
              display: "inline-block",
              lineHeight: "1.625rem",
            }}
          >
            <span style={{ fontWeight: 600, fontSize: "0.9rem" }}>{data?.name || account.login}</span>{" "}
            {data?.name && (
              <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>{account.login}</span>
            )}
          </Link>
          {data?.bio && (
            <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", margin: 0 }}>{data.bio}</p>
          )}
          {data?.location && (
            <p
              className="inline-flex items-center gap-1"
              style={{ fontSize: "0.76rem", color: "var(--color-fg-subtle)", margin: 0 }}
            >
              <LocationIcon size={12} /> {data.location}
            </p>
          )}
        </div>
        <FollowButton login={account.login} />
      </div>
    </Box>
  );
}
