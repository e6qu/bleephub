/**
 * Page-bootstrap aggregation + query-cache seeding.
 *
 * /ui-data/bootstrap collapses a page's first-paint fan-out into one request.
 * Each sub-payload is byte-identical to the standalone endpoint's, so on success
 * pages seed the hooks' query keys (cache hits); on failure nothing seeds and
 * hooks fetch standalone as before.
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
 * Freshness window for seeded entries: the default staleTime is 0, so seeded keys
 * also get a per-key staleTime or a mounting observer refetches immediately.
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
   * state=ALL — seeds ["milestones",o,r,"all"]. The "open" key is seeded from
   * this list filtered to state==="open" client-side.
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
  /** Sidebar sub-payloads (labels / milestones state=ALL / assignees), same as the issue aggregate. */
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

/** A malformed payload throws (pages then fall back to standalone) rather than seeding garbage. */
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
 * Seed each [key, data] pair and register a per-key staleTime so observers treat
 * it as fresh. null/undefined data is skipped so that hook fetches standalone.
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
 * Copy a cache entry to a second key whose hook hits the same endpoint
 * (["current-user"] and ["viewer"] both GET /api/v3/user). Keeps dataUpdatedAt.
 */
export function mirrorQueryData(queryClient: QueryClient, from: QueryKey, to: QueryKey): void {
  const state = queryClient.getQueryState(from);
  if (!state || state.status !== "success" || state.data === undefined) return;
  queryClient.setQueryDefaults(to, { staleTime: SEED_STALE_TIME });
  queryClient.setQueryData(to, state.data, { updatedAt: state.dataUpdatedAt });
}

/** Register the staleTime default WITHOUT data, so a warm session's earlier fetch is reused. */
export function freshenQueryDefaults(queryClient: QueryClient, keys: QueryKey[]): void {
  for (const key of keys) queryClient.setQueryDefaults(key, { staleTime: SEED_STALE_TIME });
}

// ─── Seed-shape adapters (post-processed forms hooks actually cache) ──────

/** fetchCheckRuns caches the envelope as an EnvelopePage; seed that form. */
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
 * Prefers the cached bootstrap's open counts (issues_open_count excludes PRs),
 * falling back to standalone count fetches only when no bootstrap is cached or
 * the caller gates it (fallbackEnabled avoids racing the bootstrap).
 */
export function useSeededOpenCounts(
  owner: string,
  repo: string,
  opts: { fallbackEnabled?: boolean } = {},
): { issueCount?: number | string | undefined; prCount?: number | string | undefined } {
  // Read-only observer (enabled: false); reflects the cached repo bootstrap.
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
