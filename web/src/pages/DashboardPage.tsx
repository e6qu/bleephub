import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import {
  fetchCurrentUser,
  fetchUserReposPage,
  fetchDashboardIssues,
  fetchReceivedEvents,
  isForbidden,
  isRateLimited,
} from "../api.js";
import type { BleephubRepo, GithubFeedIssue, GithubUserEvent } from "../types.js";
import { Avatar } from "../components/Avatar.js";
import { Box, SectionLabel, Blankslate } from "../components/ui.js";
import {
  RepoIcon,
  IssueOpenedIcon,
  IssueClosedIcon,
  SearchIcon,
  GistIcon,
  PackageIcon,
  NotificationBellIcon,
  ServerIcon,
  GlobeIcon,
} from "../components/octicons.js";

export function DashboardPage() {
  const user = useQuery({ queryKey: ["current-user"], queryFn: ({ signal }) => fetchCurrentUser(signal) });
  const repos = useQuery({
    queryKey: ["dashboard-repos"],
    queryFn: ({ signal }) => fetchUserReposPage({ sort: "pushed" }, undefined, signal),
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 30000,
  });
  const issues = useQuery({
    queryKey: ["dashboard-issues"],
    queryFn: ({ signal }) => fetchDashboardIssues(signal),
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 30000,
  });
  const viewerLogin = user.data?.login;
  const feed = useQuery({
    queryKey: ["dashboard-feed", viewerLogin],
    queryFn: () => fetchReceivedEvents(viewerLogin as string),
    enabled: !!viewerLogin,
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 30000,
  });

  const topRepos = repos.data?.items.slice(0, 8) ?? [];

  return (
    <div className="grid gap-6 lg:grid-cols-[280px_1fr_260px]">
      {/* Left rail: top repositories + New */}
      <aside className="flex flex-col gap-3">
        <div className="flex items-center justify-between gap-2">
          {user.data ? (
            <Link
              to={`/ui/${user.data.login}`}
              className="inline-flex min-w-0 items-center gap-2"
              style={{ textDecoration: "none", color: "var(--color-fg)" }}
            >
              <Avatar login={user.data.login} src={user.data.avatar_url} size={26} />
              <span className="truncate" style={{ fontWeight: 600, fontSize: "0.9rem" }}>
                {user.data.login}
              </span>
            </Link>
          ) : (
            <SectionLabel>Top repositories</SectionLabel>
          )}
          {/* A single primary-styled anchor — NOT a <Link> wrapping a <Button>:
              nesting interactive-in-interactive left the anchor's own hit box
              obscured by the inner button, tripping WCAG 2.5.8 target-size. */}
          <Link
            to="/ui/repos"
            aria-label="New repository"
            className="inline-flex items-center justify-center gap-1.5"
            style={{
              textDecoration: "none",
              background: "var(--gh-open-solid)",
              color: "#ffffff",
              border: "1px solid color-mix(in srgb, #000 12%, var(--gh-open-solid))",
              padding: "0.2rem 0.65rem",
              minHeight: "1.75rem",
              fontSize: "0.78rem",
              fontWeight: 600,
              borderRadius: "var(--radius-md)",
              whiteSpace: "nowrap",
            }}
          >
            <RepoIcon size={14} /> New
          </Link>
        </div>
        <Box>
          {repos.isLoading && <Spinner label="loading repositories" />}
          {repos.isError && (
            <InlineError title="Failed to load repositories" detail={String(repos.error)} />
          )}
          {repos.data &&
            (topRepos.length === 0 ? (
              <div style={{ padding: "0.9rem 1rem", fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
                No repositories yet.{" "}
                <Link to="/ui/repos" style={{ color: "var(--color-accent)", textDecoration: "none" }}>
                  Create one
                </Link>
                .
              </div>
            ) : (
              topRepos.map((repo, i) => (
                <TopRepoRow key={repo.id} repo={repo} last={i === topRepos.length - 1} />
              ))
            ))}
        </Box>
      </aside>

      {/* Center: the following/news feed (github.com's home feed), then your issues */}
      <section className="flex flex-col gap-6">
        <div>
          <SectionLabel>Following</SectionLabel>
          {feed.isLoading && <Spinner label="loading activity feed" />}
          {feed.isError && (
            <InlineError title="Failed to load activity feed" detail={String(feed.error)} />
          )}
          {feed.data &&
            (feed.data.length === 0 ? (
              <Blankslate icon={<GlobeIcon size={28} />} title="Your feed is quiet">
                When you follow people and watch repositories, their activity — pushes, issues,
                and pull requests — shows up here.
              </Blankslate>
            ) : (
              <Box>
                {feed.data.slice(0, 30).map((event, i) => (
                  <FollowFeedRow key={event.id ?? i} event={event} last={i === Math.min(feed.data!.length, 30) - 1} />
                ))}
              </Box>
            ))}
        </div>

        <div>
          <SectionLabel>Your issues</SectionLabel>
          {issues.isLoading && <Spinner label="loading issues" />}
          {issues.isError && (
            <InlineError title="Failed to load issues" detail={String(issues.error)} />
          )}
          {issues.data &&
            (issues.data.length === 0 ? (
              <Blankslate icon={<IssueOpenedIcon size={28} />} title="No open issues">
                Issues you open or are assigned to across your repositories show up here.
              </Blankslate>
            ) : (
              <Box>
                {issues.data.map((issue, i) => (
                  <FeedIssueRow key={issue.id} issue={issue} last={i === issues.data.length - 1} />
                ))}
              </Box>
            ))}
        </div>
      </section>

      {/* Right panel: quick links */}
      <aside className="flex flex-col gap-3">
        <SectionLabel>Explore</SectionLabel>
        <Box>
          <QuickLink to="/ui/search" icon={<SearchIcon size={15} />} label="Search" />
          <QuickLink to="/ui/repos" icon={<RepoIcon size={15} />} label="Repositories" />
          <QuickLink to="/ui/gists" icon={<GistIcon size={15} />} label="Gists" />
          <QuickLink to="/ui/packages" icon={<PackageIcon size={15} />} label="Packages" />
          <QuickLink to="/ui/notifications" icon={<NotificationBellIcon size={15} />} label="Notifications" />
          <QuickLink to="/ui/operations" icon={<ServerIcon size={15} />} label="System status" last />
        </Box>
        <div
          className="flex items-start gap-2"
          style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)", padding: "0 0.25rem" }}
        >
          <GlobeIcon size={14} />
          <span>bleephub — a GitHub-faithful control plane for your runners.</span>
        </div>
      </aside>
    </div>
  );
}

function TopRepoRow({ repo, last }: { repo: BleephubRepo; last: boolean }) {
  const [owner, name] = repo.full_name.split("/");
  return (
    <div
      className="flex items-center gap-2"
      style={{
        padding: "0.5rem 0.85rem",
        borderBottom: last ? "none" : "1px solid var(--color-border)",
      }}
    >
      <RepoIcon size={14} style={{ color: "var(--color-fg-muted)" }} />
      <Link
        to={`/ui/repos/${owner}/${name}`}
        className="truncate"
        style={{ color: "var(--color-accent)", fontSize: "0.85rem", textDecoration: "none" }}
      >
        {repo.full_name}
      </Link>
    </div>
  );
}

function FeedIssueRow({ issue, last }: { issue: GithubFeedIssue; last: boolean }) {
  const repo = issue.repository;
  const [owner, name] = repo.full_name.split("/");
  const open = issue.state === "open";
  return (
    <div
      className="flex items-start gap-2.5"
      style={{
        padding: "0.7rem 1rem",
        borderBottom: last ? "none" : "1px solid var(--color-border)",
      }}
    >
      <span style={{ color: open ? "var(--gh-open-solid)" : "var(--gh-closed)", marginTop: "0.1rem" }}>
        {open ? <IssueOpenedIcon size={16} /> : <IssueClosedIcon size={16} />}
      </span>
      <div className="min-w-0 flex-1">
        <Link
          to={`/ui/repos/${owner}/${name}/issues/${issue.number}`}
          style={{ color: "var(--color-fg)", fontWeight: 600, fontSize: "0.9rem", textDecoration: "none" }}
        >
          {issue.title}
        </Link>
        <div style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
          <Link
            to={`/ui/repos/${owner}/${name}`}
            style={{ color: "var(--color-fg-muted)", textDecoration: "none" }}
          >
            {repo.full_name}
          </Link>{" "}
          #{issue.number} · updated {new Date(issue.updated_at).toLocaleDateString()}
        </div>
      </div>
      {issue.comments > 0 && (
        <span className="tabular-nums" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
          {issue.comments}
        </span>
      )}
    </div>
  );
}

function followEventPhrase(e: GithubUserEvent): string {
  const action = e.payload?.action;
  switch (e.type) {
    case "PushEvent": {
      const n = e.payload?.size;
      return typeof n === "number" ? `pushed ${n} commit${n === 1 ? "" : "s"} to` : "pushed to";
    }
    case "CreateEvent":
      return `created ${e.payload?.ref_type ?? "a ref"} in`;
    case "DeleteEvent":
      return `deleted ${e.payload?.ref_type ?? "a ref"} in`;
    case "IssuesEvent":
      return `${action ?? "updated"} an issue in`;
    case "IssueCommentEvent":
      return "commented on an issue in";
    case "PullRequestEvent":
      return `${action ?? "updated"} a pull request in`;
    default:
      return "was active in";
  }
}

function FollowFeedRow({ event, last }: { event: GithubUserEvent; last: boolean }) {
  const actor = event.actor?.login ?? "someone";
  const repo = event.repo?.name;
  return (
    <div
      className="flex items-start gap-2.5"
      style={{ padding: "0.7rem 1rem", borderBottom: last ? "none" : "1px solid var(--color-border)" }}
    >
      <Avatar login={actor} src={event.actor?.avatar_url} size={22} />
      <div className="min-w-0 flex-1" style={{ fontSize: "0.85rem" }}>
        <span>
          <Link to={`/ui/${actor}`} style={{ color: "var(--color-fg)", fontWeight: 600, textDecoration: "none" }}>
            {actor}
          </Link>{" "}
          <span style={{ color: "var(--color-fg-muted)" }}>{followEventPhrase(event)}</span>{" "}
          {repo && (
            <Link to={`/ui/repos/${repo}`} style={{ color: "var(--color-accent)", textDecoration: "none" }}>
              {repo}
            </Link>
          )}
        </span>
        <div style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}>
          {new Date(event.created_at).toLocaleDateString()}
        </div>
      </div>
    </div>
  );
}

function QuickLink({
  to,
  icon,
  label,
  last,
}: {
  to: string;
  icon: React.ReactNode;
  label: string;
  last?: boolean;
}) {
  return (
    <Link
      to={to}
      className="flex items-center gap-2"
      style={{
        padding: "0.55rem 0.85rem",
        borderBottom: last ? "none" : "1px solid var(--color-border)",
        color: "var(--color-fg)",
        fontSize: "0.85rem",
        textDecoration: "none",
      }}
    >
      <span style={{ color: "var(--color-fg-muted)" }}>{icon}</span>
      {label}
    </Link>
  );
}
