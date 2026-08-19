/**
 * Page-bootstrap aggregation client + TanStack Query cache seeding.
 *
 * The `/ui-data/bootstrap` endpoints collapse the repo-home / issue-detail /
 * PR-detail / insights first-paint fan-out into one request per page. Every
 * sub-payload is produced server-side by the SAME handler the standalone
 * endpoint runs, so it is byte-identical to what the page's existing hooks
 * would fetch — on bootstrap success a page seeds those hooks' exact query
 * keys and the hooks become cache hits. On bootstrap failure nothing is
 * seeded and every hook fetches standalone exactly as before, so resilience
 * is unchanged.
 *
 * This module lives in utils/ and is imported only by lazy pages (the fetch
 * primitive is api.ts's exported ghFetch), keeping the entry bundle flat.
 */
import { useQuery, type QueryClient, type QueryKey } from "@tanstack/react-query";
import { ghFetch, fetchRepoIssuesPage, fetchRepoPRsPage, type Page } from "../api.js";
import type {
  BleephubRepo,
  GithubBranch,
  GithubCheckRun,
  GithubCombinedStatus,
  GithubComment,
  GithubCommit,
  GithubCommitActivityWeek,
  GithubContentFile,
  GithubContentItem,
  GithubContributor,
  GithubIssue,
  GithubLabel,
  GithubMilestone,
  GithubPR,
  GithubPRReview,
  GithubPRReviewComment,
  GithubRelease,
  GithubReviewRequest,
  GithubTag,
  GithubTimelineItem,
} from "../types.js";

/**
 * Freshness window for seeded cache entries. Seeding alone does not stop a
 * mounting observer from refetching (the app's default staleTime is 0), so
 * seeded keys also get a per-key staleTime default. Mutations invalidate
 * their keys explicitly throughout the app, so the window only affects
 * passive cross-page navigation — matching the existing 60s conventions
 * (current-user, branch-heads).
 */
export const SEED_STALE_TIME = 60_000;

// ─── Payload shapes (value shapes owned by the standalone endpoints) ──────

export interface RepoBootstrap {
  repo: BleephubRepo & { topics?: string[] };
  readme: GithubContentFile | null;
  root_entries: GithubContentItem[] | null;
  branches: { first_page: GithubBranch[]; total_count: number };
  tags: { first_page: GithubTag[]; total_count: number };
  languages: Record<string, number> | null;
  contributors: GithubContributor[];
  latest_release: GithubRelease | null;
  latest_commit: GithubCommit | null;
  pulls_open_count: number;
  issues_open_count: number;
  discussions_enabled: boolean;
}

export interface IssueBootstrap {
  issue: GithubIssue;
  comments: GithubComment[];
  timeline: GithubTimelineItem[];
  labels: GithubLabel[];
  /**
   * state=ALL — the list IssueSidebar's ["milestones", o, r, "all"] hook
   * reads. NewIssue's ["milestones", o, r, "open"] key is seeded from the
   * same list filtered client-side to state === "open" (identical to what
   * the standalone state=open fetch answers).
   */
  milestones: GithubMilestone[];
  assignees_available: Array<{ login: string }>;
}

export interface PullBootstrap {
  pull: GithubPR;
  timeline: GithubTimelineItem[];
  comments: GithubComment[];
  reviews: GithubPRReview[];
  review_comments: GithubPRReviewComment[];
  requested_reviewers: GithubReviewRequest | null;
  /** Raw check-runs envelope, exactly as GET /commits/{sha}/check-runs answers. */
  check_runs: { total_count: number; check_runs: GithubCheckRun[] } | null;
  combined_status: GithubCombinedStatus | null;
  files_summary: { changed_files: number; additions: number; deletions: number };
  /** Same sidebar sub-payloads the issue aggregate carries, so the PR
   * sidebar's labels / milestones (state=ALL) / assignees hooks are cache
   * hits too. */
  labels: GithubLabel[];
  milestones: GithubMilestone[];
  assignees_available: Array<{ login: string }>;
}

export interface InsightsBootstrap {
  period: string;
  merged_prs_count: number;
  opened_prs_count: number;
  closed_issues_count: number;
  new_issues_count: number;
  active_contributors: number;
  top_contributors: Array<{ login: string; commits: number }>;
  commit_activity: GithubCommitActivityWeek[];
  languages: Record<string, number> | null;
}

export interface TreeMetaLatest {
  sha: string;
  message_headline: string;
  author_login: string;
  author_date: string;
}

export interface TreeMetaEntry {
  name: string;
  path: string;
  type: "file" | "dir" | "symlink" | "submodule";
  size: number;
  /** null when no touching commit was found within the server's walk cap. */
  latest: TreeMetaLatest | null;
}

export interface TreeMeta {
  ref: string;
  path: string;
  /** Commits-list-shaped commit touching `path` (the tip itself for the root). */
  latest_commit: GithubCommit | null;
  entries: TreeMetaEntry[];
}

// ─── Fetchers ─────────────────────────────────────────────────────────────

const enc = encodeURIComponent;

/**
 * A bootstrap payload that is not the expected aggregate object must surface
 * as a bootstrap ERROR (which the pages treat as "seed nothing, fall back to
 * standalone fetches"), never seed garbage into other hooks' caches.
 */
function expectKeys<T>(body: unknown, keys: string[], what: string): T {
  if (
    body === null ||
    typeof body !== "object" ||
    Array.isArray(body) ||
    !keys.every((key) => key in (body as Record<string, unknown>))
  ) {
    throw new Error(`malformed ${what} bootstrap payload`);
  }
  return body as T;
}

export const fetchRepoBootstrap = async (
  owner: string,
  repo: string,
  signal?: AbortSignal,
): Promise<RepoBootstrap> =>
  expectKeys<RepoBootstrap>(
    await ghFetch<unknown>(`/ui-data/bootstrap/repos/${enc(owner)}/${enc(repo)}`, signal),
    ["repo", "branches", "tags", "contributors"],
    "repo",
  );

export const fetchIssueBootstrap = async (
  owner: string,
  repo: string,
  number: number,
  signal?: AbortSignal,
): Promise<IssueBootstrap> =>
  expectKeys<IssueBootstrap>(
    await ghFetch<unknown>(
      `/ui-data/bootstrap/repos/${enc(owner)}/${enc(repo)}/issues/${number}`,
      signal,
    ),
    ["issue", "timeline", "labels", "assignees_available"],
    "issue",
  );

export const fetchPullBootstrap = async (
  owner: string,
  repo: string,
  number: number,
  signal?: AbortSignal,
): Promise<PullBootstrap> =>
  expectKeys<PullBootstrap>(
    await ghFetch<unknown>(
      `/ui-data/bootstrap/repos/${enc(owner)}/${enc(repo)}/pulls/${number}`,
      signal,
    ),
    ["pull", "timeline", "reviews", "review_comments"],
    "pull",
  );

export const fetchInsightsBootstrap = async (
  owner: string,
  repo: string,
  period: string,
  signal?: AbortSignal,
): Promise<InsightsBootstrap> =>
  expectKeys<InsightsBootstrap>(
    await ghFetch<unknown>(
      `/ui-data/bootstrap/repos/${enc(owner)}/${enc(repo)}/insights?period=${enc(period)}`,
      signal,
    ),
    ["period", "merged_prs_count", "commit_activity"],
    "insights",
  );

export const fetchTreeMeta = async (
  owner: string,
  repo: string,
  ref: string,
  path: string,
  signal?: AbortSignal,
): Promise<TreeMeta> => {
  const meta = expectKeys<TreeMeta>(
    await ghFetch<unknown>(
      `/ui-data/repos/${enc(owner)}/${enc(repo)}/tree-meta?ref=${enc(ref)}&path=${enc(path)}`,
      signal,
    ),
    ["entries"],
    "tree-meta",
  );
  if (!Array.isArray(meta.entries)) throw new Error("malformed tree-meta bootstrap payload");
  return meta;
};

// ─── Cache seeding ────────────────────────────────────────────────────────

export const repoBootstrapKey = (owner: string, repo: string) =>
  ["repo-bootstrap", owner, repo] as const;

/**
 * Seed each [key, data] pair into the cache and register a per-key staleTime
 * default so the observers that read the key (including ones in shared
 * components that set no staleTime of their own) treat the seed as fresh
 * instead of refetching on mount. null/undefined data means "the aggregate
 * could not provide this sub-resource" — the entry is skipped so the owning
 * hook fetches standalone (and surfaces its own error state) as before.
 */
export function seedQueryCache(
  queryClient: QueryClient,
  entries: Array<[QueryKey, unknown]>,
): void {
  for (const [key, data] of entries) {
    if (data === undefined || data === null) continue;
    queryClient.setQueryDefaults(key, { staleTime: SEED_STALE_TIME });
    queryClient.setQueryData(key, data);
  }
}

/**
 * Copy an already-fetched cache entry to a second key whose hook calls the
 * byte-identical endpoint (["current-user"] and ["viewer"] both GET
 * /api/v3/user). The copy keeps the source's dataUpdatedAt, so it never
 * claims to be fresher than the fetch that produced it.
 */
export function mirrorQueryData(queryClient: QueryClient, from: QueryKey, to: QueryKey): void {
  const state = queryClient.getQueryState(from);
  if (!state || state.status !== "success" || state.data === undefined) return;
  queryClient.setQueryDefaults(to, { staleTime: SEED_STALE_TIME });
  queryClient.setQueryData(to, state.data, { updatedAt: state.dataUpdatedAt });
}

/**
 * Register the staleTime default WITHOUT data for keys the aggregate cannot
 * supply but that a warm session has already fetched (e.g. ["repo-viewer"]),
 * so cross-page navigation inside the window reuses the earlier response
 * instead of refetching on every mount.
 */
export function freshenQueryDefaults(queryClient: QueryClient, keys: QueryKey[]): void {
  for (const key of keys) queryClient.setQueryDefaults(key, { staleTime: SEED_STALE_TIME });
}

// ─── Seed-shape adapters (post-processed forms hooks actually cache) ──────

/**
 * fetchCheckRuns post-processes the {total_count, check_runs} envelope into
 * an EnvelopePage ({items, totalCount, nextUrl}); seed that form.
 */
export function checkRunsEnvelopeToPage(
  raw: PullBootstrap["check_runs"],
): { items: GithubCheckRun[]; totalCount: number; nextUrl: null } | null {
  if (!raw || !Array.isArray(raw.check_runs) || typeof raw.total_count !== "number") return null;
  return { items: raw.check_runs, totalCount: raw.total_count, nextUrl: null };
}

/** fetchPRRequestedReviewers rejects payloads without users/teams arrays — only seed valid ones. */
export function validReviewRequest(raw: GithubReviewRequest | null): GithubReviewRequest | null {
  return raw && Array.isArray(raw.users) && Array.isArray(raw.teams) ? raw : null;
}

/** fetchCombinedStatus rejects payloads without a statuses array — only seed valid ones. */
export function validCombinedStatus(
  raw: GithubCombinedStatus | null,
): GithubCombinedStatus | null {
  return raw && Array.isArray(raw.statuses) ? raw : null;
}

// ─── Open-count badges ────────────────────────────────────────────────────

/**
 * Drop-in for hooks/useOpenCounts that prefers the cached repo bootstrap's
 * exact open counts (issues_open_count excludes PRs) and only falls back to
 * the two standalone first-page count fetches when no bootstrap is cached —
 * or when the caller gates the fallback (the repo home passes
 * `fallbackEnabled: bootstrapQ.isError` so the fallback never races the
 * bootstrap it is a fallback for).
 */
export function useSeededOpenCounts(
  owner: string,
  repo: string,
  opts: { fallbackEnabled?: boolean } = {},
): { issueCount?: number | string | undefined; prCount?: number | string | undefined } {
  // Read-only observer: never fetches (enabled: false); it just reflects the
  // repo-home bootstrap already in the cache, reactively.
  const bootstrap = useQuery<RepoBootstrap>({
    queryKey: repoBootstrapKey(owner, repo),
    queryFn: ({ signal }) => fetchRepoBootstrap(owner, repo, signal),
    enabled: false,
    staleTime: SEED_STALE_TIME,
  });
  const haveCounts =
    bootstrap.data !== undefined &&
    typeof bootstrap.data.issues_open_count === "number" &&
    typeof bootstrap.data.pulls_open_count === "number";
  const fallbackEnabled = (opts.fallbackEnabled ?? true) && !haveCounts && !!owner && !!repo;
  const { data: issuePage } = useQuery({
    queryKey: ["issues", owner, repo, "open", "count"],
    queryFn: ({ signal }) => fetchRepoIssuesPage(owner, repo, "open", undefined, signal),
    enabled: fallbackEnabled,
    staleTime: SEED_STALE_TIME,
  });
  const { data: prPage } = useQuery({
    queryKey: ["prs", owner, repo, "open", "count"],
    queryFn: ({ signal }) => fetchRepoPRsPage(owner, repo, "open", undefined, signal),
    enabled: fallbackEnabled,
    staleTime: SEED_STALE_TIME,
  });
  if (haveCounts) {
    return {
      issueCount: bootstrap.data!.issues_open_count,
      prCount: bootstrap.data!.pulls_open_count,
    };
  }
  const badge = (page?: Page<unknown>) =>
    page ? (page.nextUrl ? `${page.items.length}+` : page.items.length) : undefined;
  return { issueCount: badge(issuePage), prCount: badge(prPage) };
}
