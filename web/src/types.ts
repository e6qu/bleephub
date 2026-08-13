import type { components } from "../../third_party/github-openapi.js";

// Enum unions mirror the exact strings the server emits (bleephub Go:
// workflows.go / store_workflow_files.go). Empty result = still in flight.
// Keeping these as unions makes a typo'd comparison (e.g. "failed" vs the
// real "failure") a compile error rather than a silently-dead branch.
// Only values the server actually ASSIGNS belong here — a workflow is never
// "queued"/"skipped", a workflow file is never anything but "active".
// "waiting" = held on a reviewer-protected environment approval.
/**
 * Like Partial<T> but each optional member also explicitly admits `undefined`,
 * for exactOptionalPropertyTypes: callers build patch payloads with
 * `field: value || undefined`, which is a present-but-undefined property.
 */
export type Undef<T> = { [K in keyof T]?: T[K] | undefined };

export type WorkflowStatus =
  | "queued"
  | "in_progress"
  | "running"
  | "completed"
  | "pending_concurrency"
  | "waiting";
export type JobStatus =
  | "pending"
  | "queued"
  | "running"
  | "completed"
  | "skipped"
  | "waiting";
export type JobResult =
  | "success"
  | "failure"
  | "cancelled"
  | "skipped"
  | "neutral"
  | "timed_out"
  | "action_required";
export type WorkflowResult = "" | JobResult;
export type WorkflowFileState = "active" | "disabled_manually" | "disabled_inactivity";
export type WorkflowFileSource = "submitted" | "discovered";

/**
 * Workflow represents a running multi-job workflow, as projected by the
 * management API's workflowView (handle_mgmt.go) — NOT the full Go
 * Workflow struct. Fields the view never emits (runNumber, ref, sha,
 * concurrencyGroup, …) are deliberately absent.
 */
export interface BleephubWorkflow {
  id: string;
  name: string;
  runId: number;
  jobs: Record<string, BleephubWorkflowJob>;
  status: WorkflowStatus;
  result: WorkflowResult;
  createdAt: string;
  eventName?: string;
  repoFullName?: string;
}

/** WorkflowJob represents a single job within a workflow. */
export interface BleephubWorkflowJob {
  key: string;
  jobId: string;
  displayName: string;
  needs?: string[];
  status: JobStatus;
  result: WorkflowResult;
  outputs?: Record<string, string>;
  matrix?: Record<string, unknown>;
  continueOnError?: boolean;
  /**
   * startedAt / completedAt are Go time.Time fields, always serialized —
   * a job that hasn't started/finished carries the zero-time sentinel
   * "0001-01-01T00:00:00Z" rather than omitting the field.
   */
  startedAt: string;
  completedAt: string;
  matrixGroup?: string;
}

/** Filters the repo list endpoints support server-side. */
export interface RepoListFilters {
  type?: string | undefined;
  visibility?: "public" | "private" | "internal" | undefined;
  sort?: "created" | "updated" | "pushed" | "full_name" | undefined;
  direction?: "asc" | "desc" | undefined;
}

/** Repo represents a GitHub repository. */
export interface BleephubRepo {
  id: number;
  node_id: string;
  name: string;
  full_name: string;
  description: string;
  homepage: string | null;
  default_branch: string;
  visibility: string;
  private: boolean;
  fork?: boolean;
  language?: string | null;
  stargazers_count?: number;
  forks_count?: number;
  created_at: string;
  updated_at: string;
  pushed_at: string | null;
  ssh_url?: string;
  size: number;
  owner: { login: string; type: string; avatar_url?: string };
  organization?: { login: string; type: string; avatar_url?: string };
  license: { key: string; name: string; spdx_id: string; url: string; node_id: string } | null;
  has_issues: boolean;
  has_projects: boolean;
  has_wiki: boolean;
  has_pull_requests: boolean;
  is_template: boolean;
  archived: boolean;
  web_commit_signoff_required: boolean;
  allow_squash_merge: boolean;
  allow_merge_commit: boolean;
  allow_rebase_merge: boolean;
  allow_auto_merge: boolean;
  allow_update_branch: boolean;
  delete_branch_on_merge: boolean;
  use_squash_pr_title_as_default: boolean;
  squash_merge_commit_title: string;
  squash_merge_commit_message: string;
  merge_commit_title: string;
  merge_commit_message: string;
  pull_request_creation_policy: string;
  topics?: string[];
}

/**
 * Dashboard metrics, as reported by the server's own counters.
 *
 * `workflow_submissions` is the server's submission counter; it is not a count
 * of stored workflow runs, and the server exposes no such total, so nothing
 * here may be labelled "workflow runs".
 */
export interface BleephubMetrics {
  workflow_submissions: number;
  job_dispatches: number;
  jobs_by_status: Record<string, number>;
  job_completions: Record<string, number>;
  active_workflows: number;
  connected_runners: number;
  uptime_seconds: number;
  goroutines: number;
  heap_alloc_mb: number;
  job_duration_p50_seconds: number;
  job_duration_p95_seconds: number;
  job_duration_p99_seconds: number;
}

/** Runtime status reported by the server. */
export interface BleephubStatus {
  active_workflows: number;
  jobs_by_status: Record<string, number>;
  connected_runners: number;
}

/** Health response from /health. */
export interface BleephubHealth {
  status: string;
  service: string;
  enterprise_slug: string;
  version: string;
  commit: string;
  published_at: string;
}

/** WorkflowFile is the file-level workflow YAML entity. */
export interface BleephubWorkflowFile {
  id: number;
  name: string;
  path: string;
  state: WorkflowFileState;
  repoFullName: string;
  source?: WorkflowFileSource;
  createdAt: string;
  updatedAt: string;
}

/** Body for POST /api/v3/repos/{o}/{r}/actions/workflows/{id}/dispatches. */
export interface BleephubDispatchRequest {
  ref?: string;
  inputs?: Record<string, string>;
}

/** GitHub App row from the settings-owned browser surface. */
export interface BleephubApp {
  id: number;
  slug: string;
  name: string;
  clientId: string;
  description: string;
  url: string;
  callbackUrl: string;
  webhookUrl: string;
  webhookActive: boolean;
  webhookContentType: "json" | "form";
  permissions: Record<string, string>;
  events: string[];
  ownerId: number;
  createdAt: string;
  updatedAt: string;
}

/** GitHub App installation row normalized from GitHub's REST installation shape. */
export interface BleephubInstallation {
  id: number;
  appId: number;
  appSlug: string;
  targetType: string;
  targetLogin: string;
  repositorySelection: string;
  permissions: Record<string, string>;
  events: string[];
  createdAt: string;
  /** Always present on the wire; null when the installation is active. */
  suspendedAt: string | null;
}

/** OAuth App row from the settings-owned browser surface, distinct from GitHub App. */
export interface BleephubOAuthApp {
  clientId: string;
  name: string;
  description: string;
  url: string;
  callbackUrl: string;
  ownerId: number;
  createdAt: string;
}

export interface BleephubOAuthGrant {
  client_id: string;
  name: string;
  type: "OAuthApp" | "GitHubApp";
  url: string;
  scopes: string[];
  created_at: string;
}

export interface WireGitHubApp {
  id: number;
  slug: string;
  name: string;
  description: string;
  owner: { id: number };
  client_id: string;
  external_url: string;
  callback_url?: string;
  webhook_url?: string;
  webhook_active?: boolean;
  webhook_content_type?: "json" | "form";
  permissions: Record<string, string> | null;
  events: string[] | null;
  created_at: string;
  updated_at: string;
}

export interface WireInstallation {
  id: number;
  app_id: number;
  app_slug: string;
  target_type: string;
  repository_selection: string;
  permissions: Record<string, string> | null;
  events: string[] | null;
  created_at: string;
  suspended_at: string | null;
  account: { login: string };
}

// Wire shapes: the snake_case JSON the `/api/v3/bleephub/*` endpoints emit
// (server: oauthAppToJSON / appToJSON). Typing the raw response lets the
// snake→camel normalizers in api.ts drop their `as` casts, so a renamed or
// missing server field becomes a compile error at the mapping site.
export interface WireOAuthApp {
  client_id: string;
  name: string;
  description: string;
  url: string;
  callback_url: string;
  owner_id: number;
  created_at: string;
  updated_at: string;
}

/** The secret-bearing fields the GitHub-App create endpoint returns once. */
export interface WireAppCreated {
  client_id: string;
  pem: string;
  client_secret: string;
  webhook_secret: string;
}

/** GitHub REST issue/PR state. */
export type GithubState = "open" | "closed";

/** GitHub Issue. */
// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubIssue = components["schemas"]["issue"];

/** GitHub Pull Request. */
export interface GithubPR {
  id: number;
  node_id: string;
  number: number;
  title: string;
  body: string;
  state: GithubState;
  draft: boolean;
  /** null when the authoring user no longer resolves (GitHub parity). */
  user: { login: string; avatar_url: string } | null;
  head: { ref: string; sha: string };
  base: { ref: string; sha: string };
  labels: { name: string; color: string }[];
  created_at: string;
  updated_at: string;
  merged_at: string | null;
  merged: boolean;
  /** Only present on the single-PR detail response, not list items. */
  mergeable_state?: "clean" | "dirty" | "blocked" | "unstable" | "unknown";
}

/** GitHub comment. */
export interface GithubComment {
  id: number;
  /** null when the authoring user no longer resolves (GitHub parity). */
  user: { login: string; avatar_url: string } | null;
  body: string;
  created_at: string;
  updated_at: string;
}

/** Git commit. */
// Generated from the vendored GitHub OpenAPI description (WEB-013). Spec marks
// commit.author nullable; consumers optional-chain it.
export type GithubCommit = components["schemas"]["commit"];

export interface GithubComparison {
  url: string;
  html_url: string;
  status: "ahead" | "behind" | "diverged" | "identical";
  ahead_by: number;
  behind_by: number;
  total_commits: number;
  commits: GithubCommit[];
  files?: NonNullable<GithubCommit["files"]>;
}

/** Git branch. */
export interface GithubBranch {
  name: string;
  commit: { sha: string };
  protected?: boolean;
  protection_url?: string;
}

export interface GithubStatusCheck {
  context: string;
  app_id: number | null;
}

export interface GithubBranchProtectionStatusChecks {
  strict?: boolean;
  enforcement_level: string;
  contexts: string[];
  checks: GithubStatusCheck[];
  include_admins?: boolean;
}

export interface GithubBranchProtectionReviewDismissalRestrictions {
  users: GithubActor[];
  teams: GithubTeamRef[];
  apps?: GithubActor[];
  url?: string;
  users_url?: string;
  teams_url?: string;
}

export interface GithubActor {
  login: string;
  id: number;
  node_id: string;
  avatar_url: string;
  html_url: string;
  type: string;
  site_admin: boolean;
}

export interface GithubTeamRef {
  id: number;
  node_id: string;
  url: string;
  html_url: string;
  name: string;
  slug: string;
  description: string | null;
  privacy: string;
  permission: string;
}

export interface GithubBranchProtectionReviews {
  url?: string;
  dismissal_restrictions?: GithubBranchProtectionReviewDismissalRestrictions;
  dismiss_stale_reviews: boolean;
  require_code_owner_reviews: boolean;
  required_approving_review_count: number;
  bypass_pull_request_allowances?: GithubBranchProtectionBypassAllowances;
  require_last_push_approval?: boolean;
  required_review_thread_resolution?: boolean;
}

export interface GithubBranchProtectionBypassAllowances {
  users: GithubActor[];
  teams: GithubTeamRef[];
  apps?: GithubActor[];
}

export interface GithubBranchProtectionRestrictions {
  url?: string;
  users_url?: string;
  teams_url?: string;
  apps_url?: string;
  users: GithubActor[];
  teams: GithubTeamRef[];
  apps?: GithubActor[];
}

export interface GithubProtectionToggle {
  enabled: boolean;
  url?: string;
  html_url?: string;
}

/** Branch protection configuration from /api/v3/repos/{o}/{r}/branches/{b}/protection */
export interface GithubBranchProtection {
  url: string;
  html_url: string;
  required_status_checks: GithubBranchProtectionStatusChecks | null;
  required_pull_request_reviews: GithubBranchProtectionReviews | null;
  restrictions: GithubBranchProtectionRestrictions | null;
  enforce_admins: { url?: string; enabled: boolean } | null;
  allow_force_pushes: GithubProtectionToggle;
  allow_deletions: GithubProtectionToggle;
  required_conversation_resolution?: GithubProtectionToggle;
  required_linear_history?: GithubProtectionToggle;
  required_signatures?: GithubProtectionToggle;
  lock_branch?: GithubProtectionToggle;
  block_creations?: GithubProtectionToggle;
}

export interface GithubWebhook {
  id: number;
  name: string;
  active: boolean;
  events: string[];
  config: { url: string; content_type: string };
  created_at: string;
  updated_at: string;
  url: string;
  deliveries_url: string;
  last_response: { code: number | null; status: string; message: string | null };
}

export interface GithubSecret {
  name: string;
  created_at: string;
  updated_at: string;
  /** Org-scope secrets only (all | private | selected). */
  visibility?: GithubOrgVisibility;
}

export interface GithubEnvironment {
  id: number;
  name: string;
  node_id: string;
  url: string;
}

/** An Actions variable — GET .../actions/variables and env variables. */
export interface GithubActionsVariable {
  name: string;
  value: string;
  created_at: string;
  updated_at: string;
}

/** GET/PUT .../actions/permissions. */
export interface GithubActionsPermissions {
  enabled: boolean;
  allowed_actions?: "all" | "local_only" | "selected";
}

/** GET/PUT .../actions/permissions/workflow — default GITHUB_TOKEN scope. */
export interface GithubWorkflowPermissions {
  default_workflow_permissions: "read" | "write";
  can_approve_pull_request_reviews: boolean;
}

// ─── GitHub Actions REST shapes (/api/v3/repos/{o}/{r}/actions/*) ───────

/** GitHub workflow-run status. */
export type GHRunStatus = "queued" | "in_progress" | "completed" | "waiting";
/** GitHub run/job/step conclusion (null while in flight). */
export type GHConclusion =
  | "success"
  | "failure"
  | "cancelled"
  | "skipped"
  | "neutral"
  | "timed_out"
  | "action_required";

/** Workflow run — GET .../actions/runs (items) + .../actions/runs/{id}. */
export interface GithubWorkflowRun {
  id: number;
  name: string;
  run_number: number;
  run_attempt: number;
  event: string;
  status: GHRunStatus;
  conclusion: GHConclusion | null;
  head_branch: string;
  head_sha: string;
  path: string;
  workflow_id: number;
  created_at: string;
  updated_at: string;
  /** null when the server can't attribute the run to a user. */
  actor: { login: string } | null;
}

/** Workflow file — GET .../actions/workflows (items). */
export interface GithubWorkflow {
  id: number;
  name: string;
  path: string;
  state: "active" | "disabled_manually" | "disabled_inactivity";
  created_at: string;
  updated_at: string;
  badge_url: string;
}

/** Per-step entry inside a job. */
export interface GithubJobStep {
  name: string;
  status: GHRunStatus;
  conclusion: GHConclusion | null;
  number: number;
  started_at: string | null;
  completed_at: string | null;
}

/** Job — GET .../actions/runs/{run_id}/jobs (items). */
export interface GithubJob {
  id: number;
  run_id: number;
  name: string;
  status: GHRunStatus;
  conclusion: GHConclusion | null;
  started_at: string | null;
  completed_at: string | null;
  steps: GithubJobStep[];
  labels: string[];
  run_attempt: number;
}

/** Artifact — GET .../actions/runs/{run_id}/artifacts (items). */
export interface GithubArtifact {
  id: number;
  name: string;
  size_in_bytes: number;
  expired: boolean;
  created_at: string;
  workflow_run?: { id: number; head_branch: string; head_sha: string };
}

export interface GithubActionsCache {
  id: number;
  ref: string;
  key: string;
  version: string;
  last_accessed_at: string;
  created_at: string;
  size_in_bytes: number;
}

export interface GithubActionsCacheUsage {
  full_name: string;
  active_caches_size_in_bytes: number;
  active_caches_count: number;
}

/** Pending deployment — GET .../actions/runs/{run_id}/pending_deployments. */
// Generated from the vendored GitHub OpenAPI description (WEB-013). The spec
// marks environment.id optional, so consumers filter it before building
// environment_ids (see DeploymentsPage/RunDetailPage).
export type GithubPendingDeployment = components["schemas"]["pending-deployment"];

/** Check run — GET .../commits/{sha}/check-runs (items). */
export interface GithubCheckRun {
  id: number;
  name: string;
  status: GHRunStatus;
  conclusion: GHConclusion | null;
  started_at: string | null;
  completed_at: string | null;
  details_url: string;
  html_url?: string;
  app: { id: number } | null;
}

/** Actions secrets public key — GET {scope}/secrets/public-key. */
// Generated from the vendored GitHub OpenAPI description (WEB-013). `key` is a
// base64-encoded 32-byte X25519 public key for sealed-box encryption.
export type GithubPublicKey = components["schemas"]["actions-public-key"];

export type GithubOrgVisibility = "all" | "private" | "selected";

/** Actions variable — GET {scope}/variables (items). */
export interface GithubVariable {
  name: string;
  value: string;
  created_at: string;
  updated_at: string;
  /** Org-scope variables only. */
  visibility?: GithubOrgVisibility;
}

/** Self-hosted runner — GET .../actions/runners (items). */
export interface GithubRunner {
  id: number;
  name: string;
  os: string;
  status: "online" | "offline";
  busy: boolean;
  labels: { id: number; name: string; type: string }[];
}

/** Content-file response — GET .../contents/{path} (file variant). */
// Generated from the vendored GitHub OpenAPI description (WEB-013). The full
// spec shape is a superset of the six fields the UI reads; adopting the
// generated schema replaces a hand-written cast with the documented contract.
export type GithubContentFile = components["schemas"]["content-file"];

/** Content directory entry — GET .../contents/{path} (dir variant). */
export interface GithubContentItem {
  name: string;
  path: string;
  sha: string;
  type: "file" | "dir" | "symlink" | "submodule";
  size?: number;
  target?: string;
  submodule_git_url?: string;
  download_url?: string | null;
}

/** `on.workflow_dispatch.inputs.<name>` entry parsed from workflow YAML. */
export interface WorkflowDispatchInput {
  description?: string;
  required?: boolean;
  default?: string | boolean;
  type?: "string" | "choice" | "boolean" | "environment" | "number";
  options?: string[];
}

export interface GithubRelease {
  id: number;
  node_id: string;
  tag_name: string;
  target_commitish: string;
  name: string;
  body: string;
  draft: boolean;
  prerelease: boolean;
  created_at: string;
  /** null until the release is published (drafts). */
  published_at: string | null;
  html_url: string;
  url: string;
  assets_url: string;
  upload_url: string;
  assets: GithubReleaseAsset[];
}

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubReleaseAsset = components["schemas"]["release-asset"];

export type GithubMigrationState = "pending" | "exporting" | "exported" | "failed";

/** Request body for POST /user/migrations and /orgs/{org}/migrations. */
export interface GithubMigrationStartPayload {
  repositories: string[];
  lock_repositories?: boolean;
  exclude_metadata?: boolean;
  exclude_git_data?: boolean;
  exclude_attachments?: boolean;
  exclude_releases?: boolean;
  exclude_owner_projects?: boolean;
  org_metadata_only?: boolean;
}

/** GitHub migration export object (Migrations REST API). */
export interface GithubMigration {
  id: number;
  node_id: string;
  guid: string;
  state: GithubMigrationState;
  repositories: BleephubRepo[];
  lock_repositories: boolean;
  exclude_metadata: boolean;
  exclude_git_data: boolean;
  exclude_attachments: boolean;
  exclude_releases: boolean;
  exclude_owner_projects: boolean;
  org_metadata_only: boolean;
  url: string;
  html_url: string;
  archive_url: string;
  created_at: string;
  updated_at: string;
}

export interface BleephubUser {
  id: number;
  login: string;
  type: "User" | "Bot" | "Organization";
  site_admin: boolean;
  created_at: string;
  avatar_url?: string;
}

export interface BleephubOrg {
  id: number;
  login: string;
  name: string;
  description: string;
  billing_email?: string;
  members_can_create_repositories: boolean;
  created_at: string;
  avatar_url?: string;
}

export interface BleephubTeam {
  id: number;
  slug: string;
  name: string;
  description: string;
  privacy: "secret" | "closed";
  organization?: { id: number; login: string };
  created_at: string;
}

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubTeamMember = components["schemas"]["team-member"];

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubTeamMembership = components["schemas"]["team-membership"];

export interface GithubTeamRepo {
  id: number;
  full_name: string;
  name: string;
  owner: { login: string; type: string };
  permissions?: Record<string, boolean>;
  role_name?: string;
}

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubDeployKey = components["schemas"]["deploy-key"];
export type GithubAutolink = components["schemas"]["autolink"];
export type GithubCommitComment = components["schemas"]["commit-comment"];

export interface GithubProjectV2 {
  id: number;
  number: number;
  title: string;
  short_description: string | null;
}

export interface GithubProjectV2FieldOption {
  id: string;
  name: { raw: string; html: string };
}

export interface GithubProjectV2Field {
  id: number;
  name: string;
  data_type: string;
  options?: GithubProjectV2FieldOption[];
}

export interface GithubProjectV2ItemFieldValue {
  id: number;
  name: string;
  data_type: string;
  value: unknown;
}

export interface GithubProjectV2Item {
  id: number;
  content_type: "Issue" | "PullRequest" | "DraftIssue";
  content: { title?: string; number?: number; html_url?: string } | null;
  fields?: GithubProjectV2ItemFieldValue[];
}

/**
 * A single entry from GET /users/{login}/events. The simulator derives a small
 * set of event types (CreateEvent, DeleteEvent, PushEvent, IssuesEvent,
 * IssueCommentEvent, PullRequestEvent); the profile Overview buckets these by
 * day for the contribution graph and lists them as recent activity.
 */
export interface GithubUserEvent {
  id?: string;
  type: string;
  created_at: string;
  actor?: { login?: string; avatar_url?: string };
  repo?: { name?: string };
  payload?: {
    action?: string;
    ref?: string;
    ref_type?: string;
    number?: number;
    size?: number;
    [key: string]: unknown;
  };
}

/** A single wiki page from the simulator's repo wiki page-store. */
export interface GithubWikiPage {
  slug: string;
  title: string;
  body: string;
  author?: string;
  created_at: string;
  updated_at: string;
}

export interface BleephubAuditEvent {
  id: number;
  actor_login: string;
  action: string;
  entity_type: string;
  entity_id: number | string;
  details: Record<string, unknown>;
  created_at: string;
}

export interface BleephubGistFile {
  filename?: string | undefined;
  content?: string | undefined;
  raw_url?: string | undefined;
  size?: number | undefined;
  type?: string | undefined;
  language?: string | undefined;
}

export interface BleephubGist {
  id: string;
  description: string;
  public: boolean;
  owner: { login: string; type: string; avatar_url?: string };
  files: Record<string, BleephubGistFile>;
  html_url?: string;
  created_at: string;
  updated_at: string;
  history?: GithubGistCommit[];
  forks?: BleephubGist[];
  forks_url?: string;
  commits_url?: string;
}

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubGistCommit = components["schemas"]["gist-commit"];
export type GithubGistComment = components["schemas"]["gist-comment"];

export interface GithubNotificationThread {
  id: string;
  repository: Record<string, unknown>;
  subject: {
    title: string;
    url: string;
    latest_comment_url: string;
    type: string;
  };
  reason: string;
  unread: boolean;
  updated_at: string;
  last_read_at: string | null;
  subscription_url: string;
  url: string;
}

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubThreadSubscription = components["schemas"]["thread-subscription"];

// ─── GitHub Discussions GraphQL shapes ──────────────────────────────────

export interface GithubDiscussionCategory {
  id: string;
  name: string;
  emoji: string;
  description: string;
  isAnswerable: boolean;
}

export interface GithubDiscussionAuthor {
  login: string;
  avatarUrl?: string;
}

export interface GithubDiscussion {
  id: string;
  number: number;
  title: string;
  body: string;
  bodyText: string;
  author: GithubDiscussionAuthor | null;
  category: GithubDiscussionCategory;
  createdAt: string;
  updatedAt: string;
  comments: { totalCount: number };
}

export interface GithubDiscussionComment {
  id: string;
  databaseId: number;
  author: GithubDiscussionAuthor | null;
  body: string;
  createdAt: string;
  updatedAt: string;
  isAnswer: boolean;
  replies: { nodes: GithubDiscussionComment[] };
}

export interface GithubDiscussionConnection {
  nodes: GithubDiscussion[];
  totalCount: number;
  pageInfo: {
    hasNextPage: boolean;
    endCursor: string | null;
  };
}

export interface GithubDiscussionCategoryConnection {
  nodes: GithubDiscussionCategory[];
  totalCount: number;
}

export interface GithubDiscussionCommentConnection {
  nodes: GithubDiscussionComment[];
  totalCount: number;
}


export interface GithubProjectClassic {
  id: number;
  node_id: string;
  name: string;
  body: string;
  state: "open" | "closed";
  number: number;
  creator: { login: string; avatar_url?: string } | null;
  created_at: string;
  updated_at: string;
  url: string;
  html_url: string;
  columns_url: string;
}

export interface GithubProjectColumn {
  id: number;
  node_id: string;
  name: string;
  created_at: string;
  updated_at: string;
  url: string;
  project_url: string;
  cards_url: string;
}

export interface GithubProjectCard {
  id: number;
  node_id: string;
  note: string | null;
  creator: { login: string; avatar_url?: string } | null;
  created_at: string;
  updated_at: string;
  url: string;
  column_url: string;
  project_url: string;
  content_url: string | null;
}

export interface GithubSecretScanningLocationDetails {
  path: string;
  start_line: number;
  end_line: number;
  start_column: number;
  end_column: number;
  blob_sha: string;
  blob_url: string;
  commit_sha: string;
  commit_url: string;
  html_url: string;
}

export interface GithubSecretScanningLocation {
  type: "commit";
  details: GithubSecretScanningLocationDetails;
}

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubSecretScanningAlert = components["schemas"]["secret-scanning-alert"];

export type GithubSecretScanningResolution =
  | "false_positive"
  | "wont_fix"
  | "revoked"
  | "used_in_tests"
  | "pattern_deleted"
  | "pattern_edited";

// ─── GitHub Code Scanning shapes ────────────────────────────────────────

export type GithubCodeScanningAlertState = "open" | "dismissed" | "fixed";

export type GithubCodeScanningDismissedReason =
  | "false positive"
  | "won't fix"
  | "used in tests";

// Generated from the vendored GitHub OpenAPI description (WEB-013). Spec marks
// location/commit_sha optional; CodeScanningPage optional-chains them.
export type GithubCodeScanningAlertInstance = components["schemas"]["code-scanning-alert-instance"];

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubCodeScanningAlert = components["schemas"]["code-scanning-alert"];

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubCodeScanningAnalysis = components["schemas"]["code-scanning-analysis"];

export interface GithubCodeScanningSARIFUpload {
  id: string;
  url: string;
}

export interface GithubCodeScanningSARIFStatus {
  processing_status: "pending" | "complete" | "failed";
  analyses_url: string | null;
  errors: string[] | null;
}

export interface GithubCodeQLDatabase {
  id: number;
  name: string;
  language: string;
  uploader: { login: string; type: string; avatar_url?: string } | null;
  content_type: string;
  size: number;
  created_at: string;
  updated_at: string;
  url: string;
  commit_oid: string | null;
}

// ─── GitHub Dependabot shapes ───────────────────────────────────────────

export type GithubDependabotAlertState = "open" | "dismissed" | "fixed" | "auto_dismissed";

export type GithubDependabotDismissedReason =
  | "fix_started"
  | "inaccurate"
  | "no_bandwidth"
  | "not_used"
  | "tolerable_risk";

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubDependabotAlert = components["schemas"]["dependabot-alert"];

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubDependabotSecret = components["schemas"]["dependabot-secret"];

// ─── GitHub Codespaces shapes ───────────────────────────────────────────

export type GithubCodespaceState =
  | "Unknown"
  | "Created"
  | "Queued"
  | "Provisioning"
  | "Available"
  | "Awaiting"
  | "Deleted"
  | "Moved"
  | "Shutdown"
  | "Archived"
  | "Starting"
  | "ShuttingDown"
  | "Failed"
  | "Exporting"
  | "Updating"
  | "Rebuilding"
  | "Unavailable";

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubCodespaceMachine = components["schemas"]["codespace-machine"];

export interface GithubCodespace {
  id: number;
  name: string;
  display_name: string;
  environment_id: string;
  owner: { login: string; type: string; avatar_url?: string } | null;
  billable_owner: { login: string; type: string; avatar_url?: string } | null;
  repository: { id: number; full_name: string; name: string; owner: { login: string; type: string } } | null;
  machine: GithubCodespaceMachine | null;
  created_at: string;
  updated_at: string;
  last_used_at: string;
  state: GithubCodespaceState;
  url: string;
  web_url: string;
  git_status: { ahead: number; behind: number; has_uncommitted_changes: boolean; ref: string };
  devcontainer_path: string;
  retention_period_minutes: number;
}

export interface CodespaceCreatePayload {
  repository_id?: number;
  ref?: string;
  machine?: string;
  display_name?: string;
  location?: string;
}

// ─── GitHub Packages REST shapes ────────────────────────────────────────

export type GithubPackageType = "npm" | "maven" | "rubygems" | "nuget" | "docker" | "container";
export type GithubPackageVisibility = "public" | "private" | "internal";

export interface GithubPackage {
  id: number;
  name: string;
  package_type: GithubPackageType;
  visibility: GithubPackageVisibility;
  url: string;
  html_url: string;
  version_count: number;
  created_at: string;
  updated_at: string;
  owner: { login: string; type: string; avatar_url?: string } | null;
  repository: BleephubRepo | null;
}

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubPackageVersion = components["schemas"]["package-version"];

export interface GithubPackageFile {
  id: number;
  node_id: string;
  name: string;
  content_type: string;
  size: number;
  url: string;
  html_url: string;
  download_url: string;
}

// ─── GitHub Security Advisories shapes ──────────────────────────────────

export type GithubSecurityAdvisorySeverity = "critical" | "high" | "medium" | "low";
export type GithubSecurityAdvisoryState =
  | "triage"
  | "draft"
  | "published"
  | "closed"
  | "withdrawn";

export interface GithubSecurityAdvisory {
  id?: number;
  ghsa_id: string;
  cve_id: string | null;
  summary: string;
  description: string | null;
  severity: GithubSecurityAdvisorySeverity;
  cwe_ids?: string[];
  state: GithubSecurityAdvisoryState;
  author: { login: string } | null;
  created_at: string;
  updated_at: string;
  published_at: string | null;
  url: string;
  html_url: string;
  private_fork?: BleephubRepo | null;
}

export interface GithubSecurityAdvisoryCreatePayload {
  summary: string;
  description: string;
  severity: GithubSecurityAdvisorySeverity;
  cwe_ids?: string[] | undefined;
}

export interface GithubSecurityAdvisoryUpdatePayload {
  summary?: string;
  description?: string;
  severity?: GithubSecurityAdvisorySeverity;
  cwe_ids?: string[];
  state?: GithubSecurityAdvisoryState;
}

export interface GithubVulnerabilityReportPayload {
  summary: string;
  description: string;
  severity?: GithubSecurityAdvisorySeverity | undefined;
  cwe_ids?: string[] | undefined;
}

// ─── GitHub Repository Rulesets shapes ──────────────────────────────────

export type GithubRulesetTarget = "branch" | "tag" | "push" | "repository";
export type GithubRulesetEnforcement = "disabled" | "active" | "evaluate";

export interface GithubRuleset {
  id: number;
  name: string;
  target: GithubRulesetTarget;
  source_type: "Repository" | "Organization";
  source: string;
  enforcement: GithubRulesetEnforcement;
  bypass_actors?: Array<{
    actor_id: number;
    actor_type: string;
    bypass_mode: string;
  }>;
  conditions?: Record<string, unknown>;
  rules?: Array<{
    type: string;
    parameters?: Record<string, unknown>;
  }>;
  created_at?: string;
  updated_at?: string;
}

export interface GithubRulesetCreatePayload {
  name: string;
  target: GithubRulesetTarget;
  enforcement: GithubRulesetEnforcement;
  rules?: Array<{ type: string; parameters?: Record<string, unknown> }>;
  conditions?: Record<string, unknown>;
  bypass_actors?: Array<{
    actor_id: number;
    actor_type: string;
    bypass_mode: string;
  }>;
}

export type GithubRuleSuiteResult = "pass" | "fail" | "bypass";

export interface GithubRuleEvaluation {
  rule_source: {
    type: string;
    id: number | null;
    name: string | null;
  };
  enforcement: "active" | "evaluate" | "deleted ruleset";
  result: "pass" | "fail";
  rule_type: string;
  details: string | null;
}

export interface GithubRulesetSuite {
  id: number;
  actor_id: number | null;
  actor_name: string | null;
  before_sha: string;
  after_sha: string;
  ref: string;
  repository_id: number;
  repository_name: string;
  pushed_at: string;
  result: GithubRuleSuiteResult;
  evaluation_result: GithubRuleSuiteResult | null;
  rule_evaluations?: GithubRuleEvaluation[];
}

// ─── Repo insights ───────────────────────────────────────────────────────

/** One entry of GET /repos/{o}/{r}/contributors — a resolved account or an
 * anonymous git author (type "Anonymous", identified by name/email). */
export interface GithubContributor {
  login?: string;
  avatar_url?: string;
  type: string;
  contributions: number;
  name?: string;
  email?: string;
}

export interface GithubTrafficBucket {
  timestamp: string;
  count: number;
  uniques: number;
}

export interface GithubTrafficViews {
  count: number;
  uniques: number;
  views: GithubTrafficBucket[];
}

export interface GithubTrafficClones {
  count: number;
  uniques: number;
  clones: GithubTrafficBucket[];
}

export interface GithubTrafficPath {
  path: string;
  title: string;
  count: number;
  uniques: number;
}

export interface GithubTrafficReferrer {
  referrer: string;
  count: number;
  uniques: number;
}

/** One week of GET /repos/{o}/{r}/stats/commit_activity: Unix week start,
 * per-weekday commit counts (Sunday first), and the week total. */
export interface GithubCommitActivityWeek {
  week: number;
  days: number[];
  total: number;
}

/** One weekly bucket from GET /repos/{o}/{r}/stats/code_frequency:
 * [unix week timestamp, additions (>=0), deletions (<=0)]. */
export type GithubCodeFrequencyWeek = [number, number, number];

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubCommunityProfile = components["schemas"]["community-profile"];

// ─── Labels + milestones ────────────────────────────────────────────────

export interface GithubLabel {
  id: number;
  name: string;
  color: string;
  description: string;
  default: boolean;
}

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubMilestone = NonNullable<components["schemas"]["nullable-milestone"]>;

// ─── Organization governance ────────────────────────────────────────────

/** GitHub `simple-user` — the shape people-management lists return. */
export interface GithubAccount {
  id: number;
  login: string;
  avatar_url: string;
  type: string;
  site_admin: boolean;
}

export interface GithubOrgInvitation {
  id: number;
  login: string | null;
  email: string | null;
  role: string;
  created_at: string;
  failed_at: string | null;
  failed_reason: string | null;
  inviter: GithubAccount | null;
  team_count: number;
  invitation_source: string;
}

export type GithubCustomPropertyValueType =
  | "string"
  | "single_select"
  | "multi_select"
  | "true_false"
  | "url";

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubCustomProperty = components["schemas"]["custom-property"];

// Generated from the vendored GitHub OpenAPI description (WEB-013). The spec
// models the whole type nullable; NonNullable strips that for list consumers.
export type GithubIssueType = NonNullable<components["schemas"]["issue-type"]>;

export interface GithubOrgRole {
  id: number;
  name: string;
  description: string;
  base_role: string;
  source: string;
  permissions: string[];
  created_at: string;
  updated_at: string;
}

/** Team assigned an organization role (team-simple + assignment). */
export interface GithubOrgRoleTeam {
  id: number;
  slug: string;
  name: string;
  description: string | null;
  assignment: string;
}

/** User assigned an organization role (simple-user + assignment). */
export interface GithubOrgRoleUser extends GithubAccount {
  assignment: string;
}

// ─── Enterprise administration ──────────────────────────────────────────

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubEnterpriseTeam = components["schemas"]["enterprise-team"];

export interface GithubEnterpriseDependabotAccess {
  default_level: string | null;
  accessible_repositories: BleephubRepo[];
}

// ─── Copilot ────────────────────────────────────────────────────────────

export interface GithubCopilotSeatBreakdown {
  total: number;
  added_this_cycle: number;
  pending_cancellation: number;
  pending_invitation: number;
  active_this_cycle: number;
  inactive_this_cycle: number;
}

export interface GithubCopilotBilling {
  seat_breakdown: GithubCopilotSeatBreakdown;
  public_code_suggestions: string;
  ide_chat: string;
  platform_chat: string;
  cli: string;
  seat_management_setting: string;
  plan_type: string;
}

export interface GithubCopilotSeat {
  assignee: GithubAccount | null;
  assigning_team: { slug: string; name: string } | null;
  pending_cancellation_date: string | null;
  last_activity_at: string | null;
  last_activity_editor: string | null;
  created_at: string;
  updated_at: string;
  plan_type: string;
}

export interface GithubCopilotSpace {
  id: number;
  number: number;
  name: string;
  description: string | null;
  general_instructions: string | null;
  base_role: string;
  owner: { login: string } | null;
  creator: GithubAccount | null;
  created_at: string;
  updated_at: string;
}

// ─── Copilot Spaces CRUD · enterprise team orgs · custom property values ─

/** Copilot Space collaborator: `simple-user` or team-simple plus actor_type + role. */
export interface GithubCopilotSpaceCollaborator {
  actor_type: "User" | "Team";
  role: string;
  id: number;
  /** Present when actor_type is "User". */
  login?: string;
  /** Present when actor_type is "Team". */
  slug?: string;
  name?: string;
}

// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubCopilotSpaceResource = components["schemas"]["copilot-space-resource"];

// ─── Deployments + webhook deliveries + Pages ───────────────────────────

/** Deployment — GET /repos/{o}/{r}/deployments (items). */
// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubDeployment = NonNullable<components["schemas"]["nullable-deployment"]>;

/** Deployment status state (the POST statuses `state` enum). */
export type GithubDeploymentState =
  | "error"
  | "failure"
  | "inactive"
  | "in_progress"
  | "queued"
  | "pending"
  | "success";

/** Deployment status — GET .../deployments/{id}/statuses (items). */
// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubDeploymentStatus = components["schemas"]["deployment-status"];

// ─── PR reviews, statuses, reactions & timeline ─────────────────────────

export type GithubReviewState =
  | "APPROVED"
  | "CHANGES_REQUESTED"
  | "COMMENTED"
  | "DISMISSED"
  | "PENDING";

/** Pull request review — GET .../pulls/{n}/reviews (items). */
export interface GithubPRReview {
  id: number;
  /** null when the authoring user no longer resolves (GitHub parity). */
  user: { login: string; avatar_url: string } | null;
  body: string;
  state: GithubReviewState;
  commit_id: string;
  submitted_at: string | null;
}

/** Pull request review comment — GET .../pulls/{n}/comments (items). */
export interface GithubPRReviewComment {
  id: number;
  pull_request_review_id: number;
  in_reply_to_id?: number;
  diff_hunk: string;
  path: string;
  line: number | null;
  side: string;
  body: string;
  /** null when the authoring user no longer resolves (GitHub parity). */
  user: { login: string; avatar_url: string } | null;
  created_at: string;
  updated_at: string;
}

/** GitHub `organization-simple` — enterprise team organization assignments. */
export interface GithubOrgSimple {
  id: number;
  login: string;
  avatar_url: string;
  description: string | null;
}

/** One repository row of GET /orgs/{org}/properties/values. */
// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubOrgRepoCustomPropertyValues = components["schemas"]["org-repo-custom-property-values"];

/** Wait-timer / required-reviewers rule nested in the environment object. */
export interface GithubEnvironmentProtectionRule {
  id: number;
  node_id: string;
  type: string;
  wait_timer?: number;
  reviewers?: { type: string; reviewer?: { login?: string } }[];
}

/**
 * Full environment — GET /repos/{o}/{r}/environments (items). Carries the
 * protection config the slim GithubEnvironment omits.
 */
export interface GithubEnvironmentDetail {
  id: number;
  node_id: string;
  name: string;
  url: string;
  html_url: string;
  created_at: string;
  updated_at: string;
  deployment_branch_policy: {
    protected_branches: boolean;
    custom_branch_policies: boolean;
  } | null;
  protection_rules: GithubEnvironmentProtectionRule[];
}

/** Branch/tag pattern — GET .../environments/{env}/deployment-branch-policies. */
// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubDeploymentBranchPolicy = components["schemas"]["deployment-branch-policy"];

/** Custom (app-backed) rule — GET .../environments/{env}/deployment_protection_rules. */
export interface GithubEnvCustomProtectionRule {
  id: number;
  node_id: string;
  enabled: boolean;
  app: { id: number; slug: string; integration_url: string; node_id: string } | null;
}

/** Delivery summary — GET {hook}/deliveries (items). */
export interface GithubHookDelivery {
  id: number;
  guid: string;
  delivered_at: string;
  redelivery: boolean;
  duration: number;
  status: string;
  status_code: number;
  event: string;
  action: string | null;
  installation_id: number | null;
  repository_id: number | null;
  throttled_at: string | null;
}

/** Full delivery — GET {hook}/deliveries/{id} (adds request/response). */
export interface GithubHookDeliveryDetail extends GithubHookDelivery {
  url: string;
  request: { headers: Record<string, string> | null; payload: unknown };
  response: { headers: Record<string, string> | null; payload: string | null };
}

/** Organization webhook — GET /orgs/{org}/hooks (items). */
export interface GithubOrgWebhook {
  id: number;
  type: string;
  name: string;
  active: boolean;
  events: string[];
  config: { url: string; content_type: string; insecure_ssl: string };
  created_at: string;
  updated_at: string;
  url: string;
  ping_url: string;
  deliveries_url: string;
}

/** Pages site — GET /repos/{o}/{r}/pages. */
export interface GithubPagesSite {
  cname: string;
  url: string;
  html_url: string;
  status: string;
  source: { branch?: string; path?: string } | null;
  public: boolean;
  custom_404: boolean;
  protected_domain_state: string | null;
  build_type: string | null;
  https_enforced: boolean;
}

/** Pages build — GET /repos/{o}/{r}/pages/builds (items). */
export interface GithubPagesBuild {
  url: string;
  status: string;
  pusher: { login: string; id: number; type: string } | null;
  commit: string;
  created_at: string;
  updated_at: string;
  duration: number;
  error: { message: string | null } | null;
}

/** One domain's checks inside the Pages health-check response. */
export interface GithubPagesDomainHealth {
  host: string;
  uri: string;
  nameservers: string;
  dns_resolves: boolean;
  is_valid_domain: boolean;
  is_apex_domain: boolean;
  is_pages_domain: boolean;
  is_valid: boolean;
  reason: string | null;
  enforces_https: boolean;
}

/** Pages health check — GET /repos/{o}/{r}/pages/health. */
export interface GithubPagesHealth {
  domain: GithubPagesDomainHealth | null;
  alt_domain: GithubPagesDomainHealth | null;
}

/** Pull request review thread — GraphQL PullRequest.reviewThreads node. */
export interface GithubPRReviewThread {
  id: string;
  isResolved: boolean;
  comments: { databaseId: number }[];
}

/** Requested reviewers — GET .../pulls/{n}/requested_reviewers. */
export interface GithubReviewRequest {
  users: GithubAccount[];
  teams: { id: number; slug: string; name: string }[];
}

export type GithubCommitStatusState = "success" | "failure" | "error" | "pending";

/** One status context inside the combined commit status. */
export interface GithubCommitStatus {
  context: string;
  state: GithubCommitStatusState;
  description: string | null;
  target_url: string | null;
}

/** Combined commit status — GET .../commits/{ref}/status. */
export interface GithubCombinedStatus {
  state: GithubCommitStatusState;
  sha: string;
  total_count: number;
  statuses: GithubCommitStatus[];
}

export type GithubReactionContent =
  | "+1"
  | "-1"
  | "laugh"
  | "confused"
  | "heart"
  | "hooray"
  | "rocket"
  | "eyes";

/** Reaction — GET .../reactions (items). */
export interface GithubReaction {
  id: number;
  content: GithubReactionContent;
  /** null when the reacting user no longer resolves (GitHub parity). */
  user: { login: string } | null;
  created_at: string;
}

/**
 * Issue-timeline union member — GET .../issues/{n}/timeline (items).
 * Only `event` is guaranteed; every other member depends on the event
 * variant (commented, reviewed, labeled, assigned, renamed, …).
 */
export interface GithubTimelineItem {
  event: string;
  id?: number;
  actor?: { login: string; avatar_url: string } | null;
  user?: { login: string; avatar_url: string } | null;
  body?: string;
  created_at?: string;
  submitted_at?: string | null;
  state?: string;
  label?: { name: string; color: string } | null;
  assignee?: { login: string } | null;
  rename?: { from: string; to: string } | null;
  commit_id?: string | null;
}

// ─── Search + repo social + account ─────────────────────────────────────

/** Item from GET /search/issues: issue shape plus PR marker + repository. */
export interface GithubSearchIssueItem {
  id: number;
  number: number;
  title: string;
  state: string;
  user: { login: string } | null;
  comments: number;
  created_at: string;
  updated_at: string;
  /** null for plain issues; set when the result is a pull request. */
  pull_request: { url: string } | null;
  repository: { full_name: string };
}

/** Item from GET /search/code. */
export interface GithubSearchCodeItem {
  name: string;
  path: string;
  sha: string;
  html_url: string;
  language: string | null;
  repository: { full_name: string };
}

/** Item from GET /search/users (users and orgs share the shape). */
export interface GithubSearchUserItem {
  id: number;
  login: string;
  type: string;
  name?: string | null;
  bio?: string | null;
}

/** Item from GET /search/commits. */
export interface GithubSearchCommitItem {
  sha: string;
  commit: {
    message: string;
    author: { name: string; email: string; date: string };
  };
  author: { login: string } | null;
  repository: { full_name: string };
}

/** Item from GET /search/labels. */
export interface GithubSearchLabelItem {
  id: number;
  name: string;
  color: string;
  default: boolean;
  description: string | null;
}

/** Item from GET /search/topics. */
export interface GithubSearchTopicItem {
  name: string;
  repository_count: number;
  created_at: string;
  updated_at: string;
}

/** Repo collaborator: simple user plus permission grants. */
// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubCollaborator = NonNullable<components["schemas"]["nullable-collaborator"]>;

/** Pending repository invitation. */
export interface GithubRepoInvitation {
  id: number;
  invitee: { login: string } | null;
  inviter: { login: string } | null;
  permissions: string;
  created_at: string;
  expired: boolean;
}

/** Git tag with source-archive download links. */
export interface GithubTag {
  name: string;
  zipball_url: string;
  tarball_url: string;
  commit: { sha: string; url: string };
}

/** Social counters served on the full-repository shape. */
export interface GithubRepoSocialCounts {
  stargazers_count: number;
  subscribers_count: number;
  forks_count: number;
}

/** Authenticated viewer's repository notification preference. */
export interface GithubRepoSubscription {
  subscribed: boolean;
  ignored: boolean;
  reason: string | null;
  created_at: string;
  url: string;
  repository_url: string;
}

/** Viewer-specific state normalized for repository page rendering. */
export interface GithubRepoViewerState {
  starred: boolean;
  subscribed: boolean;
}

/** Email address on the authenticated user's account. */
export interface GithubUserEmail {
  email: string;
  primary: boolean;
  verified: boolean;
  visibility: string | null;
}

/** SSH authentication key on the authenticated user's account. */
export interface GithubSSHKey {
  id: number;
  key: string;
  title: string;
  verified: boolean;
  created_at: string;
  read_only: boolean;
}

export interface GithubGPGKeyEmail {
  email: string;
  verified: boolean;
  primary: boolean;
}

/** GPG key on the authenticated user's account. */
export interface GithubGPGKey {
  id: number;
  key_id: string;
  public_key: string;
  can_sign: boolean;
  can_encrypt_commits: boolean;
  can_certify: boolean;
  created_at: string;
  name?: string;
  emails?: GithubGPGKeyEmail[];
  expires_at?: string;
}

/** SSH signing key on the authenticated user's account. */
// Generated from the vendored GitHub OpenAPI description (WEB-013).
export type GithubSSHSigningKey = components["schemas"]["ssh-signing-key"];

/** Entry in GET /user/blocks. */
export interface GithubBlockedUser {
  login: string;
}

// ─── Bleephub dashboard, user profile, and organization pages ───────────

/**
 * The GitHub `public-user` shape served by GET /users/{login} and
 * GET /user — the simple-user members plus the profile fields and live
 * counters (followers/following/public_repos). company/location/
 * twitter_username are null when unset, matching real GitHub.
 */
export interface GithubUserProfile {
  login: string;
  id: number;
  avatar_url: string;
  type: string;
  site_admin: boolean;
  name: string | null;
  email: string | null;
  bio: string | null;
  blog: string | null;
  company: string | null;
  location: string | null;
  twitter_username: string | null;
  followers: number;
  following: number;
  public_repos: number;
  created_at: string;
  html_url?: string;
}

/**
 * The GitHub `organization-full` shape served by GET /orgs/{org} —
 * profile fields plus the live public_repos counter.
 */
export interface GithubOrgProfile {
  login: string;
  id: number;
  avatar_url: string;
  description: string | null;
  name: string | null;
  company: string | null;
  blog: string | null;
  location: string | null;
  email: string | null;
  twitter_username: string | null;
  public_repos: number;
  followers: number;
  following: number;
  html_url: string;
  created_at: string;
}

/** The GitHub `organization-simple` shape in GET /users/{login}/orgs. */
export interface GithubOrgSummary {
  login: string;
  id: number;
  avatar_url: string;
  description: string | null;
}

/** The GitHub `team` (team-simple) shape in GET /orgs/{org}/teams. */
export interface GithubOrgTeam {
  id: number;
  slug: string;
  name: string;
  description: string | null;
  privacy: string;
  permission: string;
  html_url: string;
  parent: { slug: string; name: string } | null;
}

/**
 * An issue from GET /issues (the authenticated user's cross-repo issue
 * feed). Unlike the repo-scoped issue shape it carries the `repository`
 * each result lives in, since results span repositories.
 */
export interface GithubFeedIssue {
  id: number;
  number: number;
  title: string;
  state: GithubState;
  comments: number;
  updated_at: string;
  html_url: string;
  repository: { full_name: string; name: string; owner: { login: string } };
}

// ─── GitHub Marketplace browser workflow ──────────────────────────────

export interface GithubMarketplacePlan {
  url: string;
  accounts_url: string;
  id: number;
  number: number;
  name: string;
  description: string;
  monthly_price_in_cents: number;
  yearly_price_in_cents: number;
  price_model: "FREE" | "FLAT_RATE" | "PER_UNIT";
  has_free_trial: boolean;
  unit_name: string | null;
  state: "draft" | "published";
  bullets: string[];
}

export interface GithubMarketplaceListing {
  slug: string;
  name: string;
  description: string;
  full_description: string;
  setup_url: string | null;
  installation_url: string | null;
  github_app_id: number | null;
  oauth_app_client_id: string | null;
  published: boolean;
  created_at: string;
  updated_at: string;
  plans: GithubMarketplacePlan[];
}

export interface GithubMarketplaceListingSettings extends GithubMarketplaceListing {
  webhook_url: string | null;
  webhook_content_type: "json" | "form";
  webhook_active: boolean;
  webhook_id: number | null;
}

export interface GithubMarketplaceAccount {
  id: number;
  login: string;
  type: "User" | "Organization";
  avatar_url: string;
}

export interface GithubMarketplacePendingChange {
  effective_date: string;
  billing_cycle: string | null;
  unit_count: number | null;
  cancellation: boolean;
  plan?: GithubMarketplacePlan;
}

export interface GithubMarketplaceSubscription {
  id: number;
  login: string;
  type: "User" | "Organization";
  marketplace_pending_change: GithubMarketplacePendingChange | null;
  marketplace_purchase: {
    billing_cycle: "monthly" | "yearly";
    next_billing_date: string | null;
    is_installed: boolean;
    unit_count: number | null;
    on_free_trial: boolean;
    free_trial_ends_on: string | null;
    updated_at: string | null;
    plan: GithubMarketplacePlan;
  };
  listing: GithubMarketplaceListing;
  account_login: string;
  setup_url: string | null;
}

// ─── WP-C: Issues & Pull Requests GitHub-faithful layout ────────────────

/** A changed file in a pull request — GET /pulls/{n}/files (items). */
export interface GithubPRFile {
  sha: string;
  filename: string;
  status: "added" | "removed" | "modified" | "renamed" | "changed" | "copied" | "unchanged";
  additions: number;
  deletions: number;
  changes: number;
  /** Unified-diff hunk text; absent for binary files. */
  patch?: string;
  previous_filename?: string;
  blob_url?: string;
}

/** Client-side list filters shared by the Issues and Pull Requests list bars. */
export interface ListFilterState {
  label: string | null;
  author: string | null;
  assignee: string | null;
  milestone: string | null;
  sort: "newest" | "oldest" | "comments" | "updated";
}
