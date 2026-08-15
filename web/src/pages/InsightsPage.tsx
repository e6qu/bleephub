import { useParams, Link } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchRepoForksPage,
  fetchDependencySBOM,
  fetchRepoPRsPage,
  fetchRepoIssuesPage,
  fetchRepoContributors,
  fetchCommunityProfile,
  fetchCommitActivity,
  fetchCodeFrequency,
  fetchRepoCommits,
  fetchRepoBranches,
  fetchTrafficViews,
  fetchTrafficClones,
  fetchTrafficPopularPaths,
  fetchTrafficPopularReferrers,
} from "../api.js";
import type { GithubCommunityProfile, GithubTrafficBucket, GithubCommit } from "../types.js";
import { RepoHeader } from "../components/PageHeader.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
import { Box, Blankslate, SectionLabel, StatCard } from "../components/ui.js";
import { GraphIcon, PeopleIcon, CheckCircleIcon, XCircleIcon } from "../components/octicons.js";

export function InsightsPage() {
  const { owner = "", repo = "" } = useParams<{ owner: string; repo: string }>();
  const counts = useOpenCounts(owner, repo);

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="insights" {...counts} />
      <div className="flex flex-col gap-6">
        <PulseSection owner={owner} repo={repo} />
        <CommunityProfileSection owner={owner} repo={repo} />
        <ContributorsSection owner={owner} repo={repo} />
        <CommitActivitySection owner={owner} repo={repo} />
        <CodeFrequencySection owner={owner} repo={repo} />
        <TrafficSection owner={owner} repo={repo} />
        <PopularContentSection owner={owner} repo={repo} />
        <DependencyGraphSection owner={owner} repo={repo} />
        <NetworkSection owner={owner} repo={repo} />
        <ForksSection owner={owner} repo={repo} />
      </div>
    </div>
  );
}

const COMMUNITY_CHECKS: { key: keyof GithubCommunityProfile["files"]; label: string }[] = [
  { key: "readme", label: "README" },
  { key: "license", label: "License" },
  { key: "contributing", label: "Contributing guidelines" },
  { key: "code_of_conduct_file", label: "Code of conduct" },
  { key: "issue_template", label: "Issue template" },
  { key: "pull_request_template", label: "Pull request template" },
];

function CommunityProfileSection({ owner, repo }: { owner: string; repo: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["community-profile", owner, repo],
    queryFn: () => fetchCommunityProfile(owner, repo),
  });

  return (
    <section>
      <SectionLabel>Community profile</SectionLabel>
      {isLoading && <Spinner label="loading community profile" />}
      {isError && <InlineError title="Failed to load community profile" detail={String(error)} />}
      {data && (
        <div className="grid gap-3 sm:grid-cols-[12rem_1fr]">
          <StatCard title="Health score" value={`${data.health_percentage}%`} emphasized />
          <Box>
            <ul className="grid gap-x-6 sm:grid-cols-2" style={{ listStyle: "none", margin: 0, padding: "0.75rem 1rem" }}>
              <li className="flex items-center gap-2 py-1" style={{ fontSize: "0.85rem" }}>
                {data.description ? (
                  <CheckCircleIcon size={15} style={{ color: "var(--gh-open)" }} />
                ) : (
                  <XCircleIcon size={15} style={{ color: "var(--color-fg-subtle)" }} />
                )}
                Description
              </li>
              {COMMUNITY_CHECKS.map((check) => (
                <li key={check.key} className="flex items-center gap-2 py-1" style={{ fontSize: "0.85rem" }}>
                  {data.files[check.key] ? (
                    <CheckCircleIcon size={15} style={{ color: "var(--gh-open)" }} />
                  ) : (
                    <XCircleIcon size={15} style={{ color: "var(--color-fg-subtle)" }} />
                  )}
                  {check.label}
                </li>
              ))}
            </ul>
          </Box>
        </div>
      )}
    </section>
  );
}

function ContributorsSection({ owner, repo }: { owner: string; repo: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["contributors", owner, repo],
    queryFn: ({ signal }) => fetchRepoContributors(owner, repo, signal),
  });

  return (
    <section>
      <SectionLabel>Contributors</SectionLabel>
      {isLoading && <Spinner label="loading contributors" />}
      {isError && <InlineError title="Failed to load contributors" detail={String(error)} />}
      {data &&
        (data.length === 0 ? (
          <Blankslate icon={<PeopleIcon size={26} />} title="No contributors yet">
            Contributors appear once the default branch has commits.
          </Blankslate>
        ) : (
          <Box>
            {data.map((c, i) => (
              <div
                key={c.login ?? `${c.name}<${c.email}>`}
                className="flex items-center justify-between gap-3"
                style={{
                  padding: "0.6rem 1rem",
                  borderBottom: i < data.length - 1 ? "1px solid var(--color-border)" : "none",
                }}
              >
                <span style={{ fontWeight: 500, fontSize: "0.9rem" }}>
                  {c.login ? `@${c.login}` : `${c.name} <${c.email}>`}
                  {c.type === "Anonymous" && (
                    <span style={{ marginLeft: "0.5rem", color: "var(--color-fg-muted)", fontWeight: 400, fontSize: "0.78rem" }}>
                      anonymous
                    </span>
                  )}
                </span>
                <span className="tabular-nums" style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
                  {c.contributions} commit{c.contributions === 1 ? "" : "s"}
                </span>
              </div>
            ))}
          </Box>
        ))}
    </section>
  );
}

function CommitActivitySection({ owner, repo }: { owner: string; repo: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["commit-activity", owner, repo],
    queryFn: () => fetchCommitActivity(owner, repo),
  });

  const total = data?.reduce((sum, w) => sum + w.total, 0) ?? 0;
  const max = data?.reduce((m, w) => Math.max(m, w.total), 0) ?? 0;

  return (
    <section>
      <SectionLabel>Commit activity (last 52 weeks)</SectionLabel>
      {isLoading && <Spinner label="loading commit activity" />}
      {isError && <InlineError title="Failed to load commit activity" detail={String(error)} />}
      {data &&
        (total === 0 ? (
          <Blankslate icon={<GraphIcon size={26} />} title="No commits in the last year" />
        ) : (
          <Box>
            <div style={{ padding: "1rem" }}>
              <div style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", marginBottom: "0.6rem" }}>
                {total} commit{total === 1 ? "" : "s"} on the default branch
              </div>
              <div className="flex items-end gap-px" style={{ height: "4rem" }} role="img" aria-label={`Weekly commit counts, most recent week last; peak week has ${max} commits`}>
                {data.map((week) => (
                  <div
                    key={week.week}
                    title={`Week of ${new Date(week.week * 1000).toLocaleDateString()}: ${week.total} commit${week.total === 1 ? "" : "s"}`}
                    style={{
                      flex: 1,
                      minWidth: "2px",
                      height: `${max > 0 ? Math.max((week.total / max) * 100, week.total > 0 ? 4 : 0) : 0}%`,
                      background: week.total > 0 ? "var(--color-accent)" : "transparent",
                      borderRadius: "1px 1px 0 0",
                    }}
                  />
                ))}
              </div>
            </div>
          </Box>
        ))}
    </section>
  );
}

function CodeFrequencySection({ owner, repo }: { owner: string; repo: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["code-frequency", owner, repo],
    queryFn: () => fetchCodeFrequency(owner, repo),
  });

  const additions = data?.reduce((s, w) => s + Math.max(w[1], 0), 0) ?? 0;
  const deletions = data?.reduce((s, w) => s + Math.abs(Math.min(w[2], 0)), 0) ?? 0;
  const peak = data?.reduce((m, w) => Math.max(m, Math.max(w[1], 0), Math.abs(w[2])), 0) ?? 0;

  return (
    <section>
      <SectionLabel>Code frequency (last 52 weeks)</SectionLabel>
      {isLoading && <Spinner label="loading code frequency" />}
      {isError && <InlineError title="Failed to load code frequency" detail={String(error)} />}
      {data &&
        (additions === 0 && deletions === 0 ? (
          <Blankslate icon={<GraphIcon size={26} />} title="No code changes in the last year" />
        ) : (
          <Box>
            <div style={{ padding: "1rem" }}>
              <div style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", marginBottom: "0.6rem" }}>
                <span style={{ color: "var(--color-success-text)" }}>+{additions.toLocaleString()}</span>{" "}
                additions and{" "}
                <span style={{ color: "var(--color-danger-text)" }}>−{deletions.toLocaleString()}</span>{" "}
                deletions across {data.length} week{data.length === 1 ? "" : "s"}
              </div>
              <div
                className="flex items-center gap-px"
                style={{ height: "5rem" }}
                role="img"
                aria-label={`Weekly additions and deletions, most recent week last; +${additions} additions and −${deletions} deletions in total`}
              >
                {data.map((week) => (
                  <div key={week[0]} className="flex flex-1 flex-col justify-center" style={{ minWidth: "2px", height: "100%" }}>
                    <div style={{ flex: 1, display: "flex", alignItems: "flex-end" }}>
                      <div
                        title={`Week of ${new Date(week[0] * 1000).toLocaleDateString()}: +${week[1]} / −${Math.abs(week[2])}`}
                        style={{
                          width: "100%",
                          height: `${peak > 0 ? Math.max((Math.max(week[1], 0) / peak) * 100, week[1] > 0 ? 4 : 0) : 0}%`,
                          background: week[1] > 0 ? "var(--gh-open-solid)" : "transparent",
                          borderRadius: "1px 1px 0 0",
                        }}
                      />
                    </div>
                    <div style={{ flex: 1, display: "flex", alignItems: "flex-start" }}>
                      <div
                        style={{
                          width: "100%",
                          height: `${peak > 0 ? Math.max((Math.abs(Math.min(week[2], 0)) / peak) * 100, week[2] < 0 ? 4 : 0) : 0}%`,
                          background: week[2] < 0 ? "var(--gh-closed)" : "transparent",
                          borderRadius: "0 0 1px 1px",
                        }}
                      />
                    </div>
                  </div>
                ))}
              </div>
            </div>
          </Box>
        ))}
    </section>
  );
}

function TrafficBucketList({ buckets, noun }: { buckets: GithubTrafficBucket[]; noun: string }) {
  if (buckets.length === 0) {
    return (
      <div style={{ padding: "0.75rem 1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
        No {noun} in the last 14 days.
      </div>
    );
  }
  return (
    <div>
      {buckets.map((b, i) => (
        <div
          key={b.timestamp}
          className="flex items-center justify-between gap-3"
          style={{
            padding: "0.5rem 1rem",
            fontSize: "0.85rem",
            borderBottom: i < buckets.length - 1 ? "1px solid var(--color-border)" : "none",
          }}
        >
          <span>{new Date(b.timestamp).toLocaleDateString()}</span>
          <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
            {b.count} ({b.uniques} unique)
          </span>
        </div>
      ))}
    </div>
  );
}

function TrafficSection({ owner, repo }: { owner: string; repo: string }) {
  const views = useQuery({
    queryKey: ["traffic-views", owner, repo],
    queryFn: () => fetchTrafficViews(owner, repo),
  });
  const clones = useQuery({
    queryKey: ["traffic-clones", owner, repo],
    queryFn: () => fetchTrafficClones(owner, repo),
  });

  return (
    <section>
      <SectionLabel>Traffic (last 14 days)</SectionLabel>
      {(views.isLoading || clones.isLoading) && <Spinner label="loading traffic" />}
      {views.isError && <InlineError title="Failed to load view traffic" detail={String(views.error)} />}
      {clones.isError && <InlineError title="Failed to load clone traffic" detail={String(clones.error)} />}
      {views.data && clones.data && (
        <>
          <div className="mb-3 grid gap-3 sm:grid-cols-4">
            <StatCard title="Views" value={views.data.count} />
            <StatCard title="Unique visitors" value={views.data.uniques} />
            <StatCard title="Clones" value={clones.data.count} />
            <StatCard title="Unique cloners" value={clones.data.uniques} />
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <Box header={<span style={{ fontWeight: 600 }}>Views by day</span>}>
              <TrafficBucketList buckets={views.data.views} noun="views" />
            </Box>
            <Box header={<span style={{ fontWeight: 600 }}>Clones by day</span>}>
              <TrafficBucketList buckets={clones.data.clones} noun="clones" />
            </Box>
          </div>
        </>
      )}
    </section>
  );
}

function PopularContentSection({ owner, repo }: { owner: string; repo: string }) {
  const paths = useQuery({
    queryKey: ["traffic-paths", owner, repo],
    queryFn: () => fetchTrafficPopularPaths(owner, repo),
  });
  const referrers = useQuery({
    queryKey: ["traffic-referrers", owner, repo],
    queryFn: () => fetchTrafficPopularReferrers(owner, repo),
  });

  return (
    <section>
      <SectionLabel>Popular content</SectionLabel>
      {(paths.isLoading || referrers.isLoading) && <Spinner label="loading popular content" />}
      {paths.isError && <InlineError title="Failed to load popular paths" detail={String(paths.error)} />}
      {referrers.isError && (
        <InlineError title="Failed to load referrers" detail={String(referrers.error)} />
      )}
      {paths.data && referrers.data && (
        <div className="grid gap-3 sm:grid-cols-2">
          <Box header={<span style={{ fontWeight: 600 }}>Popular paths</span>}>
            {paths.data.length === 0 ? (
              <div style={{ padding: "0.75rem 1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
                No path traffic recorded.
              </div>
            ) : (
              paths.data.map((p, i) => (
                <div
                  key={p.path}
                  className="flex items-center justify-between gap-3"
                  style={{
                    padding: "0.5rem 1rem",
                    fontSize: "0.85rem",
                    borderBottom: i < paths.data.length - 1 ? "1px solid var(--color-border)" : "none",
                  }}
                >
                  <span className="min-w-0 truncate">{p.path}</span>
                  <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
                    {p.count} ({p.uniques} unique)
                  </span>
                </div>
              ))
            )}
          </Box>
          <Box header={<span style={{ fontWeight: 600 }}>Referring sites</span>}>
            {referrers.data.length === 0 ? (
              <div style={{ padding: "0.75rem 1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
                No referrer traffic recorded.
              </div>
            ) : (
              referrers.data.map((r, i) => (
                <div
                  key={r.referrer}
                  className="flex items-center justify-between gap-3"
                  style={{
                    padding: "0.5rem 1rem",
                    fontSize: "0.85rem",
                    borderBottom: i < referrers.data.length - 1 ? "1px solid var(--color-border)" : "none",
                  }}
                >
                  <span className="min-w-0 truncate">{r.referrer}</span>
                  <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
                    {r.count} ({r.uniques} unique)
                  </span>
                </div>
              ))
            )}
          </Box>
        </div>
      )}
    </section>
  );
}

function ForksSection({ owner, repo }: { owner: string; repo: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["repo-forks", owner, repo],
    queryFn: () => fetchRepoForksPage(owner, repo),
  });
  const forks = data?.items ?? [];
  return (
    <section>
      <SectionLabel>Forks</SectionLabel>
      {isLoading && <Spinner label="loading forks" />}
      {isError && <InlineError title="Failed to load forks" detail={String(error)} />}
      {data &&
        (forks.length === 0 ? (
          <Blankslate icon={<GraphIcon size={26} />} title="No forks yet">
            When people fork this repository, they show up here.
          </Blankslate>
        ) : (
          <Box>
            <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
              {forks.map((fork, i) => (
                <li
                  key={fork.id}
                  style={{ borderBottom: i < forks.length - 1 ? "1px solid var(--color-border)" : "none" }}
                >
                  <Link
                    to={`/ui/repos/${fork.full_name}`}
                    style={{ display: "inline-block", padding: "0.6rem 1rem", color: "var(--color-accent)", fontSize: "0.9rem", lineHeight: "1.625rem", textDecoration: "none" }}
                  >
                    {fork.full_name}
                  </Link>
                </li>
              ))}
            </ul>
          </Box>
        ))}
    </section>
  );
}

function DependencyGraphSection({ owner, repo }: { owner: string; repo: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["dependency-sbom", owner, repo],
    queryFn: () => fetchDependencySBOM(owner, repo),
  });
  const deps = data ?? [];
  return (
    <section>
      <SectionLabel>Dependency graph</SectionLabel>
      {isLoading && <Spinner label="loading dependencies" />}
      {isError && <InlineError title="Failed to load dependencies" detail={String(error)} />}
      {data &&
        (deps.length === 0 ? (
          <Blankslate icon={<GraphIcon size={26} />} title="No dependencies detected">
            Dependencies parsed from this repository's manifests appear here.
          </Blankslate>
        ) : (
          <Box>
            <div style={{ padding: "0.6rem 1rem", fontSize: "0.8rem", color: "var(--color-fg-muted)", borderBottom: "1px solid var(--color-border)" }}>
              {deps.length} dependenc{deps.length === 1 ? "y" : "ies"} from the default branch
            </div>
            <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
              {deps.map((pkg, i) => (
                <li
                  key={pkg.SPDXID}
                  className="flex items-center justify-between gap-3"
                  style={{ padding: "0.5rem 1rem", borderBottom: i < deps.length - 1 ? "1px solid var(--color-border)" : "none", fontSize: "0.85rem" }}
                >
                  <span className="font-mono truncate">{pkg.name}</span>
                  {pkg.versionInfo && (
                    <span className="tabular-nums" style={{ color: "var(--color-fg-muted)", fontSize: "0.8rem" }}>{pkg.versionInfo}</span>
                  )}
                </li>
              ))}
            </ul>
          </Box>
        ))}
    </section>
  );
}

// Lane palette — every entry is a token defined in both the light and dark
// theme blocks, so the graph never leaks a hardcoded colour.
const NETWORK_LANE_COLORS = [
  "var(--color-accent)",
  "var(--gh-merged)",
  "var(--color-success-text)",
  "var(--color-brand-purple)",
  "var(--gh-open-solid)",
  "var(--color-brand-pink)",
  "var(--color-danger-text)",
  "var(--color-brand-blue)",
];
const laneColor = (lane: number) => NETWORK_LANE_COLORS[lane % NETWORK_LANE_COLORS.length];

interface NetworkNode {
  sha: string;
  lane: number;
  x: number;
  y: number;
  message: string;
  author: string;
  date: string;
}
interface NetworkEdge {
  x1: number;
  y1: number;
  x2: number;
  y2: number;
  lane: number;
}

const NET_DX = 16;
const NET_DY = 20;
const NET_PAD = 12;
const NET_R = 4;

// computeCommitGraph lays out the commit ancestry DAG as a horizontal railroad
// (time flows left→old to right→new). Merge commits and branch points spawn and
// retire lanes exactly as git's own graph does.
function computeCommitGraph(commits: GithubCommit[]): {
  nodes: NetworkNode[];
  edges: NetworkEdge[];
  laneCount: number;
  width: number;
  height: number;
} {
  const n = commits.length;
  const indexBySha = new Map<string, number>();
  commits.forEach((c, i) => indexBySha.set(c.sha, i));

  const active: (string | null)[] = []; // active[lane] = sha that lane is waiting to place
  const col: Record<string, number> = {};
  const claim = (sha: string): number => {
    let lane = active.indexOf(sha);
    if (lane === -1) {
      lane = active.indexOf(null);
      if (lane === -1) {
        lane = active.length;
        active.push(null);
      }
      active[lane] = sha;
    }
    return lane;
  };

  for (let i = 0; i < n; i++) {
    const c = commits[i];
    if (!c) continue;
    const lane = claim(c.sha);
    // A branch point: two children both reserved this sha in different lanes —
    // collapse the duplicates into the chosen lane so it isn't leaked forever.
    for (let l = 0; l < active.length; l++) {
      if (l !== lane && active[l] === c.sha) active[l] = null;
    }
    col[c.sha] = lane;

    const parents = (c.parents ?? []).map((p) => p.sha);
    if (parents.length === 0) {
      active[lane] = null;
    } else {
      const first = parents[0] as string;
      const existing = active.indexOf(first);
      // If the first parent is already expected elsewhere, free this lane;
      // otherwise this lane keeps following it.
      active[lane] = existing !== -1 && existing !== lane ? null : first;
      for (let k = 1; k < parents.length; k++) {
        const p = parents[k];
        if (p === undefined) continue;
        if (active.indexOf(p) === -1) {
          let free = active.indexOf(null);
          if (free === -1) {
            free = active.length;
            active.push(null);
          }
          active[free] = p;
        }
      }
    }
  }

  const maxLane = Object.values(col).reduce((m, l) => Math.max(m, l), 0);
  const xAt = (i: number) => NET_PAD + (n - 1 - i) * NET_DX;
  const yAt = (lane: number) => NET_PAD + lane * NET_DY;

  const nodes: NetworkNode[] = commits.map((c, i) => ({
    sha: c.sha,
    lane: col[c.sha] ?? 0,
    x: xAt(i),
    y: yAt(col[c.sha] ?? 0),
    message: (c.commit?.message ?? "").split("\n")[0] ?? "",
    author: c.author?.login ?? c.commit?.author?.name ?? "unknown",
    date: c.commit?.author?.date ?? "",
  }));

  const edges: NetworkEdge[] = [];
  commits.forEach((c, i) => {
    const childLane = col[c.sha] ?? 0;
    const x1 = xAt(i);
    const y1 = yAt(childLane);
    (c.parents ?? []).forEach((p, k) => {
      const pi = indexBySha.get(p.sha);
      if (pi === undefined) return; // parent outside the fetched window
      const parentLane = col[p.sha] ?? 0;
      edges.push({
        x1,
        y1,
        x2: xAt(pi),
        y2: yAt(parentLane),
        // First-parent edge keeps the child's lane colour; a merge edge takes
        // the incoming branch's colour.
        lane: k === 0 ? childLane : parentLane,
      });
    });
  });

  return {
    nodes,
    edges,
    laneCount: maxLane + 1,
    width: NET_PAD * 2 + Math.max(n - 1, 0) * NET_DX + NET_R,
    height: NET_PAD * 2 + maxLane * NET_DY + NET_R,
  };
}

function edgePath(e: NetworkEdge): string {
  if (e.y1 === e.y2) return `M${e.x1},${e.y1} L${e.x2},${e.y2}`;
  const mx = (e.x1 + e.x2) / 2;
  return `M${e.x1},${e.y1} C${mx},${e.y1} ${mx},${e.y2} ${e.x2},${e.y2}`;
}

function NetworkSection({ owner, repo }: { owner: string; repo: string }) {
  const commitsQ = useQuery({
    queryKey: ["network-commits", owner, repo],
    queryFn: () => fetchRepoCommits(owner, repo, { perPage: 50 }),
  });
  const branchesQ = useQuery({
    queryKey: ["network-branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
  });

  const commits = commitsQ.data ?? [];
  const graph = computeCommitGraph(commits);
  const branchTips = new Map<string, string[]>();
  for (const b of branchesQ.data ?? []) {
    const list = branchTips.get(b.commit.sha) ?? [];
    list.push(b.name);
    branchTips.set(b.commit.sha, list);
  }

  return (
    <section>
      <SectionLabel>Network</SectionLabel>
      {commitsQ.isLoading && <Spinner label="loading commit network" />}
      {commitsQ.isError && (
        <InlineError title="Failed to load commit network" detail={String(commitsQ.error)} />
      )}
      {commitsQ.data &&
        (commits.length === 0 ? (
          <Blankslate icon={<GraphIcon size={26} />} title="No commits yet">
            The commit network graph appears once the default branch has history.
          </Blankslate>
        ) : (
          <Box>
            <div
              style={{
                padding: "0.6rem 1rem",
                fontSize: "0.8rem",
                color: "var(--color-fg-muted)",
                borderBottom: "1px solid var(--color-border)",
              }}
            >
              Latest {commits.length} commit{commits.length === 1 ? "" : "s"} across{" "}
              {graph.laneCount} lane{graph.laneCount === 1 ? "" : "s"}
            </div>
            <div style={{ overflowX: "auto", padding: "0.75rem 1rem" }}>
              <svg
                width={graph.width}
                height={graph.height}
                viewBox={`0 0 ${graph.width} ${graph.height}`}
                role="img"
                aria-label={`Commit network graph: ${commits.length} commits laid out across ${graph.laneCount} branch lanes, oldest on the left.`}
                style={{ display: "block" }}
              >
                {graph.edges.map((e, i) => (
                  <path
                    key={i}
                    d={edgePath(e)}
                    fill="none"
                    stroke={laneColor(e.lane)}
                    strokeWidth={2}
                    opacity={0.8}
                  />
                ))}
                {graph.nodes.map((node) => {
                  const tips = branchTips.get(node.sha);
                  return (
                    <g key={node.sha}>
                      <circle cx={node.x} cy={node.y} r={NET_R} fill={laneColor(node.lane)}>
                        <title>{`${node.sha.slice(0, 7)} · ${node.message} · ${node.author}`}</title>
                      </circle>
                      {tips && tips.length > 0 && (
                        <text
                          x={node.x + NET_R + 3}
                          y={node.y - NET_R - 2}
                          fontSize={9}
                          fill="var(--color-fg-muted)"
                        >
                          {tips.join(", ")}
                        </text>
                      )}
                    </g>
                  );
                })}
              </svg>
            </div>
            {/* Screen-reader fallback: the SVG is decorative topology, so expose
                the same commits as a linked, ordered list off-screen. */}
            <ol
              style={{
                position: "absolute",
                width: 1,
                height: 1,
                padding: 0,
                margin: -1,
                overflow: "hidden",
                clip: "rect(0 0 0 0)",
                whiteSpace: "nowrap",
                border: 0,
              }}
            >
              {graph.nodes.map((node) => (
                <li key={node.sha}>
                  <Link to={`/ui/repos/${owner}/${repo}/commits/${node.sha}`}>
                    {node.sha.slice(0, 7)} — {node.message} — {node.author}
                    {node.date ? ` — ${new Date(node.date).toLocaleDateString()}` : ""}
                  </Link>
                </li>
              ))}
            </ol>
          </Box>
        ))}
    </section>
  );
}

function PulseSection({ owner, repo }: { owner: string; repo: string }) {
  const openPRs = useQuery({ queryKey: ["pulse-pr-open", owner, repo], queryFn: () => fetchRepoPRsPage(owner, repo, "open") });
  const closedPRs = useQuery({ queryKey: ["pulse-pr-closed", owner, repo], queryFn: () => fetchRepoPRsPage(owner, repo, "closed") });
  const openIssues = useQuery({ queryKey: ["pulse-iss-open", owner, repo], queryFn: () => fetchRepoIssuesPage(owner, repo, "open") });
  const closedIssues = useQuery({ queryKey: ["pulse-iss-closed", owner, repo], queryFn: () => fetchRepoIssuesPage(owner, repo, "closed") });
  const loading = openPRs.isLoading || closedPRs.isLoading || openIssues.isLoading || closedIssues.isLoading;
  // The /issues endpoint also returns PRs (GitHub quirk); exclude them for issue counts.
  const issuesOnly = (items: { pull_request?: unknown }[] = []) => items.filter((i) => !i.pull_request).length;
  const merged = (closedPRs.data?.items ?? []).filter((pr) => Boolean((pr as { merged_at?: string | null }).merged_at)).length;
  const stats = [
    { label: "Merged pull requests", value: merged },
    { label: "Open pull requests", value: openPRs.data?.items.length ?? 0 },
    { label: "Closed issues", value: issuesOnly(closedIssues.data?.items) },
    { label: "Open issues", value: issuesOnly(openIssues.data?.items) },
  ];
  return (
    <section>
      <SectionLabel>Pulse</SectionLabel>
      {loading && <Spinner label="loading activity overview" />}
      {!loading && (
        <div className="grid gap-3" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(9rem, 1fr))" }}>
          {stats.map((s) => (
            <StatCard key={s.label} title={s.label} value={String(s.value)} />
          ))}
        </div>
      )}
    </section>
  );
}
