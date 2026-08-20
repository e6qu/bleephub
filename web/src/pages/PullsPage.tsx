import { useMemo, useState, type ReactNode } from "react";
import { useParams, Link, useLocation, useNavigate, useSearchParams } from "react-router";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchRepoPRsFilteredPage,
  fetchPRDetail,
  fetchPRCommits,
  fetchCheckRuns,
  fetchRepoBranches,
  createPull,
  updatePull,
  updatePRBranch,
  markPullRequestReadyForReview,
  convertPullRequestToDraft,
  isNotFound,
  fetchAuthenticatedUser,
  fetchPRReviews,
  dismissPRReview,
  fetchPRReviewComments,
  fetchPRReviewThreads,
  fetchPRRequestedReviewers,
  fetchAssignableUsers,
  fetchOrgTeams,
  requestPRReviewers,
  removePRRequestedReviewers,
  fetchCombinedStatus,
  fetchIssueTimeline,
  fetchIssueReactions,
  addIssueReaction,
  removeIssueReaction,
  fetchIssueCommentReactions,
  addIssueCommentReaction,
  removeIssueCommentReaction,
  fetchRepoDetail,
  ghFetch,
  ghGraphQL,
  ghSend,
  deleteRef,
  createRef,
} from "../api.js";
import {
  checkRunsEnvelopeToPage,
  fetchPullBootstrap,
  freshenQueryDefaults,
  mirrorQueryData,
  seedQueryCache,
  useSeededOpenCounts,
  validCombinedStatus,
  validReviewRequest,
  SEED_STALE_TIME,
} from "../utils/bootstrap.js";
import type {
  GithubCheckRun,
  GithubCommitStatus,
  GithubCommitStatusState,
  GithubPR,
  GithubPRReview,
  GithubPRReviewComment,
  GithubReviewState,
  GithubTimelineItem,
  ListFilterState,
} from "../types.js";
import { formatDuration } from "../utils/format.js";
import { useRepoPermissions } from "../hooks/useRepoPermissions.js";
import { CommentCard } from "../components/CommentCard.js";
import { RepoHeader } from "../components/PageHeader.js";
import { RunStatusIcon } from "../components/RunStatusIcon.js";
import { ReactionBar } from "../components/ReactionBar.js";
import { TimelineEventRow } from "../components/TimelineEventRow.js";
import { IssueSidebar } from "../components/IssueSidebar.js";
import { PRFilesView } from "../components/PRFilesView.js";
import {
  groupReviewThreads,
  ReviewThreadCard,
  type ReviewThreadGroup,
} from "../components/PRReviewThread.js";
import { Avatar } from "../components/Avatar.js";
import { RelativeTime } from "../components/RelativeTime.js";
import { MarkdownComposer } from "../components/MarkdownComposer.js";
import Markdown from "../components/Markdown.js";
import {
  ListControls,
  filterAndSortItems,
  sortToServerParams,
  emptyFilters,
  type ListItemAccessors,
} from "../components/ListControls.js";
import { Button, Box, Blankslate, StateLabel, FormLabel, Tabs, Modal, DialogActions } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { CommentComposer } from "../components/CommentComposer.js";
import {
  PullRequestIcon,
  MergedIcon,
  PullClosedIcon,
  BranchIcon,
  CheckCircleIcon,
  XCircleIcon,
  DotFillIcon,
  IssueOpenedIcon,
  IssueClosedIcon,
} from "../components/octicons.js";

const prAccessors: ListItemAccessors<GithubPR> = {
  labels: (p) => p.labels,
  author: (p) => p.user?.login ?? null,
  assignees: (p) => (p.assignees ?? []).map((a) => a.login),
  milestone: (p) => p.milestone?.title ?? null,
  comments: (p) => p.comments ?? 0,
  createdAt: (p) => p.created_at,
  updatedAt: (p) => p.updated_at,
};

export function PullsPage() {
  const { owner = "", repo = "", number } = useParams<{
    owner: string;
    repo: string;
    number?: string;
  }>();

  if (number) {
    return <PRDetail owner={owner} repo={repo} number={parseInt(number, 10)} />;
  }
  return <PRList owner={owner} repo={repo} />;
}

function prState(pr: GithubPR): "open" | "merged" | "closed" | "draft" {
  if (pr.merged) return "merged";
  if (pr.state === "open") return pr.draft ? "draft" : "open";
  return "closed";
}

function PRStateIcon({ pr, size }: { pr: GithubPR; size?: number }) {
  const s = prState(pr);
  if (s === "merged") return <MergedIcon size={size} style={{ color: "var(--gh-merged)" }} />;
  if (s === "closed") return <PullClosedIcon size={size} style={{ color: "var(--gh-closed)" }} />;
  if (s === "draft") return <PullRequestIcon size={size} style={{ color: "var(--gh-draft)" }} />;
  return <PullRequestIcon size={size} style={{ color: "var(--gh-open)" }} />;
}

function usePRClosedCount(owner: string, repo: string): number | string | undefined {
  const { data } = useQuery({
    queryKey: ["prs", owner, repo, "closed", "count"],
    queryFn: ({ signal }) => fetchRepoPRsFilteredPage(owner, repo, { state: "closed" }, undefined, signal),
    enabled: !!owner && !!repo,
  });
  if (!data) return undefined;
  return data.nextUrl ? `${data.items.length}+` : data.items.length;
}

function PRList({ owner, repo }: { owner: string; repo: string }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [state, setState] = useState<"open" | "closed" | "all">("open");
  const [filters, setFilters] = useState<ListFilterState>(emptyFilters);
  const counts = useSeededOpenCounts(owner, repo);
  const closedCount = usePRClosedCount(owner, repo);

  // Compare deep-link: /pulls?compare=base...head (the compare view and
  // branch rows link here) opens the create-PR flow prefilled.
  const [searchParams] = useSearchParams();
  const compare = searchParams.get("compare");
  const compareParts =
    compare !== null && compare.includes("...") ? compare.split("...", 2) : null;
  const [creating, setCreating] = useState(compareParts !== null);
  const [prTitle, setPrTitle] = useState("");
  const [prBody, setPrBody] = useState("");
  const [prHead, setPrHead] = useState(compareParts?.[1] ?? "");
  const [prBase, setPrBase] = useState(compareParts?.[0] ?? "");
  const [prDraft, setPrDraft] = useState(false);
  const { data: branches = [] } = useQuery({
    queryKey: ["branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
    enabled: creating,
  });
  const createMut = useMutation({
    mutationFn: () =>
      createPull(owner, repo, { title: prTitle, head: prHead, base: prBase, body: prBody, draft: prDraft }),
    onSuccess: (pr: GithubPR) => {
      qc.invalidateQueries({ queryKey: ["prs", owner, repo] });
      setCreating(false);
      navigate(`/ui/repos/${owner}/${repo}/pulls/${pr.number}`);
    },
  });

  // base/head are PR-only server filters (github's REST /pulls supports them,
  // but issues don't have them, so they live here rather than in the shared
  // ListControls). sort/direction come from the shared sort facet.
  const [baseFilter, setBaseFilter] = useState("");
  const [headFilter, setHeadFilter] = useState("");
  const serverSort = sortToServerParams(filters.sort);
  // The shared sort facet speaks the issues dialect; GitHub's /pulls endpoint
  // calls the most-commented sort "popularity".
  if (serverSort.sort === "comments") serverSort.sort = "popularity";
  const serverOpts = {
    state,
    base: baseFilter || undefined,
    head: headFilter || undefined,
    ...serverSort,
  };
  const query = useInfiniteQuery({
    queryKey: ["prs", owner, repo, serverOpts, "paged"],
    queryFn: ({ pageParam, signal }) => fetchRepoPRsFilteredPage(owner, repo, serverOpts, pageParam, signal),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.nextUrl ?? undefined,
    placeholderData: (previous) => previous,
    enabled: !!owner && !!repo,
  });
  const rawPRs = useMemo(() => query.data?.pages.flatMap((p) => p.items) ?? [], [query.data]);
  const prs = useMemo(() => filterAndSortItems(rawPRs, filters, prAccessors), [rawPRs, filters]);

  if (query.isLoading) return <Spinner label="loading pull requests" />;
  if (query.isError)
    return <InlineError title="Failed to load pull requests" detail={String(query.error)} />;

  const hasMore = query.hasNextPage;
  const isLoadingMore = query.isFetchingNextPage;

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="pulls" {...counts} />

      <ListControls
        kind="pr"
        state={state}
        onState={setState}
        openCount={counts.prCount}
        closedCount={closedCount}
        items={rawPRs}
        filters={filters}
        onFilters={setFilters}
        accessors={prAccessors}
        resultCount={prs.length}
        actions={
          <div className="flex items-center gap-2">
            <input
              aria-label="Filter by base branch"
              placeholder="base branch"
              value={baseFilter}
              onChange={(e) => setBaseFilter(e.target.value)}
              style={{ width: "9rem", fontSize: "0.82rem" }}
            />
            <input
              aria-label="Filter by head branch"
              placeholder="head (owner:branch)"
              value={headFilter}
              onChange={(e) => setHeadFilter(e.target.value)}
              style={{ width: "10rem", fontSize: "0.82rem" }}
            />
            <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
              New pull request
            </Button>
          </div>
        }
      />

      {creating && (
        <Modal title="Open a pull request" onClose={() => setCreating(false)}>
          <div className="mb-3 flex items-center gap-2">
            <div className="flex-1">
              <FormLabel id="pr-base">Base</FormLabel>
              <select
                id="pr-base"
                value={prBase}
                onChange={(e) => setPrBase(e.target.value)}
                className="w-full"
              >
                <option value="">Choose base…</option>
                {branches.map((b) => (
                  <option key={b.name} value={b.name}>
                    {b.name}
                  </option>
                ))}
              </select>
            </div>
            <span style={{ marginTop: "1.2rem", color: "var(--color-fg-muted)" }}>←</span>
            <div className="flex-1">
              <FormLabel id="pr-head">Compare</FormLabel>
              <select
                id="pr-head"
                value={prHead}
                onChange={(e) => setPrHead(e.target.value)}
                className="w-full"
              >
                <option value="">Choose head…</option>
                {branches.map((b) => (
                  <option key={b.name} value={b.name}>
                    {b.name}
                  </option>
                ))}
              </select>
            </div>
          </div>
          <FormLabel id="pr-title">Title</FormLabel>
          <input
            id="pr-title"
            value={prTitle}
            onChange={(e) => setPrTitle(e.target.value)}
            placeholder="Pull request title"
            className="mb-3 w-full"
          />
          <FormLabel id="pr-body">Description (optional)</FormLabel>
          <MarkdownComposer
            id="pr-body"
            value={prBody}
            onChange={setPrBody}
            rows={5}
            label="Description (optional)"
            placeholder="Describe the change…"
          />
          <label className="mb-3 mt-3 flex items-center gap-2" style={{ fontSize: "0.85rem" }}>
            <input type="checkbox" checked={prDraft} onChange={(e) => setPrDraft(e.target.checked)} />
            Create as a draft pull request
          </label>
          <MutationError of={createMut} />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setCreating(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="sm"
              disabled={!prTitle.trim() || !prHead || !prBase || prHead === prBase || createMut.isPending}
              onClick={() => createMut.mutate()}
            >
              {createMut.isPending ? "Creating…" : prDraft ? "Create draft pull request" : "Create pull request"}
            </Button>
          </DialogActions>
        </Modal>
      )}

      {prs.length === 0 ? (
        <Blankslate icon={<PullRequestIcon size={26} />} title={`No ${state} pull requests`} />
      ) : (
        <>
        <Box>
          {prs.map((pr, i) => (
            <Link
              key={pr.id}
              to={`/ui/repos/${owner}/${repo}/pulls/${pr.number}`}
              className="flex items-start gap-2.5"
              style={{
                padding: "0.7rem 1rem",
                borderBottom: i < prs.length - 1 ? "1px solid var(--color-border)" : "none",
                textDecoration: "none",
              }}
            >
              <span style={{ marginTop: "0.1rem" }}>
                <PRStateIcon pr={pr} />
              </span>
              <div className="min-w-0 flex-1">
                <div style={{ fontSize: "0.92rem", fontWeight: 600, color: "var(--color-fg)" }}>
                  {pr.title}
                  {pr.draft && (
                    <span style={{ marginLeft: "0.5rem", fontSize: "0.74rem", color: "var(--color-fg-subtle)", fontWeight: 400 }}>
                      Draft
                    </span>
                  )}
                </div>
                <div className="mt-1 flex flex-wrap items-center gap-x-2" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                  <span>#{pr.number}</span>
                  <span className="inline-flex items-center gap-1">
                    <BranchIcon size={12} />
                    <span className="font-mono" style={{ color: "var(--color-accent)" }}>{pr.head.ref}</span>
                    {" → "}
                    <span className="font-mono">{pr.base.ref}</span>
                  </span>
                  <span className="inline-flex items-center gap-1">
                    · opened <RelativeTime iso={pr.created_at} /> by{" "}
                    <Avatar login={pr.user?.login ?? "?"} src={pr.user?.avatar_url} size={16} />
                    {pr.user?.login}
                  </span>
                </div>
              </div>
            </Link>
          ))}
        </Box>
        {hasMore && (
          <div className="mt-3 flex justify-center">
            <Button variant="ghost" size="sm" disabled={isLoadingMore} onClick={() => query.fetchNextPage()}>
              {isLoadingMore ? "Loading…" : "Load more"}
            </Button>
          </div>
        )}
        </>
      )}
    </div>
  );
}

type PRTab = "conversation" | "commits" | "files" | "checks";

function PRDetail({ owner, repo, number }: { owner: string; repo: string; number: number }) {
  const counts = useSeededOpenCounts(owner, repo);
  const location = useLocation();
  const navigate = useNavigate();
  const suffix = location.pathname.split(`/pulls/${number}`)[1]?.split("/")[1];
  const tab: PRTab =
    suffix === "commits" || suffix === "files" || suffix === "checks" ? suffix : "conversation";

  const qc = useQueryClient();
  // One aggregate request replaces the detail's first-paint fan-out: it seeds
  // the exact keys the conversation tab, merge box, checks badge and
  // reviewers panel read, so those hooks are cache hits. On failure nothing
  // is seeded and every hook fetches standalone as before. (The files list is
  // NOT aggregated — the Files tab keeps its own fetch.)
  const bootstrapQ = useQuery({
    queryKey: ["pull-bootstrap", owner, repo, number],
    queryFn: async ({ signal }) => {
      const data = await fetchPullBootstrap(owner, repo, number, signal);
      const sha = data.pull.head.sha;
      seedQueryCache(qc, [
        [["pr", owner, repo, number], data.pull],
        [["pr-timeline", owner, repo, number], data.timeline],
        [["pr-reviews", owner, repo, number], data.reviews],
        [["pr-review-comments", owner, repo, number], data.review_comments],
        [["pr-requested-reviewers", owner, repo, number], validReviewRequest(data.requested_reviewers)],
        [["check-runs", owner, repo, sha], checkRunsEnvelopeToPage(data.check_runs)],
        [["combined-status", owner, repo, sha], validCombinedStatus(data.combined_status)],
        // Sidebar sub-payloads (same keys IssueSidebar and the reviewers
        // picker read), so the PR sidebar stops fetching them standalone.
        [["labels", owner, repo], data.labels],
        [["milestones", owner, repo, "all"], data.milestones],
        [["assignable-users", owner, repo], data.assignees_available],
      ]);
      // ["viewer"] and ["current-user"] both GET /api/v3/user; reuse whichever
      // response the session already holds instead of refetching it here.
      mirrorQueryData(qc, ["current-user"], ["viewer"]);
      freshenQueryDefaults(qc, [
        ["repo", owner, repo],
        ["repo-social-counts", owner, repo],
        ["repo-viewer", owner, repo],
      ]);
      return data;
    },
    // No numeric guard: a garbage number (NaN) 404s the bootstrap, which
    // settles it into the error fallback and the not-found path, exactly
    // like the old standalone fetch.
    enabled: !!owner && !!repo,
    staleTime: SEED_STALE_TIME,
  });
  const bootstrapSettled = bootstrapQ.isSuccess || bootstrapQ.isError;

  const { data: pr, isLoading, isError, error } = useQuery({
    queryKey: ["pr", owner, repo, number],
    queryFn: ({ signal }) => fetchPRDetail(owner, repo, number, signal),
    enabled: bootstrapSettled,
  });

  // Checks-tab count: check runs + commit statuses on the head SHA (both
  // queries are shared with the merge box / checks tab via their keys).
  const headSha = pr?.head.sha ?? "";
  const checksCountQ = useQuery({
    queryKey: ["check-runs", owner, repo, headSha],
    queryFn: () => fetchCheckRuns(owner, repo, headSha),
    enabled: !!headSha,
  });
  const statusCountQ = useQuery({
    queryKey: ["combined-status", owner, repo, headSha],
    queryFn: ({ signal }) => fetchCombinedStatus(owner, repo, headSha, signal),
    enabled: !!headSha,
  });
  const checksCount =
    (checksCountQ.data?.items.length ?? 0) + (statusCountQ.data?.statuses.length ?? 0);

  // Draft-review pending count for the Files-changed tab badge. This is the
  // usePendingReview logic inlined so its viewer/reviews queries wait for the
  // bootstrap (the hook itself would fire them on mount and race the seeding
  // with duplicate fetches); the Files tab's own usePendingReview then reads
  // the same keys as cache hits.
  const pendingViewerQ = useQuery({
    queryKey: ["viewer"],
    queryFn: fetchAuthenticatedUser,
    enabled: bootstrapSettled,
  });
  const pendingLogin =
    typeof pendingViewerQ.data?.login === "string" ? pendingViewerQ.data.login : null;
  const pendingReviewsQ = useQuery({
    queryKey: ["pr-reviews", owner, repo, number],
    queryFn: () => fetchPRReviews(owner, repo, number),
    enabled: bootstrapSettled,
  });
  const pendingReview =
    (Array.isArray(pendingReviewsQ.data) ? pendingReviewsQ.data : []).find(
      (r) => r.state === "PENDING" && r.user?.login != null && r.user.login === pendingLogin,
    ) ?? null;
  const pendingCommentsQ = useQuery({
    queryKey: ["pr-pending-review-comments", owner, repo, number, pendingReview?.id ?? 0],
    queryFn: () =>
      ghFetch<GithubPRReviewComment[]>(
        `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/reviews/${pendingReview?.id}/comments`,
      ),
    enabled: pendingReview != null,
  });
  const pendingComments =
    pendingReview && Array.isArray(pendingCommentsQ.data) ? pendingCommentsQ.data : [];

  // Title/body editing follows github.com's author-or-write rule; while
  // permissions load the neutral (hidden) state renders.
  const { canPush } = useRepoPermissions(owner, repo);
  const canEdit = canPush || (pendingLogin !== null && pendingLogin === pr?.user?.login);

  const invalidatePR = () => {
    qc.invalidateQueries({ queryKey: ["pr", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["prs", owner, repo] });
  };
  const stateMut = useMutation({
    mutationFn: () =>
      updatePull(owner, repo, number, { state: pr?.state === "open" ? "closed" : "open" }),
    onSuccess: invalidatePR,
  });
  const [editing, setEditing] = useState(false);
  const [editTitle, setEditTitle] = useState("");
  const [editBody, setEditBody] = useState("");
  const editMut = useMutation({
    mutationFn: () => updatePull(owner, repo, number, { title: editTitle, body: editBody }),
    onSuccess: () => {
      invalidatePR();
      setEditing(false);
    },
  });

  if (isError) {
    if (isNotFound(error)) {
      return (
        <div>
          <RepoHeader owner={owner} repo={repo} active="pulls" {...counts} />
          <Blankslate
            icon={<PullRequestIcon size={26} />}
            title={`Pull request #${number} not found`}
          >
            It may have been deleted, or the number may be wrong.
          </Blankslate>
        </div>
      );
    }
    return <InlineError title={`Failed to load PR #${number}`} detail={String(error)} />;
  }
  if (isLoading || !pr) return <Spinner label={`loading PR #${number}`} />;

  const s = prState(pr);
  const stateLabel = s === "merged" ? "Merged" : s === "closed" ? "Closed" : s === "draft" ? "Draft" : "Open";

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="pulls" {...counts} />

      <div className="mb-2 flex flex-wrap items-baseline gap-2">
        <h1 style={{ fontSize: "1.5rem", fontWeight: 400, color: "var(--color-fg)" }}>
          {pr.title} <span style={{ color: "var(--color-fg-muted)" }}>#{pr.number}</span>
        </h1>
        {canEdit && (
          <Button
            size="sm"
            onClick={() => {
              setEditTitle(pr.title);
              setEditBody(pr.body ?? "");
              setEditing(true);
            }}
          >
            Edit
          </Button>
        )}
      </div>

      {editing && (
        <Modal title="Edit pull request" onClose={() => setEditing(false)}>
          <FormLabel id="edit-pr-title">Title</FormLabel>
          <input
            id="edit-pr-title"
            autoFocus
            value={editTitle}
            onChange={(e) => setEditTitle(e.target.value)}
            className="mb-3 w-full"
          />
          <FormLabel id="edit-pr-body">Description</FormLabel>
          <textarea
            id="edit-pr-body"
            value={editBody}
            onChange={(e) => setEditBody(e.target.value)}
            rows={6}
            className="mb-4 w-full"
            style={{ resize: "vertical" }}
          />
          <MutationError of={editMut} />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setEditing(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="sm"
              disabled={!editTitle.trim() || editMut.isPending}
              onClick={() => editMut.mutate()}
            >
              {editMut.isPending ? "Saving…" : "Save"}
            </Button>
          </DialogActions>
        </Modal>
      )}
      <div className="mb-4 flex flex-wrap items-center gap-3">
        <StateLabel state={s} icon={<PRStateIcon pr={pr} size={15} />}>
          {stateLabel}
        </StateLabel>
        <span style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          <strong style={{ color: "var(--color-fg)" }}>{pr.user?.login}</strong> wants to merge{" "}
          {pr.commits != null && (
            <>
              {pr.commits} commit{pr.commits === 1 ? "" : "s"}{" "}
            </>
          )}
          into{" "}
          <span className="font-mono" style={{ color: "var(--color-accent)" }}>{pr.base.ref}</span>
          {" from "}
          <span className="font-mono" style={{ color: "var(--color-accent)" }}>{pr.head.ref}</span>
        </span>
      </div>

      {pr.changed_files != null && (
        <div
          className="mb-4"
          style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}
        >
          <strong style={{ color: "var(--color-fg)" }}>
            {pr.changed_files} changed {pr.changed_files === 1 ? "file" : "files"}
          </strong>
          {" with "}
          <span style={{ color: "var(--gh-open)" }}>+{pr.additions ?? 0}</span>
          {" "}
          <span style={{ color: "var(--color-status-error)" }}>−{pr.deletions ?? 0}</span>
        </div>
      )}

      <Tabs
        active={tab}
        onChange={(next) => {
          const base = `/ui/repos/${owner}/${repo}/pulls/${number}`;
          navigate(next === "conversation" ? base : `${base}/${next}`);
        }}
        items={[
          { key: "conversation", label: "Conversation" },
          {
            key: "commits",
            label: pr.commits != null ? `Commits ${pr.commits}` : "Commits",
          },
          {
            key: "files",
            label: (
              <>
                {pr.changed_files != null ? `Files changed ${pr.changed_files}` : "Files changed"}
                {pendingComments.length > 0 && (
                  <span
                    style={{
                      marginLeft: "0.4rem",
                      border: "1px solid var(--color-status-warn)",
                      color: "var(--color-status-warn)",
                      borderRadius: "2rem",
                      padding: "0.02rem 0.45rem",
                      fontSize: "0.72rem",
                      fontWeight: 600,
                    }}
                  >
                    Pending {pendingComments.length}
                  </span>
                )}
              </>
            ),
          },
          { key: "checks", label: checksCount > 0 ? `Checks ${checksCount}` : "Checks" },
        ]}
      />

      {tab === "conversation" && (
        <ConversationTab owner={owner} repo={repo} number={number} pr={pr} stateMut={stateMut} />
      )}
      {tab === "commits" && <PRCommitsTab owner={owner} repo={repo} number={number} />}
      {tab === "files" && (
        <PRFilesView owner={owner} repo={repo} number={number} headSha={pr.head.sha} />
      )}
      {tab === "checks" && <ChecksSection owner={owner} repo={repo} sha={pr.head.sha} standalone />}
    </div>
  );
}

// ─── Conversation tab (stream + sidebar) ─────────────────────────────────

function ConversationTab({
  owner,
  repo,
  number,
  pr,
  stateMut,
}: {
  owner: string;
  repo: string;
  number: number;
  pr: GithubPR;
  stateMut: { isPending: boolean; mutate: () => void; isError: boolean; error: unknown };
}) {
  const viewerQ = useQuery({ queryKey: ["viewer"], queryFn: fetchAuthenticatedUser });
  const viewerLogin = typeof viewerQ.data?.login === "string" ? viewerQ.data.login : null;
  const s = prState(pr);

  // github.com's viewer-role rules: merge-area actions need write access;
  // closing/reopening (like ready-for-review) follow author-or-write.
  const { canPush } = useRepoPermissions(owner, repo);
  const isPRAuthor = viewerLogin !== null && viewerLogin === pr.user?.login;
  const canClose = canPush || isPRAuthor;

  const timelineQ = useQuery({
    queryKey: ["pr-timeline", owner, repo, number],
    queryFn: () => fetchIssueTimeline(owner, repo, number),
  });
  const reviewsQ = useQuery({
    queryKey: ["pr-reviews", owner, repo, number],
    queryFn: () => fetchPRReviews(owner, repo, number),
  });
  const commentsQ = useQuery({
    queryKey: ["pr-review-comments", owner, repo, number],
    queryFn: () => fetchPRReviewComments(owner, repo, number),
  });
  const threadsQ = useQuery({
    queryKey: ["pr-review-threads", owner, repo, number],
    queryFn: () => fetchPRReviewThreads(owner, repo, number),
  });

  const timeline = Array.isArray(timelineQ.data) ? timelineQ.data : [];
  const reviews = Array.isArray(reviewsQ.data) ? reviewsQ.data : [];
  const allComments = Array.isArray(commentsQ.data) ? commentsQ.data : [];

  // Everyone who authored something in the conversation: the PR author,
  // conversation commenters, reviewers, and inline-thread commenters.
  const participants = useMemo(() => {
    const logins = new Set<string>();
    if (pr.user?.login) logins.add(pr.user.login);
    for (const item of timeline) {
      if ((item.event === "commented" || item.event === "reviewed") && item.user?.login) {
        logins.add(item.user.login);
      }
    }
    for (const r of reviews) {
      if (r.state !== "PENDING" && r.user?.login) logins.add(r.user.login);
    }
    for (const c of allComments) {
      if (c.user?.login) logins.add(c.user.login);
    }
    return [...logins];
  }, [pr.user?.login, timeline, reviews, allComments]);

  return (
    <div className="flex flex-col gap-6 lg:flex-row">
      <div className="min-w-0 flex-1">
        {viewerQ.isError && (
          <InlineError inline title="Failed to load current user" detail={String(viewerQ.error)} />
        )}
        <CommentCard login={pr.user?.login} body={pr.body} date={pr.created_at} isOp />
        <ReactionBar
          queryKey={["pr-body-reactions", owner, repo, number]}
          fetchList={() => fetchIssueReactions(owner, repo, number)}
          add={(content) => addIssueReaction(owner, repo, number, content)}
          remove={(reactionId) => removeIssueReaction(owner, repo, number, reactionId)}
          viewerLogin={viewerLogin}
        />
        <ConversationStream
          owner={owner}
          repo={repo}
          number={number}
          viewerLogin={viewerLogin}
          timeline={timeline}
          timelineError={timelineQ.isError ? String(timelineQ.error) : null}
          timelineLoading={timelineQ.isLoading}
          reviews={reviews}
          reviewsError={reviewsQ.isError ? String(reviewsQ.error) : null}
          comments={allComments}
          commentsError={commentsQ.isError ? String(commentsQ.error) : null}
          threadsError={threadsQ.isError ? String(threadsQ.error) : null}
          threads={threadsQ.data ?? []}
        />
        <MergeBox owner={owner} repo={repo} number={number} pr={pr} canPush={canPush} isPRAuthor={isPRAuthor} />
        <MutationError of={stateMut} />
        <CommentComposer
          owner={owner}
          repo={repo}
          number={number}
          invalidateKeys={[
            ["pr-timeline", owner, repo, number],
            ["pr", owner, repo, number],
          ]}
          extraActions={
            s !== "merged" &&
            canClose && (
              <Button
                size="sm"
                disabled={stateMut.isPending}
                onClick={() => stateMut.mutate()}
              >
                {s === "closed" ? "Reopen pull request" : "Close pull request"}
              </Button>
            )
          }
        />
      </div>
      <div style={{ width: "100%", maxWidth: "16rem", flexShrink: 0 }}>
        <IssueSidebar
          owner={owner}
          repo={repo}
          number={number}
          kind="pr"
          assignees={(pr.assignees ?? []).map((a) => a.login)}
          labels={pr.labels}
          milestone={pr.milestone ?? null}
          participants={participants}
          reviewers={<RequestedReviewersSection owner={owner} repo={repo} number={number} />}
          development={
            <DevelopmentSection
              owner={owner}
              repo={repo}
              number={number}
              headRef={pr.head.ref}
              baseRef={pr.base.ref}
            />
          }
        />
      </div>
    </div>
  );
}

// ─── Development section (sidebar: branch + linked/closing issues) ───────

const CLOSING_ISSUES_QUERY = `query($owner:String!,$repo:String!,$number:Int!){
  repository(owner:$owner,name:$repo){
    pullRequest(number:$number){
      closingIssuesReferences(first:20){
        nodes { number title state url }
      }
    }
  }
}`;

interface ClosingIssueNode {
  number: number;
  title: string;
  state: string;
  url: string;
}

interface ClosingIssuesResponse {
  repository?: {
    pullRequest?: {
      closingIssuesReferences?: {
        nodes?: (ClosingIssueNode | null)[] | null;
      } | null;
    } | null;
  } | null;
}

function DevelopmentSection({
  owner,
  repo,
  number,
  headRef,
  baseRef,
}: {
  owner: string;
  repo: string;
  number: number;
  headRef: string;
  baseRef: string;
}) {
  const q = useQuery({
    queryKey: ["pr-closing-issues", owner, repo, number],
    queryFn: ({ signal }) =>
      ghGraphQL<ClosingIssuesResponse>(
        CLOSING_ISSUES_QUERY,
        { owner, repo, number },
        signal,
      ),
  });
  const nodes = q.data?.repository?.pullRequest?.closingIssuesReferences?.nodes;
  const issues = (nodes ?? []).filter((n): n is ClosingIssueNode => n != null);

  return (
    <div style={{ fontSize: "0.82rem", color: "var(--color-fg)" }}>
      <span>
        <span className="font-mono" style={{ color: "var(--color-accent)" }}>
          {headRef}
        </span>
        {" → "}
        <span className="font-mono">{baseRef}</span>
      </span>
      {issues.length > 0 && (
        <ul className="mt-2 flex flex-col gap-1" style={{ listStyle: "none", margin: 0, padding: 0 }}>
          {issues.map((issue) => {
            const isOpen = String(issue.state).toUpperCase() === "OPEN";
            return (
              <li key={issue.number}>
                <Link
                  to={`/ui/repos/${owner}/${repo}/issues/${issue.number}`}
                  className="inline-flex items-start gap-1.5"
                  style={{ textDecoration: "none", color: "var(--color-fg)" }}
                >
                  <span style={{ marginTop: "0.15rem", flexShrink: 0 }}>
                    {isOpen ? (
                      <IssueOpenedIcon size={13} style={{ color: "var(--gh-open)" }} />
                    ) : (
                      <IssueClosedIcon size={13} style={{ color: "var(--gh-closed)" }} />
                    )}
                  </span>
                  <span className="min-w-0">
                    <span style={{ color: "var(--color-fg-muted)" }}>#{issue.number}</span>{" "}
                    {issue.title}
                  </span>
                </Link>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

// ─── Merge box (bottom of Conversation) ──────────────────────────────────

const MERGE_METHODS: { value: "merge" | "squash" | "rebase"; label: string }[] = [
  { value: "merge", label: "Create a merge commit" },
  { value: "squash", label: "Squash and merge" },
  { value: "rebase", label: "Rebase and merge" },
];

function defaultCommitTitle(
  method: "merge" | "squash" | "rebase",
  owner: string,
  pr: GithubPR,
): string {
  if (method === "squash") return `${pr.title} (#${pr.number})`;
  return `Merge pull request #${pr.number} from ${owner}/${pr.head.ref}`;
}

function defaultCommitMessage(method: "merge" | "squash" | "rebase", pr: GithubPR): string {
  return method === "merge" ? pr.title : "";
}

/** Delete/restore the head branch after a merge (GitHub's merged-box actions). */
function MergedBranchActions({
  owner,
  repo,
  pr,
}: {
  owner: string;
  repo: string;
  pr: GithubPR;
}) {
  const qc = useQueryClient();
  const { canPush } = useRepoPermissions(owner, repo);
  const branchesQ = useQuery({
    queryKey: ["branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
  });
  const invalidate = () => qc.invalidateQueries({ queryKey: ["branches", owner, repo] });
  const deleteMut = useMutation({
    mutationFn: () => deleteRef(owner, repo, `heads/${pr.head.ref}`),
    onSuccess: invalidate,
  });
  const restoreMut = useMutation({
    mutationFn: () => createRef(owner, repo, `refs/heads/${pr.head.ref}`, pr.head.sha),
    onSuccess: invalidate,
  });

  // Cross-repo heads and the base branch itself are not deletable from here;
  // branch deletion/restoration needs push, like the branches tab.
  if (!canPush || !branchesQ.data || pr.head.ref === pr.base.ref) return null;
  const exists = branchesQ.data.some((b) => b.name === pr.head.ref);

  return (
    <div className="flex flex-col items-end gap-1">
      {exists ? (
        <Button size="sm" disabled={deleteMut.isPending} onClick={() => deleteMut.mutate()}>
          {deleteMut.isPending ? "Deleting…" : "Delete branch"}
        </Button>
      ) : (
        <Button size="sm" disabled={restoreMut.isPending} onClick={() => restoreMut.mutate()}>
          {restoreMut.isPending ? "Restoring…" : "Restore branch"}
        </Button>
      )}
      {deleteMut.isError && (
        <InlineError inline title="Failed to delete branch" detail={String(deleteMut.error)} />
      )}
      {restoreMut.isError && (
        <InlineError inline title="Failed to restore branch" detail={String(restoreMut.error)} />
      )}
    </div>
  );
}

function MergeBox({
  owner,
  repo,
  number,
  pr,
  canPush,
  isPRAuthor,
}: {
  owner: string;
  repo: string;
  number: number;
  pr: GithubPR;
  /** Write access: merge/auto-merge/update-branch/convert-to-draft need it. */
  canPush: boolean;
  /** "Ready for review" follows github.com's author-or-write rule. */
  isPRAuthor: boolean;
}) {
  const qc = useQueryClient();
  const [method, setMethod] = useState<"merge" | "squash" | "rebase">("merge");
  // GitHub's two-step flow: the merge (or enable-auto-merge) button opens a
  // confirmation panel with the editable commit title/message; only
  // "Confirm …" performs the mutation.
  const [confirming, setConfirming] = useState<false | "merge" | "auto-merge">(false);
  // null = "use the GitHub-style default for the chosen method".
  const [commitTitle, setCommitTitle] = useState<string | null>(null);
  const [commitMessage, setCommitMessage] = useState<string | null>(null);
  const effectiveTitle = commitTitle ?? defaultCommitTitle(method, owner, pr);
  const effectiveMessage = commitMessage ?? defaultCommitMessage(method, pr);
  const checksQ = useQuery({
    queryKey: ["check-runs", owner, repo, pr.head.sha],
    queryFn: () => fetchCheckRuns(owner, repo, pr.head.sha),
    enabled: !!pr.head.sha,
  });
  const statusQ = useQuery({
    queryKey: ["combined-status", owner, repo, pr.head.sha],
    queryFn: ({ signal }) => fetchCombinedStatus(owner, repo, pr.head.sha, signal),
    enabled: !!pr.head.sha,
  });
  const mergeMutation = useMutation({
    mutationFn: () =>
      ghSend(
        "PUT",
        `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/merge`,
        {
          merge_method: method,
          ...(method !== "rebase" && effectiveTitle.trim()
            ? { commit_title: effectiveTitle.trim() }
            : {}),
          ...(method !== "rebase" && effectiveMessage.trim()
            ? { commit_message: effectiveMessage.trim() }
            : {}),
        },
      ),
    onSuccess: () => {
      // Stay on the PR — the refetched detail flips to the merged state
      // (GitHub keeps you on the page and shows the merged box).
      setConfirming(false);
      qc.invalidateQueries({ queryKey: ["prs", owner, repo] });
      qc.invalidateQueries({ queryKey: ["pr", owner, repo, number] });
      qc.invalidateQueries({ queryKey: ["pr-timeline", owner, repo, number] });
      qc.invalidateQueries({ queryKey: ["branches", owner, repo] });
    },
  });
  const updateBranchMut = useMutation({
    mutationFn: () => updatePRBranch(owner, repo, number),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["pr", owner, repo, number] }),
  });
  // Whether the repo allows auto-merge, read from the repo detail RepoHeader
  // already fetched (enabled: false makes this a read-only cache observer).
  const repoQ = useQuery({
    queryKey: ["repo", owner, repo],
    queryFn: ({ signal }) => fetchRepoDetail(owner, repo, signal),
    enabled: false,
  });
  const invalidateAutoMerge = () => {
    setConfirming(false);
    qc.invalidateQueries({ queryKey: ["pr", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["pr-timeline", owner, repo, number] });
  };
  const enableAutoMergeMut = useMutation({
    mutationFn: () =>
      ghGraphQL(
        `mutation($input: EnablePullRequestAutoMergeInput!) { enablePullRequestAutoMerge(input: $input) { clientMutationId } }`,
        {
          input: {
            pullRequestId: pr.node_id,
            mergeMethod: method.toUpperCase(),
            ...(method !== "rebase" && effectiveTitle.trim()
              ? { commitHeadline: effectiveTitle.trim() }
              : {}),
            ...(method !== "rebase" && effectiveMessage.trim()
              ? { commitBody: effectiveMessage.trim() }
              : {}),
          },
        },
      ),
    onSuccess: invalidateAutoMerge,
  });
  const disableAutoMergeMut = useMutation({
    mutationFn: () =>
      ghGraphQL(
        `mutation($input: DisablePullRequestAutoMergeInput!) { disablePullRequestAutoMerge(input: $input) { clientMutationId } }`,
        { input: { pullRequestId: pr.node_id } },
      ),
    onSuccess: invalidateAutoMerge,
  });
  const invalidatePR = () => qc.invalidateQueries({ queryKey: ["pr", owner, repo, number] });
  const readyMut = useMutation({
    mutationFn: () => markPullRequestReadyForReview(pr.node_id),
    onSuccess: invalidatePR,
  });
  const draftMut = useMutation({
    mutationFn: () => convertPullRequestToDraft(pr.node_id),
    onSuccess: invalidatePR,
  });

  const s = prState(pr);
  if (s === "merged") {
    return (
      <div className="mt-4">
        <Box>
          <div className="flex flex-wrap items-center gap-2" style={{ padding: "0.85rem 1rem" }}>
            <MergedIcon size={18} style={{ color: "var(--gh-merged)" }} />
            <span className="min-w-0 flex-1" style={{ fontWeight: 600, color: "var(--gh-merged)" }}>
              Pull request successfully merged and closed
            </span>
            <MergedBranchActions owner={owner} repo={repo} pr={pr} />
          </div>
        </Box>
      </div>
    );
  }
  if (s === "closed") {
    return (
      <div className="mt-4">
        <Box>
          <div className="flex items-center gap-2" style={{ padding: "0.85rem 1rem" }}>
            <PullClosedIcon size={18} style={{ color: "var(--gh-closed)" }} />
            <span style={{ fontWeight: 600, color: "var(--color-fg)" }}>
              This pull request is closed
            </span>
          </div>
        </Box>
      </div>
    );
  }

  const checks = checksQ.data?.items ?? [];
  const statuses = statusQ.data?.statuses ?? [];
  const summary = mergeBoxSummary(checks, statuses);
  const mergeBlocked = pr.mergeable_state === "blocked" || pr.draft;
  // Auto-merge only arms while merging is not currently possible (GitHub
  // refuses to arm a clean PR), and only when the repo allows it.
  const notMergeableNow =
    pr.mergeable_state === "blocked" ||
    pr.mergeable_state === "unstable" ||
    pr.mergeable_state === "unknown";
  const canEnableAutoMerge =
    repoQ.data?.allow_auto_merge === true && notMergeableNow && !pr.draft && !pr.auto_merge;

  return (
    <div className="mt-4">
      <Box>
        <div style={{ padding: "0.85rem 1rem" }}>
          {(checks.length > 0 || statuses.length > 0) && (
            <div className="mb-2 flex items-center gap-2" style={{ color: summary.color, fontWeight: 600 }}>
              {summary.pending ? (
                <DotFillIcon size={16} />
              ) : summary.color === "var(--gh-open)" ? (
                <CheckCircleIcon size={16} />
              ) : (
                <XCircleIcon size={16} />
              )}
              {summary.label}
            </div>
          )}
          <div
            className="mb-2 flex items-center gap-2"
            style={{ fontSize: "0.86rem", color: pr.draft ? "var(--color-fg-muted)" : "var(--gh-open)" }}
          >
            {pr.draft ? (
              <>
                <PullRequestIcon size={16} /> This pull request is still a work in progress
              </>
            ) : mergeBlocked ? (
              <span style={{ color: "var(--color-status-error)" }}>
                Merging is blocked — required checks must pass
              </span>
            ) : (
              <>
                <CheckCircleIcon size={16} /> This branch has no conflicts with the base branch
              </>
            )}
          </div>
          {pr.auto_merge && (
            <div
              className="mb-2 flex flex-wrap items-center gap-2"
              style={{ fontSize: "0.86rem", color: "var(--color-fg)" }}
            >
              <span style={{ fontWeight: 600 }}>
                Auto-merge enabled by {pr.auto_merge.enabled_by?.login ?? "unknown"} (
                {pr.auto_merge.merge_method})
              </span>
              {canPush && (
                <Button
                  size="sm"
                  disabled={disableAutoMergeMut.isPending}
                  onClick={() => disableAutoMergeMut.mutate()}
                >
                  {disableAutoMergeMut.isPending ? "Disabling…" : "Disable auto-merge"}
                </Button>
              )}
            </div>
          )}
          {canPush && confirming && method !== "rebase" && (
            <div className="mb-2">
              <FormLabel id="merge-commit-title">Commit title</FormLabel>
              <input
                id="merge-commit-title"
                value={effectiveTitle}
                onChange={(e) => setCommitTitle(e.target.value)}
                disabled={mergeMutation.isPending}
                className="mb-2 w-full"
                style={{ fontSize: "0.84rem" }}
              />
              <FormLabel id="merge-commit-message">Commit message</FormLabel>
              <textarea
                id="merge-commit-message"
                value={effectiveMessage}
                onChange={(e) => setCommitMessage(e.target.value)}
                disabled={mergeMutation.isPending}
                rows={2}
                placeholder="Add an optional extended description…"
                className="w-full"
                style={{ fontSize: "0.84rem", resize: "vertical" }}
              />
            </div>
          )}
          {!canPush ? (
            // github.com's read-only merge box: the checks/conflict status
            // above stays visible, the action row is replaced by the notice.
            <>
              <div style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
                Only those with write access to this repository can merge pull requests.
              </div>
              {pr.draft && isPRAuthor && (
                <div className="mt-2">
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={readyMut.isPending}
                    onClick={() => readyMut.mutate()}
                  >
                    {readyMut.isPending ? "…" : "Ready for review"}
                  </Button>
                </div>
              )}
            </>
          ) : (
          <div className="flex flex-wrap items-center gap-2">
            {confirming === "merge" ? (
              <>
                <Button
                  variant="primary"
                  size="sm"
                  disabled={mergeMutation.isPending}
                  onClick={() => mergeMutation.mutate()}
                >
                  {mergeMutation.isPending
                    ? "Merging…"
                    : method === "squash"
                      ? "Confirm squash and merge"
                      : method === "rebase"
                        ? "Confirm rebase and merge"
                        : "Confirm merge"}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={mergeMutation.isPending}
                  onClick={() => setConfirming(false)}
                >
                  Cancel
                </Button>
              </>
            ) : confirming === "auto-merge" ? (
              <>
                <Button
                  variant="primary"
                  size="sm"
                  disabled={enableAutoMergeMut.isPending}
                  onClick={() => enableAutoMergeMut.mutate()}
                >
                  {enableAutoMergeMut.isPending ? "Enabling…" : "Confirm auto-merge"}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={enableAutoMergeMut.isPending}
                  onClick={() => setConfirming(false)}
                >
                  Cancel
                </Button>
              </>
            ) : (
              <>
                <Button
                  variant="primary"
                  size="sm"
                  disabled={mergeBlocked}
                  onClick={() => setConfirming("merge")}
                >
                  {method === "squash"
                    ? "Squash and merge"
                    : method === "rebase"
                      ? "Rebase and merge"
                      : "Merge pull request"}
                </Button>
                {canEnableAutoMerge && (
                  <Button variant="primary" size="sm" onClick={() => setConfirming("auto-merge")}>
                    Enable auto-merge
                  </Button>
                )}
              </>
            )}
            <select
              aria-label="Merge method"
              value={method}
              onChange={(e) => {
                setMethod(e.target.value as "merge" | "squash" | "rebase");
                // A new method gets its own GitHub-style defaults.
                setCommitTitle(null);
                setCommitMessage(null);
              }}
              disabled={mergeMutation.isPending}
              style={{ fontSize: "0.82rem" }}
            >
              {MERGE_METHODS.map((m) => (
                <option key={m.value} value={m.value}>
                  {m.label}
                </option>
              ))}
            </select>
            {pr.draft ? (
              <Button
                size="sm"
                variant="secondary"
                disabled={readyMut.isPending}
                onClick={() => readyMut.mutate()}
              >
                {readyMut.isPending ? "…" : "Ready for review"}
              </Button>
            ) : (
              <Button
                size="sm"
                variant="secondary"
                disabled={draftMut.isPending}
                onClick={() => draftMut.mutate()}
              >
                {draftMut.isPending ? "…" : "Convert to draft"}
              </Button>
            )}
            <Button
              variant="secondary"
              size="sm"
              disabled={updateBranchMut.isPending}
              onClick={() => updateBranchMut.mutate()}
            >
              {updateBranchMut.isPending ? "Updating…" : "Update branch"}
            </Button>
          </div>
          )}
          <MutationError of={[readyMut, draftMut]} />
          {updateBranchMut.isError && (
            <div className="mt-2" style={{ fontSize: "0.8rem", color: "var(--color-status-error)" }}>
              Update branch failed:{" "}
              {updateBranchMut.error instanceof Error ? updateBranchMut.error.message : "unknown error"}
            </div>
          )}
          {mergeMutation.isError && (
            <div className="mt-2" style={{ fontSize: "0.8rem", color: "var(--color-status-error)" }}>
              Merge failed:{" "}
              {mergeMutation.error instanceof Error ? mergeMutation.error.message : "unknown error"}
            </div>
          )}
          {enableAutoMergeMut.isError && (
            <div className="mt-2" style={{ fontSize: "0.8rem", color: "var(--color-status-error)" }}>
              {enableAutoMergeMut.error instanceof Error
                ? enableAutoMergeMut.error.message
                : "Failed to enable auto-merge"}
            </div>
          )}
          {disableAutoMergeMut.isError && (
            <div className="mt-2" style={{ fontSize: "0.8rem", color: "var(--color-status-error)" }}>
              Failed to disable auto-merge:{" "}
              {disableAutoMergeMut.error instanceof Error
                ? disableAutoMergeMut.error.message
                : "unknown error"}
            </div>
          )}
        </div>
      </Box>
    </div>
  );
}

// ─── Commits tab ─────────────────────────────────────────────────────────

/** Rolled-up check/status icon for one commit, hidden when nothing reported. */
function CommitChecksIcon({ owner, repo, sha }: { owner: string; repo: string; sha: string }) {
  const checksQ = useQuery({
    queryKey: ["check-runs", owner, repo, sha],
    queryFn: () => fetchCheckRuns(owner, repo, sha),
    enabled: !!sha,
  });
  const statusQ = useQuery({
    queryKey: ["combined-status", owner, repo, sha],
    queryFn: ({ signal }) => fetchCombinedStatus(owner, repo, sha, signal),
    enabled: !!sha,
  });
  const checks = checksQ.data?.items ?? [];
  const statuses = statusQ.data?.statuses ?? [];
  if (checks.length === 0 && statuses.length === 0) return null;
  const summary = mergeBoxSummary(checks, statuses);
  const icon = summary.pending ? (
    <DotFillIcon size={14} style={{ color: "var(--color-status-warn)" }} />
  ) : summary.color === "var(--gh-open)" ? (
    <CheckCircleIcon size={14} style={{ color: "var(--gh-open)" }} />
  ) : (
    <XCircleIcon size={14} style={{ color: "var(--color-status-error)" }} />
  );
  return (
    <span role="img" aria-label={summary.label} title={summary.label}>
      {icon}
    </span>
  );
}

function PRCommitsTab({ owner, repo, number }: { owner: string; repo: string; number: number }) {
  const q = useQuery({
    queryKey: ["pr-commits", owner, repo, number],
    queryFn: () => fetchPRCommits(owner, repo, number),
  });
  if (q.isLoading) return <Spinner label="loading commits" />;
  if (q.isError) return <InlineError title="Failed to load commits" detail={String(q.error)} />;
  const commits = q.data ?? [];
  if (commits.length === 0) {
    return <Blankslate icon={<BranchIcon size={26} />} title="No commits" />;
  }
  return (
    <Box>
      {commits.map((c, i) => {
        const authorLogin = c.author?.login ?? c.commit.author?.name ?? "?";
        return (
          <div
            key={c.sha}
            className="flex items-start gap-3"
            style={{
              padding: "0.7rem 1rem",
              borderBottom: i < commits.length - 1 ? "1px solid var(--color-border)" : "none",
            }}
          >
            <Avatar login={authorLogin} src={c.author?.avatar_url} size={20} />
            <div className="min-w-0 flex-1">
              <div style={{ fontSize: "0.88rem", fontWeight: 600, color: "var(--color-fg)" }}>
                {c.commit.message.split("\n")[0]}
              </div>
              <div className="mt-0.5" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                {authorLogin} committed <RelativeTime iso={c.commit.author?.date} />
              </div>
            </div>
            <CommitChecksIcon owner={owner} repo={repo} sha={c.sha} />
            <Link
              to={`/ui/repos/${owner}/${repo}/commits/${c.sha}`}
              className="font-mono tabular-nums inline-block"
              style={{
                fontSize: "0.76rem",
                lineHeight: "1.625rem",
                color: "var(--color-accent)",
                textDecoration: "none",
              }}
            >
              {c.sha.slice(0, 7)}
            </Link>
          </div>
        );
      })}
    </Box>
  );
}

// ─── Merge-box checks + commit statuses summary ──────────────────────────

function mergeBoxSummary(
  checks: GithubCheckRun[],
  statuses: GithubCommitStatus[],
): { label: string; color: string; pending: boolean } {
  const checkFailed = checks.some(
    (c) =>
      c.status === "completed" &&
      c.conclusion !== null &&
      !["success", "neutral", "skipped"].includes(c.conclusion),
  );
  const statusFailed = statuses.some((st) => st.state === "failure" || st.state === "error");
  if (checkFailed || statusFailed) {
    return { label: "Some checks were not successful", color: "var(--color-status-error)", pending: false };
  }
  const pending =
    checks.some((c) => c.status !== "completed") || statuses.some((st) => st.state === "pending");
  if (pending) {
    return { label: "Some checks haven't completed yet", color: "var(--color-status-warn)", pending: true };
  }
  return { label: "All checks have passed", color: "var(--gh-open)", pending: false };
}

/** Turn a check's details_url into an in-app run link when it points at a run. */
function runLinkFor(owner: string, repo: string, detailsUrl: string): string | null {
  const m = detailsUrl.match(/\/actions\/runs\/(\d+)/);
  if (!m) return null;
  return `/ui/repos/${owner}/${repo}/actions/runs/${m[1]}`;
}

function CommitStatusIcon({ state }: { state: GithubCommitStatusState }) {
  if (state === "success") {
    return <CheckCircleIcon size={15} style={{ color: "var(--gh-open)" }} />;
  }
  if (state === "failure" || state === "error") {
    return <XCircleIcon size={15} style={{ color: "var(--color-status-error)" }} />;
  }
  return <DotFillIcon size={15} style={{ color: "var(--color-status-warn)" }} />;
}

function ChecksSection({
  owner,
  repo,
  sha,
  standalone,
}: {
  owner: string;
  repo: string;
  sha: string;
  standalone?: boolean;
}) {
  const checksQ = useQuery({
    queryKey: ["check-runs", owner, repo, sha],
    queryFn: () => fetchCheckRuns(owner, repo, sha),
    enabled: !!sha,
    refetchInterval: (query) =>
      query.state.data?.items.some((c) => c.status !== "completed") ? 5000 : false,
  });
  const statusQ = useQuery({
    queryKey: ["combined-status", owner, repo, sha],
    queryFn: ({ signal }) => fetchCombinedStatus(owner, repo, sha, signal),
    enabled: !!sha,
    refetchInterval: (query) =>
      query.state.data?.statuses.some((st) => st.state === "pending") ? 5000 : false,
  });

  if (checksQ.isLoading || statusQ.isLoading) {
    return standalone ? <Spinner label="loading checks" /> : null;
  }
  if (checksQ.isError) {
    return <InlineError title="Failed to load checks" detail={String(checksQ.error)} />;
  }
  const checks = checksQ.data?.items ?? [];
  const statuses = statusQ.data?.statuses ?? [];
  // GitHub hides the checks box entirely for commits with neither check
  // runs nor commit statuses.
  if (statusQ.isError && checks.length === 0) {
    return <InlineError title="Failed to load commit statuses" detail={String(statusQ.error)} />;
  }
  if (checks.length === 0 && statuses.length === 0) {
    return standalone ? (
      <Blankslate icon={<CheckCircleIcon size={26} />} title="No checks reported for this commit" />
    ) : null;
  }

  const summary = mergeBoxSummary(checks, statuses);
  const rowStyle = (last: boolean) =>
    ({
      padding: "0.55rem 1rem",
      borderBottom: last ? "none" : "1px solid var(--color-border)",
      textDecoration: "none",
    }) as const;

  return (
    <div className="mb-4">
      {statusQ.isError && (
        <InlineError inline title="Failed to load commit statuses" detail={String(statusQ.error)} />
      )}
      <Box
        header={
          <span className="inline-flex items-center gap-2" style={{ color: summary.color, fontWeight: 600 }}>
            {summary.pending && (
              <span
                aria-hidden
                className="animate-spin inline-block"
                style={{
                  width: 12,
                  height: 12,
                  border: "2px solid var(--color-status-warn)",
                  borderTopColor: "transparent",
                  borderRadius: "999px",
                }}
              />
            )}
            {summary.label}
          </span>
        }
      >
        {statuses.map((status, i) => {
          const last = i === statuses.length - 1 && checks.length === 0;
          const row = (
            <>
              <CommitStatusIcon state={status.state} />
              <span className="min-w-0 flex-1 truncate" style={{ fontSize: "0.86rem", color: "var(--color-fg)" }}>
                {status.context}
                {status.description && (
                  <span style={{ color: "var(--color-fg-muted)" }}> — {status.description}</span>
                )}
              </span>
            </>
          );
          return status.target_url ? (
            <a key={status.context} href={status.target_url} className="flex items-center gap-2" style={rowStyle(last)}>
              {row}
            </a>
          ) : (
            <div key={status.context} className="flex items-center gap-2" style={rowStyle(last)}>
              {row}
            </div>
          );
        })}
        {checks.map((check, i) => {
          const last = i === checks.length - 1;
          const runLink = runLinkFor(owner, repo, check.details_url);
          const row = (
            <>
              <RunStatusIcon status={check.status} conclusion={check.conclusion} size={15} />
              <span className="min-w-0 flex-1 truncate" style={{ fontSize: "0.86rem", color: "var(--color-fg)" }}>
                {check.name}
              </span>
              <span className="tabular-nums" style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}>
                {formatDuration(check.started_at, check.completed_at)}
              </span>
            </>
          );
          return runLink ? (
            <Link key={check.id} to={runLink} className="flex items-center gap-2" style={rowStyle(last)}>
              {row}
            </Link>
          ) : check.details_url ? (
            <a key={check.id} href={check.details_url} className="flex items-center gap-2" style={rowStyle(last)}>
              {row}
            </a>
          ) : (
            <div key={check.id} className="flex items-center gap-2" style={rowStyle(last)}>
              {row}
            </div>
          );
        })}
      </Box>
    </div>
  );
}

// ─── Requested reviewers ─────────────────────────────────────────────────

function reviewerStateIcon(state: GithubReviewState | null): ReactNode {
  if (state === "APPROVED") {
    return <CheckCircleIcon size={13} style={{ color: "var(--gh-open)" }} />;
  }
  if (state === "CHANGES_REQUESTED") {
    return <XCircleIcon size={13} style={{ color: "var(--color-status-error)" }} />;
  }
  return <DotFillIcon size={13} style={{ color: "var(--color-status-warn)" }} />;
}

function RequestedReviewersSection({
  owner,
  repo,
  number,
}: {
  owner: string;
  repo: string;
  number: number;
}) {
  const qc = useQueryClient();
  // Requesting, re-requesting, and removing reviewers needs write access —
  // github.com shows read-only reviewer chips to everyone else.
  const { canPush } = useRepoPermissions(owner, repo);
  const q = useQuery({
    queryKey: ["pr-requested-reviewers", owner, repo, number],
    queryFn: ({ signal }) => fetchPRRequestedReviewers(owner, repo, number, signal),
  });
  // Latest submitted verdict per reviewer, for the per-reviewer state icons.
  const reviewsQ = useQuery({
    queryKey: ["pr-reviews", owner, repo, number],
    queryFn: () => fetchPRReviews(owner, repo, number),
  });
  // Reviewers must be repo collaborators; a free-text login just produced a 422
  // on a typo. Offer the assignable users the same way IssueSidebar offers
  // assignees.
  const { data: assignableUsers = [] } = useQuery({
    queryKey: ["assignable-users", owner, repo],
    queryFn: () => fetchAssignableUsers(owner, repo),
  });
  // Teams belonging to the repo's owning org, for the team-reviewer picker.
  // Only orgs have a teams endpoint, so gate on the owner type from the
  // already-cached repo detail (RepoHeader owns that fetch; enabled: false
  // makes this a read-only cache observer) instead of issuing a fetch that is
  // guaranteed to fail on user-owned repos.
  const repoQ = useQuery({
    queryKey: ["repo", owner, repo],
    queryFn: ({ signal }) => fetchRepoDetail(owner, repo, signal),
    enabled: false,
  });
  const { data: orgTeams = [] } = useQuery({
    queryKey: ["org-teams", owner],
    queryFn: () => fetchOrgTeams(owner),
    retry: false,
    enabled: repoQ.data?.owner?.type === "Organization",
  });
  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["pr-requested-reviewers", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["pr-reviews", owner, repo, number] });
  };
  const add = useMutation({
    mutationFn: (l: string) => requestPRReviewers(owner, repo, number, [l]),
    onSuccess: invalidate,
  });
  const remove = useMutation({
    mutationFn: (l: string) => removePRRequestedReviewers(owner, repo, number, [l]),
    onSuccess: invalidate,
  });
  const addTeam = useMutation({
    mutationFn: (slug: string) => requestPRReviewers(owner, repo, number, [], [slug]),
    onSuccess: invalidate,
  });
  const removeTeam = useMutation({
    mutationFn: (slug: string) => removePRRequestedReviewers(owner, repo, number, [], [slug]),
    onSuccess: invalidate,
  });

  if (q.isLoading) return null;

  const requestedLogins = new Set((q.data?.users ?? []).map((u) => u.login));
  const addableReviewers = assignableUsers.filter((u) => !requestedLogins.has(u.login));
  const requestedTeamSlugs = new Set((q.data?.teams ?? []).map((t) => t.slug));
  const addableTeams = orgTeams.filter((t) => !requestedTeamSlugs.has(t.slug));

  // Latest non-pending verdict per login. Reviews come oldest-first, so the
  // last write wins.
  const verdictByLogin = new Map<string, GithubReviewState>();
  const reviews = Array.isArray(reviewsQ.data) ? reviewsQ.data : [];
  for (const r of reviews) {
    if (r.state === "PENDING" || !r.user?.login) continue;
    if (r.state === "DISMISSED") {
      verdictByLogin.delete(r.user.login);
      continue;
    }
    verdictByLogin.set(r.user.login, r.state);
  }
  // Reviewers who already submitted a verdict but are not (or no longer)
  // requested — shown with their state icon and a re-request affordance.
  const reviewedNotRequested = [...verdictByLogin.entries()].filter(
    ([login]) => !requestedLogins.has(login),
  );

  const chipStyle = {
    border: "1px solid var(--color-border)",
    borderRadius: "2rem",
    padding: "0.15rem 0.3rem 0.15rem 0.6rem",
    fontSize: "0.8rem",
    color: "var(--color-fg)",
  } as const;

  // Rendered inside the sidebar's "Reviewers" section, which supplies the
  // heading — so this body carries none of its own.
  return (
    <div>
      {q.isError || !q.data ? (
        <InlineError inline title="Failed to load requested reviewers" detail={String(q.error)} />
      ) : (
        <div className="flex flex-wrap items-center gap-2">
          {q.data.users.length === 0 && q.data.teams.length === 0 && reviewedNotRequested.length === 0 && (
            <span style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
              No reviewers requested.
            </span>
          )}
          {q.data.users.map((u) => (
            <span key={u.login} className="inline-flex items-center gap-1.5" style={chipStyle}>
              {reviewerStateIcon(verdictByLogin.get(u.login) ?? null)}
              {u.login}
              {canPush && (
              <button
                type="button"
                aria-label={`remove reviewer ${u.login}`}
                disabled={remove.isPending}
                onClick={() => remove.mutate(u.login)}
                style={{
                  border: "none",
                  background: "transparent",
                  cursor: "pointer",
                  color: "var(--color-fg-muted)",
                  fontSize: "0.85rem",
                  lineHeight: 1,
                  padding: "0.1rem 0.3rem",
                }}
              >
                ✕
              </button>
              )}
            </span>
          ))}
          {reviewedNotRequested.map(([login, state]) => (
            <span key={login} className="inline-flex items-center gap-1.5" style={chipStyle}>
              {reviewerStateIcon(state)}
              {login}
              {canPush && (
              <button
                type="button"
                aria-label={`re-request review from ${login}`}
                title={`Re-request review from ${login}`}
                disabled={add.isPending}
                onClick={() => add.mutate(login)}
                style={{
                  border: "none",
                  background: "transparent",
                  cursor: "pointer",
                  color: "var(--color-fg-muted)",
                  fontSize: "0.85rem",
                  lineHeight: 1,
                  padding: "0.1rem 0.3rem",
                }}
              >
                ↻
              </button>
              )}
            </span>
          ))}
          {q.data.teams.map((t) => (
            <span key={t.slug} className="inline-flex items-center gap-1.5" style={chipStyle}>
              {t.name}
              {canPush && (
              <button
                type="button"
                aria-label={`remove team ${t.slug}`}
                disabled={removeTeam.isPending}
                onClick={() => removeTeam.mutate(t.slug)}
                style={{
                  border: "none",
                  background: "transparent",
                  cursor: "pointer",
                  color: "var(--color-fg-muted)",
                  fontSize: "0.85rem",
                  lineHeight: 1,
                  padding: "0.1rem 0.3rem",
                }}
              >
                ✕
              </button>
              )}
            </span>
          ))}
          {canPush && addableReviewers.length > 0 && (
            <select
              aria-label="reviewer login"
              value=""
              onChange={(e) => {
                if (e.target.value) add.mutate(e.target.value);
              }}
              disabled={add.isPending}
              style={{ fontSize: "0.8rem" }}
            >
              <option value="">{add.isPending ? "Requesting…" : "Request review…"}</option>
              {addableReviewers.map((u) => (
                <option key={u.login} value={u.login}>
                  {u.login}
                </option>
              ))}
            </select>
          )}
          {canPush && addableTeams.length > 0 && (
            <select
              aria-label="reviewer team"
              value=""
              onChange={(e) => {
                if (e.target.value) addTeam.mutate(e.target.value);
              }}
              disabled={addTeam.isPending}
              style={{ fontSize: "0.8rem" }}
            >
              <option value="">{addTeam.isPending ? "Requesting…" : "Request team review…"}</option>
              {addableTeams.map((t) => (
                <option key={t.slug} value={t.slug}>
                  {t.name}
                </option>
              ))}
            </select>
          )}
        </div>
      )}
      {add.isError && (
        <InlineError inline title="Failed to request reviewer" detail={String(add.error)} />
      )}
      {remove.isError && (
        <InlineError inline title="Failed to remove reviewer" detail={String(remove.error)} />
      )}
      {addTeam.isError && (
        <InlineError inline title="Failed to request team review" detail={String(addTeam.error)} />
      )}
      {removeTeam.isError && (
        <InlineError inline title="Failed to remove team" detail={String(removeTeam.error)} />
      )}
    </div>
  );
}

// ─── Reviews ─────────────────────────────────────────────────────────────

const reviewBadge: Record<GithubReviewState, { label: string; color: string }> = {
  APPROVED: { label: "Approved", color: "var(--gh-open)" },
  CHANGES_REQUESTED: { label: "Changes requested", color: "var(--color-status-error)" },
  COMMENTED: { label: "Commented", color: "var(--color-fg-muted)" },
  DISMISSED: { label: "Dismissed", color: "var(--color-fg-muted)" },
  PENDING: { label: "Pending", color: "var(--color-status-warn)" },
};

function ReviewStateBadge({ state }: { state: GithubReviewState }) {
  const badge = reviewBadge[state];
  return (
    <span
      style={{
        border: `1px solid ${badge.color}`,
        color: badge.color,
        borderRadius: "2rem",
        padding: "0.05rem 0.55rem",
        fontSize: "0.74rem",
        fontWeight: 600,
      }}
    >
      {badge.label}
    </span>
  );
}

/**
 * One submitted review in the conversation stream: verdict header, optional
 * summary body, and the review's inline comment threads nested inside
 * (GitHub-style), instead of separate stacked sections.
 */
function ReviewCard({
  owner,
  repo,
  number,
  review,
  groups,
  threadInfoByCommentId,
  viewerLogin,
}: {
  owner: string;
  repo: string;
  number: number;
  review: GithubPRReview;
  groups: ReviewThreadGroup[];
  threadInfoByCommentId: Map<number, { id: string; isResolved: boolean }>;
  viewerLogin: string | null;
}) {
  const qc = useQueryClient();
  const { canPush } = useRepoPermissions(owner, repo);
  const [dismissing, setDismissing] = useState(false);
  const [message, setMessage] = useState("");
  const dismiss = useMutation({
    mutationFn: () => dismissPRReview(owner, repo, number, review.id, message.trim()),
    onSuccess: () => {
      setDismissing(false);
      setMessage("");
      qc.invalidateQueries({ queryKey: ["pr-reviews", owner, repo, number] });
      qc.invalidateQueries({ queryKey: ["pr-timeline", owner, repo, number] });
    },
  });
  // Dismissing someone's review is a write-access action on GitHub.
  const dismissable = canPush && (review.state === "APPROVED" || review.state === "CHANGES_REQUESTED");

  return (
    <div className="mb-3">
      <Box>
        <div
          style={{
            padding: "0.6rem 1rem",
            fontSize: "0.86rem",
            borderBottom: groups.length > 0 ? "1px solid var(--color-border)" : "none",
          }}
        >
          <div className="flex flex-wrap items-center gap-2">
            <Avatar login={review.user?.login ?? "?"} src={review.user?.avatar_url} size={20} />
            <span style={{ color: "var(--color-fg)", fontWeight: 600 }}>{review.user?.login}</span>
            <ReviewStateBadge state={review.state} />
            {review.submitted_at && (
              <span style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}>
                <RelativeTime iso={review.submitted_at} />
              </span>
            )}
            <span className="flex-1" />
            {dismissable && !dismissing && (
              <Button size="sm" variant="ghost" onClick={() => setDismissing(true)}>
                Dismiss
              </Button>
            )}
          </div>
          {review.body && (
            <div className="markdown-body" style={{ marginTop: "0.3rem", color: "var(--color-fg)" }}>
              <Markdown>{review.body}</Markdown>
            </div>
          )}
          {dismissing && (
            <div className="mt-2 flex items-center gap-2">
              <input
                aria-label={`dismissal message for review ${review.id}`}
                value={message}
                onChange={(e) => setMessage(e.target.value)}
                placeholder="Reason for dismissing…"
                className="min-w-0 flex-1"
                style={{ fontSize: "0.82rem", padding: "0.25rem 0.5rem" }}
              />
              <Button
                size="sm"
                variant="danger"
                disabled={!message.trim() || dismiss.isPending}
                onClick={() => dismiss.mutate()}
              >
                {dismiss.isPending ? "Dismissing…" : "Confirm dismiss"}
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setDismissing(false)}>
                Cancel
              </Button>
            </div>
          )}
          {dismiss.isError && (
            <InlineError inline title="Failed to dismiss review" detail={String(dismiss.error)} />
          )}
        </div>
        {groups.length > 0 && (
          <div style={{ padding: "0.6rem 0.75rem 0.1rem" }}>
            {groups.map((g) => (
              <ReviewThreadCard
                key={g.root.id}
                owner={owner}
                repo={repo}
                number={number}
                group={g}
                threadInfo={threadInfoByCommentId.get(g.root.id) ?? null}
                viewerLogin={viewerLogin}
              />
            ))}
          </div>
        )}
      </Box>
    </div>
  );
}

// ─── Conversation stream ─────────────────────────────────────────────────
// Timeline events, issue comments, and reviews (with their inline threads
// nested inside the review card) merged into one chronological stream.

interface StreamEntry {
  at: string;
  key: string;
  node: ReactNode;
}

function ConversationStream({
  owner,
  repo,
  number,
  viewerLogin,
  timeline,
  timelineError,
  timelineLoading,
  reviews,
  reviewsError,
  comments,
  commentsError,
  threads,
  threadsError,
}: {
  owner: string;
  repo: string;
  number: number;
  viewerLogin: string | null;
  timeline: GithubTimelineItem[];
  timelineError: string | null;
  timelineLoading: boolean;
  reviews: GithubPRReview[];
  reviewsError: string | null;
  comments: GithubPRReviewComment[];
  commentsError: string | null;
  threads: { id: string; isResolved: boolean; comments: { databaseId: number }[] }[];
  threadsError: string | null;
}) {
  if (timelineLoading) return null;

  // Draft (pending) reviews and their comments stay out of the public
  // conversation — GitHub only shows them to their author, in the Files tab.
  const pendingIds = new Set(reviews.filter((r) => r.state === "PENDING").map((r) => r.id));
  const submittedById = new Map(
    reviews.filter((r) => r.state !== "PENDING").map((r) => [r.id, r]),
  );
  const publicComments = comments.filter((c) => !pendingIds.has(c.pull_request_review_id));

  const threadInfoByCommentId = new Map<number, { id: string; isResolved: boolean }>();
  for (const t of threads) {
    for (const c of t.comments) {
      threadInfoByCommentId.set(c.databaseId, { id: t.id, isResolved: t.isResolved });
    }
  }

  const groups = groupReviewThreads(publicComments);
  const groupsByReview = new Map<number, ReviewThreadGroup[]>();
  const orphanGroups: ReviewThreadGroup[] = [];
  for (const g of groups) {
    if (submittedById.has(g.root.pull_request_review_id)) {
      const list = groupsByReview.get(g.root.pull_request_review_id) ?? [];
      list.push(g);
      groupsByReview.set(g.root.pull_request_review_id, list);
    } else {
      orphanGroups.push(g);
    }
  }

  const renderReviewCard = (review: GithubPRReview) => (
    <ReviewCard
      owner={owner}
      repo={repo}
      number={number}
      review={review}
      groups={groupsByReview.get(review.id) ?? []}
      threadInfoByCommentId={threadInfoByCommentId}
      viewerLogin={viewerLogin}
    />
  );

  const entries: StreamEntry[] = [];
  const consumedReviewIds = new Set<number>();
  timeline.forEach((item, i) => {
    const at = item.created_at ?? item.submitted_at ?? "";
    if (
      item.event === "reviewed" &&
      typeof item.id === "number" &&
      submittedById.has(item.id)
    ) {
      // The full review card replaces the bare timeline row — one rendering
      // per review, not two.
      consumedReviewIds.add(item.id);
      entries.push({
        at: item.submitted_at ?? at,
        key: `review-${item.id}`,
        node: renderReviewCard(submittedById.get(item.id)!),
      });
      return;
    }
    if (item.event === "reviewed" && typeof item.id === "number" && pendingIds.has(item.id)) {
      return;
    }
    if (item.event === "commented" && typeof item.id === "number") {
      entries.push({
        at,
        key: `commented-${item.id}`,
        node: (
          <div>
            <CommentCard
              login={item.user?.login}
              body={item.body}
              date={item.created_at ?? ""}
            />
            <ReactionBar
              queryKey={["issue-comment-reactions", owner, repo, item.id]}
              fetchList={() => fetchIssueCommentReactions(owner, repo, item.id as number)}
              add={(content) => addIssueCommentReaction(owner, repo, item.id as number, content)}
              remove={(reactionId) =>
                removeIssueCommentReaction(owner, repo, item.id as number, reactionId)
              }
              viewerLogin={viewerLogin}
            />
          </div>
        ),
      });
      return;
    }
    if (item.event === "reviewed" && item.body) {
      // A review the reviews endpoint did not return (e.g. hidden by
      // pagination) still renders from its timeline payload.
      entries.push({
        at: item.submitted_at ?? at,
        key: `reviewed-${item.id ?? i}`,
        node: (
          <div>
            <TimelineEventRow item={item} />
            <div style={{ marginLeft: "1.4rem" }}>
              <CommentCard
                login={item.user?.login}
                body={item.body}
                date={item.submitted_at ?? item.created_at ?? ""}
              />
            </div>
          </div>
        ),
      });
      return;
    }
    entries.push({
      at,
      key: `${item.event}-${item.id ?? i}`,
      node: <TimelineEventRow item={item} />,
    });
  });

  // Reviews the timeline did not carry (or an empty timeline) still stream in
  // chronological position.
  for (const review of submittedById.values()) {
    if (consumedReviewIds.has(review.id)) continue;
    entries.push({
      at: review.submitted_at ?? "",
      key: `review-${review.id}`,
      node: renderReviewCard(review),
    });
  }

  // Threads whose review is unknown (single inline comments, unfetched
  // reviews) render standalone at their root comment's timestamp.
  for (const g of orphanGroups) {
    entries.push({
      at: g.root.created_at,
      key: `thread-${g.root.id}`,
      node: (
        <ReviewThreadCard
          owner={owner}
          repo={repo}
          number={number}
          group={g}
          threadInfo={threadInfoByCommentId.get(g.root.id) ?? null}
          viewerLogin={viewerLogin}
        />
      ),
    });
  }

  entries.sort((a, b) => a.at.localeCompare(b.at));

  return (
    <>
      {timelineError !== null && (
        <InlineError title="Failed to load conversation" detail={timelineError} />
      )}
      {reviewsError !== null && (
        <InlineError inline title="Failed to load reviews" detail={reviewsError} />
      )}
      {commentsError !== null && (
        <InlineError inline title="Failed to load review comments" detail={commentsError} />
      )}
      {threadsError !== null && (
        <InlineError
          inline
          title="Failed to load thread resolution state"
          detail={threadsError}
        />
      )}
      {entries.map((e) => (
        <div key={e.key}>{e.node}</div>
      ))}
    </>
  );
}
