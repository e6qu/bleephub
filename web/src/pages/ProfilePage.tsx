import { useMemo, useState } from "react";
import { useParams, Link, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import {
  fetchUserProfile,
  fetchUserReposByLoginPage,
  fetchUserOrgsByLogin,
  fetchUserStarredRepos,
  fetchUserProjectsV2,
  fetchUserEvents,
  fetchRepoReadme,
  fetchPackages,
  fetchAuthenticatedUser,
  checkFollowing,
  followUser,
  unfollowUser,
} from "../api.js";
import { decodeContentsBase64 } from "../utils/workflowDispatch.js";
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

type ProfileTab = "overview" | "repositories" | "projects" | "packages" | "stars";
const TABS: { key: ProfileTab; label: string; icon: React.ReactNode }[] = [
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
  const tab: ProfileTab = TABS.some((t) => t.key === rawTab)
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
          <strong style={{ color: "var(--color-fg)" }}>{p.followers}</strong> followers
        </span>
        <span>·</span>
        <span>
          <strong style={{ color: "var(--color-fg)" }}>{p.following}</strong> following
        </span>
      </div>
      <ul className="flex flex-col gap-1.5" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", listStyle: "none", margin: 0, padding: 0 }}>
        {p.company && <MetaRow>{p.company}</MetaRow>}
        {p.location && <MetaRow>{p.location}</MetaRow>}
        {p.email && <MetaRow>{p.email}</MetaRow>}
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
        {new Date(event.created_at).toLocaleDateString()}
      </span>
    </li>
  );
}

// ─── Repositories tab (paginated owned repos) ───────────────────────────────────

function ProfileRepos({ login }: { login: string }) {
  const [filter, setFilter] = useState("");
  const [pageUrl, setPageUrl] = useState<string | undefined>(undefined);
  const [pageStack, setPageStack] = useState<string[]>([]);

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["user-profile-repos", login, pageUrl ?? "first"],
    queryFn: () => fetchUserReposByLoginPage(login, { sort: "updated" }, pageUrl),
  });

  const filtered = useMemo(() => {
    if (!data) return [];
    const q = filter.trim().toLowerCase();
    if (!q) return data.items;
    return data.items.filter(
      (r) => r.name.toLowerCase().includes(q) || (r.description ?? "").toLowerCase().includes(q),
    );
  }, [data, filter]);

  const goNext = () => {
    if (!data?.nextUrl) return;
    setPageStack((s) => [...s, pageUrl ?? ""]);
    setPageUrl(data.nextUrl);
  };
  const goPrev = () => {
    setPageStack((s) => {
      const prev = s[s.length - 1];
      setPageUrl(prev || undefined);
      return s.slice(0, -1);
    });
  };

  return (
    <section>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
        <SectionLabel>Repositories</SectionLabel>
        <input
          type="search"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Find a repository…"
          aria-label="Find a repository"
          style={{ fontSize: "0.82rem", minWidth: "14rem" }}
        />
      </div>

      {isLoading && <Spinner label="loading repositories" />}
      {isError && <InlineError title="Failed to load repositories" detail={String(error)} />}
      {data &&
        (data.items.length === 0 ? (
          <Blankslate icon={<RepoIcon size={28} />} title="No repositories">
            This user has no repositories.
          </Blankslate>
        ) : filtered.length === 0 ? (
          <Blankslate icon={<RepoIcon size={28} />} title="No matches">
            No repository matches “{filter}”.
          </Blankslate>
        ) : (
          <ul style={{ borderTop: "1px solid var(--color-border)" }}>
            {filtered.map((repo) => (
              <ProfileRepoRow key={repo.id} repo={repo} />
            ))}
          </ul>
        ))}

      {(pageStack.length > 0 || data?.nextUrl) && (
        <div className="mt-4 flex items-center gap-2">
          <Button onClick={goPrev} disabled={pageStack.length === 0}>
            Previous
          </Button>
          <Button onClick={goNext} disabled={!data?.nextUrl}>
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
      <div
        className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1"
        style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}
      >
        <span className="inline-flex items-center gap-1">
          <BranchIcon size={13} /> {repo.default_branch}
        </span>
        <span>Updated {new Date(repo.updated_at).toLocaleDateString()}</span>
      </div>
    </li>
  );
}

// ─── Stars tab ──────────────────────────────────────────────────────────────────

function ProfileStars({ login }: { login: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["user-starred", login],
    queryFn: () => fetchUserStarredRepos(login),
  });
  return (
    <section>
      <SectionLabel>Starred repositories</SectionLabel>
      {isLoading && <Spinner label="loading stars" />}
      {isError && <InlineError title="Failed to load stars" detail={String(error)} />}
      {data &&
        (data.length === 0 ? (
          <Blankslate icon={<StarIcon size={28} />} title="No starred repositories">
            This user hasn’t starred any repositories yet.
          </Blankslate>
        ) : (
          <ul style={{ borderTop: "1px solid var(--color-border)" }}>
            {data.map((repo) => (
              <ProfileRepoRow key={repo.id} repo={repo} />
            ))}
          </ul>
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
