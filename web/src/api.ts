import type {
  BleephubWorkflow,
  BleephubWorkflowFile,
  BleephubDispatchRequest,
  BleephubRepo,
  RepoListFilters,
  BleephubMetrics,
  BleephubHealth,
  BleephubApp,
  BleephubInstallation,
  BleephubOAuthApp,
  WireGitHubApp,
  WireInstallation,
  WireOAuthApp,
  WireAppCreated,
  GithubIssue,
  GithubComment,
  GithubPR,
  GithubBranch,
  GithubBranchProtection,
  GithubActor,
  GithubTeamRef,
  GithubCommit,
  GithubComparison,
  GithubWebhook,
  GithubSecret,
  GithubEnvironment,
  GithubRelease,
  GithubReleaseAsset,
  GithubMigration,
  GithubMigrationStartPayload,
  GithubRepoSubscription,
  GithubRepoViewerState,
  GithubWorkflow,
  GithubWorkflowRun,
  GithubJob,
  GithubArtifact,
  GithubActionsCache,
  GithubActionsCacheUsage,
  GithubPendingDeployment,
  GithubCheckRun,
  GithubPublicKey,
  GithubVariable,
  GithubRunner,
  GithubContentFile,
  GithubOrgVisibility,
  GithubContentItem,
  BleephubUser,
  BleephubOrg,
  BleephubTeam,
  GithubTeamMember,
  GithubTeamMembership,
  GithubTeamRepo,
  GithubDeployKey,
  BleephubAuditEvent,
  BleephubGist,
  BleephubGistFile,
  GithubGistCommit,
  GithubNotificationThread,
  GithubThreadSubscription,
  GithubProjectClassic,
  GithubProjectColumn,
  GithubProjectCard,
  GithubSecretScanningAlert,
  GithubSecretScanningLocation,
  GithubSecretScanningResolution,
  GithubCodeScanningAlert,
  GithubCodeScanningAlertInstance,
  GithubCodeScanningAlertState,
  GithubCodeScanningAnalysis,
  GithubCodeScanningDismissedReason,
  GithubCodeScanningSARIFStatus,
  GithubCodeScanningSARIFUpload,
  GithubCodeQLDatabase,
  GithubDependabotAlert,
  GithubDependabotAlertState,
  GithubDependabotDismissedReason,
  GithubDependabotSecret,
  GithubCodespace,
  GithubCodespaceMachine,
  CodespaceCreatePayload,
  GithubPackage,
  GithubPackageVersion,
  GithubPackageFile,
  GithubDiscussion,
  GithubDiscussionCategory,
  GithubDiscussionCategoryConnection,
  GithubDiscussionConnection,
  GithubDiscussionCommentConnection,
  GithubSecurityAdvisory,
  GithubSecurityAdvisoryCreatePayload,
  GithubSecurityAdvisoryUpdatePayload,
  GithubVulnerabilityReportPayload,
  GithubRuleset,
  GithubRulesetCreatePayload,
  GithubRulesetSuite,
  GithubContributor,
  GithubTrafficViews,
  GithubTrafficClones,
  GithubTrafficPath,
  GithubTrafficReferrer,
  GithubCommitActivityWeek,
  GithubCommunityProfile,
  GithubLabel,
  GithubMilestone,
  GithubAccount,
  GithubOrgInvitation,
  GithubCustomProperty,
  GithubCustomPropertyValueType,
  GithubIssueType,
  GithubOrgRole,
  GithubOrgRoleTeam,
  GithubOrgRoleUser,
  GithubEnterpriseTeam,
  GithubEnterpriseDependabotAccess,
  GithubCopilotBilling,
  GithubCopilotSeat,
  GithubCopilotSpace,
  GithubPRReview,
  GithubPRReviewComment,
  GithubPRReviewThread,
  GithubReviewRequest,
  GithubCombinedStatus,
  GithubReaction,
  GithubReactionContent,
  GithubTimelineItem,
  GithubSearchIssueItem,
  GithubSearchCodeItem,
  GithubSearchUserItem,
  GithubSearchCommitItem,
  GithubSearchLabelItem,
  GithubSearchTopicItem,
  GithubCollaborator,
  GithubRepoInvitation,
  GithubTag,
  GithubRepoSocialCounts,
  GithubUserEmail,
  GithubSSHKey,
  GithubGPGKey,
  GithubSSHSigningKey,
  GithubBlockedUser,
  GithubUserProfile,
  GithubOrgProfile,
  GithubOrgSummary,
  GithubOrgTeam,
  GithubFeedIssue,
  GithubPRFile,
  GithubMarketplaceAccount,
  BleephubOAuthGrant,
  GithubMarketplaceListing,
  GithubMarketplaceListingSettings,
  GithubMarketplacePlan,
  GithubMarketplaceSubscription,
  Undef,
} from "./types.js";
import { encodeContentsBase64 } from "./utils/workflowDispatch.js";

// One AbortController behind every request this module makes.
//
// The client had no cancellation of any kind: TanStack Query passes an
// AbortSignal into every queryFn and the API layer discarded it, so
// queryClient.cancelQueries() — which sign-out calls — could not actually stop
// anything. In-flight polls therefore landed after the token was cleared, got
// 401, and the browser logged a console error the end-to-end suite treats as a
// failure. Aborting is the only fix available: a 401 console entry is emitted
// by the browser for the response itself and cannot be suppressed from script.
//
// This is deliberately coarse — one controller for the whole module rather than
// per-query plumbing. Per-request signals are the better shape and are tracked
// separately; this gives sign-out something real to cancel with.
let pendingRequests = new AbortController();

/** Aborts every request this module currently has in flight. */
export function abortPendingRequests(): void {
  pendingRequests.abort();
  pendingRequests = new AbortController();
}

/**
 * apiFetch is the single exit point to the network for this module. Every
 * request inherits the module-wide abort signal; a caller-supplied signal is
 * combined with it rather than replacing it, so passing a per-request deadline
 * never costs the request its sign-out cancellation.
 */
type ApiFetchInit = Omit<RequestInit, "signal" | "body"> & {
  signal?: AbortSignal | null | undefined;
  body?: BodyInit | null | undefined;
};

function apiFetch(input: RequestInfo | URL, init?: ApiFetchInit): Promise<Response> {
  const signal = init?.signal
    ? AbortSignal.any([pendingRequests.signal, init.signal])
    : pendingRequests.signal;
  return fetch(input, { ...init, signal } as RequestInit);
}

/**
 * Default deadline for a single HTTP request the query layer makes. It bounds a
 * hung connection so a poll or a page navigation cannot wait forever on a
 * socket the server will never answer. It is applied by the JSON read helpers
 * only; long-poll and download paths (job logs, blob exports) call apiFetch
 * directly and are deliberately left unbounded.
 */
const DEFAULT_REQUEST_TIMEOUT_MS = 30_000;

/**
 * Builds the signal a read helper hands to apiFetch: the caller's per-request
 * signal — the one TanStack Query passes into a queryFn and aborts on unmount,
 * key change, or a superseding refetch — combined with a default timeout.
 * apiFetch folds the module-wide sign-out signal in on top, so any of the three
 * aborts the in-flight request.
 */
function readSignal(signal?: AbortSignal): AbortSignal {
  const timeout = AbortSignal.timeout(DEFAULT_REQUEST_TIMEOUT_MS);
  return signal ? AbortSignal.any([signal, timeout]) : timeout;
}

let transientToken: string | null = null;

export function getToken(): string | null {
  return transientToken;
}

export function setToken(token: string): void {
  transientToken = token;
}

export function clearToken(): void {
  transientToken = null;
  // Purge the legacy localStorage entry written by versions that predate the
  // HttpOnly session-cookie exchange. This is the name of a browser storage
  // slot we only ever delete — not a credential value — and it is never read
  // back into JavaScript.
  localStorage.removeItem("bleephub_token");
}

export function isLoggedIn(): boolean {
  return !!getToken();
}

/** How long the cookie-session probe may take before it is treated as failed. */
export const SESSION_PROBE_TIMEOUT_MS = 5000;

/**
 * Reports whether the browser holds a valid cookie session.
 *
 * `/auth/session` answers 200 with `{authenticated: false}` for an anonymous
 * visitor, so any rejection here is a transport or server fault and must stay
 * distinguishable from "signed out" — it is thrown, never folded into `false`.
 */
export async function fetchBrowserSession(
  timeoutMs: number = SESSION_PROBE_TIMEOUT_MS,
  signal?: AbortSignal,
): Promise<boolean> {
  const timeout = AbortSignal.timeout(timeoutMs);
  const res = await apiFetch("/auth/session", {
    signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
  });
  if (!res.ok) {
    throw new ApiError(res.status, `session probe failed: ${res.status} ${res.statusText}`);
  }
  const session = (await res.json()) as { authenticated?: boolean };
  return session.authenticated === true;
}

export function authHeaders(): Record<string, string> {
  const token = getToken();
  if (!token) return {};
  return { Authorization: `Bearer ${token}` };
}

/** Exchange a pasted user token for a cookie session without persisting it. */
export async function createTokenBrowserSession(token: string): Promise<void> {
  const response = await apiFetch("/auth/token", {
    method: "POST",
    headers: { Authorization: `Bearer ${token}` },
  });
  if (!response.ok) {
    const text = await response.text();
    throw new ApiError(response.status, text || `${response.status} ${response.statusText}`);
  }
}

export const UNAUTHORIZED_EVENT = "bleephub:unauthorized";

// A 401 means the browser credential is no longer valid. Tell the application
// session state rather than navigating from a background request: the router
// can preserve the exact return location and concurrent polls collapse into
// the same idempotent signed-out transition.
//
// A 403 whose headers say the budget is spent (Retry-After or
// X-RateLimit-Remaining: 0) is a throttle, not an authorization answer. Throw
// it here — ahead of the call sites' generic throw — as an ApiError carrying
// retryAfterSeconds, so callers can tell it apart (isRateLimited) and back
// off instead of hot-looping against an exhausted window.
function handleUnauthorized(res: Response): void {
  if (res.status === 401) {
    clearToken();
    window.dispatchEvent(new CustomEvent(UNAUTHORIZED_EVENT));
    return;
  }
  const retryAfterSeconds = rateLimitRetryAfterSeconds(res);
  if (retryAfterSeconds !== undefined) {
    throw new ApiError(res.status, `${res.status} API rate limit exceeded, retry after ${retryAfterSeconds}s`, {
      retryAfterSeconds,
    });
  }
}

/** Seconds to wait when a 403 is a rate-limit refusal; undefined otherwise. */
function rateLimitRetryAfterSeconds(res: Response): number | undefined {
  if (res.status !== 403) return undefined;
  const retryAfter = Number(res.headers.get("Retry-After"));
  if (Number.isFinite(retryAfter) && retryAfter > 0) return retryAfter;
  // Remaining 0 without Retry-After still means the window is exhausted.
  if (res.headers.get("X-RateLimit-Remaining") === "0") return 60;
  return undefined;
}

/** Error carrying the HTTP status so callers can branch on 404 vs failure. */
export class ApiError extends Error {
  status: number;
  /** Present when the server throttled the request (see isRateLimited). */
  retryAfterSeconds?: number | undefined;
  constructor(status: number, message: string, options?: { retryAfterSeconds?: number }) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.retryAfterSeconds = options?.retryAfterSeconds;
  }
}

export function isNotFound(err: unknown): boolean {
  return err instanceof ApiError && err.status === 404;
}

/**
 * True when the server refused on authorization grounds. This is an answer,
 * not a fault: retrying or reporting it as a failure is wrong.
 */
export function isForbidden(err: unknown): boolean {
  return err instanceof ApiError && err.status === 403;
}

/**
 * True when the server refused because the rate-limit budget is exhausted
 * (403 with Retry-After / X-RateLimit-Remaining: 0). Unlike a plain
 * forbidden answer this is transient, but retrying before the window resets
 * only deepens the exhaustion — pollers must stop, not spin.
 */
export function isRateLimited(err: unknown): boolean {
  return err instanceof ApiError && err.status === 403 && err.retryAfterSeconds !== undefined;
}

async function fetchJSON<T>(url: string, signal?: AbortSignal): Promise<T> {
  const res = await apiFetch(url, { headers: authHeaders(), signal: readSignal(signal) });
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  return res.json() as Promise<T>;
}

function splitRepoFullName(repoFullName: string): [string, string] {
  const [owner, repo] = repoFullName.split("/", 2);
  if (!owner || !repo) throw new Error(`invalid repository full name: ${repoFullName}`);
  return [owner, repo];
}

function workflowRouteID(repoFullName: string, runID: number): string {
  const [owner, repo] = splitRepoFullName(repoFullName);
  return `${owner}~${repo}~${runID}`;
}

function parseWorkflowRouteID(id: string): { owner: string; repo: string; repoFullName: string; runID: number } {
  const parts = id.split("~");
  if (parts.length < 3) throw new Error(`invalid workflow run route id: ${id}`);
  const owner = parts[0];
  const runIDText = parts[parts.length - 1];
  const repo = parts.slice(1, -1).join("~");
  const runID = Number(runIDText);
  if (!owner || !repo || !Number.isFinite(runID)) {
    throw new Error(`invalid workflow run route id: ${id}`);
  }
  return { owner, repo, repoFullName: `${owner}/${repo}`, runID };
}

async function fetchAllUserRepos(signal?: AbortSignal): Promise<BleephubRepo[]> {
  const repos: BleephubRepo[] = [];
  let nextUrl: string | null = "/api/v3/user/repos?per_page=100";
  while (nextUrl) {
    const repoPage: Page<BleephubRepo> = await ghFetchPage<BleephubRepo>(nextUrl, signal);
    repos.push(...repoPage.items);
    nextUrl = repoPage.nextUrl;
  }
  return repos;
}

function mapWorkflowFile(repoFullName: string, workflow: GithubWorkflow): BleephubWorkflowFile {
  return {
    id: workflow.id,
    name: workflow.name,
    path: workflow.path,
    state: workflow.state,
    repoFullName,
    createdAt: workflow.created_at,
    updatedAt: workflow.updated_at,
  };
}

function mapJob(job: GithubJob): BleephubWorkflow["jobs"][string] {
  return {
    key: job.name,
    jobId: String(job.id),
    displayName: job.name,
    needs: [],
    status: job.status === "in_progress" ? "running" : job.status,
    result: job.conclusion ?? "",
    startedAt: job.started_at ?? "0001-01-01T00:00:00Z",
    completedAt: job.completed_at ?? "0001-01-01T00:00:00Z",
  };
}

function mapWorkflowRun(repoFullName: string, run: GithubWorkflowRun, jobs: GithubJob[]): BleephubWorkflow {
  return {
    id: workflowRouteID(repoFullName, run.id),
    name: run.name,
    runId: run.id,
    jobs: Object.fromEntries(jobs.map((job) => [job.name, mapJob(job)])),
    status: run.status,
    result: run.conclusion ?? "",
    createdAt: run.created_at,
    eventName: run.event,
    repoFullName,
  };
}

/**
 * Most recent workflow runs across every repository the viewer can see.
 *
 * This cannot become a single request. There is no cross-repository runs
 * route, so the repository list plus one runs page per repository is
 * irreducible over the public API; and `GET .../actions/runs` carries no
 * per-run job count (it mirrors GitHub's payload, which has none), so a job
 * count costs one request per run.
 *
 * What is avoidable is fetching jobs for runs nobody displays: this used to
 * ask for the jobs of every run on every repository's first page. Runs are
 * merged and trimmed to `limit` first, so the job requests are bounded by
 * what the caller actually renders.
 */
export async function fetchWorkflows(limit: number, signal?: AbortSignal): Promise<BleephubWorkflow[]> {
  const repos = await fetchAllUserRepos(signal);
  const perRepo = await Promise.all(
    repos.map(async (repo) => {
      const [owner, repoName] = splitRepoFullName(repo.full_name);
      const runsPage = await fetchWorkflowRunsPage(owner, repoName, {}, undefined, signal);
      return runsPage.items.map((run) => ({ repoFullName: repo.full_name, owner, repoName, run }));
    }),
  );
  const newestFirst = perRepo
    .flat()
    .sort((a, b) => b.run.created_at.localeCompare(a.run.created_at))
    .slice(0, limit);

  return Promise.all(
    newestFirst.map(async ({ repoFullName, owner, repoName, run }) => {
      const jobsPage = await fetchRunJobs(owner, repoName, run.id, signal);
      return mapWorkflowRun(repoFullName, run, jobsPage.items);
    }),
  );
}

export async function fetchWorkflowDetail(id: string, signal?: AbortSignal): Promise<BleephubWorkflow> {
  const { owner, repo, repoFullName, runID } = parseWorkflowRouteID(id);
  const [run, jobsPage] = await Promise.all([
    fetchWorkflowRun(owner, repo, runID, signal),
    fetchRunJobs(owner, repo, runID, signal),
  ]);
  return mapWorkflowRun(repoFullName, run, jobsPage.items);
}

export async function fetchWorkflowLogs(id: string, signal?: AbortSignal): Promise<Record<string, string[]>> {
  const { owner, repo } = parseWorkflowRouteID(id);
  const workflow = await fetchWorkflowDetail(id, signal);
  const logs: Record<string, string[]> = {};
  await Promise.all(
    Object.values(workflow.jobs)
      .filter((job) => job.status === "completed")
      .map(async (job) => {
        const text = await fetchJobLogs(owner, repo, Number(job.jobId), signal);
        if (text) logs[job.jobId] = text.split(/\r?\n/);
      }),
  );
  return logs;
}

export const fetchRepos = () =>
  fetchAllUserRepos();

/**
 * Wire shape of `GET /internal/metrics` (MetricsSnapshot) and
 * `GET /internal/status`. Both are site-admin only.
 */
interface WireInternalMetrics {
  workflow_submissions: number;
  job_dispatches: number;
  job_completions: Record<string, number>;
  active_workflows: number;
  uptime_seconds: number;
  goroutines: number;
  heap_alloc_mb: number;
  job_duration_p50_seconds: number;
  job_duration_p95_seconds: number;
  job_duration_p99_seconds: number;
}

interface WireInternalStatus {
  active_workflows: number;
  jobs_by_status: Record<string, number>;
  connected_runners: number;
}

/**
 * Reads the server's own counters.
 *
 * This used to be computed in the browser by walking every repository, every
 * page of its workflow runs, and every run's jobs — several hundred requests
 * per poll. The server already counts all of it.
 *
 * `jobs_by_status` and `connected_runners` live on /internal/status; the rest
 * on /internal/metrics. Two requests, not several hundred.
 */
export async function fetchMetrics(signal?: AbortSignal): Promise<BleephubMetrics> {
  const [metrics, status] = await Promise.all([
    fetchJSON<WireInternalMetrics>("/internal/metrics", signal),
    fetchJSON<WireInternalStatus>("/internal/status", signal),
  ]);
  return {
    workflow_submissions: metrics.workflow_submissions,
    job_dispatches: metrics.job_dispatches,
    job_completions: metrics.job_completions ?? {},
    jobs_by_status: status.jobs_by_status ?? {},
    active_workflows: status.active_workflows,
    connected_runners: status.connected_runners,
    uptime_seconds: metrics.uptime_seconds ?? 0,
    goroutines: metrics.goroutines ?? 0,
    heap_alloc_mb: metrics.heap_alloc_mb ?? 0,
    job_duration_p50_seconds: metrics.job_duration_p50_seconds ?? 0,
    job_duration_p95_seconds: metrics.job_duration_p95_seconds ?? 0,
    job_duration_p99_seconds: metrics.job_duration_p99_seconds ?? 0,
  };
}


export const fetchHealth = (signal?: AbortSignal) =>
  fetchJSON<BleephubHealth>("/health", signal);

export async function fetchWorkflowFiles(signal?: AbortSignal): Promise<BleephubWorkflowFile[]> {
  const repos = await fetchAllUserRepos(signal);
  const perRepo = await Promise.all(
    repos.map(async (repo) => {
      const [owner, repoName] = splitRepoFullName(repo.full_name);
      const page = await fetchActionsWorkflows(owner, repoName, signal);
      return page.items.map((workflow) => mapWorkflowFile(repo.full_name, workflow));
    }),
  );
  return perRepo.flat().sort((a, b) => b.updatedAt.localeCompare(a.updatedAt));
}


export async function fetchApps(signal?: AbortSignal): Promise<BleephubApp[]> {
  const raw = await fetchJSON<WireGitHubApp[]>("/settings/apps", signal);
  return raw.map(normalizeGitHubApp);
}

export async function fetchInstallations(signal?: AbortSignal): Promise<BleephubInstallation[]> {
  const raw = await ghFetch<{ total_count: number; installations: WireInstallation[] }>(
    "/api/v3/user/installations?per_page=100",
    signal,
  );
  return raw.installations.map(normalizeInstallation);
}
// The browser settings and GitHub REST surfaces return snake_case wire shapes.
// The user interface types are camelCase, so normalize at this boundary.
// Fields are mapped 1:1 from the server contract, with no defaults, so a
// contract break shows as undefined rather than a plausible-looking blank.
function normalizeGitHubApp(raw: WireGitHubApp): BleephubApp {
  return {
    id: raw.id,
    slug: raw.slug,
    name: raw.name,
    clientId: raw.client_id,
    description: raw.description,
    url: raw.external_url,
    callbackUrl: raw.callback_url ?? "",
    webhookUrl: raw.webhook_url ?? "",
    webhookActive: raw.webhook_active ?? false,
    webhookContentType: raw.webhook_content_type ?? "form",
    permissions: raw.permissions ?? {},
    events: raw.events ?? [],
    ownerId: raw.owner.id,
    createdAt: raw.created_at,
    updatedAt: raw.updated_at,
  };
}

function normalizeInstallation(raw: WireInstallation): BleephubInstallation {
  return {
    id: raw.id,
    appId: raw.app_id,
    appSlug: raw.app_slug,
    targetType: raw.target_type,
    targetLogin: raw.account.login,
    repositorySelection: raw.repository_selection,
    permissions: raw.permissions ?? {},
    events: raw.events ?? [],
    createdAt: raw.created_at,
    suspendedAt: raw.suspended_at,
  };
}

function normalizeOAuthApp(raw: WireOAuthApp): BleephubOAuthApp {
  return {
    clientId: raw.client_id,
    name: raw.name,
    description: raw.description,
    url: raw.url,
    callbackUrl: raw.callback_url,
    ownerId: raw.owner_id,
    createdAt: raw.created_at,
  };
}

export async function createApp(payload: {
  name: string;
  description?: string | undefined;
  url?: string | undefined;
  callback_url?: string | undefined;
  webhook_url?: string | undefined;
  webhook_active?: boolean | undefined;
  permissions?: Record<string, string> | undefined;
  events?: string[] | undefined;
}): Promise<{ clientId: string; pem: string; client_secret: string; webhook_secret: string }> {
  const origin = globalThis.location?.origin || "http://localhost";
  const redirectUrl = `${origin}/ui/apps`;
  const manifest = {
    name: payload.name,
    url: payload.url || origin,
    redirect_url: redirectUrl,
    description: payload.description || "",
    callback_urls: payload.callback_url ? [payload.callback_url] : [],
    hook_attributes: payload.webhook_url
      ? { url: payload.webhook_url, active: payload.webhook_active ?? true }
      : undefined,
    default_permissions: payload.permissions || {},
    default_events: payload.events || [],
  };
  const form = new URLSearchParams();
  form.set("manifest", JSON.stringify(manifest));

  const createRes = await apiFetch("/settings/apps/new", {
    method: "POST",
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      ...authHeaders(),
    },
    body: form,
  });
	// Follow the real App Manifest redirect. Browser Fetch deliberately hides
	// manual redirect status and Location, but exposes the final same-origin
	// response URL; GitHub returns the one-time conversion code there.
  if (!createRes.ok) {
    const text = await createRes.text();
    throw new Error(`createApp manifest ${createRes.status}: ${text || createRes.statusText}`);
  }
  const code = new URL(createRes.url, origin).searchParams.get("code");
  if (!code) {
    throw new Error("createApp manifest: missing conversion code");
  }

  const convertRes = await apiFetch(`/api/v3/app-manifests/${encodeURIComponent(code)}/conversions`, {
    method: "POST",
    headers: authHeaders(),
  });
  if (!convertRes.ok) {
    const text = await convertRes.text();
    throw new Error(`createApp conversion ${convertRes.status}: ${text || convertRes.statusText}`);
  }
  const raw = (await convertRes.json()) as WireAppCreated;
  return {
    clientId: raw.client_id,
    pem: raw.pem,
    client_secret: raw.client_secret,
    webhook_secret: raw.webhook_secret,
  };
}

export async function fetchOAuthApps(signal?: AbortSignal): Promise<BleephubOAuthApp[]> {
  const res = await apiFetch("/settings/oauth-apps", {
    headers: authHeaders(),
    signal,
  });
  if (!res.ok) {
    handleUnauthorized(res);
    throw new Error(`${res.status} ${res.statusText}`);
  }
  const raw = (await res.json()) as WireOAuthApp[];
  return raw.map(normalizeOAuthApp);
}

export async function fetchAppSettings(slug: string, signal?: AbortSignal): Promise<BleephubApp> {
  const raw = await fetchJSON<WireGitHubApp>(`/settings/apps/${encodeURIComponent(slug)}`, signal);
  return normalizeGitHubApp(raw);
}

export async function updateAppSettings(
  slug: string,
  payload: {
    name: string;
    description: string;
    url: string;
    callback_url: string;
    webhook_url: string;
    webhook_active: boolean;
    webhook_content_type: "json" | "form";
    permissions: Record<string, string>;
    events: string[];
  },
): Promise<BleephubApp> {
  const raw = await ghPatchJSON<WireGitHubApp>(
    `/settings/apps/${encodeURIComponent(slug)}`,
    payload,
  );
  return normalizeGitHubApp(raw);
}

export const rotateAppClientSecret = (slug: string) =>
  ghPostJSON<{ client_secret: string }>(
    `/settings/apps/${encodeURIComponent(slug)}/client-secret`,
    {},
  );

export const rotateAppPrivateKey = (slug: string) =>
  ghPostJSON<{ pem: string }>(
    `/settings/apps/${encodeURIComponent(slug)}/private-key`,
    {},
  );

export const deleteApp = (slug: string) =>
  ghDelete(`/settings/apps/${encodeURIComponent(slug)}`);

export async function updateOAuthApp(
  clientId: string,
  payload: { name: string; description: string; url: string; callback_url: string },
): Promise<BleephubOAuthApp> {
  const raw = await ghPatchJSON<WireOAuthApp>(
    `/settings/oauth-apps/${encodeURIComponent(clientId)}`,
    payload,
  );
  return normalizeOAuthApp(raw);
}

export const rotateOAuthAppClientSecret = (clientId: string) =>
  ghPostJSON<{ client_secret: string }>(
    `/settings/oauth-apps/${encodeURIComponent(clientId)}/client-secret`,
    {},
  );

export const deleteOAuthApp = (clientId: string) =>
  ghDelete(`/settings/oauth-apps/${encodeURIComponent(clientId)}`);

export const fetchOAuthGrants = () =>
  ghFetch<BleephubOAuthGrant[]>("/settings/connections/applications");

export const revokeOAuthGrant = (clientId: string) =>
  ghDelete(`/settings/connections/applications/${encodeURIComponent(clientId)}`);

export async function installApp(
  slug: string,
  payload: {
    target_login: string;
    repository_selection: "all" | "selected";
    repository_ids: number[];
  },
): Promise<WireInstallation> {
  const form = new URLSearchParams();
  form.set("target_login", payload.target_login);
  form.set("repository_selection", payload.repository_selection);
  for (const id of payload.repository_ids) form.append("repository_ids", String(id));
  const res = await apiFetch(`/settings/apps/${encodeURIComponent(slug)}/installations/new`, {
    method: "POST",
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      ...authHeaders(),
    },
    body: form,
  });
  if (!res.ok) {
    const text = await res.text();
    throw new ApiError(res.status, text || res.statusText);
  }
  return (await res.json()) as WireInstallation;
}

export const fetchInstallationRepositories = (installationId: number) =>
  ghFetch<{ total_count: number; repositories: BleephubRepo[] }>(
    `/api/v3/user/installations/${installationId}/repositories?per_page=100`,
  );

export const addInstallationRepository = (installationId: number, repoId: number) =>
  ghPutJSON<void>(
    `/api/v3/user/installations/${installationId}/repositories/${repoId}`,
    {},
  );

export const removeInstallationRepository = (installationId: number, repoId: number) =>
  ghDelete(`/api/v3/user/installations/${installationId}/repositories/${repoId}`);

export async function fetchInstallableRepositories(
  login: string,
  accountType: "User" | "Organization",
  signal?: AbortSignal,
): Promise<BleephubRepo[]> {
  const path =
    accountType === "Organization"
      ? `/api/v3/orgs/${encodeURIComponent(login)}/repos?per_page=100`
      : "/api/v3/user/repos?affiliation=owner&per_page=100";
  const repos = await ghFetch<BleephubRepo[]>(path, signal);
  return repos.filter((repo) => repo.owner.login === login);
}

export async function createOAuthApp(payload: {
  name: string;
  description?: string;
  url?: string;
  callback_url?: string;
}): Promise<BleephubOAuthApp & { client_secret: string }> {
  const form = new URLSearchParams();
  form.set("name", payload.name);
  if (payload.description) form.set("description", payload.description);
  if (payload.url) form.set("url", payload.url);
  if (payload.callback_url) form.set("callback_url", payload.callback_url);
  const res = await apiFetch("/settings/oauth-apps/new", {
    method: "POST",
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      ...authHeaders(),
    },
    body: form,
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`createOAuthApp ${res.status}: ${text || res.statusText}`);
  }
  const raw = (await res.json()) as WireOAuthApp & { client_secret: string };
  return {
    ...normalizeOAuthApp(raw),
    client_secret: raw.client_secret,
  };
}

export async function suspendInstallation(installationID: number, suspend: boolean): Promise<void> {
  const verb = suspend ? "suspend" : "unsuspend";
  const res = await apiFetch(`/settings/installations/${installationID}/${verb}`, {
    method: "POST",
    headers: authHeaders(),
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`${verb} ${res.status}: ${text || res.statusText}`);
  }
}

export async function deleteInstallation(installationID: number): Promise<void> {
  const res = await apiFetch(`/settings/installations/${installationID}`, {
    method: "DELETE",
    headers: authHeaders(),
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`delete ${res.status}: ${text || res.statusText}`);
  }
}

export async function dispatchWorkflow(
  repoFullName: string,
  workflowId: number | string,
  body: BleephubDispatchRequest = {},
): Promise<void> {
  const res = await apiFetch(
    `/api/v3/repos/${repoFullName}/actions/workflows/${workflowId}/dispatches`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify(body),
    },
  );
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`dispatch ${res.status}: ${text || res.statusText}`);
  }
}

async function ghFetch<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await apiFetch(path, { headers: authHeaders(), signal: readSignal(signal) });
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  return res.json() as Promise<T>;
}

/** One page of a Link-paginated list plus parsed rel links. */
export interface Page<T> {
  items: T[];
  nextUrl: string | null;
  lastPage: number | null;
}

/** Extract the rel="last" page number from a GitHub-style Link header. */
export function parseLinkLast(link: string | null): number | null {
  if (!link) return null;
  for (const part of link.split(",")) {
    const m = part.match(/<[^>]*[?&]page=(\d+)[^>]*>\s*;\s*rel="last"/);
    if (m) return parseInt(m[1]!, 10);
  }
  return null;
}

/** Extract the rel="next" target from a GitHub-style Link header. */
export function parseLinkNext(link: string | null): string | null {
  if (!link) return null;
  for (const part of link.split(",")) {
    const m = part.match(/<([^>]+)>\s*;\s*rel="next"/);
    if (m) return m[1] ?? null;
  }
  return null;
}

// The server paginates list endpoints (per_page max 100) and advertises the
// follow-up page via the Link header — honor it instead of silently showing
// only the first 50 items.
async function ghFetchPage<T>(url: string, signal?: AbortSignal): Promise<Page<T>> {
  const res = await apiFetch(url, { headers: authHeaders(), signal: readSignal(signal) });
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  // No silent cast: a Link-paginated list endpoint must answer with a JSON
  // array. A non-array body (an error object, a wrapped envelope) would
  // otherwise flow to the caller as fake "items" and render as garbage or a
  // mid-render crash — surface it as a clear error instead.
  const body = (await res.json()) as unknown;
  if (!Array.isArray(body)) {
    throw new Error("malformed response: expected a JSON array");
  }
  const items = body as T[];
  const link = res.headers.get("Link");
  return { items, nextUrl: parseLinkNext(link), lastPage: parseLinkLast(link) };
}

export const fetchRepoDetail = (owner: string, repo: string, signal?: AbortSignal) =>
  ghFetch<BleephubRepo>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`, signal);

function buildRepoListURL(
  base: string,
  filters: RepoListFilters,
  perPage: number,
  pageUrl?: string,
): string {
  if (pageUrl) return pageUrl;
  const params = new URLSearchParams({ per_page: String(perPage) });
  if (filters.type) params.set("type", filters.type);
  if (filters.visibility) params.set("visibility", filters.visibility);
  if (filters.sort) params.set("sort", filters.sort);
  if (filters.direction) params.set("direction", filters.direction);
  return `${base}?${params}`;
}

/** First page of the authenticated user's repos; follow pages via Link header. */
export const fetchUserReposPage = (
  filters: RepoListFilters = {},
  pageUrl?: string,
  signal?: AbortSignal,
): Promise<Page<BleephubRepo>> =>
  ghFetchPage<BleephubRepo>(
    buildRepoListURL("/api/v3/user/repos", filters, 30, pageUrl),
    signal,
  );

/** First page of an organization's repos; follow pages via Link header. */
export const fetchOrgReposPage = (
  org: string,
  filters: RepoListFilters = {},
  pageUrl?: string,
  signal?: AbortSignal,
): Promise<Page<BleephubRepo>> =>
  ghFetchPage<BleephubRepo>(
    buildRepoListURL(`/api/v3/orgs/${encodeURIComponent(org)}/repos`, filters, 30, pageUrl),
    signal,
  );

export const createRepo = (payload: {
  name: string;
  description?: string | undefined;
  private?: boolean | undefined;
  visibility?: "public" | "private" | "internal" | undefined;
  default_branch?: string | undefined;
  auto_init?: boolean | undefined;
  gitignore_template?: string | undefined;
  license_template?: string | undefined;
}): Promise<BleephubRepo> =>
  ghPostJSON("/api/v3/user/repos", payload);

export const createOrgRepo = (
  org: string,
  payload: {
    name: string;
    description?: string | undefined;
    private?: boolean | undefined;
    visibility?: "public" | "private" | "internal" | undefined;
    default_branch?: string | undefined;
    auto_init?: boolean | undefined;
    gitignore_template?: string | undefined;
    license_template?: string | undefined;
  },
): Promise<BleephubRepo> => ghPostJSON(`/api/v3/orgs/${encodeURIComponent(org)}/repos`, payload);

export async function updateRepo(
  owner: string,
  repo: string,
  payload: Partial<BleephubRepo>,
): Promise<BleephubRepo> {
  const res = await apiFetch(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(payload),
  });
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    throw new ApiError(res.status, `${res.status} ${res.statusText}: ${text || res.statusText}`);
  }
  return res.json() as Promise<BleephubRepo>;
}

export const fetchRepoContents = (
  owner: string,
  repo: string,
  path = "",
  ref?: string,
): Promise<GithubContentItem[]> => {
  const qs = ref ? `?ref=${encodeURIComponent(ref)}` : "";
  return ghFetch<GithubContentItem[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/contents/${path}${qs}`);
};

export const fetchRepoFile = (
  owner: string,
  repo: string,
  path: string,
  ref?: string,
): Promise<GithubContentFile> => {
  const qs = ref ? `?ref=${encodeURIComponent(ref)}` : "";
  const encodedPath = path.split("/").map(encodeURIComponent).join("/");
  return ghFetch<GithubContentFile>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/contents/${encodedPath}${qs}`,
  );
};

/**
 * Create or update a file through the contents API. `content` is raw text; it is
 * base64-encoded here. Pass `sha` (the blob sha from the file response) to update
 * an existing file, omit it to create a new one. `branch` targets a branch.
 */
export const putFile = (
  owner: string,
  repo: string,
  path: string,
  payload: { message: string; content: string; sha?: string; branch?: string },
) => {
  const encodedPath = path.split("/").map(encodeURIComponent).join("/");
  const body: Record<string, unknown> = {
    message: payload.message,
    content: encodeContentsBase64(payload.content),
  };
  if (payload.sha) body.sha = payload.sha;
  if (payload.branch) body.branch = payload.branch;
  return ghPutJSON<{ commit: { sha: string } }>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/contents/${encodedPath}`,
    body,
  );
};

/** Delete a file through the contents API. `sha` is the file's current blob sha. */
export const deleteFile = (
  owner: string,
  repo: string,
  path: string,
  payload: { message: string; sha: string; branch?: string },
) => {
  const encodedPath = path.split("/").map(encodeURIComponent).join("/");
  const body: Record<string, unknown> = { message: payload.message, sha: payload.sha };
  if (payload.branch) body.branch = payload.branch;
  return ghSend(
    "DELETE",
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/contents/${encodedPath}`,
    body,
  );
};

/**
 * Create a git ref (branch or tag) at `sha`. `ref` is the fully-qualified name,
 * e.g. "refs/heads/feature" for a branch or "refs/tags/v1" for a lightweight tag.
 */
export const createRef = (owner: string, repo: string, ref: string, sha: string) =>
  ghPostJSON<{ ref: string; object: { sha: string } }>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/git/refs`,
    { ref, sha },
  );

export const fetchRepoReadme = (owner: string, repo: string, ref?: string): Promise<GithubContentFile> => {
  const qs = ref ? `?ref=${encodeURIComponent(ref)}` : "";
  return ghFetch<GithubContentFile>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/readme${qs}`);
};

export const fetchRepoTopics = (owner: string, repo: string): Promise<{ names: string[] }> =>
  ghFetch<{ names: string[] }>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/topics`);

export const updateRepoTopics = (owner: string, repo: string, names: string[]): Promise<{ names: string[] }> =>
  ghPutJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/topics`, { names });

async function ghPutJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await apiFetch(path, {
    method: "PUT",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    throw new ApiError(res.status, `${res.status} ${res.statusText}: ${text || res.statusText}`);
  }
  if (res.status === 204) {
    return undefined as T;
  }
  return res.json() as Promise<T>;
}

async function ghDelete(path: string): Promise<void> {
  const res = await apiFetch(path, {
    method: "DELETE",
    headers: authHeaders(),
  });
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    throw new ApiError(res.status, `${res.status} ${res.statusText}: ${text || res.statusText}`);
  }
}

async function ghDeleteJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await apiFetch(path, {
    method: "DELETE",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    throw new ApiError(res.status, `${res.status} ${res.statusText}: ${text || res.statusText}`);
  }
  if (res.status === 204) {
    return undefined as T;
  }
  return res.json() as Promise<T>;
}

export const fetchGitignoreTemplates = () => ghFetch<string[]>("/api/v3/gitignore/templates");
export const fetchLicenseTemplates = () =>
  ghFetch<{ key: string; name: string; spdx_id: string }[]>("/api/v3/licenses");

async function ghPostJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await apiFetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    throw new ApiError(res.status, `${res.status} ${res.statusText}: ${text || res.statusText}`);
  }
  return res.json() as Promise<T>;
}

export interface ClassroomOrganization {
  id: number;
  login: string;
  name: string;
  avatar_url: string;
}

export interface ClassroomRosterEntry {
  id: number;
  login: string;
  avatar_url: string;
  roster_identifier: string;
}

export interface ClassroomAutogradingTest {
  name: string;
  command: string;
  points: number;
}

export interface ClassroomAssignment {
  id: number;
  title: string;
  type: "individual" | "group";
  slug: string;
  invite_link: string;
  invitations_enabled: boolean;
  public_repo: boolean;
  accepted: number;
  submitted: number;
  passing: number;
  deadline: string | null;
  autograding_tests?: ClassroomAutogradingTest[];
  starter_code_repository?: { full_name: string };
  roster_identifier_required?: boolean;
}

export interface ClassroomAcceptedAssignment {
  id: number;
  submitted: boolean;
  passing: boolean;
  commit_count: number;
  grade: string;
  students: Array<{ id: number; login: string; avatar_url: string; html_url: string }>;
  repository: { id: number; full_name: string; html_url: string };
}

export interface ClassroomGrade {
  assignment_name: string;
  assignment_url: string;
  starter_code_url: string;
  github_username: string;
  roster_identifier: string;
  student_repository_name: string;
  student_repository_url: string;
  submission_timestamp: string;
  points_awarded: number;
  points_available: number;
  group_name?: string;
}

export interface Classroom {
  id: number;
  name: string;
  archived: boolean;
  url: string;
  organization: ClassroomOrganization;
  roster: ClassroomRosterEntry[];
  assignments: ClassroomAssignment[];
}

export interface ClassroomDashboard {
  classrooms: Classroom[];
  organizations: ClassroomOrganization[];
  can_create_organization: boolean;
}

function responseObject(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`malformed ${label}: expected an object`);
  }
  return value as Record<string, unknown>;
}

function decodeClassroomOrganization(value: unknown): ClassroomOrganization {
  const organization = responseObject(value, "Classroom organization");
  if (typeof organization.id !== "number" || typeof organization.login !== "string") {
    throw new Error('malformed Classroom organization: "id" and "login" are required');
  }
  return value as ClassroomOrganization;
}

function decodeClassroomAssignment(value: unknown): ClassroomAssignment {
  const assignment = responseObject(value, "Classroom assignment");
  if (
    typeof assignment.id !== "number"
    || typeof assignment.title !== "string"
    || (assignment.type !== "individual" && assignment.type !== "group")
    || typeof assignment.invite_link !== "string"
  ) {
    throw new Error("malformed Classroom assignment: incomplete identity");
  }
  for (const field of ["accepted", "submitted", "passing"] as const) {
    if (typeof assignment[field] !== "number") {
      throw new Error(`malformed Classroom assignment: "${field}" must be a number`);
    }
  }
  return value as ClassroomAssignment;
}

function decodeClassroomDashboard(value: unknown): ClassroomDashboard {
  const dashboard = responseObject(value, "Classroom dashboard");
  if (!Array.isArray(dashboard.classrooms) || !Array.isArray(dashboard.organizations)) {
    throw new Error('malformed Classroom dashboard: "classrooms" and "organizations" must be arrays');
  }
  dashboard.organizations.forEach(decodeClassroomOrganization);
  for (const classroom of dashboard.classrooms) {
    const item = responseObject(classroom, "Classroom");
    if (
      typeof item.id !== "number"
      || typeof item.name !== "string"
      || typeof item.archived !== "boolean"
      || !Array.isArray(item.roster)
      || !Array.isArray(item.assignments)
    ) {
      throw new Error("malformed Classroom dashboard: incomplete classroom");
    }
    decodeClassroomOrganization(item.organization);
    for (const rosterEntry of item.roster) {
      const student = responseObject(rosterEntry, "Classroom roster entry");
      if (typeof student.id !== "number" || typeof student.login !== "string") {
        throw new Error("malformed Classroom roster entry: incomplete student");
      }
    }
    item.assignments.forEach(decodeClassroomAssignment);
  }
  if (typeof dashboard.can_create_organization !== "boolean") {
    throw new Error('malformed Classroom dashboard: "can_create_organization" must be boolean');
  }
  return value as ClassroomDashboard;
}

export const fetchClassroomDashboard = () =>
  ghFetch<unknown>("/classroom-data").then(decodeClassroomDashboard);
export const createClassroom = (body: { name: string; organization: string }) =>
  ghPostJSON<Classroom>("/classroom-data/classrooms", body);
export const updateClassroom = (id: number, body: { name?: string; archived?: boolean }) =>
  ghPatchJSON<Classroom>(`/classroom-data/classrooms/${id}`, body);
export const deleteClassroom = (id: number) =>
  ghDeleteJSON<void>(`/classroom-data/classrooms/${id}`, {});
export const replaceClassroomRoster = (
  id: number,
  students: Array<{ login: string; roster_identifier: string }>,
) => ghPutJSON<Classroom>(`/classroom-data/classrooms/${id}/roster`, { students });
export const createClassroomAssignment = (
  classroomID: number,
  body: {
    title: string;
    type: "individual" | "group";
    starter_code_repository: string;
    public_repo: boolean;
    students_are_repo_admins: boolean;
    feedback_pull_requests_enabled: boolean;
    deadline?: string | undefined;
    max_teams?: number | undefined;
    max_members?: number | undefined;
    autograding_tests: ClassroomAutogradingTest[];
  },
) => ghPostJSON<ClassroomAssignment>(`/classroom-data/classrooms/${classroomID}/assignments`, body);
export const updateClassroomAssignment = (
  assignmentID: number,
  body: {
    title?: string | undefined;
    type?: "individual" | "group" | undefined;
    invitations_enabled?: boolean | undefined;
    deadline?: string | undefined;
    autograding_tests?: ClassroomAutogradingTest[] | undefined;
  },
) => ghPatchJSON<ClassroomAssignment>(`/classroom-data/assignments/${assignmentID}`, body);
export const deleteClassroomAssignment = (assignmentID: number) =>
  ghDeleteJSON<void>(`/classroom-data/assignments/${assignmentID}`, {});
export const fetchClassroomInvitation = (code: string) =>
  ghFetch<ClassroomAssignment>(`/classroom-data/invitations/${encodeURIComponent(code)}`);
export const fetchClassroomAcceptedAssignments = (assignmentID: number) =>
  ghFetch<ClassroomAcceptedAssignment[]>(
    `/api/v3/assignments/${assignmentID}/accepted_assignments?per_page=100`,
  );
export const fetchClassroomGrades = (assignmentID: number) =>
  ghFetch<ClassroomGrade[]>(`/api/v3/assignments/${assignmentID}/grades`);
export const acceptClassroomInvitation = (code: string, groupName?: string, rosterIdentifier?: string) =>
  ghPostJSON<{ id: number; repository: { full_name: string; html_url: string } }>(
    `/classroom-data/invitations/${encodeURIComponent(code)}/accept`,
    { ...(groupName ? { group_name: groupName } : {}), ...(rosterIdentifier ? { roster_identifier: rosterIdentifier } : {}) },
  );
export async function exportClassroomTransition(): Promise<Blob> {
  const response = await apiFetch("/classroom-data/export", { headers: authHeaders() });
  if (!response.ok) throw new ApiError(response.status, `${response.status} ${response.statusText}`);
  return response.blob();
}
export const importClassroomTransition = (bundle: unknown) =>
  ghPostJSON<{ classrooms: Classroom[] }>("/classroom-data/import", bundle);

export interface FineGrainedPATPermissions {
  organization?: Record<string, string>;
  repository?: Record<string, string>;
  other?: Record<string, string>;
}

export interface FineGrainedPAT {
  id: number;
  name: string;
  resource_owner: string;
  repository_selection: "all" | "subset" | "none";
  repository_ids: number[];
  permissions: FineGrainedPATPermissions;
  created_at: string;
  expires_at: string | null;
  status: "active" | "pending" | "revoked" | "expired";
}

export interface FineGrainedPATRequest {
  id: number;
  organization: string;
  owner: { login: string };
  token_name: string;
  reason: string | null;
  repository_selection: "all" | "subset" | "none";
  permissions: FineGrainedPATPermissions;
  token_expires_at: string | null;
}

export interface FineGrainedPATDashboard {
  tokens: FineGrainedPAT[];
  resource_owners: Array<{ login: string; type: "User" | "Organization" }>;
  repositories: Record<string, Array<{ id: number; name: string; private: boolean }>>;
  pending_requests: FineGrainedPATRequest[];
}

export const fetchFineGrainedPATDashboard = () =>
  ghFetch<FineGrainedPATDashboard>("/settings/personal-access-tokens");
export const createFineGrainedPAT = (body: {
  name: string;
  resource_owner: string;
  repository_selection: "all" | "subset" | "none";
  repository_ids: number[];
  permissions: FineGrainedPATPermissions;
  expires_at?: string;
  reason?: string;
}) => ghPostJSON<FineGrainedPAT & { token: string }>("/settings/personal-access-tokens", body);
export const deleteFineGrainedPAT = (id: number) =>
  ghSend("DELETE", `/settings/personal-access-tokens/${id}`);
export const reviewFineGrainedPATRequest = (org: string, id: number, action: "approve" | "deny") =>
  ghSend("POST", `/settings/organizations/${encodeURIComponent(org)}/personal-access-token-requests/${id}`, { action });

/** First page by (owner, repo, state); follow-up pages by the Link rel="next" URL. */
export const fetchRepoIssuesPage = (
  owner: string,
  repo: string,
  state = "open",
  pageUrl?: string,
  signal?: AbortSignal,
) =>
  ghFetchPage<GithubIssue>(
    pageUrl ?? `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues?state=${state}&per_page=50`,
    signal,
  );

export const fetchIssueDetail = (owner: string, repo: string, number: number, signal?: AbortSignal) =>
  ghFetch<GithubIssue>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}`, signal);

export const fetchIssueComments = (owner: string, repo: string, number: number) =>
  ghFetch<GithubComment[]>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/comments`
  );

/**
 * Post a comment on an issue or pull request. GitHub models a PR's conversation
 * on the shared issue-comments endpoint, so this serves both surfaces.
 */
export const createIssueComment = (owner: string, repo: string, number: number, body: string) =>
  ghPostJSON<GithubComment>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/comments`,
    { body },
  );

/**
 * Patch an issue's editable fields. `state` closes/reopens it; `title`/`body`
 * edit it. GitHub uses one PATCH endpoint for all three.
 */
export const updateIssue = (
  owner: string,
  repo: string,
  number: number,
  patch: { title?: string; body?: string; state?: "open" | "closed" },
) =>
  ghPatchJSON<GithubIssue>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}`,
    patch,
  );

/** Edit an issue/PR comment body (shared endpoint for both surfaces). */
export const updateIssueComment = (owner: string, repo: string, commentId: number, body: string) =>
  ghPatchJSON<GithubComment>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/comments/${commentId}`,
    { body },
  );

/** Delete an issue/PR comment. */
export const deleteIssueComment = (owner: string, repo: string, commentId: number) =>
  ghSend(
    "DELETE",
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/comments/${commentId}`,
  );

/** Users who can be assigned to issues/PRs in this repo. */
export const fetchAssignableUsers = (owner: string, repo: string) =>
  ghFetch<Array<{ login: string }>>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/assignees`,
  );

/** Add assignees to an issue or PR (PRs share the issue-assignees endpoint). */
export const addAssignees = (owner: string, repo: string, number: number, assignees: string[]) =>
  ghPostJSON<GithubIssue>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/assignees`,
    { assignees },
  );

/** Remove assignees from an issue or PR (DELETE carries the assignee list). */
export const removeAssignees = (owner: string, repo: string, number: number, assignees: string[]) =>
  ghSend(
    "DELETE",
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/assignees`,
    { assignees },
  );

/** Lock an issue/PR conversation (optionally with a GitHub lock reason). */
export const lockIssue = (
  owner: string,
  repo: string,
  number: number,
  lockReason?: "off-topic" | "too heated" | "resolved" | "spam",
) =>
  ghSend(
    "PUT",
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/lock`,
    lockReason ? { lock_reason: lockReason } : undefined,
  );

/** Unlock an issue/PR conversation. */
export const unlockIssue = (owner: string, repo: string, number: number) =>
  ghSend(
    "DELETE",
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/lock`,
  );

/** First page by (owner, repo, state); follow-up pages by the Link rel="next" URL. */
export const fetchRepoPRsPage = (
  owner: string,
  repo: string,
  state = "open",
  pageUrl?: string,
  signal?: AbortSignal,
) =>
  ghFetchPage<GithubPR>(
    pageUrl ?? `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls?state=${state}&per_page=50`,
    signal,
  );

export const fetchPRDetail = (owner: string, repo: string, number: number, signal?: AbortSignal) =>
  ghFetch<GithubPR>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}`, signal);

/** Open a pull request from `head` into `base`. */
export const createPull = (
  owner: string,
  repo: string,
  payload: { title: string; head: string; base: string; body?: string; draft?: boolean },
) =>
  ghPostJSON<GithubPR>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls`,
    payload,
  );

/** Patch a pull request's editable fields (title/body, or state to close/reopen). */
export const updatePull = (
  owner: string,
  repo: string,
  number: number,
  patch: { title?: string; body?: string; state?: "open" | "closed" },
) =>
  ghPatchJSON<GithubPR>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}`,
    patch,
  );

export const fetchRepoBranches = (owner: string, repo: string) =>
  ghFetch<GithubBranch[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branches`);

export const fetchRepoBranch = (owner: string, repo: string, branch: string) =>
  ghFetch<GithubBranch>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branches/${encodeURIComponent(branch)}`);

export const fetchBranchProtection = async (
  owner: string,
  repo: string,
  branch: string,
): Promise<GithubBranchProtection | null> => {
  try {
    return await ghFetch<GithubBranchProtection>(
      `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branches/${encodeURIComponent(branch)}/protection`,
    );
  } catch (err) {
    if (isNotFound(err)) return null;
    throw err;
  }
};

export const updateBranchProtection = (
  owner: string,
  repo: string,
  branch: string,
  payload: Partial<GithubBranchProtection>,
) => ghPutJSON<GithubBranchProtection>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branches/${encodeURIComponent(branch)}/protection`, payload);

export const deleteBranchProtection = (owner: string, repo: string, branch: string) =>
  ghDeleteJSON<void>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branches/${encodeURIComponent(branch)}/protection`, {});

export interface RepoCommitListOptions {
  sha?: string | undefined;
  path?: string | undefined;
  author?: string | undefined;
  since?: string | undefined;
  until?: string | undefined;
  page?: number | undefined;
  perPage?: number | undefined;
}

export const fetchRepoCommits = (
  owner: string,
  repo: string,
  options: string | RepoCommitListOptions = {},
) => {
  const normalized = typeof options === "string" ? { sha: options } : options;
  const query = new URLSearchParams({ per_page: String(normalized.perPage ?? 100) });
  if (normalized.sha) query.set("sha", normalized.sha);
  if (normalized.path) query.set("path", normalized.path);
  if (normalized.author) query.set("author", normalized.author);
  if (normalized.since) query.set("since", normalized.since);
  if (normalized.until) query.set("until", normalized.until);
  if (normalized.page && normalized.page > 1) query.set("page", String(normalized.page));
  return ghFetch<GithubCommit[]>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/commits?${query}`,
  );
};

export const fetchRepoCommit = (owner: string, repo: string, ref: string) =>
  ghFetch<GithubCommit>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/commits/${encodeURIComponent(ref)}`,
  );

export const fetchRepoComparison = (owner: string, repo: string, base: string, head: string) =>
  ghFetch<GithubComparison>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/compare/${encodeURIComponent(base)}...${encodeURIComponent(head)}`,
  );

export const setBranchRestrictionUsers = (
  owner: string,
  repo: string,
  branch: string,
  users: string[],
) => ghPutJSON<GithubActor[]>(
  `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branches/${encodeURIComponent(branch)}/protection/restrictions/users`,
  { users },
);

export const setBranchRestrictionTeams = (
  owner: string,
  repo: string,
  branch: string,
  teams: string[],
) => ghPutJSON<GithubTeamRef[]>(
  `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branches/${encodeURIComponent(branch)}/protection/restrictions/teams`,
  { teams },
);

export async function createIssue(
  owner: string,
  repo: string,
  payload: { title: string; body?: string },
): Promise<GithubIssue> {
  const res = await apiFetch(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues`, {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(payload),
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`createIssue ${res.status}: ${text || res.statusText}`);
  }
  return res.json();
}

export async function mergePR(
  owner: string,
  repo: string,
  number: number,
  mergeMethod = "merge",
): Promise<void> {
  const res = await apiFetch(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/merge`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify({ merge_method: mergeMethod }),
  });
  if (!res.ok) {
    handleUnauthorized(res);
    // 405 = "not mergeable" (already merged/closed) — a real failure the
    // caller must see, not a success path.
    const text = await res.text();
    throw new Error(`merge ${res.status}: ${text || res.statusText}`);
  }
}

export const fetchWebhooks = (owner: string, repo: string) =>
  ghFetch<GithubWebhook[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/hooks`);

// Secrets + environments come back in GitHub's list envelope
// ({secrets:[…], total_count}) — unwrap to the array the user interface renders.
// No `?? []`: if the server ever stops sending the array, the missing
// field should surface as an error, not a silent "none configured".
export const fetchSecrets = (owner: string, repo: string) =>
  ghFetch<{ secrets: GithubSecret[] }>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/secrets`
  ).then((r) => {
    if (!Array.isArray(r.secrets)) {
      throw new Error(`malformed response: missing "secrets" array`);
    }
    return r.secrets;
  });

export const fetchEnvironments = (owner: string, repo: string) =>
  ghFetch<{ environments: GithubEnvironment[] }>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/environments`
  ).then((r) => {
    if (!Array.isArray(r.environments)) {
      throw new Error(`malformed response: missing "environments" array`);
    }
    return r.environments;
  });

export const fetchReleases = (owner: string, repo: string) =>
  ghFetch<GithubRelease[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/releases`);

export const fetchRelease = (owner: string, repo: string, releaseId: number) =>
  ghFetch<GithubRelease>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/releases/${releaseId}`);

export interface ReleasePayload {
  tag_name: string;
  target_commitish?: string | undefined;
  name?: string | undefined;
  body?: string | undefined;
  draft?: boolean | undefined;
  prerelease?: boolean | undefined;
}

export const createRelease = (owner: string, repo: string, payload: ReleasePayload) =>
  ghPostJSON<GithubRelease>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/releases`, payload);

export async function updateRelease(
  owner: string,
  repo: string,
  releaseId: number,
  payload: Partial<ReleasePayload>,
): Promise<GithubRelease> {
  const res = await apiFetch(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/releases/${releaseId}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(payload),
  });
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    throw new ApiError(res.status, `${res.status} ${res.statusText}: ${text || res.statusText}`);
  }
  return res.json() as Promise<GithubRelease>;
}

export async function deleteRelease(owner: string, repo: string, releaseId: number): Promise<void> {
  await ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/releases/${releaseId}`);
}

export async function uploadReleaseAsset(
  owner: string,
  repo: string,
  releaseId: number,
  file: File,
  label = "",
): Promise<GithubReleaseAsset> {
  const params = new URLSearchParams({ name: file.name });
  if (label) params.set("label", label);
  const res = await apiFetch(
    `/api/uploads/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/releases/${releaseId}/assets?${params}`,
    {
      method: "POST",
      headers: { "Content-Type": file.type || "application/octet-stream", ...authHeaders() },
      body: file,
    },
  );
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    throw new ApiError(res.status, `${res.status} ${res.statusText}: ${text || res.statusText}`);
  }
  return res.json() as Promise<GithubReleaseAsset>;
}

export async function downloadReleaseAsset(owner: string, repo: string, assetId: number): Promise<Blob> {
  const res = await apiFetch(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/releases/assets/${assetId}`, {
    headers: { Accept: "application/octet-stream", ...authHeaders() },
  });
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  return res.blob();
}

export async function deleteReleaseAsset(owner: string, repo: string, assetId: number): Promise<void> {
  await ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/releases/assets/${assetId}`);
}

// ─── GitHub Actions Representational State Transfer ─────────────────────

/**
 * One page of a GitHub envelope list ({total_count, <key>: [...]}) plus
 * the Link rel="next" URL. total_count is the full filtered count, not
 * the page size — list pages use it for "N workflow runs" headers.
 */
export interface EnvelopePage<T> {
  items: T[];
  totalCount: number;
  nextUrl: string | null;
}

async function ghFetchEnvelope<T>(
  url: string,
  key: string,
  decode?: (value: unknown, index: number) => T,
  signal?: AbortSignal,
): Promise<EnvelopePage<T>> {
  const res = await apiFetch(url, { headers: authHeaders(), signal: readSignal(signal) });
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  const body = (await res.json()) as unknown;
  if (!body || typeof body !== "object" || Array.isArray(body)) {
    throw new Error("malformed response: expected a JSON object");
  }
  // No `?? []`: a missing array member is a contract break that must
  // surface as an error, not render as an empty list.
  const items = (body as Record<string, unknown>)[key];
  if (!Array.isArray(items)) {
    throw new Error(`malformed response: missing "${key}" array`);
  }
  const totalCount = (body as Record<string, unknown>).total_count;
  if (typeof totalCount !== "number" || !Number.isInteger(totalCount) || totalCount < 0) {
    throw new Error('malformed response: missing non-negative integer "total_count"');
  }
  return {
    items: decode ? items.map(decode) : items as T[],
    totalCount,
    nextUrl: parseLinkNext(res.headers.get("Link")),
  };
}

/** Non-GET request that returns no JSON the caller renders. */
async function ghSend(method: string, path: string, body?: unknown): Promise<void> {
  const res = await apiFetch(path, {
    method,
    headers: body !== undefined
      ? { "Content-Type": "application/json", ...authHeaders() }
      : authHeaders(),
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    throw new ApiError(res.status, `${method} ${res.status}: ${text || res.statusText}`);
  }
}

export const fetchActionsWorkflows = (owner: string, repo: string, signal?: AbortSignal) =>
  ghFetchEnvelope<GithubWorkflow>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/workflows?per_page=100`,
    "workflows",
    undefined,
    signal,
  );

/** Filters the runs-list endpoint supports server-side. */
export interface RunFilters {
  /** Numeric workflow-file id; scopes to .../workflows/{id}/runs. */
  workflowId?: number | undefined;
  status?: string | undefined;
  branch?: string | undefined;
  event?: string | undefined;
}

/** First page by filters; follow-up pages by the Link rel="next" URL. */
export function fetchWorkflowRunsPage(
  owner: string,
  repo: string,
  filters: RunFilters,
  pageUrl?: string,
  signal?: AbortSignal,
): Promise<EnvelopePage<GithubWorkflowRun>> {
  if (pageUrl) return ghFetchEnvelope<GithubWorkflowRun>(pageUrl, "workflow_runs", undefined, signal);
  const base = filters.workflowId
    ? `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/workflows/${filters.workflowId}/runs`
    : `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runs`;
  const params = new URLSearchParams({ per_page: "30" });
  if (filters.status) params.set("status", filters.status);
  if (filters.branch) params.set("branch", filters.branch);
  if (filters.event) params.set("event", filters.event);
  return ghFetchEnvelope<GithubWorkflowRun>(`${base}?${params}`, "workflow_runs", undefined, signal);
}

export const fetchWorkflowRun = (owner: string, repo: string, runId: number, signal?: AbortSignal) =>
  ghFetch<GithubWorkflowRun>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runs/${runId}`, signal);

/**
 * Run shape for a specific attempt. 404s on servers that don't model
 * attempts — the caller treats that as "hide the attempt selector".
 */
export const fetchWorkflowRunAttempt = (
  owner: string,
  repo: string,
  runId: number,
  attempt: number,
) =>
  ghFetch<GithubWorkflowRun>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runs/${runId}/attempts/${attempt}`,
  );

export const fetchRunJobs = (owner: string, repo: string, runId: number, signal?: AbortSignal) =>
  ghFetchEnvelope<GithubJob>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runs/${runId}/jobs?per_page=100`,
    "jobs",
    undefined,
    signal,
  );

/** Job logs are text/plain, not JSON. */
export async function fetchJobLogs(owner: string, repo: string, jobId: number, signal?: AbortSignal): Promise<string> {
  const res = await apiFetch(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/jobs/${jobId}/logs`, {
    headers: authHeaders(),
    signal,
  });
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  return res.text();
}

export const fetchJobSummary = (owner: string, repo: string, jobId: number) =>
  ghFetch<{ summary: string }>(
    `/internal/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/jobs/${jobId}/summary`,
  ).then((result) => result.summary);

export const cancelRun = (owner: string, repo: string, runId: number) =>
  ghSend("POST", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runs/${runId}/cancel`);

export const rerunRun = (owner: string, repo: string, runId: number) =>
  ghSend("POST", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runs/${runId}/rerun`);

export const rerunFailedJobs = (owner: string, repo: string, runId: number) =>
  ghSend("POST", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runs/${runId}/rerun-failed-jobs`);

export const fetchRunArtifacts = (owner: string, repo: string, runId: number) =>
  ghFetchEnvelope<GithubArtifact>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runs/${runId}/artifacts`,
    "artifacts",
  );

export const fetchRepoArtifacts = (owner: string, repo: string) =>
  ghFetchEnvelope<GithubArtifact>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/artifacts?per_page=100`,
    "artifacts",
  );

export const deleteRepoArtifact = (owner: string, repo: string, artifactId: number) =>
  ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/artifacts/${artifactId}`);

export const fetchRepoActionsCaches = (owner: string, repo: string) =>
  ghFetchEnvelope<GithubActionsCache>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/caches?per_page=100`,
    "actions_caches",
  );

export const fetchRepoActionsCacheUsage = (owner: string, repo: string) =>
  ghFetch<GithubActionsCacheUsage>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/cache/usage`,
  );

export const deleteRepoActionsCache = (owner: string, repo: string, cacheId: number) =>
  ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/caches/${cacheId}`);

export const fetchPendingDeployments = (owner: string, repo: string, runId: number) =>
  ghFetch<GithubPendingDeployment[]>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runs/${runId}/pending_deployments`,
  );

export const reviewPendingDeployments = (
  owner: string,
  repo: string,
  runId: number,
  body: { environment_ids: number[]; state: "approved" | "rejected"; comment: string },
) =>
  ghSend("POST", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runs/${runId}/pending_deployments`, body);

export const enableWorkflow = (owner: string, repo: string, workflowId: number) =>
  ghSend("PUT", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/workflows/${workflowId}/enable`);

export const disableWorkflow = (owner: string, repo: string, workflowId: number) =>
  ghSend("PUT", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/workflows/${workflowId}/disable`);

export const fetchFileContent = (owner: string, repo: string, path: string) =>
  ghFetch<GithubContentFile>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/contents/${path}`);

export const fetchCheckRuns = (owner: string, repo: string, sha: string) =>
  ghFetchEnvelope<GithubCheckRun>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/commits/${sha}/check-runs`,
    "check_runs",
  );

export const fetchActionsRunners = (owner: string, repo: string) =>
  ghFetchEnvelope<GithubRunner>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runners`, "runners");

// ─── Secrets & variables (repo / environment / org scopes) ──────────────

/**
 * The three scopes GitHub stores Actions secrets + variables under. Each
 * maps to a URL prefix that `/secrets`, `/secrets/public-key`,
 * `/secrets/{name}`, `/variables` and `/variables/{name}` append to.
 */
export type SecretsScope =
  | { kind: "repo"; owner: string; repo: string }
  | { kind: "env"; owner: string; repo: string; env: string }
  | { kind: "org"; org: string };

function scopeBase(s: SecretsScope): string {
  switch (s.kind) {
    case "repo":
      return `/api/v3/repos/${encodeURIComponent(s.owner)}/${encodeURIComponent(s.repo)}/actions`;
    case "env":
      return `/api/v3/repos/${encodeURIComponent(s.owner)}/${encodeURIComponent(s.repo)}/environments/${encodeURIComponent(s.env)}`;
    case "org":
      return `/api/v3/orgs/${encodeURIComponent(s.org)}/actions`;
  }
}

export const fetchScopedSecrets = (scope: SecretsScope) =>
  ghFetchEnvelope<GithubSecret>(`${scopeBase(scope)}/secrets?per_page=100`, "secrets");

export const fetchScopedPublicKey = (scope: SecretsScope) =>
  ghFetch<GithubPublicKey>(`${scopeBase(scope)}/secrets/public-key`);

/** Body carries the sealed-box ciphertext only — plaintext never leaves the client. */
export const putScopedSecret = (
  scope: SecretsScope,
  name: string,
  body: { encrypted_value: string; key_id: string; visibility?: GithubOrgVisibility },
) => ghSend("PUT", `${scopeBase(scope)}/secrets/${encodeURIComponent(name)}`, body);

export const deleteScopedSecret = (scope: SecretsScope, name: string) =>
  ghSend("DELETE", `${scopeBase(scope)}/secrets/${encodeURIComponent(name)}`);

export const fetchScopedVariables = (scope: SecretsScope) =>
  ghFetchEnvelope<GithubVariable>(`${scopeBase(scope)}/variables?per_page=100`, "variables");

export const createScopedVariable = (
  scope: SecretsScope,
  body: { name: string; value: string; visibility?: GithubOrgVisibility },
) => ghSend("POST", `${scopeBase(scope)}/variables`, body);

export const updateScopedVariable = (
  scope: SecretsScope,
  name: string,
  body: { name: string; value: string },
) => ghSend("PATCH", `${scopeBase(scope)}/variables/${encodeURIComponent(name)}`, body);

export const deleteScopedVariable = (scope: SecretsScope, name: string) =>
  ghSend("DELETE", `${scopeBase(scope)}/variables/${encodeURIComponent(name)}`);

// ─── Internal admin endpoints ───────────────────────────────────────────

export async function fetchUsers(signal?: AbortSignal): Promise<BleephubUser[]> {
  const users = await ghFetch<BleephubUser[]>("/api/v3/users?per_page=100", signal);
  return Promise.all(users.map((user) => ghFetch<BleephubUser>(`/api/v3/users/${encodeURIComponent(user.login)}`, signal)));
}

export async function createUser(payload: { login: string; email?: string | undefined; site_admin?: boolean | undefined }): Promise<BleephubUser> {
  const user = await ghPostJSON<BleephubUser>("/api/v3/admin/users", { login: payload.login, email: payload.email });
  if (payload.site_admin) {
    await ghSend("PUT", `/api/v3/users/${encodeURIComponent(user.login)}/site_admin`);
  }
  return ghFetch<BleephubUser>(`/api/v3/users/${encodeURIComponent(user.login)}`);
}

export async function updateUser(login: string, payload: Pick<Partial<BleephubUser>, "site_admin">): Promise<BleephubUser> {
  if (payload.site_admin === true) {
    await ghSend("PUT", `/api/v3/users/${encodeURIComponent(login)}/site_admin`);
  } else if (payload.site_admin === false) {
    await ghSend("DELETE", `/api/v3/users/${encodeURIComponent(login)}/site_admin`);
  }
  return ghFetch<BleephubUser>(`/api/v3/users/${encodeURIComponent(login)}`);
}

export const deleteUser = (login: string) =>
  ghSend("DELETE", `/api/v3/admin/users/${encodeURIComponent(login)}`);

export async function fetchOrgs(signal?: AbortSignal): Promise<BleephubOrg[]> {
  const orgs = await ghFetch<GithubOrgSummary[]>("/api/v3/organizations?per_page=100", signal);
  return Promise.all(orgs.map((org) => ghFetch<BleephubOrg>(`/api/v3/orgs/${encodeURIComponent(org.login)}`, signal)));
}

export async function createOrg(payload: {
  login: string;
  name?: string | undefined;
  description?: string | undefined;
  billing_email?: string | undefined;
}): Promise<BleephubOrg> {
  const viewer = await fetchCurrentUser();
  const org = await ghPostJSON<BleephubOrg>("/api/v3/admin/organizations", {
    login: payload.login,
    admin: viewer.login,
    profile_name: payload.name || payload.login,
  });
  if (payload.description || payload.billing_email) {
    return ghPatchJSON<BleephubOrg>(`/api/v3/orgs/${encodeURIComponent(org.login)}`, {
      description: payload.description,
      billing_email: payload.billing_email,
    });
  }
  return org;
}

export const updateOrg = (login: string, payload: Undef<BleephubOrg>) =>
  ghPatchJSON<BleephubOrg>(`/api/v3/orgs/${encodeURIComponent(login)}`, payload);

export const deleteOrg = (login: string) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(login)}`);

export const fetchTeams = () => ghFetch<BleephubTeam[]>("/api/v3/user/teams?per_page=100");

export const createTeam = (payload: { org: string; name: string; description?: string | undefined; privacy?: "secret" | "closed" | undefined }) =>
  ghPostJSON<BleephubTeam>(`/api/v3/orgs/${encodeURIComponent(payload.org)}/teams`, {
    name: payload.name,
    description: payload.description,
    privacy: payload.privacy,
  });

export const updateTeam = (org: string, slug: string, payload: Undef<BleephubTeam>) =>
  ghPatchJSON<BleephubTeam>(`/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}`, payload);

export const deleteTeam = (org: string, slug: string) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}`);

export const fetchTeamMembers = (org: string, slug: string) =>
  ghFetch<GithubTeamMember[]>(`/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}/members`);

export const addTeamMember = (org: string, slug: string, username: string, role: string) =>
  ghPutJSON<GithubTeamMembership>(`/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}/memberships/${encodeURIComponent(username)}`, { role });

export const removeTeamMember = (org: string, slug: string, username: string) =>
  ghDeleteJSON<void>(`/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}/memberships/${encodeURIComponent(username)}`, {});

export const fetchTeamRepos = (org: string, slug: string) =>
  ghFetch<GithubTeamRepo[]>(`/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}/repos`);

export const addTeamRepo = (org: string, slug: string, owner: string, repo: string, permission: string) =>
  ghPutJSON<void>(`/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`, { permission });

export const removeTeamRepo = (org: string, slug: string, owner: string, repo: string) =>
  ghDeleteJSON<void>(`/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`, {});

export const fetchChildTeams = (org: string, slug: string) =>
  ghFetch<BleephubTeam[]>(`/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}/teams`);

export const fetchRepoDeployKeys = (owner: string, repo: string) =>
  ghFetch<GithubDeployKey[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/keys`);

export const addRepoDeployKey = (owner: string, repo: string, title: string, key: string, readOnly: boolean) =>
  ghPostJSON<GithubDeployKey>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/keys`, { title, key, read_only: readOnly });

export const deleteRepoDeployKey = (owner: string, repo: string, keyId: number) =>
  ghDeleteJSON<void>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/keys/${keyId}`, {});

export async function setRepoFlag(owner: string, repo: string, flag: string, enabled: boolean): Promise<void> {
  const path = `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`;
  let body: Record<string, unknown>;
  switch (flag) {
    case "automated_security_fixes":
      body = { security_and_analysis: { automated_security_fixes: { status: enabled ? "enabled" : "disabled" } } };
      break;
    case "vulnerability_alerts":
      body = { security_and_analysis: { advanced_security: { status: enabled ? "enabled" : "disabled" } } };
      break;
    case "private_vulnerability_reporting":
      body = { security_and_analysis: { secret_scanning_non_provider_patterns: { status: enabled ? "enabled" : "disabled" } } };
      break;
    default:
      body = { [flag]: enabled };
  }
  await ghPatchJSON<void>(path, body);
}

/**
 * Reads the repo's current interaction limit. The endpoint returns `{}` (no
 * active limit) or an object whose `limit` field matches the values accepted by
 * {@link setRepoInteractionLimit}.
 */
export const fetchRepoInteractionLimit = (owner: string, repo: string) =>
  ghFetch<{ limit?: string }>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/interaction-limits`);

export const setRepoInteractionLimit = (owner: string, repo: string, limit: string | null) => {
  const path = `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/interaction-limits`;
  if (limit === null) {
    return ghDeleteJSON<void>(path, {});
  }
  return ghPutJSON<void>(path, { limit, expiry: "one_month" });
};

export const transferRepo = (owner: string, repo: string, newOwner: string) =>
  ghPostJSON<BleephubRepo>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/transfer`, { new_owner: newOwner });

export const renameBranch = (owner: string, repo: string, branch: string, newName: string) =>
  ghPostJSON<{ url: string }>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branches/${encodeURIComponent(branch)}/rename`, { new_name: newName });

interface GithubAuditEntry {
  _document_id: number | string;
  "@timestamp": string;
  action: string;
  actor: string;
  org?: string;
  data?: Record<string, unknown>;
}

export const fetchAuditLog = (filters: {
  org: string;
  phrase?: string | undefined;
  order?: "asc" | "desc" | undefined;
}): Promise<BleephubAuditEvent[]> => {
  const params = new URLSearchParams();
  params.set("include", "all");
  params.set("per_page", "100");
  if (filters.order) params.set("order", filters.order);
  if (filters.phrase) params.set("phrase", filters.phrase);
  const qs = params.toString();
  return ghFetch<GithubAuditEntry[]>(`/api/v3/orgs/${encodeURIComponent(filters.org)}/audit-log?${qs}`).then((entries) =>
    entries.map((entry) => ({
      id: Number(entry._document_id),
      actor_login: entry.actor,
      action: entry.action,
      entity_type: String(entry.data?.entity_type ?? entry.data?.target_type ?? entry.org ?? filters.org),
      entity_id: String(entry.data?.entity_id ?? entry.data?.target_id ?? entry.data?.repo ?? entry.org ?? filters.org),
      details: entry.data ?? {},
      created_at: entry["@timestamp"],
    })),
  );
};

export const fetchAuthenticatedUserOrgs = (signal?: AbortSignal) =>
  ghFetch<BleephubOrg[]>("/api/v3/user/orgs?per_page=100", signal);

// The audit log is an operator surface: a site admin viewing it typically holds
// no personal org membership, so listing only the viewer's memberships (the old
// behaviour) wrongly reported "you don't belong to an organization". List every
// organization on the instance instead, which is what an operator can audit.
export const fetchAuditLogOrgs = async (signal?: AbortSignal): Promise<BleephubOrg[]> => {
  const orgs = await ghFetch<BleephubOrg[]>("/api/v3/organizations?per_page=100", signal);
  return [...orgs].sort((a, b) => a.login.localeCompare(b.login));
};

export const buildAuditLogPhrase = (filters: {
  actor?: string | undefined;
  action?: string | undefined;
  text?: string | undefined;
}) => {
  const terms: string[] = [];
  if (filters.actor) terms.push(filters.actor);
  if (filters.action) terms.push(filters.action);
  if (filters.text) terms.push(filters.text);
  return terms.join(" ");
};

export interface NotificationFilters {
  all?: boolean;
  participating?: boolean;
  since?: string;
  before?: string;
}

export const fetchNotifications = (filters: NotificationFilters = {}, signal?: AbortSignal) => {
  const query = new URLSearchParams();
  if (filters.all) query.set("all", "true");
  if (filters.participating) query.set("participating", "true");
  if (filters.since) query.set("since", filters.since);
  if (filters.before) query.set("before", filters.before);
  const suffix = query.size ? `?${query}` : "";
  return ghFetch<GithubNotificationThread[]>(`/api/v3/notifications${suffix}`, signal);
};

export const markAllNotificationsRead = () =>
  ghSend("PUT", "/api/v3/notifications");

export const markThreadRead = (threadId: string) =>
  ghSend("PATCH", `/api/v3/notifications/threads/${threadId}`);

export const markThreadDone = (threadId: string) =>
  ghSend("DELETE", `/api/v3/notifications/threads/${threadId}`);

export async function getThreadSubscription(
  threadId: string,
): Promise<GithubThreadSubscription | null> {
  const res = await apiFetch(`/api/v3/notifications/threads/${threadId}/subscription`, {
    headers: authHeaders(),
  });
  if (res.status === 404) return null;
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  return res.json() as Promise<GithubThreadSubscription>;
}

export const setThreadSubscription = (threadId: string, subscribed: boolean) =>
  ghPutJSON<GithubThreadSubscription>(`/api/v3/notifications/threads/${threadId}/subscription`, {
    subscribed,
    ignored: false,
  });

export const deleteThreadSubscription = (threadId: string) =>
  ghSend("DELETE", `/api/v3/notifications/threads/${threadId}/subscription`);

export const fetchGists = () => ghFetch<BleephubGist[]>("/api/v3/gists");

export const fetchPublicGists = () => ghFetch<BleephubGist[]>("/api/v3/gists/public");

export const fetchStarredGists = () => ghFetch<BleephubGist[]>("/api/v3/gists/starred");

export const fetchGist = (id: string) => ghFetch<BleephubGist>(`/api/v3/gists/${id}`);

export const fetchGistCommits = (id: string) => ghFetch<GithubGistCommit[]>(`/api/v3/gists/${id}/commits`);

export const fetchGistForks = (id: string) => ghFetch<BleephubGist[]>(`/api/v3/gists/${id}/forks`);

export const forkGist = (id: string) => ghPostJSON<BleephubGist>(`/api/v3/gists/${id}/forks`, {});

export async function isGistStarred(id: string): Promise<boolean> {
  const res = await apiFetch(`/api/v3/gists/${id}/star`, { headers: authHeaders() });
  if (res.status === 204) return true;
  if (res.status === 404) return false;
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  return false;
}

export const starGist = (id: string) => ghSend("PUT", `/api/v3/gists/${id}/star`);

export const unstarGist = (id: string) => ghSend("DELETE", `/api/v3/gists/${id}/star`);

export const createGist = (payload: {
  description: string;
  public: boolean;
  files: Record<string, { content: string }>;
}) => ghPostJSON<BleephubGist>("/api/v3/gists", payload);

export const updateGist = (
  id: string,
  payload: { description?: string; files?: Record<string, BleephubGistFile | null> },
) => ghPatchJSON<BleephubGist>(`/api/v3/gists/${id}`, payload);

export const deleteGist = (id: string) => ghSend("DELETE", `/api/v3/gists/${id}`);

// ─── GitHub Projects classic (v1) ───────────────────────────────────────

export const fetchProjectsClassic = (owner: string, repo: string) =>
  ghFetch<GithubProjectClassic[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/projects`);

export const createProjectClassic = (
  owner: string,
  repo: string,
  payload: { name: string; body?: string; state?: "open" | "closed" },
) => ghPostJSON<GithubProjectClassic>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/projects`, payload);

export const updateProjectClassic = (
  projectId: number,
  payload: Partial<{ name: string; body: string; state: "open" | "closed" }>,
) => ghPatchJSON<GithubProjectClassic>(`/api/v3/projects/${projectId}`, payload);

export const deleteProjectClassic = (projectId: number) =>
  ghDeleteJSON<void>(`/api/v3/projects/${projectId}`, {});

export const fetchProjectColumns = (projectId: number) =>
  ghFetch<GithubProjectColumn[]>(`/api/v3/projects/${projectId}/columns`);

export const createProjectColumn = (projectId: number, name: string) =>
  ghPostJSON<GithubProjectColumn>(`/api/v3/projects/${projectId}/columns`, { name });

export const updateProjectColumn = (columnId: number, name: string) =>
  ghPatchJSON<GithubProjectColumn>(`/api/v3/projects/columns/${columnId}`, { name });

export const deleteProjectColumn = (columnId: number) =>
  ghDeleteJSON<void>(`/api/v3/projects/columns/${columnId}`, {});

export const moveProjectColumn = (columnId: number, position: string) =>
  ghPostJSON<{ id: number; url: string }>(`/api/v3/projects/columns/${columnId}/moves`, { position });

export const fetchProjectCards = (columnId: number) =>
  ghFetch<GithubProjectCard[]>(`/api/v3/projects/columns/${columnId}/cards`);

export const createProjectCard = (
  columnId: number,
  payload: { note?: string; content_id?: number; content_type?: "Issue" },
) => ghPostJSON<GithubProjectCard>(`/api/v3/projects/columns/${columnId}/cards`, payload);

export const updateProjectCard = (cardId: number, note: string) =>
  ghPatchJSON<GithubProjectCard>(`/api/v3/projects/columns/cards/${cardId}`, { note });

export const deleteProjectCard = (cardId: number) =>
  ghDeleteJSON<void>(`/api/v3/projects/columns/cards/${cardId}`, {});

export const moveProjectCard = (
  cardId: number,
  payload: { position: string; column_id?: number },
) => ghPostJSON<{ id: number; url: string }>(`/api/v3/projects/columns/cards/${cardId}/moves`, payload);

// ─── Secret scanning ────────────────────────────────────────────────────

export interface SecretScanningFilters {
  state?: "open" | "resolved" | undefined;
  secret_type?: string | undefined;
  resolution?: string | undefined;
}

export const fetchSecretScanningAlerts = (
  owner: string,
  repo: string,
  filters: SecretScanningFilters = {},
): Promise<GithubSecretScanningAlert[]> => {
  const params = new URLSearchParams();
  if (filters.state) params.set("state", filters.state);
  if (filters.secret_type) params.set("secret_type", filters.secret_type);
  if (filters.resolution) params.set("resolution", filters.resolution);
  const qs = params.toString();
  return ghFetch<GithubSecretScanningAlert[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/secret-scanning/alerts${qs ? `?${qs}` : ""}`);
};

export const fetchSecretScanningAlert = (owner: string, repo: string, number: number) =>
  ghFetch<GithubSecretScanningAlert>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/secret-scanning/alerts/${number}`);

export const fetchSecretScanningAlertLocations = (owner: string, repo: string, number: number) =>
  ghFetch<GithubSecretScanningLocation[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/secret-scanning/alerts/${number}/locations`);

export const updateSecretScanningAlert = (
  owner: string,
  repo: string,
  number: number,
  body: { state: "open" | "resolved"; resolution?: GithubSecretScanningResolution; resolution_comment?: string },
) => ghPatchJSON<GithubSecretScanningAlert>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/secret-scanning/alerts/${number}`, body);

// ─── Code scanning ──────────────────────────────────────────────────────

export interface CodeScanningFilters {
  state?: GithubCodeScanningAlertState | undefined;
  severity?: string | undefined;
  tool_name?: string | undefined;
  rule?: string | undefined;
  sort?: "created" | "updated" | undefined;
  direction?: "asc" | "desc" | undefined;
}

export const fetchCodeScanningAlerts = (
  owner: string,
  repo: string,
  filters: CodeScanningFilters = {},
): Promise<GithubCodeScanningAlert[]> => {
  const params = new URLSearchParams();
  if (filters.state) params.set("state", filters.state);
  if (filters.severity) params.set("severity", filters.severity);
  if (filters.tool_name) params.set("tool_name", filters.tool_name);
  if (filters.rule) params.set("rule", filters.rule);
  if (filters.sort) params.set("sort", filters.sort);
  if (filters.direction) params.set("direction", filters.direction);
  const qs = params.toString();
  return ghFetch<GithubCodeScanningAlert[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/code-scanning/alerts${qs ? `?${qs}` : ""}`);
};

export const fetchCodeScanningAlertInstances = (owner: string, repo: string, number: number) =>
  ghFetch<GithubCodeScanningAlertInstance[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/code-scanning/alerts/${number}/instances`);

export const updateCodeScanningAlert = (
  owner: string,
  repo: string,
  number: number,
  body: { state: GithubCodeScanningAlertState; dismissed_reason?: GithubCodeScanningDismissedReason; dismissed_comment?: string },
) => ghPatchJSON<GithubCodeScanningAlert>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/code-scanning/alerts/${number}`, body);

export const fetchCodeScanningAnalyses = (owner: string, repo: string) =>
  ghFetch<GithubCodeScanningAnalysis[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/code-scanning/analyses`);

export const deleteCodeScanningAnalysis = (owner: string, repo: string, id: number) =>
  ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/code-scanning/analyses/${id}`);

export const uploadSARIF = (
  owner: string,
  repo: string,
  body: { commit_sha: string; ref: string; sarif: string; tool_name?: string },
): Promise<GithubCodeScanningSARIFUpload> =>
  ghPostJSON<GithubCodeScanningSARIFUpload>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/code-scanning/sarifs`, body);

export const fetchSARIFStatus = (owner: string, repo: string, id: string) =>
  ghFetch<GithubCodeScanningSARIFStatus>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/code-scanning/sarifs/${id}`);

export const fetchCodeQLDatabases = (owner: string, repo: string) =>
  ghFetch<GithubCodeQLDatabase[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/code-scanning/codeql/databases`);

export const deleteCodeQLDatabase = (owner: string, repo: string, language: string) =>
  ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/code-scanning/codeql/databases/${encodeURIComponent(language)}`);

export async function downloadCodeQLDatabase(owner: string, repo: string, language: string): Promise<Blob> {
  const response = await apiFetch(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/code-scanning/codeql/databases/${encodeURIComponent(language)}`,
    { headers: { Accept: "application/zip", ...authHeaders() } },
  );
  if (!response.ok) {
    handleUnauthorized(response);
    throw new ApiError(response.status, `${response.status} ${response.statusText}`);
  }
  return response.blob();
}

async function ghPatchJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await apiFetch(path, {
    method: "PATCH",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    throw new ApiError(res.status, `${res.status} ${res.statusText}: ${text || res.statusText}`);
  }
  return res.json() as Promise<T>;
}

// ─── GitHub Dependabot ──────────────────────────────────────────────────

export interface DependabotFilters {
  state?: GithubDependabotAlertState | undefined;
  severity?: string | undefined;
  package_name?: string | undefined;
  ecosystem?: string | undefined;
  manifest?: string | undefined;
  sort?: "created" | "updated" | undefined;
  direction?: "asc" | "desc" | undefined;
}

export const fetchDependabotAlerts = (
  owner: string,
  repo: string,
  filters: DependabotFilters = {},
): Promise<GithubDependabotAlert[]> => {
  const params = new URLSearchParams();
  if (filters.state) params.set("state", filters.state);
  if (filters.severity) params.set("severity", filters.severity);
  if (filters.package_name) params.set("package_name", filters.package_name);
  if (filters.ecosystem) params.set("ecosystem", filters.ecosystem);
  if (filters.manifest) params.set("manifest", filters.manifest);
  if (filters.sort) params.set("sort", filters.sort);
  if (filters.direction) params.set("direction", filters.direction);
  const qs = params.toString();
  return ghFetch<GithubDependabotAlert[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/dependabot/alerts${qs ? `?${qs}` : ""}`);
};

export const fetchDependabotAlert = (owner: string, repo: string, number: number) =>
  ghFetch<GithubDependabotAlert>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/dependabot/alerts/${number}`);

export const updateDependabotAlert = (
  owner: string,
  repo: string,
  number: number,
  body: {
    state: "open" | "dismissed";
    dismissed_reason?: GithubDependabotDismissedReason;
    dismissed_comment?: string;
  },
) => ghPatchJSON<GithubDependabotAlert>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/dependabot/alerts/${number}`, body);

export const fetchDependabotRepoSecrets = (owner: string, repo: string) =>
  ghFetchEnvelope<GithubDependabotSecret>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/dependabot/secrets?per_page=100`, "secrets");

export const fetchDependabotRepoPublicKey = (owner: string, repo: string) =>
  ghFetch<GithubPublicKey>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/dependabot/secrets/public-key`);

export const putDependabotRepoSecret = (
  owner: string,
  repo: string,
  name: string,
  body: { encrypted_value: string; key_id: string },
) => ghSend("PUT", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/dependabot/secrets/${encodeURIComponent(name)}`, body);

export const deleteDependabotRepoSecret = (owner: string, repo: string, name: string) =>
  ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/dependabot/secrets/${encodeURIComponent(name)}`);

// ─── GitHub Security Advisories Representational State Transfer ─────────

export const fetchSecurityAdvisories = (owner: string, repo: string) =>
  ghFetch<GithubSecurityAdvisory[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/security-advisories`);

export const createSecurityAdvisory = (
  owner: string,
  repo: string,
  payload: GithubSecurityAdvisoryCreatePayload,
) => ghPostJSON<GithubSecurityAdvisory>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/security-advisories`, payload);

export const fetchSecurityAdvisory = (owner: string, repo: string, ghsaId: string) =>
  ghFetch<GithubSecurityAdvisory>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/security-advisories/${encodeURIComponent(ghsaId)}`,
  );

export const updateSecurityAdvisory = (
  owner: string,
  repo: string,
  ghsaId: string,
  payload: GithubSecurityAdvisoryUpdatePayload,
) =>
  ghPatchJSON<GithubSecurityAdvisory>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/security-advisories/${encodeURIComponent(ghsaId)}`,
    payload,
  );

export const createSecurityAdvisoryTemporaryFork = (
  owner: string,
  repo: string,
  ghsaId: string,
) =>
  ghPostJSON<BleephubRepo>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/security-advisories/${encodeURIComponent(ghsaId)}/forks`,
    {},
  );

export const requestCVE = (owner: string, repo: string, ghsaId: string) =>
  ghPostJSON<GithubSecurityAdvisory>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/security-advisories/${encodeURIComponent(ghsaId)}/cve`, {});

export const reportVulnerability = (
  owner: string,
  repo: string,
  payload: GithubVulnerabilityReportPayload,
) => ghPostJSON<GithubSecurityAdvisory>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/security-advisories/reports`, payload);

// ─── GitHub Organization Rulesets Representational State Transfer ───────

export const fetchOrgRulesets = (org: string) =>
  ghFetch<GithubRuleset[]>(`/api/v3/orgs/${encodeURIComponent(org)}/rulesets`);

export const createOrgRuleset = (org: string, payload: GithubRulesetCreatePayload) =>
  ghPostJSON<GithubRuleset>(`/api/v3/orgs/${encodeURIComponent(org)}/rulesets`, payload);

export const updateOrgRuleset = (org: string, rulesetId: number, payload: GithubRulesetCreatePayload) =>
  ghPutJSON<GithubRuleset>(`/api/v3/orgs/${encodeURIComponent(org)}/rulesets/${rulesetId}`, payload);

export const deleteOrgRuleset = (org: string, rulesetId: number) =>
  ghDeleteJSON<void>(`/api/v3/orgs/${encodeURIComponent(org)}/rulesets/${rulesetId}`, {});

export interface GithubRulesetSuiteFilters {
  repositoryName?: string;
  ref?: string;
  actorName?: string;
  result?: "all" | "pass" | "fail" | "bypass";
  evaluateStatus?: "all" | "active" | "evaluate";
  timePeriod?: "hour" | "day" | "week" | "month";
}

export const fetchOrgRulesetSuites = (org: string, filters: GithubRulesetSuiteFilters = {}) => {
  const query = new URLSearchParams({ per_page: "100" });
  if (filters.repositoryName) query.set("repository_name", filters.repositoryName);
  if (filters.ref) query.set("ref", filters.ref);
  if (filters.actorName) query.set("actor_name", filters.actorName);
  if (filters.result && filters.result !== "all") query.set("rule_suite_result", filters.result);
  if (filters.evaluateStatus && filters.evaluateStatus !== "all") query.set("evaluate_status", filters.evaluateStatus);
  if (filters.timePeriod) query.set("time_period", filters.timePeriod);
  return ghFetch<GithubRulesetSuite[]>(
    `/api/v3/orgs/${encodeURIComponent(org)}/rulesets/rule-suites?${query}`,
  );
};

export const fetchOrgRulesetSuite = (org: string, suiteId: number) =>
  ghFetch<GithubRulesetSuite>(
    `/api/v3/orgs/${encodeURIComponent(org)}/rulesets/rule-suites/${suiteId}`,
  );

// ─── GitHub Migrations Representational State Transfer ──────────────────

type MigrationScope = { kind: "user" } | { kind: "org"; org: string };

function migrationBase(scope: MigrationScope): string {
  return scope.kind === "user"
    ? "/api/v3/user/migrations"
    : `/api/v3/orgs/${encodeURIComponent(scope.org)}/migrations`;
}

export const fetchUserMigrations = () =>
  ghFetch<GithubMigration[]>("/api/v3/user/migrations");

export const fetchOrgMigrations = (org: string) =>
  ghFetch<GithubMigration[]>(`/api/v3/orgs/${encodeURIComponent(org)}/migrations`);

export const createUserMigration = (payload: GithubMigrationStartPayload) =>
  ghPostJSON<GithubMigration>("/api/v3/user/migrations", payload);

export const createOrgMigration = (org: string, payload: GithubMigrationStartPayload) =>
  ghPostJSON<GithubMigration>(`/api/v3/orgs/${encodeURIComponent(org)}/migrations`, payload);

export const deleteMigrationArchive = (scope: MigrationScope, id: number) =>
  ghDeleteJSON<void>(`${migrationBase(scope)}/${id}/archive`, {});

export const unlockMigrationRepo = (scope: MigrationScope, id: number, repoName: string) =>
  ghDeleteJSON<void>(`${migrationBase(scope)}/${id}/repos/${encodeURIComponent(repoName)}/lock`, {});

export const fetchOrgMigrationLockStatus = (org: string, id: number, repoName: string) =>
  ghFetch<{ locked: boolean }>(
    `/api/v3/orgs/${encodeURIComponent(org)}/migrations/${id}/repos/${encodeURIComponent(repoName)}/lock`,
  );

/** Download a migration archive by fetching the authenticated binary and
 *  triggering a browser save-as for the given filename. */
export async function downloadMigrationArchive(
  scope: MigrationScope,
  id: number,
  filename: string,
): Promise<void> {
  const res = await apiFetch(`${migrationBase(scope)}/${id}/archive`, {
    headers: authHeaders(),
  });
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

// ─── GitHub Codespaces Representational State Transfer ──────────────────

const codespaceStates = new Set<GithubCodespace["state"]>([
  "Unknown", "Created", "Queued", "Provisioning", "Available", "Awaiting",
  "Unavailable", "Deleted", "Moved", "Shutdown", "Archived", "Starting",
  "ShuttingDown", "Failed", "Exporting", "Updating", "Rebuilding",
]);

function decodeCodespaceMachine(value: unknown): GithubCodespaceMachine {
  const machine = responseObject(value, "codespace machine");
  for (const field of ["name", "display_name", "operating_system"] as const) {
    if (typeof machine[field] !== "string") {
      throw new Error(`malformed codespace machine: "${field}" must be a string`);
    }
  }
  for (const field of ["storage_in_bytes", "memory_in_bytes", "cpus"] as const) {
    if (typeof machine[field] !== "number") {
      throw new Error(`malformed codespace machine: "${field}" must be a number`);
    }
  }
  return value as GithubCodespaceMachine;
}

function decodeCodespace(value: unknown): GithubCodespace {
  const codespace = responseObject(value, "codespace response");
  if (typeof codespace.id !== "number" || typeof codespace.name !== "string") {
    throw new Error('malformed codespace response: "id" and "name" are required');
  }
  for (const field of ["display_name", "created_at", "updated_at", "last_used_at"] as const) {
    if (typeof codespace[field] !== "string") {
      throw new Error(`malformed codespace response: "${field}" must be a string`);
    }
  }
  if (typeof codespace.state !== "string" || !codespaceStates.has(codespace.state as GithubCodespace["state"])) {
    throw new Error(`malformed codespace response: unknown state "${String(codespace.state)}"`);
  }
  if (codespace.machine !== null) decodeCodespaceMachine(codespace.machine);
  if (codespace.repository !== null) {
    const repository = responseObject(codespace.repository, "codespace repository");
    if (typeof repository.id !== "number" || typeof repository.full_name !== "string") {
      throw new Error("malformed codespace repository: incomplete identity");
    }
  }
  const gitStatus = responseObject(codespace.git_status, "codespace git_status");
  if (typeof gitStatus.ref !== "string") {
    throw new Error('malformed codespace git_status: "ref" must be a string');
  }
  return value as GithubCodespace;
}

export const fetchUserCodespaces = () =>
  ghFetchEnvelope<GithubCodespace>("/api/v3/user/codespaces", "codespaces", decodeCodespace);

export const fetchRepoCodespaces = (owner: string, repo: string) =>
  ghFetchEnvelope<GithubCodespace>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/codespaces`,
    "codespaces",
    decodeCodespace,
  );

export const fetchCodespace = (name: string) =>
  ghFetch<unknown>(`/api/v3/user/codespaces/${encodeURIComponent(name)}`).then(decodeCodespace);

export const createUserCodespace = (payload: CodespaceCreatePayload) =>
  ghPostJSON<unknown>("/api/v3/user/codespaces", payload).then(decodeCodespace);

export const createRepoCodespace = (owner: string, repo: string, payload: CodespaceCreatePayload) =>
  ghPostJSON<unknown>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/codespaces`, payload).then(decodeCodespace);

export const startCodespace = (name: string) =>
  ghPostJSON<unknown>(`/api/v3/user/codespaces/${encodeURIComponent(name)}/start`, {}).then(decodeCodespace);

export const stopCodespace = (name: string) =>
  ghPostJSON<unknown>(`/api/v3/user/codespaces/${encodeURIComponent(name)}/stop`, {}).then(decodeCodespace);

export const deleteCodespace = (name: string) =>
  ghDelete(`/api/v3/user/codespaces/${encodeURIComponent(name)}`);

export const fetchCodespaceMachines = (owner: string, repo: string) =>
  ghFetchEnvelope<GithubCodespaceMachine>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/codespaces/machines`,
    "machines",
    decodeCodespaceMachine,
  );

export const fetchCurrentUser = (signal?: AbortSignal) => ghFetch<BleephubUser>("/api/v3/user", signal);

// ─── GitHub Packages Representational State Transfer ────────────────────

export type PackageScope =
  | { kind: "user"; username: string }
  | { kind: "org"; org: string }
  | { kind: "repo"; owner: string; repo: string };

function packageBasePath(scope: PackageScope, pkgType: string, pkgName: string): string {
  const pt = encodeURIComponent(pkgType);
  const pn = encodeURIComponent(pkgName);
  switch (scope.kind) {
    case "user":
      return `/api/v3/users/${encodeURIComponent(scope.username)}/packages/${pt}/${pn}`;
    case "org":
      return `/api/v3/orgs/${encodeURIComponent(scope.org)}/packages/${pt}/${pn}`;
    case "repo":
      return `/ui-data/repos/${encodeURIComponent(scope.owner)}/${encodeURIComponent(scope.repo)}/packages/${pt}/${pn}`;
  }
}

export function packageListPath(scope: PackageScope, pkgType?: string): string {
  const query = pkgType ? `?package_type=${encodeURIComponent(pkgType)}` : "";
  switch (scope.kind) {
    case "user":
      return `/api/v3/user/packages${query}`;
    case "org":
      return `/api/v3/orgs/${encodeURIComponent(scope.org)}/packages${query}`;
    case "repo":
      return `/ui-data/repos/${encodeURIComponent(scope.owner)}/${encodeURIComponent(scope.repo)}/packages${query}`;
  }
}

export const fetchPackages = (scope: PackageScope, pkgType?: string) =>
  ghFetch<GithubPackage[]>(packageListPath(scope, pkgType));

export const fetchPackageVersions = (scope: PackageScope, pkgType: string, pkgName: string) =>
  ghFetch<GithubPackageVersion[]>(`${packageBasePath(scope, pkgType, pkgName)}/versions`);

export const fetchPackageFiles = (
  scope: PackageScope,
  pkgType: string,
  pkgName: string,
  versionID: number,
) => {
  const suffix = `/packages/${encodeURIComponent(pkgType)}/${encodeURIComponent(pkgName)}/versions/${versionID}/files`;
  switch (scope.kind) {
    case "user":
      return ghFetch<GithubPackageFile[]>(`/ui-data/users/${encodeURIComponent(scope.username)}${suffix}`);
    case "org":
      return ghFetch<GithubPackageFile[]>(`/ui-data/orgs/${encodeURIComponent(scope.org)}${suffix}`);
    case "repo":
      return ghFetch<GithubPackageFile[]>(`/ui-data/repos/${encodeURIComponent(scope.owner)}/${encodeURIComponent(scope.repo)}${suffix}`);
  }
};

export const deletePackageVersion = (
  scope: PackageScope,
  pkgType: string,
  pkgName: string,
  versionID: number,
) => ghDeleteJSON<void>(`${packageBasePath(scope, pkgType, pkgName)}/versions/${versionID}`, {});

export const restorePackageVersion = (
  scope: PackageScope,
  pkgType: string,
  pkgName: string,
  versionID: number,
) =>
  ghPostJSON<void>(
    `${packageBasePath(scope, pkgType, pkgName)}/versions/${versionID}/restore`,
    {},
  );

export const deletePackage = (scope: PackageScope, pkgType: string, pkgName: string) =>
  ghDeleteJSON<void>(packageBasePath(scope, pkgType, pkgName), {});

// ─── GitHub GraphQL ─────────────────────────────────────────────────────

interface GraphQLResponse<T> {
  data?: T;
  errors?: Array<{ message: string; type?: string }>;
}

async function ghGraphQL<T>(query: string, variables?: Record<string, unknown>, signal?: AbortSignal): Promise<T> {
  const res = await apiFetch("/api/graphql", {
    method: "POST",
    headers: { "Content-Type": "application/json", ...authHeaders() },
    body: JSON.stringify({ query, variables }),
    signal,
  });
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  const json = (await res.json()) as GraphQLResponse<T>;
  if (json.errors && json.errors.length > 0) {
    const status = json.errors.some((error) => error.type === "NOT_FOUND") ? 404 : 422;
    throw new ApiError(status, json.errors.map((error) => error.message).join("; "));
  }
  if (json.data === undefined) {
    throw new Error("graphql response missing data");
  }
  return json.data;
}

// ─── GitHub Discussions GraphQL ─────────────────────────────────────────

const DISCUSSION_LIST_FRAGMENT = `
  id
  number
  title
  bodyText
  author { login avatarUrl }
  category { id name emoji isAnswerable }
  createdAt
  updatedAt
  comments(first: 0) { totalCount }
`;

export async function fetchDiscussionCategories(
  owner: string,
  repo: string,
  signal?: AbortSignal,
): Promise<GithubDiscussionCategory[]> {
  const data = await ghGraphQL<{
    repository: { discussionCategories: GithubDiscussionCategoryConnection };
  }>(
    `query($owner: String!, $repo: String!) {
      repository(owner: $owner, name: $repo) {
        discussionCategories(first: 100) {
          nodes { id name emoji description isAnswerable }
        }
      }
    }`,
    { owner, repo },
    signal,
  );
  return data.repository.discussionCategories.nodes;
}

export async function fetchDiscussionsPage(
  owner: string,
  repo: string,
  categoryId: string | null,
  after: string | null,
  signal?: AbortSignal,
): Promise<GithubDiscussionConnection> {
  const data = await ghGraphQL<{
    repository: { discussions: GithubDiscussionConnection };
  }>(
    `query($owner: String!, $repo: String!, $categoryId: ID, $after: String) {
      repository(owner: $owner, name: $repo) {
        discussions(first: 30, categoryId: $categoryId, after: $after, orderBy: {field: CREATED_AT, direction: DESC}) {
          nodes { ${DISCUSSION_LIST_FRAGMENT} }
          totalCount
          pageInfo { hasNextPage endCursor }
        }
      }
    }`,
    { owner, repo, categoryId, after },
    signal,
  );
  return data.repository.discussions;
}

export async function fetchDiscussionDetail(
  owner: string,
  repo: string,
  number: number,
  signal?: AbortSignal,
): Promise<GithubDiscussion & { comments: GithubDiscussionCommentConnection }> {
  const data = await ghGraphQL<{
    repository: {
      discussion: GithubDiscussion & { comments: GithubDiscussionCommentConnection };
    };
  }>(
    `query($owner: String!, $repo: String!, $number: Int!) {
      repository(owner: $owner, name: $repo) {
        discussion(number: $number) {
          id
          number
          title
          body
          bodyText
          author { login avatarUrl }
          category { id name emoji isAnswerable }
          createdAt
          updatedAt
          comments(first: 100) {
            nodes {
              id
              databaseId
              author { login avatarUrl }
              body
              createdAt
              updatedAt
              isAnswer
              replies(first: 100) {
                nodes {
                  id
                  databaseId
                  author { login avatarUrl }
                  body
                  createdAt
                  updatedAt
                  isAnswer
                }
              }
            }
            totalCount
          }
        }
      }
    }`,
    { owner, repo, number },
    signal,
  );
  return data.repository.discussion;
}

export async function createDiscussion(
  repoNodeId: string,
  categoryId: string,
  title: string,
  body: string,
): Promise<GithubDiscussion> {
  const data = await ghGraphQL<{ createDiscussion: { discussion: GithubDiscussion } }>(
    `mutation($input: CreateDiscussionInput!) {
      createDiscussion(input: $input) {
        discussion {
          id
          number
          title
          bodyText
          author { login avatarUrl }
          category { id name emoji isAnswerable }
          createdAt
          updatedAt
          comments(first: 0) { totalCount }
        }
      }
    }`,
    { input: { repositoryId: repoNodeId, categoryId, title, body } },
  );
  return data.createDiscussion.discussion;
}

export async function addDiscussionComment(
  discussionId: string,
  body: string,
  replyToId?: string,
): Promise<{ id: string; databaseId: number }> {
  const data = await ghGraphQL<{ addDiscussionComment: { comment: { id: string; databaseId: number } } }>(
    `mutation($input: AddDiscussionCommentInput!) {
      addDiscussionComment(input: $input) {
        comment { id databaseId }
      }
    }`,
    { input: { discussionId, body, replyToId } },
  );
  return data.addDiscussionComment.comment;
}

export async function markDiscussionCommentAsAnswer(commentId: string): Promise<void> {
  await ghGraphQL<{ markDiscussionCommentAsAnswer: unknown }>(
    `mutation($input: MarkDiscussionCommentAsAnswerInput!) {
      markDiscussionCommentAsAnswer(input: $input) { clientMutationId }
    }`,
    { input: { commentId } },
  );
}

export async function unmarkDiscussionCommentAsAnswer(commentId: string): Promise<void> {
  await ghGraphQL<{ unmarkDiscussionCommentAsAnswer: unknown }>(
    `mutation($input: UnmarkDiscussionCommentAsAnswerInput!) {
      unmarkDiscussionCommentAsAnswer(input: $input) { clientMutationId }
    }`,
    { input: { commentId } },
  );
}

export async function deleteDiscussion(discussionId: string): Promise<void> {
  await ghGraphQL<{ deleteDiscussion: unknown }>(
    `mutation($input: DeleteDiscussionInput!) {
      deleteDiscussion(input: $input) { clientMutationId }
    }`,
    { input: { discussionId } },
  );
}

export async function deleteDiscussionComment(commentId: string): Promise<void> {
  await ghGraphQL<{ deleteDiscussionComment: unknown }>(
    `mutation($input: DeleteDiscussionCommentInput!) {
      deleteDiscussionComment(input: $input) { clientMutationId }
    }`,
    { input: { commentId } },
  );
}

export async function updateDiscussionComment(commentId: string, body: string): Promise<void> {
  await ghGraphQL<{ updateDiscussionComment: unknown }>(
    `mutation($input: UpdateDiscussionCommentInput!) {
      updateDiscussionComment(input: $input) { clientMutationId }
    }`,
    { input: { commentId, body } },
  );
}

// ─── Repo insights ───────────────────────────────────────────────────────

/** GET /contributors returns 204 with an empty body for a repo with no
 * commits — that reads as an honest empty list, not a parse error. */
export async function fetchRepoContributors(owner: string, repo: string, signal?: AbortSignal): Promise<GithubContributor[]> {
  const res = await apiFetch(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/contributors`, { headers: authHeaders(), signal });
  if (!res.ok) {
    handleUnauthorized(res);
    throw new ApiError(res.status, `${res.status} ${res.statusText}`);
  }
  if (res.status === 204) return [];
  return res.json() as Promise<GithubContributor[]>;
}

export const fetchTrafficViews = (owner: string, repo: string) =>
  ghFetch<GithubTrafficViews>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/traffic/views`);

export const fetchTrafficClones = (owner: string, repo: string) =>
  ghFetch<GithubTrafficClones>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/traffic/clones`);

export const fetchTrafficPopularPaths = (owner: string, repo: string) =>
  ghFetch<GithubTrafficPath[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/traffic/popular/paths`);

export const fetchTrafficPopularReferrers = (owner: string, repo: string) =>
  ghFetch<GithubTrafficReferrer[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/traffic/popular/referrers`);

export const fetchCommitActivity = (owner: string, repo: string) =>
  ghFetch<GithubCommitActivityWeek[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/stats/commit_activity`);

export const fetchCommunityProfile = (owner: string, repo: string) =>
  ghFetch<GithubCommunityProfile>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/community/profile`);

// ─── Labels + milestones ────────────────────────────────────────────────

export const fetchRepoLabels = (owner: string, repo: string) =>
  ghFetch<GithubLabel[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/labels?per_page=100`);

export const createRepoLabel = (
  owner: string,
  repo: string,
  payload: { name: string; color?: string; description?: string },
): Promise<GithubLabel> => ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/labels`, payload);

export const updateRepoLabel = (
  owner: string,
  repo: string,
  name: string,
  payload: { new_name?: string; color?: string; description?: string },
): Promise<GithubLabel> =>
  ghPatchJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/labels/${encodeURIComponent(name)}`, payload);

export const deleteRepoLabel = (owner: string, repo: string, name: string) =>
  ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/labels/${encodeURIComponent(name)}`);

export const fetchRepoMilestones = (owner: string, repo: string, state: "open" | "closed" | "all") =>
  ghFetch<GithubMilestone[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/milestones?state=${state}&per_page=100`);

export const createRepoMilestone = (
  owner: string,
  repo: string,
  payload: { title: string; description?: string | undefined; due_on?: string | undefined },
): Promise<GithubMilestone> => ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/milestones`, payload);

export const updateRepoMilestone = (
  owner: string,
  repo: string,
  number: number,
  payload: { title?: string; description?: string; state?: "open" | "closed" },
): Promise<GithubMilestone> =>
  ghPatchJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/milestones/${number}`, payload);

export const deleteRepoMilestone = (owner: string, repo: string, number: number) =>
  ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/milestones/${number}`);

export const addIssueLabels = (
  owner: string,
  repo: string,
  number: number,
  labels: string[],
): Promise<GithubLabel[]> =>
  ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/labels`, { labels });

export const removeIssueLabel = (owner: string, repo: string, number: number, name: string) =>
  ghSend(
    "DELETE",
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/labels/${encodeURIComponent(name)}`,
  );

/** PATCH the issue's milestone: a milestone number sets it, null clears it. */
export const setIssueMilestone = (
 owner: string,
 repo: string,
 number: number,
 milestone: number | null,
): Promise<GithubIssue> =>
  ghPatchJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}`, { milestone });

/** PATCH the issue's organization issue type: an ID sets it, null clears it. */
export const setIssueType = (
  owner: string,
  repo: string,
  number: number,
  issueTypeId: number | null,
): Promise<GithubIssue> =>
  ghPatchJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}`, { issue_type_id: issueTypeId });

export interface GithubIssueGraphQLIssueType {
  id: string;
  name: string;
  description: string | null;
  color: string | null;
}

export const fetchIssueGraphQLIssueType = async (
  owner: string,
  repo: string,
  number: number,
): Promise<GithubIssueGraphQLIssueType | null> => {
  const data = await ghGraphQL<{
    repository: { issue: { issueType: GithubIssueGraphQLIssueType | null } | null } | null;
  }>(
    `query($owner:String!,$name:String!,$number:Int!){
      repository(owner:$owner,name:$name){
        issue(number:$number){issueType{id,name,description,color}}
      }
    }`,
    { owner, name: repo, number },
  );
  return data.repository?.issue?.issueType ?? null;
};

// ─── Organization governance ────────────────────────────────────────────

export const fetchOrgInvitations = (org: string) =>
  ghFetch<GithubOrgInvitation[]>(`/api/v3/orgs/${encodeURIComponent(org)}/invitations`);

export const fetchFailedOrgInvitations = (org: string) =>
  ghFetch<GithubOrgInvitation[]>(`/api/v3/orgs/${encodeURIComponent(org)}/failed_invitations`);

export const createOrgInvitation = (
  org: string,
  payload: { email: string; role: string },
): Promise<GithubOrgInvitation> => ghPostJSON(`/api/v3/orgs/${encodeURIComponent(org)}/invitations`, payload);

export const cancelOrgInvitation = (org: string, invitationId: number) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(org)}/invitations/${invitationId}`);

export const fetchOutsideCollaborators = (org: string) =>
  ghFetch<GithubAccount[]>(`/api/v3/orgs/${encodeURIComponent(org)}/outside_collaborators`);

export const removeOutsideCollaborator = (org: string, username: string) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(org)}/outside_collaborators/${encodeURIComponent(username)}`);

export const fetchOrgBlocks = (org: string) =>
  ghFetch<GithubAccount[]>(`/api/v3/orgs/${encodeURIComponent(org)}/blocks`);

export const blockOrgUser = (org: string, username: string) =>
  ghSend("PUT", `/api/v3/orgs/${encodeURIComponent(org)}/blocks/${encodeURIComponent(username)}`);

export const unblockOrgUser = (org: string, username: string) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(org)}/blocks/${encodeURIComponent(username)}`);

export const fetchOrgCustomProperties = (org: string) =>
  ghFetch<GithubCustomProperty[]>(`/api/v3/orgs/${encodeURIComponent(org)}/properties/schema`);

/** Batch-upsert definitions through the schema PATCH endpoint. */
export const upsertOrgCustomProperties = (
  org: string,
  properties: {
    property_name: string;
    value_type: GithubCustomPropertyValueType;
    required?: boolean | undefined;
    default_value?: string | null | undefined;
    description?: string | null | undefined;
    allowed_values?: string[] | undefined;
  }[],
): Promise<GithubCustomProperty[]> =>
  ghPatchJSON(`/api/v3/orgs/${encodeURIComponent(org)}/properties/schema`, { properties });

export const deleteOrgCustomProperty = (org: string, name: string) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(org)}/properties/schema/${encodeURIComponent(name)}`);

export const fetchOrgIssueTypes = (org: string) =>
  ghFetch<GithubIssueType[]>(`/api/v3/orgs/${encodeURIComponent(org)}/issue-types`);

export const createOrgIssueType = (
  org: string,
  payload: { name: string; is_enabled: boolean; description?: string | undefined; color?: string | undefined },
): Promise<GithubIssueType> => ghPostJSON(`/api/v3/orgs/${encodeURIComponent(org)}/issue-types`, payload);

export const updateOrgIssueType = (
  org: string,
  issueTypeId: number,
  payload: { name: string; is_enabled: boolean; description?: string | undefined; color?: string | undefined },
): Promise<GithubIssueType> =>
  ghPutJSON(`/api/v3/orgs/${encodeURIComponent(org)}/issue-types/${issueTypeId}`, payload);

export const deleteOrgIssueType = (org: string, issueTypeId: number) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(org)}/issue-types/${issueTypeId}`);

export const fetchOrgRoles = (org: string) =>
  ghFetchEnvelope<GithubOrgRole>(`/api/v3/orgs/${encodeURIComponent(org)}/organization-roles`, "roles");

export const fetchOrgRoleTeams = (org: string, roleId: number) =>
  ghFetch<GithubOrgRoleTeam[]>(`/api/v3/orgs/${encodeURIComponent(org)}/organization-roles/${roleId}/teams`);

export const fetchOrgRoleUsers = (org: string, roleId: number) =>
  ghFetch<GithubOrgRoleUser[]>(`/api/v3/orgs/${encodeURIComponent(org)}/organization-roles/${roleId}/users`);

export const assignOrgRoleToTeam = (org: string, teamSlug: string, roleId: number) =>
  ghSend("PUT", `/api/v3/orgs/${encodeURIComponent(org)}/organization-roles/teams/${encodeURIComponent(teamSlug)}/${roleId}`);

export const revokeOrgRoleFromTeam = (org: string, teamSlug: string, roleId: number) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(org)}/organization-roles/teams/${encodeURIComponent(teamSlug)}/${roleId}`);

export const assignOrgRoleToUser = (org: string, username: string, roleId: number) =>
  ghSend("PUT", `/api/v3/orgs/${encodeURIComponent(org)}/organization-roles/users/${encodeURIComponent(username)}/${roleId}`);

export const revokeOrgRoleFromUser = (org: string, username: string, roleId: number) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(org)}/organization-roles/users/${encodeURIComponent(username)}/${roleId}`);

// ─── Enterprise administration ──────────────────────────────────────────

export async function fetchEnterpriseSlug(signal?: AbortSignal): Promise<string> {
  const health = await fetchHealth(signal);
  if (!health.enterprise_slug) {
    throw new Error("/health response did not include enterprise_slug");
  }
  return health.enterprise_slug;
}

async function enterprisePath(path: string): Promise<string> {
  const slug = await fetchEnterpriseSlug();
  return `/api/v3/enterprises/${encodeURIComponent(slug)}${path}`;
}

export const fetchEnterpriseTeams = () =>
  enterprisePath("/teams").then((path) => ghFetch<GithubEnterpriseTeam[]>(path));

export const createEnterpriseTeam = (payload: {
  name: string;
  description?: string | undefined;
  organization_selection_type?: string | undefined;
}): Promise<GithubEnterpriseTeam> =>
  enterprisePath("/teams").then((path) => ghPostJSON(path, payload));

export const updateEnterpriseTeam = (
  slug: string,
  payload: { name?: string; description?: string; organization_selection_type?: string },
): Promise<GithubEnterpriseTeam> =>
  enterprisePath(`/teams/${encodeURIComponent(slug)}`).then((path) => ghPatchJSON(path, payload));

export const deleteEnterpriseTeam = (slug: string) =>
  enterprisePath(`/teams/${encodeURIComponent(slug)}`).then((path) => ghSend("DELETE", path));

export const fetchEnterpriseTeamMembers = (slug: string) =>
  enterprisePath(`/teams/${encodeURIComponent(slug)}/memberships`).then((path) =>
    ghFetch<GithubAccount[]>(path),
  );

export const addEnterpriseTeamMember = (slug: string, username: string): Promise<GithubAccount> =>
  enterprisePath(`/teams/${encodeURIComponent(slug)}/memberships/${encodeURIComponent(username)}`).then(
    (path) => ghPutJSON(path, {}),
  );

export const removeEnterpriseTeamMember = (slug: string, username: string) =>
  enterprisePath(`/teams/${encodeURIComponent(slug)}/memberships/${encodeURIComponent(username)}`).then(
    (path) => ghSend("DELETE", path),
  );

export const fetchEnterpriseActionsCacheLimit = () =>
  enterprisePath("/actions/cache/storage-limit").then((path) =>
    ghFetch<{ max_cache_size_gb: number }>(path),
  );

export const setEnterpriseActionsCacheLimit = (maxCacheSizeGB: number) =>
  enterprisePath("/actions/cache/storage-limit").then((path) =>
    ghSend("PUT", path, {
      max_cache_size_gb: maxCacheSizeGB,
    }),
  );

export const fetchEnterpriseDependabotAccess = () =>
  enterprisePath("/dependabot/repository-access").then((path) =>
    ghFetch<GithubEnterpriseDependabotAccess>(path),
  );

export const updateEnterpriseDependabotAccess = (payload: {
  repository_ids_to_add?: number[];
  repository_ids_to_remove?: number[];
}) =>
  enterprisePath("/dependabot/repository-access").then((path) =>
    ghSend("PATCH", path, payload),
  );

export const setEnterpriseDependabotDefaultLevel = (level: "public" | "internal") =>
  enterprisePath("/dependabot/repository-access/default-level").then((path) =>
    ghSend("PUT", path, {
      default_level: level,
    }),
  );

// ─── Copilot ────────────────────────────────────────────────────────────

export const fetchCopilotBilling = (org: string) =>
  ghFetch<GithubCopilotBilling>(`/api/v3/orgs/${encodeURIComponent(org)}/copilot/billing`);

/** Envelope list ({total_seats, seats}); keyed like ghFetchEnvelope but the
 * endpoint names its count total_seats rather than total_count. */
export async function fetchCopilotSeats(
  org: string,
  signal?: AbortSignal,
): Promise<{ totalSeats: number; seats: GithubCopilotSeat[] }> {
  const body = await ghFetch<{ total_seats: number; seats: GithubCopilotSeat[] }>(
    `/api/v3/orgs/${encodeURIComponent(org)}/copilot/billing/seats`,
    signal,
  );
  if (!Array.isArray(body.seats)) {
    throw new Error(`malformed response: missing "seats" array`);
  }
  return { totalSeats: body.total_seats, seats: body.seats };
}

export const addCopilotSeats = (org: string, usernames: string[]): Promise<{ seats_created: number }> =>
  ghPostJSON(`/api/v3/orgs/${encodeURIComponent(org)}/copilot/billing/selected_users`, {
    selected_usernames: usernames,
  });

export const cancelCopilotSeats = (org: string, usernames: string[]) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(org)}/copilot/billing/selected_users`, {
    selected_usernames: usernames,
  });

export type CopilotSpaceOwner =
  | string
  | { kind: "organization" | "user"; login: string };

const copilotSpacesBase = (owner: CopilotSpaceOwner) => {
  if (typeof owner === "string") {
    return `/api/v3/orgs/${encodeURIComponent(owner)}/copilot-spaces`;
  }
  const collection = owner.kind === "user" ? "users" : "orgs";
  return `/api/v3/${collection}/${encodeURIComponent(owner.login)}/copilot-spaces`;
};

export async function fetchCopilotSpaces(owner: CopilotSpaceOwner, signal?: AbortSignal): Promise<GithubCopilotSpace[]> {
  const body = await ghFetch<{ spaces: GithubCopilotSpace[] }>(copilotSpacesBase(owner), signal);
  if (!Array.isArray(body.spaces)) {
    throw new Error(`malformed response: missing "spaces" array`);
  }
  return body.spaces;
}

// ─── Copilot Spaces CRUD · enterprise team orgs · custom property values ─

import type {
  GithubCopilotSpaceCollaborator,
  GithubCopilotSpaceResource,
  GithubOrgSimple,
  GithubOrgRepoCustomPropertyValues,
} from "./types.js";

export const createCopilotSpace = (
  owner: CopilotSpaceOwner,
  payload: {
    name: string;
    description?: string;
    general_instructions?: string;
    base_role?: string;
  },
): Promise<GithubCopilotSpace> => ghPostJSON(copilotSpacesBase(owner), payload);

export const updateCopilotSpace = (
  owner: CopilotSpaceOwner,
  spaceNumber: number,
  payload: {
    name?: string;
    description?: string;
    general_instructions?: string;
    base_role?: string;
  },
): Promise<GithubCopilotSpace> =>
  ghPutJSON(`${copilotSpacesBase(owner)}/${spaceNumber}`, payload);

export const deleteCopilotSpace = (owner: CopilotSpaceOwner, spaceNumber: number) =>
  ghSend("DELETE", `${copilotSpacesBase(owner)}/${spaceNumber}`);

export async function fetchCopilotSpaceCollaborators(
  owner: CopilotSpaceOwner,
  spaceNumber: number,
  signal?: AbortSignal,
): Promise<GithubCopilotSpaceCollaborator[]> {
  const body = await ghFetch<{ collaborators: GithubCopilotSpaceCollaborator[] }>(
    `${copilotSpacesBase(owner)}/${spaceNumber}/collaborators`,
    signal,
  );
  if (!Array.isArray(body.collaborators)) {
    throw new Error(`malformed response: missing "collaborators" array`);
  }
  return body.collaborators;
}

export const addCopilotSpaceCollaborator = (
  owner: CopilotSpaceOwner,
  spaceNumber: number,
  payload: { actor_type: "User" | "Team"; actor_identifier: string; role: string },
): Promise<GithubCopilotSpaceCollaborator> =>
  ghPostJSON(`${copilotSpacesBase(owner)}/${spaceNumber}/collaborators`, payload);

/** Role "no_access" removes the collaborator grant (the endpoint's contract). */
export const updateCopilotSpaceCollaborator = (
  owner: CopilotSpaceOwner,
  spaceNumber: number,
  actorType: "User" | "Team",
  identifier: string,
  role: string,
) =>
  ghSend(
    "PUT",
    `${copilotSpacesBase(owner)}/${spaceNumber}/collaborators/${actorType}/${encodeURIComponent(identifier)}`,
    { role },
  );

export const removeCopilotSpaceCollaborator = (
  owner: CopilotSpaceOwner,
  spaceNumber: number,
  actorType: "User" | "Team",
  identifier: string,
) =>
  ghSend(
    "DELETE",
    `${copilotSpacesBase(owner)}/${spaceNumber}/collaborators/${actorType}/${encodeURIComponent(identifier)}`,
  );

export async function fetchCopilotSpaceResources(
  owner: CopilotSpaceOwner,
  spaceNumber: number,
  signal?: AbortSignal,
): Promise<GithubCopilotSpaceResource[]> {
  const body = await ghFetch<{ resources: GithubCopilotSpaceResource[] }>(
    `${copilotSpacesBase(owner)}/${spaceNumber}/resources`,
    signal,
  );
  if (!Array.isArray(body.resources)) {
    throw new Error(`malformed response: missing "resources" array`);
  }
  return body.resources;
}

export const addCopilotSpaceResource = (
  owner: CopilotSpaceOwner,
  spaceNumber: number,
  payload: { resource_type: string; metadata: Record<string, unknown> },
): Promise<GithubCopilotSpaceResource> =>
  ghPostJSON(`${copilotSpacesBase(owner)}/${spaceNumber}/resources`, payload);

export const updateCopilotSpaceResource = (
  owner: CopilotSpaceOwner,
  spaceNumber: number,
  resourceId: number,
  metadata: Record<string, unknown>,
): Promise<GithubCopilotSpaceResource> =>
  ghPutJSON(`${copilotSpacesBase(owner)}/${spaceNumber}/resources/${resourceId}`, { metadata });

export const removeCopilotSpaceResource = (
  owner: CopilotSpaceOwner,
  spaceNumber: number,
  resourceId: number,
) => ghSend("DELETE", `${copilotSpacesBase(owner)}/${spaceNumber}/resources/${resourceId}`);

export const fetchEnterpriseTeamOrgs = (slug: string) =>
  enterprisePath(`/teams/${encodeURIComponent(slug)}/organizations`).then((path) =>
    ghFetch<GithubOrgSimple[]>(path),
  );

export const assignEnterpriseTeamOrg = (slug: string, org: string): Promise<GithubOrgSimple> =>
  enterprisePath(`/teams/${encodeURIComponent(slug)}/organizations/${encodeURIComponent(org)}`).then(
    (path) => ghPutJSON(path, {}),
  );

export const unassignEnterpriseTeamOrg = (slug: string, org: string) =>
  enterprisePath(`/teams/${encodeURIComponent(slug)}/organizations/${encodeURIComponent(org)}`).then(
    (path) => ghSend("DELETE", path),
  );

export const fetchOrgRepoCustomPropertyValues = (
  org: string,
  repositoryQuery?: string,
): Promise<GithubOrgRepoCustomPropertyValues[]> => {
  const qs = repositoryQuery ? `?repository_query=${encodeURIComponent(repositoryQuery)}` : "";
  return ghFetch(`/api/v3/orgs/${encodeURIComponent(org)}/properties/values${qs}`);
};

/** A null value unsets the property on each named repository. */
export const setOrgRepoCustomPropertyValues = (
  org: string,
  repositoryNames: string[],
  properties: { property_name: string; value: unknown }[],
) =>
  ghSend("PATCH", `/api/v3/orgs/${encodeURIComponent(org)}/properties/values`, {
    repository_names: repositoryNames,
    properties,
  });
// ─── Deployments + webhook deliveries + Pages ───────────────────────────

import type {
  GithubDeployment,
  GithubDeploymentState,
  GithubDeploymentStatus,
  GithubEnvironmentDetail,
  GithubDeploymentBranchPolicy,
  GithubEnvCustomProtectionRule,
  GithubHookDelivery,
  GithubHookDeliveryDetail,
  GithubOrgWebhook,
  GithubPagesSite,
  GithubPagesBuild,
  GithubPagesHealth,
} from "./types.js";

/**
 * Full environment objects (protection rules + branch-policy config) from
 * the same envelope endpoint fetchEnvironments unwraps into slim rows.
 */
export const fetchEnvironmentsDetail = (owner: string, repo: string) =>
  ghFetch<{ environments: GithubEnvironmentDetail[] }>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/environments`,
  ).then((r) => {
    if (!Array.isArray(r.environments)) {
      throw new Error(`malformed response: missing "environments" array`);
    }
    return r.environments;
  });

export const fetchEnvBranchPolicies = (owner: string, repo: string, envName: string) =>
  ghFetch<{ total_count: number; branch_policies: GithubDeploymentBranchPolicy[] }>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/environments/${encodeURIComponent(envName)}/deployment-branch-policies`,
  ).then((r) => {
    if (!Array.isArray(r.branch_policies)) {
      throw new Error(`malformed response: missing "branch_policies" array`);
    }
    return r.branch_policies;
  });

export const fetchEnvProtectionRules = (owner: string, repo: string, envName: string) =>
  ghFetch<{
    total_count: number;
    custom_deployment_protection_rules: GithubEnvCustomProtectionRule[];
  }>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/environments/${encodeURIComponent(envName)}/deployment_protection_rules`,
  ).then((r) => {
    if (!Array.isArray(r.custom_deployment_protection_rules)) {
      throw new Error(
        `malformed response: missing "custom_deployment_protection_rules" array`,
      );
    }
    return r.custom_deployment_protection_rules;
  });

/** First page of a repo's deployments; follow pages via the Link header. */
export const fetchDeploymentsPage = (
  owner: string,
  repo: string,
  pageUrl?: string,
): Promise<Page<GithubDeployment>> =>
  ghFetchPage<GithubDeployment>(
    pageUrl ?? `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/deployments?per_page=30`,
  );

export const fetchDeploymentStatuses = (owner: string, repo: string, deploymentId: number) =>
  ghFetch<GithubDeploymentStatus[]>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/deployments/${deploymentId}/statuses`,
  );

export const createDeploymentStatus = (
  owner: string,
  repo: string,
  deploymentId: number,
  payload: {
    state: GithubDeploymentState;
    description?: string | undefined;
    environment?: string | undefined;
    environment_url?: string | undefined;
    log_url?: string | undefined;
  },
): Promise<GithubDeploymentStatus> =>
  ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/deployments/${deploymentId}/statuses`, payload);

/** Where a webhook lives: a repository or an organization. */
export type HookScope =
  | { kind: "repo"; owner: string; repo: string }
  | { kind: "org"; org: string };

function hookBasePath(scope: HookScope, hookId: number): string {
  return scope.kind === "repo"
    ? `/api/v3/repos/${encodeURIComponent(scope.owner)}/${encodeURIComponent(scope.repo)}/hooks/${hookId}`
    : `/api/v3/orgs/${encodeURIComponent(scope.org)}/hooks/${hookId}`;
}

/** First page of a hook's deliveries; follow pages via the Link header. */
export const fetchHookDeliveriesPage = (
  scope: HookScope,
  hookId: number,
  pageUrl?: string,
): Promise<Page<GithubHookDelivery>> =>
  ghFetchPage<GithubHookDelivery>(
    pageUrl ?? `${hookBasePath(scope, hookId)}/deliveries?per_page=30`,
  );

export const fetchHookDelivery = (
  scope: HookScope,
  hookId: number,
  deliveryId: number,
): Promise<GithubHookDeliveryDetail> =>
  ghFetch(`${hookBasePath(scope, hookId)}/deliveries/${deliveryId}`);

/** POST .../deliveries/{id}/attempts — the server answers 202, no body. */
export const redeliverHookDelivery = (scope: HookScope, hookId: number, deliveryId: number) =>
  ghSend("POST", `${hookBasePath(scope, hookId)}/deliveries/${deliveryId}/attempts`);

/** First page of an organization's webhooks; follow pages via Link header. */
export const fetchOrgHooksPage = (
  org: string,
  pageUrl?: string,
): Promise<Page<GithubOrgWebhook>> =>
  ghFetchPage<GithubOrgWebhook>(pageUrl ?? `/api/v3/orgs/${encodeURIComponent(org)}/hooks?per_page=30`);

/** Pages site, or null when Pages is not enabled on the repo (404). */
export async function fetchPagesSite(
  owner: string,
  repo: string,
  signal?: AbortSignal,
): Promise<GithubPagesSite | null> {
  try {
    return await ghFetch<GithubPagesSite>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pages`, signal);
  } catch (err) {
    if (isNotFound(err)) return null;
    throw err;
  }
}

export const createPagesSite = (
  owner: string,
  repo: string,
  payload: {
    source?: { branch: string; path?: string };
    cname?: string;
    build_type?: "legacy" | "workflow";
  },
): Promise<GithubPagesSite> => ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pages`, payload);

/** PUT /pages — the server answers 204, no body. */
export const updatePagesSite = (
  owner: string,
  repo: string,
  payload: {
    cname?: string | null;
    https_enforced?: boolean;
    build_type?: "legacy" | "workflow";
    public?: boolean;
    source?: { branch: string; path?: string };
  },
) => ghSend("PUT", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pages`, payload);

export const deletePagesSite = (owner: string, repo: string) =>
  ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pages`);

export const fetchPagesBuilds = (owner: string, repo: string) =>
  ghFetch<GithubPagesBuild[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pages/builds`);

export const requestPagesBuild = (
  owner: string,
  repo: string,
): Promise<{ status: string; url: string }> =>
  ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pages/builds`, {});

/**
 * Pages custom-domain health check. Unlike ghFetch this surfaces the
 * response body's message — the endpoint answers 400 with an explanation
 * ("There isn't a custom domain on this Pages site") the panel must show.
 */
export async function fetchPagesHealth(owner: string, repo: string, signal?: AbortSignal): Promise<GithubPagesHealth> {
  const res = await apiFetch(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pages/health`, {
    headers: authHeaders(),
    signal,
  });
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    let message = text || res.statusText;
    try {
      const body = JSON.parse(text) as { message?: string };
      if (body.message) message = body.message;
    } catch {
      // Non-JSON error body: keep the raw text.
    }
    throw new ApiError(res.status, message);
  }
  return res.json() as Promise<GithubPagesHealth>;
}

/** Status of one GitHub Pages deployment (GET /pages/deployments/{id}). */
export const fetchPagesDeploymentStatus = (
  owner: string,
  repo: string,
  deploymentId: number,
): Promise<{ status: string }> =>
  ghFetch(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pages/deployments/${deploymentId}`);
// ─── PR reviews, statuses, reactions & timeline ─────────────────────────

/** The token's own user — reaction toggles need to know "my" reactions. */
export const fetchAuthenticatedUser = () => ghFetch<GithubAccount>("/api/v3/user");

export const fetchPRReviews = (owner: string, repo: string, number: number) =>
  ghFetch<GithubPRReview[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/reviews`);

/** Create + submit a review in one call (event APPROVE/REQUEST_CHANGES/COMMENT). */
export const createPRReview = (
  owner: string,
  repo: string,
  number: number,
  payload: { body: string; event: "APPROVE" | "REQUEST_CHANGES" | "COMMENT" },
): Promise<GithubPRReview> =>
  ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/reviews`, payload);

export const dismissPRReview = (
  owner: string,
  repo: string,
  number: number,
  reviewId: number,
  message: string,
): Promise<GithubPRReview> =>
  ghPutJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/reviews/${reviewId}/dismissals`, {
    message,
  });

export const fetchPRReviewComments = (owner: string, repo: string, number: number) =>
  ghFetch<GithubPRReviewComment[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/comments`);

/** Add one immediate inline review comment to a changed-file line. */
export const createPRReviewComment = (
  owner: string,
  repo: string,
  number: number,
  payload: {
    body: string;
    commit_id: string;
    path: string;
    line: number;
    side: "LEFT" | "RIGHT";
  },
): Promise<GithubPRReviewComment> =>
  ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/comments`, payload);

export const replyToPRReviewComment = (
  owner: string,
  repo: string,
  number: number,
  inReplyTo: number,
  body: string,
): Promise<GithubPRReviewComment> =>
  ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/comments`, {
    body,
    in_reply_to: inReplyTo,
  });

export const fetchPRReviewThreads = async (
  owner: string,
  repo: string,
  number: number,
): Promise<GithubPRReviewThread[]> => {
  const data = await ghGraphQL<{
    repository: {
      pullRequest: {
        reviewThreads: {
          nodes: Array<{
            id: string;
            isResolved: boolean;
            comments: { nodes: Array<{ databaseId: number | null }> };
          }>;
        };
      } | null;
    } | null;
  }>(
    `query PullRequestReviewThreads($owner:String!,$repo:String!,$number:Int!){
      repository(owner:$owner,name:$repo){
        pullRequest(number:$number){
          reviewThreads(first:100){
            nodes{
              id
              isResolved
              comments(first:100){nodes{databaseId}}
            }
          }
        }
      }
    }`,
    { owner, repo, number },
  );
  return (
    data.repository?.pullRequest?.reviewThreads.nodes.map((thread) => ({
      id: thread.id,
      isResolved: thread.isResolved,
      comments: thread.comments.nodes
        .filter((comment): comment is { databaseId: number } => typeof comment.databaseId === "number")
        .map((comment) => ({ databaseId: comment.databaseId })),
    })) ?? []
  );
};

export const setPRReviewThreadResolved = (
  _owner: string,
  _repo: string,
  _number: number,
  threadId: string,
  resolved: boolean,
): Promise<GithubPRReviewThread> => {
  const mutation = resolved
    ? `mutation ResolveReviewThread($input:ResolveReviewThreadInput!){
        resolveReviewThread(input:$input){
          thread{id,isResolved,comments(first:100){nodes{databaseId}}}
        }
      }`
    : `mutation UnresolveReviewThread($input:UnresolveReviewThreadInput!){
        unresolveReviewThread(input:$input){
          thread{id,isResolved,comments(first:100){nodes{databaseId}}}
        }
      }`;
  return ghGraphQL<{
    resolveReviewThread?: {
      thread: {
        id: string;
        isResolved: boolean;
        comments: { nodes: Array<{ databaseId: number | null }> };
      };
    };
    unresolveReviewThread?: {
      thread: {
        id: string;
        isResolved: boolean;
        comments: { nodes: Array<{ databaseId: number | null }> };
      };
    };
  }>(mutation, { input: { threadId } }).then((data) => {
    const thread = (resolved ? data.resolveReviewThread : data.unresolveReviewThread)?.thread;
    if (!thread) throw new Error("graphql response missing review thread");
    return {
      id: thread.id,
      isResolved: thread.isResolved,
      comments: thread.comments.nodes
        .filter((comment): comment is { databaseId: number } => typeof comment.databaseId === "number")
        .map((comment) => ({ databaseId: comment.databaseId })),
    };
  });
};

export async function fetchPRRequestedReviewers(
  owner: string,
  repo: string,
  number: number,
  signal?: AbortSignal,
): Promise<GithubReviewRequest> {
  const body = await ghFetch<GithubReviewRequest>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/requested_reviewers`,
    signal,
  );
  // No `?? []`: a missing member is a contract break that must surface.
  if (!Array.isArray(body.users) || !Array.isArray(body.teams)) {
    throw new Error('malformed response: missing "users"/"teams" arrays');
  }
  return body;
}

export const requestPRReviewers = (
  owner: string,
  repo: string,
  number: number,
  logins: string[],
): Promise<GithubPR> =>
  ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/requested_reviewers`, {
    reviewers: logins,
  });

export const removePRRequestedReviewers = (
  owner: string,
  repo: string,
  number: number,
  logins: string[],
): Promise<GithubPR> =>
  ghDeleteJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/requested_reviewers`, {
    reviewers: logins,
  });

export async function fetchCombinedStatus(
  owner: string,
  repo: string,
  ref: string,
  signal?: AbortSignal,
): Promise<GithubCombinedStatus> {
  const body = await ghFetch<GithubCombinedStatus>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/commits/${ref}/status`,
    signal,
  );
  // No `?? []`: a missing member is a contract break that must surface.
  if (!Array.isArray(body.statuses)) {
    throw new Error('malformed response: missing "statuses" array');
  }
  return body;
}

export const fetchIssueTimeline = (owner: string, repo: string, number: number) =>
  ghFetch<GithubTimelineItem[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/timeline`);

export const fetchIssueReactions = (owner: string, repo: string, number: number) =>
  ghFetch<GithubReaction[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/reactions`);

export const addIssueReaction = (
  owner: string,
  repo: string,
  number: number,
  content: GithubReactionContent,
): Promise<GithubReaction> =>
  ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/reactions`, { content });

export const removeIssueReaction = (
  owner: string,
  repo: string,
  number: number,
  reactionId: number,
) => ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/${number}/reactions/${reactionId}`);

export const fetchIssueCommentReactions = (owner: string, repo: string, commentId: number) =>
  ghFetch<GithubReaction[]>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/comments/${commentId}/reactions`,
  );

export const addIssueCommentReaction = (
  owner: string,
  repo: string,
  commentId: number,
  content: GithubReactionContent,
): Promise<GithubReaction> =>
  ghPostJSON(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/comments/${commentId}/reactions`, {
    content,
  });

export const removeIssueCommentReaction = (
  owner: string,
  repo: string,
  commentId: number,
  reactionId: number,
) =>
  ghSend(
    "DELETE",
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues/comments/${commentId}/reactions/${reactionId}`,
  );
// ─── Search + repo social + account ─────────────────────────────────────

/** One page of a /search/* envelope ({total_count, incomplete_results, items}). */
export interface SearchResultPage<T> {
  totalCount: number;
  incompleteResults: boolean;
  items: T[];
}

export const SEARCH_PER_PAGE = 30;

async function ghSearch<T>(
  endpoint: string,
  q: string,
  page: number,
  extra?: Record<string, string>,
): Promise<SearchResultPage<T>> {
  const params = new URLSearchParams({ q, page: String(page), per_page: String(SEARCH_PER_PAGE) });
  for (const [k, v] of Object.entries(extra ?? {})) params.set(k, v);
  const body = await ghFetch<{ total_count: number; incomplete_results: boolean; items: T[] }>(
    `/api/v3/search/${endpoint}?${params}`,
  );
  // No `?? []`: a missing items array is a contract break, not "no results".
  if (!Array.isArray(body.items)) {
    throw new Error(`malformed response: missing "items" array`);
  }
  return {
    totalCount: body.total_count,
    incompleteResults: body.incomplete_results,
    items: body.items,
  };
}

export interface RepositorySearchOptions {
  sort?: "stars" | "forks" | "help-wanted-issues" | "updated" | undefined;
  order?: "asc" | "desc" | undefined;
}

export const searchRepositories = (q: string, page = 1, options: RepositorySearchOptions = {}) => {
  const extra: Record<string, string> = {};
  if (options.sort) extra.sort = options.sort;
  if (options.order) extra.order = options.order;
  return ghSearch<BleephubRepo>("repositories", q, page, extra);
};

export const searchCode = (q: string, page = 1) =>
  ghSearch<GithubSearchCodeItem>("code", q, page);

export const searchIssues = (q: string, page = 1) =>
  ghSearch<GithubSearchIssueItem>("issues", q, page);

export const searchUsers = (q: string, page = 1) =>
  ghSearch<GithubSearchUserItem>("users", q, page);

export const searchCommits = (q: string, page = 1) =>
  ghSearch<GithubSearchCommitItem>("commits", q, page);

/** Label search is scoped to one repository via the required repository_id. */
export const searchLabels = (q: string, repositoryId: number, page = 1) =>
  ghSearch<GithubSearchLabelItem>("labels", q, page, { repository_id: String(repositoryId) });

export const searchTopics = (q: string, page = 1) =>
  ghSearch<GithubSearchTopicItem>("topics", q, page);

export const fetchRepoCollaborators = (owner: string, repo: string) =>
  ghFetch<GithubCollaborator[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/collaborators`);

/**
 * PUT /collaborators/{username}: inviting a new user answers 201 with the
 * pending invitation; naming an existing collaborator updates the
 * permission in place and answers 204 (returned here as null).
 */
export async function inviteRepoCollaborator(
  owner: string,
  repo: string,
  username: string,
  permission: string,
): Promise<GithubRepoInvitation | null> {
  const res = await apiFetch(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/collaborators/${encodeURIComponent(username)}`,
    {
      method: "PUT",
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: JSON.stringify({ permission }),
    },
  );
  if (!res.ok) {
    handleUnauthorized(res);
    const text = await res.text();
    throw new ApiError(res.status, `${res.status} ${res.statusText}: ${text || res.statusText}`);
  }
  if (res.status === 204) return null;
  return res.json() as Promise<GithubRepoInvitation>;
}

export const removeRepoCollaborator = (owner: string, repo: string, username: string) =>
  ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/collaborators/${encodeURIComponent(username)}`);

export const fetchRepoInvitations = (owner: string, repo: string) =>
  ghFetch<GithubRepoInvitation[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/invitations`);

export const cancelRepoInvitation = (owner: string, repo: string, invitationId: number) =>
  ghSend("DELETE", `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/invitations/${invitationId}`);

export const fetchRepoStargazers = (owner: string, repo: string) =>
  ghFetch<GithubAccount[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/stargazers`);

/** First page of watchers; follow pages via the Link rel="next" URL. */
export const fetchRepoSubscribersPage = (owner: string, repo: string, pageUrl?: string) =>
  ghFetchPage<GithubAccount>(pageUrl ?? `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/subscribers?per_page=50`);

/** First page of forks; follow pages via the Link rel="next" URL. */
export const fetchRepoForksPage = (owner: string, repo: string, pageUrl?: string) =>
  ghFetchPage<BleephubRepo>(pageUrl ?? `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/forks?per_page=50`);

/** Language name → byte count, sorted by the server descending by bytes. */
export const fetchRepoLanguages = (owner: string, repo: string) =>
  ghFetch<Record<string, number>>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/languages`);

export const fetchRepoTags = (owner: string, repo: string) =>
  ghFetch<GithubTag[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/tags`);

/** Star/watch/fork counters from the full-repository shape. */
export const fetchRepoSocialCounts = (owner: string, repo: string, signal?: AbortSignal) =>
  ghFetch<GithubRepoSocialCounts>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`, signal);

export const fetchRepoViewerState = (owner: string, repo: string, signal?: AbortSignal) =>
  ghFetch<GithubRepoViewerState>(`/ui-data/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/viewer`, signal);

export const starRepo = (owner: string, repo: string) =>
  ghSend("PUT", `/api/v3/user/starred/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`);

export const unstarRepo = (owner: string, repo: string) =>
  ghSend("DELETE", `/api/v3/user/starred/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`);

export const setRepoSubscription = (owner: string, repo: string, subscribed: boolean) =>
  ghPutJSON<GithubRepoSubscription>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/subscription`, {
    subscribed,
    ignored: false,
  });

export const forkRepo = (owner: string, repo: string, organization?: string) =>
  ghPostJSON<BleephubRepo>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/forks`, organization ? { organization } : {});

export const fetchUserSSHKeys = () => ghFetch<GithubSSHKey[]>("/api/v3/user/keys");

export const createUserSSHKey = (title: string, key: string) =>
  ghPostJSON<GithubSSHKey>("/api/v3/user/keys", { title, key });

export const deleteUserSSHKey = (keyId: number) =>
  ghSend("DELETE", `/api/v3/user/keys/${keyId}`);

export const fetchUserGPGKeys = () => ghFetch<GithubGPGKey[]>("/api/v3/user/gpg_keys");

export const createUserGPGKey = (armoredPublicKey: string, name?: string) =>
  ghPostJSON<GithubGPGKey>("/api/v3/user/gpg_keys", {
    armored_public_key: armoredPublicKey,
    ...(name ? { name } : {}),
  });

export const deleteUserGPGKey = (gpgKeyId: number) =>
  ghSend("DELETE", `/api/v3/user/gpg_keys/${gpgKeyId}`);

export const fetchUserSSHSigningKeys = () =>
  ghFetch<GithubSSHSigningKey[]>("/api/v3/user/ssh_signing_keys");

export const createUserSSHSigningKey = (title: string, key: string) =>
  ghPostJSON<GithubSSHSigningKey>("/api/v3/user/ssh_signing_keys", { title, key });

export const deleteUserSSHSigningKey = (sshSigningKeyId: number) =>
  ghSend("DELETE", `/api/v3/user/ssh_signing_keys/${sshSigningKeyId}`);

export const fetchBlockedUsers = () => ghFetch<GithubBlockedUser[]>("/api/v3/user/blocks");

export const blockUser = (username: string) =>
  ghSend("PUT", `/api/v3/user/blocks/${encodeURIComponent(username)}`);

export const unblockUser = (username: string) =>
  ghSend("DELETE", `/api/v3/user/blocks/${encodeURIComponent(username)}`);

export const fetchUserEmails = () => ghFetch<GithubUserEmail[]>("/api/v3/user/emails");

export const addUserEmails = (emails: string[]) =>
  ghPostJSON<GithubUserEmail[]>("/api/v3/user/emails", { emails });

export const deleteUserEmails = (emails: string[]) =>
  ghSend("DELETE", "/api/v3/user/emails", { emails });

/** Sets the primary email's visibility; returns the updated email rows. */
export const setUserEmailVisibility = (visibility: "public" | "private") =>
  ghPatchJSON<GithubUserEmail[]>("/api/v3/user/email/visibility", { visibility });

// ─── Bleephub dashboard, user profile, and organization pages ───────────

/** Public-user profile for GET /users/{login} (the /ui/{login} page). */
export const fetchUserProfile = (login: string) =>
  ghFetch<GithubUserProfile>(`/api/v3/users/${encodeURIComponent(login)}`);

/** First page of a named user's repos; follow pages via the Link header. */
export const fetchUserReposByLoginPage = (
  login: string,
  filters: RepoListFilters = {},
  pageUrl?: string,
): Promise<Page<BleephubRepo>> =>
  ghFetchPage<BleephubRepo>(
    buildRepoListURL(`/api/v3/users/${encodeURIComponent(login)}/repos`, filters, 30, pageUrl),
  );

/** Organizations a named user belongs to (GET /users/{login}/orgs). */
export const fetchUserOrgsByLogin = (login: string) =>
  ghFetch<GithubOrgSummary[]>(`/api/v3/users/${encodeURIComponent(login)}/orgs`);

/** Organization-full profile for the org Overview tab (GET /orgs/{org}). */
export const fetchOrgProfile = (org: string) =>
  ghFetch<GithubOrgProfile>(`/api/v3/orgs/${encodeURIComponent(org)}`);

/** Members visible to the caller (GET /orgs/{org}/members). */
export const fetchOrgMembers = (org: string) =>
  ghFetch<GithubAccount[]>(`/api/v3/orgs/${encodeURIComponent(org)}/members`);

/** Add or invite a member (or change their role) — PUT orgs/{org}/memberships/{user}. */
export const setOrgMembership = (org: string, username: string, role: "member" | "admin") =>
  ghPutJSON<{ state: string; role: string }>(
    `/api/v3/orgs/${encodeURIComponent(org)}/memberships/${encodeURIComponent(username)}`,
    { role },
  );

/** Remove a member from the org — DELETE orgs/{org}/memberships/{user}. */
export const removeOrgMember = (org: string, username: string) =>
  ghSend("DELETE", `/api/v3/orgs/${encodeURIComponent(org)}/memberships/${encodeURIComponent(username)}`);

/** Whether the authenticated user follows `login` (204 = yes, 404 = no). */
export const checkFollowing = async (login: string): Promise<boolean> => {
  const res = await apiFetch(`/api/v3/user/following/${encodeURIComponent(login)}`, {
    headers: authHeaders(),
  });
  if (res.status === 204) return true;
  if (res.status === 404) return false;
  handleUnauthorized(res);
  return false;
};

/** Follow a user — PUT user/following/{login}. */
export const followUser = (login: string) =>
  ghSend("PUT", `/api/v3/user/following/${encodeURIComponent(login)}`);

/** Unfollow a user — DELETE user/following/{login}. */
export const unfollowUser = (login: string) =>
  ghSend("DELETE", `/api/v3/user/following/${encodeURIComponent(login)}`);

/** Teams in an organization (GET /orgs/{org}/teams). */
export const fetchOrgTeams = (org: string) =>
  ghFetch<GithubOrgTeam[]>(`/api/v3/orgs/${encodeURIComponent(org)}/teams`);

/**
 * The authenticated user's cross-repo issue feed (GET /issues) — issues
 * they created, are assigned to, or are involved in, most-recently
 * updated first. Powers the dashboard's activity feed.
 */
export const fetchDashboardIssues = (signal?: AbortSignal) =>
  ghFetch<GithubFeedIssue[]>(
    "/api/v3/issues?filter=all&state=all&sort=updated&per_page=15",
    signal,
  );

// ─── GitHub Marketplace browser workflow ──────────────────────────────

export const fetchMarketplaceListings = () =>
  ghFetch<GithubMarketplaceListing[]>("/ui-data/marketplace/listings");

export const fetchMarketplaceListing = (slug: string) =>
  ghFetch<GithubMarketplaceListing>(`/ui-data/marketplace/listings/${encodeURIComponent(slug)}`);

export const fetchMarketplaceAccounts = () =>
  ghFetch<GithubMarketplaceAccount[]>("/ui-data/marketplace/accounts");

export const fetchMarketplaceSubscriptions = () =>
  ghFetch<GithubMarketplaceSubscription[]>("/ui-data/marketplace/subscriptions");

export interface MarketplacePurchasePayload {
  account: string;
  plan_id: number;
  billing_cycle: "monthly" | "yearly";
  unit_count: number;
  free_trial: boolean;
}

export const purchaseMarketplacePlan = (slug: string, payload: MarketplacePurchasePayload) =>
  ghPostJSON<GithubMarketplaceSubscription>(
    `/ui-data/marketplace/listings/${encodeURIComponent(slug)}/purchase`,
    payload,
  );

export const changeMarketplacePlan = (
  slug: string,
  payload: Omit<MarketplacePurchasePayload, "free_trial">,
) => ghPatchJSON<GithubMarketplaceSubscription>(
  `/ui-data/marketplace/listings/${encodeURIComponent(slug)}/subscription`,
  payload,
);

export const cancelMarketplacePlan = (slug: string, account: string) =>
  ghDeleteJSON<GithubMarketplaceSubscription | undefined>(
    `/ui-data/marketplace/listings/${encodeURIComponent(slug)}/subscription?account=${encodeURIComponent(account)}`,
    undefined,
  );

export interface MarketplaceListingSettingsPayload {
  name: string;
  description: string;
  full_description: string;
  setup_url: string;
  installation_url: string;
  webhook_url: string;
  webhook_secret: string;
  webhook_content_type: "json" | "form";
  webhook_active: boolean;
  published: boolean;
}

export const fetchMarketplaceListingSettings = (publisher: string) =>
  ghFetch<{ listing: GithubMarketplaceListingSettings | null }>(`/ui-data/settings/apps/${encodeURIComponent(publisher)}/marketplace`)
    .then(({ listing }) => listing);

export const saveMarketplaceListingSettings = (publisher: string, payload: MarketplaceListingSettingsPayload) =>
  ghPutJSON<GithubMarketplaceListingSettings>(`/settings/apps/${encodeURIComponent(publisher)}/marketplace`, payload);

export const createMarketplacePlanSettings = (
  publisher: string,
  payload: {
    name: string;
    description: string;
    monthly_price_in_cents: number;
    yearly_price_in_cents: number;
    price_model: "FREE" | "FLAT_RATE" | "PER_UNIT";
    has_free_trial: boolean;
    unit_name: string;
    state: "draft" | "published";
    bullets: string[];
  },
) => ghPostJSON<GithubMarketplacePlan>(`/settings/apps/${encodeURIComponent(publisher)}/marketplace/plans`, payload);
// ─── WP-C: Issues & Pull Requests GitHub-faithful layout ────────────────

/**
 * First page of a repo's issues by state; follow-up pages by the Link
 * rel="next" URL. Label/author/assignee/milestone + sort are applied
 * client-side over the loaded set so picking a facet narrows instantly.
 */
export const fetchRepoIssuesFilteredPage = (
  owner: string,
  repo: string,
  opts: { state?: string },
  pageUrl?: string,
  signal?: AbortSignal,
) => {
  if (pageUrl) return ghFetchPage<GithubIssue>(pageUrl, signal);
  const params = new URLSearchParams({ state: opts.state ?? "open", per_page: "50" });
  return ghFetchPage<GithubIssue>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues?${params}`, signal);
};

/**
 * First page of a repo's pull requests by state; follow-up pages by the Link
 * rel="next" URL. Label/author/sort are applied client-side over the loaded set
 * (the pulls list endpoint honors state/head/base server-side).
 */
export const fetchRepoPRsFilteredPage = (
  owner: string,
  repo: string,
  opts: { state?: string },
  pageUrl?: string,
  signal?: AbortSignal,
) => {
  if (pageUrl) return ghFetchPage<GithubPR>(pageUrl, signal);
  const params = new URLSearchParams({ state: opts.state ?? "open", per_page: "50" });
  return ghFetchPage<GithubPR>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls?${params}`, signal);
};

/** The changed-file list + per-file diff for a pull request. */
export const fetchPRFiles = (owner: string, repo: string, number: number) =>
  ghFetch<GithubPRFile[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/files`);

/** The PR's commits, oldest first. */
export const fetchPRCommits = (owner: string, repo: string, number: number) =>
  ghFetch<GithubCommit[]>(`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/commits`);
