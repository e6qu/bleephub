import { useMemo, useState } from "react";
import { renderEmojiShortcodes } from "../utils/emoji.js";
import { useParams, Link, useLocation, useNavigate, useSearchParams } from "react-router";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { parse as parseYaml } from "yaml";
import { confirmAction } from "../components/confirmAction.js";
import {
  ghFetch,
  ghPostJSON,
  fetchRepoIssuesPage,
  fetchRepoIssuesFilteredPage,
  fetchIssueDetail,
  fetchIssueTimeline,
  ghGraphQL,
  updateIssue,
  createIssueComment,
  fetchSubIssues,
  addSubIssue,
  removeSubIssue,
  isNotFound,
  fetchRepoLabels,
  createRepoLabel,
  updateRepoLabel,
  deleteRepoLabel,
  fetchRepoMilestones,
  createRepoMilestone,
  updateRepoMilestone,
  deleteRepoMilestone,
  fetchAuthenticatedUser,
  fetchIssueReactions,
  addIssueReaction,
  removeIssueReaction,
  fetchRepoDetail,
  fetchRepoContents,
  fetchRepoFile,
  fetchAssignableUsers,
  fetchDiscussionCategories,
  ApiError,
} from "../api.js";
import { decodeContentsBase64 } from "../utils/contents.js";
import {
  fetchIssueBootstrap,
  fetchRepoBootstrap,
  freshenQueryDefaults,
  mirrorQueryData,
  seedQueryCache,
  useSeededOpenCounts,
  SEED_STALE_TIME,
} from "../utils/bootstrap.js";
import { useDismiss } from "../hooks/useDismiss.js";
import { useRepoPermissions } from "../hooks/useRepoPermissions.js";
import type { BleephubRepo, GithubIssue, GithubLabel, GithubMilestone, ListFilterState } from "../types.js";
import { CommentCard, EditableCommentList } from "../components/CommentCard.js";
import { toggleTaskInMarkdown } from "../components/Markdown.js";
import { CommentComposer } from "../components/CommentComposer.js";
import { SignInPrompt } from "../components/SignInPrompt.js";
import { loginPath, useSignedIn } from "../session.js";
import { MutationError } from "../components/MutationError.js";
import { LabelPills } from "../components/LabelPills.js";
import { StateToggle } from "../components/StateToggle.js";
import { RepoHeader } from "../components/PageHeader.js";
import { RelativeTime } from "../components/RelativeTime.js";
import { Avatar } from "../components/Avatar.js";
import { MarkdownComposer } from "../components/MarkdownComposer.js";
import {
  ListControls,
  filterAndSortItems,
  sortToServerParams,
  emptyFilters,
  type ListItemAccessors,
} from "../components/ListControls.js";
import { IssueSidebar } from "../components/IssueSidebar.js";
import { ReactionBar } from "../components/ReactionBar.js";
import {
  Button,
  ButtonLink,
  Box,
  Blankslate,
  StateLabel,
  Modal,
  FormLabel,
  ErrorBanner,
  DialogActions,
  SectionLabel,
} from "../components/ui.js";
import {
  IssueOpenedIcon,
  IssueClosedIcon,
  SkipCircleIcon,
  CommentIcon,
  TagIcon,
  PullRequestIcon,
  KebabIcon,
} from "../components/octicons.js";

// ─── Page-local fetchers (entry-bundle budget: no api.ts additions) ──────

/**
 * Exact issue count by state via the search API's total_count — the list
 * endpoint only bounds the count from below once it paginates. `is:issue`
 * keeps pull requests out of the number. Returns null when the server's
 * answer has no usable total (the caller falls back to a page-derived "N+").
 */
async function fetchIssueSearchCount(
  owner: string,
  repo: string,
  state: "open" | "closed",
  signal?: AbortSignal,
): Promise<number | null> {
  const params = new URLSearchParams({ q: `repo:${owner}/${repo} is:issue is:${state}`, per_page: "1" });
  const data = await ghFetch<{ total_count?: number }>(`/api/v3/search/issues?${params}`, signal);
  return typeof data?.total_count === "number" ? data.total_count : null;
}

/** Server-side free-text search over this repo's issues (title + body). */
async function searchRepoIssues(
  owner: string,
  repo: string,
  state: "open" | "closed" | "all",
  text: string,
  signal?: AbortSignal,
): Promise<GithubIssue[]> {
  const stateQ = state === "all" ? "" : ` is:${state}`;
  const params = new URLSearchParams({
    q: `repo:${owner}/${repo} is:issue${stateQ} ${text}`,
    per_page: "50",
  });
  const data = await ghFetch<{ items?: GithubIssue[] }>(`/api/v3/search/issues?${params}`, signal);
  return Array.isArray(data?.items) ? data.items : [];
}

/** POST /issues with the full GitHub payload (labels/assignees/milestone). */
const createIssueFull = (
  owner: string,
  repo: string,
  payload: {
    title: string;
    body?: string | undefined;
    labels?: string[] | undefined;
    assignees?: string[] | undefined;
    milestone?: number | undefined;
  },
) =>
  ghPostJSON<GithubIssue>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/issues`,
    payload,
  );

/** Repos under the same user/org — transferIssue's only legal destinations. */
const fetchSameOwnerRepos = (owner: string, ownerType: string | undefined) =>
  ghFetch<BleephubRepo[]>(
    ownerType === "Organization"
      ? `/api/v3/orgs/${encodeURIComponent(owner)}/repos?per_page=100`
      : `/api/v3/users/${encodeURIComponent(owner)}/repos?per_page=100`,
  );

// Spec issue.labels is (string | object)[] and issue.assignees is optional
// (WEB-013). bleephub always returns label objects; normalise to the pill shape.
const issueLabelPills = (labels: GithubIssue["labels"]) =>
  labels.flatMap((l) => (typeof l === "object" ? [{ name: l.name ?? "", color: l.color ?? "" }] : []));

/** The /issues list endpoint interleaves pull requests (GitHub models PRs as
 * issues); the Issues page must show only real issues. */
const isRealIssue = (i: GithubIssue) => !i.pull_request;

const issueAccessors: ListItemAccessors<GithubIssue> = {
  labels: (i) => issueLabelPills(i.labels),
  author: (i) => i.user?.login ?? null,
  assignees: (i) => (i.assignees ?? []).map((a) => a.login),
  milestone: (i) => i.milestone?.title ?? null,
  comments: (i) => i.comments,
  createdAt: (i) => i.created_at,
  updatedAt: (i) => i.updated_at,
  text: (i) => `${i.title}\n${i.body ?? ""}`,
};

/**
 * Open/closed issue counts for the list header. Prefers the search API's
 * exact totals; falls back to a first-page count (with a "+" when the page
 * is truncated) that also excludes pull requests.
 */
function useIssueCounts(owner: string, repo: string): {
  open: number | string | undefined;
  closed: number | string | undefined;
} {
  const exactOpen = useQuery({
    queryKey: ["issue-count-exact", owner, repo, "open"],
    queryFn: ({ signal }) => fetchIssueSearchCount(owner, repo, "open", signal),
    enabled: !!owner && !!repo,
  });
  const exactClosed = useQuery({
    queryKey: ["issue-count-exact", owner, repo, "closed"],
    queryFn: ({ signal }) => fetchIssueSearchCount(owner, repo, "closed", signal),
    enabled: !!owner && !!repo,
  });
  const pageOpen = useQuery({
    queryKey: ["issues", owner, repo, "open", "count"],
    queryFn: ({ signal }) => fetchRepoIssuesPage(owner, repo, "open", undefined, signal),
    enabled: !!owner && !!repo,
  });
  const pageClosed = useQuery({
    queryKey: ["issues", owner, repo, "closed", "count"],
    queryFn: ({ signal }) => fetchRepoIssuesPage(owner, repo, "closed", undefined, signal),
    enabled: !!owner && !!repo,
  });
  const fallback = (page?: { items: GithubIssue[]; nextUrl: string | null }) => {
    if (!page) return undefined;
    const n = page.items.filter(isRealIssue).length;
    return page.nextUrl ? `${n}+` : n;
  };
  return {
    open: exactOpen.data ?? fallback(pageOpen.data),
    closed: exactClosed.data ?? fallback(pageClosed.data),
  };
}

export function IssuesPage({ view }: { view?: "labels" | "milestones" }) {
  const { owner = "", repo = "", number } = useParams<{
    owner: string;
    repo: string;
    number?: string;
  }>();

  if (view === "labels") {
    return <LabelsView owner={owner} repo={repo} />;
  }
  if (view === "milestones") {
    return <MilestonesView owner={owner} repo={repo} />;
  }
  if (number) {
    return <IssueDetail owner={owner} repo={repo} number={parseInt(number, 10)} />;
  }
  return <IssueList owner={owner} repo={repo} />;
}

function IssueList({ owner, repo }: { owner: string; repo: string }) {
  // Signed out, "New issue" links to sign-in instead of opening the dialog
  // (github.com prompts anonymous visitors to log in first).
  const signedIn = useSignedIn();
  const location = useLocation();
  // Deep links (e.g. a milestone row) pre-filter the list via query params.
  const [searchParams] = useSearchParams();
  const [state, setState] = useState<"open" | "closed" | "all">(() => {
    const s = searchParams.get("state");
    return s === "closed" || s === "all" ? s : "open";
  });
  const [filters, setFilters] = useState<ListFilterState>(() => ({
    ...emptyFilters,
    label: searchParams.get("label"),
    author: searchParams.get("author"),
    assignee: searchParams.get("assignee"),
    milestone: searchParams.get("milestone"),
  }));
  const counts = useSeededOpenCounts(owner, repo);
  const issueCounts = useIssueCounts(owner, repo);
  const [creating, setCreating] = useState(false);
  const navigate = useNavigate();

  // The milestone facet filters by title in the UI, but the server's `milestone`
  // param takes the milestone NUMBER — resolve it from the repo's milestones.
  const { data: milestonesForFilter } = useQuery({
    queryKey: ["milestones-for-filter", owner, repo],
    queryFn: () => fetchRepoMilestones(owner, repo, "all"),
    enabled: !!owner && !!repo,
  });
  const milestoneNumber = useMemo(() => {
    if (!filters.milestone) return undefined;
    const m = (milestonesForFilter ?? []).find((ms) => ms.title === filters.milestone);
    return m ? String(m.number) : undefined;
  }, [filters.milestone, milestonesForFilter]);

  // state + label/author(creator)/assignee/milestone/sort are all carried to the
  // server so filtering/sorting is correct across every page, not just the loaded
  // set; filterAndSortItems then re-narrows the loaded set instantly as a client
  // overlay (consistent with the server order).
  const serverSort = sortToServerParams(filters.sort);
  const serverOpts = {
    state,
    labels: filters.label ?? undefined,
    creator: filters.author ?? undefined,
    assignee: filters.assignee ?? undefined,
    milestone: milestoneNumber,
    ...serverSort,
  };
  // Free-text terms switch the list source to the server-side issue search
  // (title/body match across the whole repo, not just the loaded pages).
  const searchText = (filters.text ?? "").trim();
  const query = useInfiniteQuery({
    queryKey: ["issues", owner, repo, serverOpts, "paged"],
    queryFn: ({ pageParam, signal }) =>
      fetchRepoIssuesFilteredPage(owner, repo, serverOpts, pageParam, signal),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.nextUrl ?? undefined,
    placeholderData: (previous) => previous,
    enabled: !!owner && !!repo && !searchText,
  });
  const textSearch = useQuery({
    queryKey: ["issue-text-search", owner, repo, state, searchText],
    queryFn: ({ signal }) => searchRepoIssues(owner, repo, state, searchText, signal),
    enabled: !!owner && !!repo && !!searchText,
    placeholderData: (previous) => previous,
  });
  const rawIssues = useMemo(
    () =>
      (searchText
        ? textSearch.data ?? []
        : query.data?.pages.flatMap((p) => p.items) ?? []
      ).filter(isRealIssue),
    [query.data, textSearch.data, searchText],
  );
  const issues = useMemo(
    () => filterAndSortItems(rawIssues, filters, issueAccessors),
    [rawIssues, filters],
  );

  const active = searchText ? textSearch : query;
  if (active.isLoading) return <Spinner label="loading issues" />;
  if (active.isError) return <InlineError title="Failed to load issues" detail={String(active.error)} />;

  const hasMore = !searchText && query.hasNextPage;
  const isLoadingMore = query.isFetchingNextPage;

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="issues" {...counts} issueCount={issueCounts.open ?? counts.issueCount} />

      <ListControls
        kind="issue"
        state={state}
        onState={setState}
        openCount={issueCounts.open}
        closedCount={issueCounts.closed}
        items={rawIssues}
        filters={filters}
        onFilters={setFilters}
        accessors={issueAccessors}
        resultCount={issues.length}
        actions={
          <div className="flex items-center gap-2">
            <Button size="sm" onClick={() => navigate(`/ui/repos/${owner}/${repo}/labels`)}>
              <TagIcon size={14} /> Labels
            </Button>
            <Button size="sm" onClick={() => navigate(`/ui/repos/${owner}/${repo}/milestones`)}>
              Milestones
            </Button>
            {signedIn ? (
              <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
                New issue
              </Button>
            ) : (
              <ButtonLink variant="primary" size="sm" to={loginPath(location)}>
                New issue
              </ButtonLink>
            )}
          </div>
        }
      />

      {creating && <NewIssueDialog owner={owner} repo={repo} onClose={() => setCreating(false)} />}

      <PinnedIssuesSection owner={owner} repo={repo} />

      {issues.length === 0 ? (
        <Blankslate icon={<CommentIcon size={26} />} title={`No ${state} issues`} />
      ) : (
        <>
        <Box>
          {issues.map((issue, i) => (
            <Link
              key={issue.id}
              to={`/ui/repos/${owner}/${repo}/issues/${issue.number}`}
              className="flex items-start gap-2.5"
              style={{
                padding: "0.7rem 1rem",
                borderBottom: i < issues.length - 1 ? "1px solid var(--color-border)" : "none",
                textDecoration: "none",
              }}
            >
              <IssueStateIcon state={issue.state} stateReason={issue.state_reason} />
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <span style={{ fontSize: "0.92rem", fontWeight: 600, color: "var(--color-fg)" }}>
                    {issue.title}
                  </span>
                  <LabelPills labels={issueLabelPills(issue.labels)} />
                </div>
                <div className="mt-1 flex flex-wrap items-center gap-1" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                  <span>
                    #{issue.number} opened <RelativeTime iso={issue.created_at} /> by
                  </span>
                  <Avatar login={issue.user?.login ?? "?"} src={issue.user?.avatar_url} size={16} />
                  <span>{issue.user?.login}</span>
                  {issue.milestone && (
                    <span
                      className="inline-flex items-center gap-1"
                      style={{
                        border: "1px solid var(--color-border)",
                        borderRadius: "2rem",
                        padding: "0 0.5rem",
                        marginLeft: "0.25rem",
                      }}
                    >
                      <MilestoneIcon />
                      {issue.milestone.title}
                    </span>
                  )}
                </div>
              </div>
              {issue.comments > 0 && (
                <span
                  className="inline-flex items-center gap-1"
                  aria-label={`${issue.comments} comment${issue.comments === 1 ? "" : "s"}`}
                  style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)", marginTop: "0.15rem" }}
                >
                  <CommentIcon size={14} />
                  {issue.comments}
                </span>
              )}
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

/** GitHub's list-row state icon: open green, closed-completed purple check,
 * closed-as-not-planned gray skip. */
function IssueStateIcon({ state, stateReason }: { state: string; stateReason?: string | null | undefined }) {
  if (state === "open") {
    return (
      <span style={{ marginTop: "0.1rem", color: "var(--gh-open)" }}>
        <IssueOpenedIcon />
      </span>
    );
  }
  if (stateReason === "not_planned" || stateReason === "duplicate") {
    return (
      <span
        role="img"
        aria-label="Closed as not planned"
        style={{ marginTop: "0.1rem", color: "var(--color-fg-muted)" }}
      >
        <SkipCircleIcon />
      </span>
    );
  }
  return (
    <span role="img" aria-label="Closed as completed" style={{ marginTop: "0.1rem", color: "var(--gh-merged)" }}>
      <IssueClosedIcon />
    </span>
  );
}

/** GitHub's milestone (signpost) octicon — not in the shared set. */
function MilestoneIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" width={12} height={12} fill="currentColor">
      <path d="M7.75 0a.75.75 0 0 1 .75.75V3h3.634c.414 0 .814.147 1.13.414l2.07 1.75a1.75 1.75 0 0 1 0 2.672l-2.07 1.75a1.75 1.75 0 0 1-1.13.414H8.5v5.25a.75.75 0 0 1-1.5 0V10H2.75A1.75 1.75 0 0 1 1 8.25v-3.5C1 3.784 1.784 3 2.75 3H7V.75A.75.75 0 0 1 7.75 0Zm4.384 4.5H2.75a.25.25 0 0 0-.25.25v3.5c0 .138.112.25.25.25h9.384a.25.25 0 0 0 .161-.06l2.07-1.75a.248.248 0 0 0 0-.38l-2.07-1.75a.25.25 0 0 0-.161-.06Z" />
    </svg>
  );
}

// ─── Pinned issues (Repository.pinnedIssues, cap 3) ──────────────────────

interface PinnedIssueNode {
  issue?: { number: number; title: string; state?: string; stateReason?: string | null } | null;
}

function PinnedIssuesSection({ owner, repo }: { owner: string; repo: string }) {
  // Pinned issues ride GraphQL, which refuses anonymous callers — the
  // section quietly disappears for signed-out visitors.
  const signedIn = useSignedIn();
  const q = useQuery({
    enabled: signedIn,
    queryKey: ["pinned-issues", owner, repo],
    queryFn: ({ signal }) =>
      ghGraphQL<{ repository?: { pinnedIssues?: { nodes?: (PinnedIssueNode | null)[] } | null } | null }>(
        `query($owner:String!,$repo:String!){
          repository(owner:$owner,name:$repo){
            pinnedIssues(first:3){ nodes { issue { number title state stateReason } } }
          }
        }`,
        { owner, repo },
        signal,
      ),
  });
  const pinned = (q.data?.repository?.pinnedIssues?.nodes ?? []).flatMap((n) =>
    n?.issue ? [n.issue] : [],
  );
  if (q.isError || pinned.length === 0) return null;
  return (
    <section aria-label="Pinned issues" className="mb-4">
      <div className="mb-2" style={{ fontSize: "0.78rem", fontWeight: 600, color: "var(--color-fg-muted)" }}>
        📌 Pinned issues
      </div>
      <div className="flex flex-wrap gap-3">
        {pinned.map((iss) => {
          const open = String(iss.state).toUpperCase() !== "CLOSED";
          return (
            <Link
              key={iss.number}
              to={`/ui/repos/${owner}/${repo}/issues/${iss.number}`}
              className="flex items-start gap-2"
              style={{
                border: "1px solid var(--color-border)",
                borderRadius: "var(--radius-md)",
                padding: "0.6rem 0.85rem",
                textDecoration: "none",
                minWidth: "14rem",
                flex: "1 1 14rem",
                maxWidth: "24rem",
              }}
            >
              <IssueStateIcon
                state={open ? "open" : "closed"}
                stateReason={iss.stateReason === "NOT_PLANNED" ? "not_planned" : null}
              />
              <span className="min-w-0">
                <span style={{ display: "block", fontSize: "0.88rem", fontWeight: 600, color: "var(--color-fg)" }}>
                  {iss.title}
                </span>
                <span style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>#{iss.number}</span>
              </span>
            </Link>
          );
        })}
      </div>
    </section>
  );
}

// ─── New-issue flow: template chooser + full create form ─────────────────

interface IssueTemplate {
  filename: string;
  name: string;
  about?: string | undefined;
  title?: string | undefined;
  labels: string[];
  assignees: string[];
  body: string;
}

/** Parse a `.github/ISSUE_TEMPLATE/*.md` file: YAML front-matter + body. */
function parseIssueTemplate(filename: string, content: string): IssueTemplate {
  const m = content.match(/^---\r?\n([\s\S]*?)\r?\n---\r?\n?/);
  let fm: Record<string, unknown> = {};
  let body = content;
  if (m) {
    body = content.slice(m[0].length);
    try {
      const parsed = parseYaml(m[1] ?? "");
      if (parsed && typeof parsed === "object") fm = parsed as Record<string, unknown>;
    } catch {
      // Malformed front-matter: treat the whole file as the body.
      body = content;
    }
  }
  const list = (v: unknown): string[] =>
    Array.isArray(v)
      ? v.map(String)
      : typeof v === "string" && v.trim()
        ? v.split(",").map((s) => s.trim()).filter(Boolean)
        : [];
  return {
    filename,
    name: typeof fm.name === "string" && fm.name ? fm.name : filename.replace(/\.(md|markdown)$/i, ""),
    about: typeof fm.about === "string" ? fm.about : undefined,
    title: typeof fm.title === "string" ? fm.title : undefined,
    labels: list(fm.labels),
    assignees: list(fm.assignees),
    body,
  };
}

function NewIssueDialog({ owner, repo, onClose }: { owner: string; repo: string; onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();

  // Markdown templates under .github/ISSUE_TEMPLATE. Walk the tree top-down
  // starting from the bootstrap aggregate (which 200s even on an empty repo,
  // root_entries: null) — each listing vouches for the next level. A blind
  // probe of the deep path 404s on template-less repos and the browser logs
  // that as a console error, which the e2e harness treats as a test failure.
  const templatesQ = useQuery({
    queryKey: ["issue-templates", owner, repo],
    queryFn: async () => {
      try {
        const boot = await fetchRepoBootstrap(owner, repo);
        const root = boot.root_entries ?? [];
        if (!root.some((it) => it.type === "dir" && it.name === ".github")) return [];
        const dotGithub = await fetchRepoContents(owner, repo, ".github");
        if (!dotGithub.some((it) => it.type === "dir" && it.name === "ISSUE_TEMPLATE")) return [];
        const items = await fetchRepoContents(owner, repo, ".github/ISSUE_TEMPLATE");
        return items.filter((it) => it.type === "file" && /\.(md|markdown)$/i.test(it.name));
      } catch (err) {
        if (isNotFound(err)) return [];
        throw err;
      }
    },
  });
  const templates = templatesQ.data ?? [];
  const [choosing, setChoosing] = useState(true);
  const [templateError, setTemplateError] = useState<string | null>(null);

  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [labels, setLabels] = useState<string[]>([]);
  const [assignees, setAssignees] = useState<string[]>([]);
  const [milestone, setMilestone] = useState<number | "">("");

  const { data: repoLabels = [] } = useQuery({
    queryKey: ["labels", owner, repo],
    queryFn: () => fetchRepoLabels(owner, repo),
  });
  const { data: assignable = [] } = useQuery({
    queryKey: ["assignable-users", owner, repo],
    queryFn: () => fetchAssignableUsers(owner, repo),
  });
  const { data: milestones = [] } = useQuery({
    queryKey: ["milestones", owner, repo, "open"],
    queryFn: () => fetchRepoMilestones(owner, repo, "open"),
  });

  const applyTemplate = async (filename: string | null) => {
    setTemplateError(null);
    if (filename === null) {
      setChoosing(false);
      return;
    }
    try {
      const file = await fetchRepoFile(owner, repo, `.github/ISSUE_TEMPLATE/${filename}`);
      const tpl = parseIssueTemplate(filename, decodeContentsBase64(file.content ?? ""));
      setTitle(tpl.title ?? "");
      setBody(tpl.body);
      // Unknown label names are ignored server-side, so pass them through.
      setLabels(tpl.labels);
      setAssignees(tpl.assignees);
      setChoosing(false);
    } catch (err) {
      setTemplateError(err instanceof Error ? err.message : String(err));
    }
  };

  const [createError, setCreateError] = useState<string | null>(null);
  const mutation = useMutation({
    mutationFn: () =>
      createIssueFull(owner, repo, {
        title: title.trim(),
        body: body || undefined,
        labels: labels.length > 0 ? labels : undefined,
        assignees: assignees.length > 0 ? assignees : undefined,
        milestone: milestone === "" ? undefined : milestone,
      }),
    onSuccess: (issue: GithubIssue) => {
      qc.invalidateQueries({ queryKey: ["issues", owner, repo] });
      qc.invalidateQueries({ queryKey: ["issue-count-exact", owner, repo] });
      onClose();
      navigate(`/ui/repos/${owner}/${repo}/issues/${issue.number}`);
    },
    onError: (err: Error) => setCreateError(err.message),
  });

  const toggle = (arr: string[], v: string) => (arr.includes(v) ? arr.filter((x) => x !== v) : [...arr, v]);

  if (templatesQ.isLoading) {
    return (
      <Modal title="New issue" onClose={onClose}>
        <Spinner label="loading issue templates" />
      </Modal>
    );
  }

  if (choosing && templates.length > 0) {
    return (
      <Modal title="New issue" onClose={onClose}>
        {templateError && <ErrorBanner>{templateError}</ErrorBanner>}
        <div className="flex flex-col" style={{ border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)", overflow: "hidden" }}>
          {templates.map((t, i) => (
            <div
              key={t.name}
              className="flex items-center justify-between gap-3"
              style={{ padding: "0.6rem 0.85rem", borderBottom: i < templates.length - 1 ? "1px solid var(--color-border)" : "none" }}
            >
              <span style={{ fontSize: "0.88rem", fontWeight: 600 }}>{t.name.replace(/\.(md|markdown)$/i, "")}</span>
              <Button size="sm" onClick={() => void applyTemplate(t.name)}>
                Get started
              </Button>
            </div>
          ))}
        </div>
        <div className="mt-3">
          <button
            type="button"
            className="inline-block"
            onClick={() => void applyTemplate(null)}
            style={{
              border: "none",
              background: "transparent",
              color: "var(--color-accent)",
              fontSize: "0.85rem",
              lineHeight: "1.625rem",
              padding: 0,
              cursor: "pointer",
            }}
          >
            Open a blank issue
          </button>
        </div>
      </Modal>
    );
  }

  return (
    <Modal title="New issue" onClose={onClose}>
      <FormLabel id="issue-title">Title</FormLabel>
      <input
        id="issue-title"
        autoFocus
        value={title}
        onChange={(e) => setTitle(e.target.value)}
        placeholder="Issue title"
        className="mb-3 w-full"
      />
      <FormLabel id="issue-body">Description (optional)</FormLabel>
      <div className="mb-3">
        <MarkdownComposer
          id="issue-body"
          label="Description (optional)"
          value={body}
          onChange={setBody}
          rows={5}
          placeholder="Describe the issue…"
        />
      </div>

      {repoLabels.length > 0 && (
        <fieldset className="mb-3" style={{ border: "none", margin: 0, padding: 0 }}>
          <legend style={{ fontSize: "0.78rem", fontWeight: 600, color: "var(--color-fg-muted)", marginBottom: "0.35rem" }}>
            Labels
          </legend>
          <div className="flex flex-wrap gap-x-3 gap-y-1" style={{ maxHeight: "7rem", overflowY: "auto" }}>
            {repoLabels.map((l) => (
              <label key={l.id} className="inline-flex items-center gap-1" style={{ fontSize: "0.82rem" }}>
                <input
                  type="checkbox"
                  checked={labels.includes(l.name)}
                  onChange={() => setLabels((cur) => toggle(cur, l.name))}
                />
                <LabelPills labels={[{ name: l.name, color: l.color ?? "" }]} />
              </label>
            ))}
          </div>
        </fieldset>
      )}

      {assignable.length > 0 && (
        <fieldset className="mb-3" style={{ border: "none", margin: 0, padding: 0 }}>
          <legend style={{ fontSize: "0.78rem", fontWeight: 600, color: "var(--color-fg-muted)", marginBottom: "0.35rem" }}>
            Assignees
          </legend>
          <div className="flex flex-wrap gap-x-3 gap-y-1" style={{ maxHeight: "7rem", overflowY: "auto" }}>
            {assignable.map((u) => (
              <label key={u.login} className="inline-flex items-center gap-1" style={{ fontSize: "0.82rem" }}>
                <input
                  type="checkbox"
                  checked={assignees.includes(u.login)}
                  onChange={() => setAssignees((cur) => toggle(cur, u.login))}
                />
                {u.login}
              </label>
            ))}
          </div>
        </fieldset>
      )}

      {milestones.length > 0 && (
        <div className="mb-3">
          <FormLabel id="issue-milestone">Milestone</FormLabel>
          <select
            id="issue-milestone"
            value={milestone}
            onChange={(e) => setMilestone(e.target.value === "" ? "" : parseInt(e.target.value, 10))}
            style={{ fontSize: "0.85rem" }}
          >
            <option value="">No milestone</option>
            {milestones.map((ms) => (
              <option key={ms.id} value={ms.number}>
                {ms.title}
              </option>
            ))}
          </select>
        </div>
      )}

      {createError && <ErrorBanner>{createError}</ErrorBanner>}
      <DialogActions>
        {templates.length > 0 && (
          <Button variant="ghost" size="sm" onClick={() => setChoosing(true)}>
            Back to templates
          </Button>
        )}
        <Button variant="ghost" size="sm" onClick={onClose}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!title.trim() || mutation.isPending}
          onClick={() => {
            setCreateError(null);
            mutation.mutate();
          }}
        >
          {mutation.isPending ? "Creating…" : "Create issue"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

function SubIssuesSection({ owner, repo, number }: { owner: string; repo: string; number: number }) {
  const qc = useQueryClient();
  const { canPush } = useRepoPermissions(owner, repo);
  const [childNumber, setChildNumber] = useState("");
  const listQ = useQuery({
    queryKey: ["sub-issues", owner, repo, number],
    queryFn: () => fetchSubIssues(owner, repo, number),
  });
  const invalidate = () => {
    void qc.invalidateQueries({ queryKey: ["sub-issues", owner, repo, number] });
    void qc.invalidateQueries({ queryKey: ["issue", owner, repo, number] });
  };
  const addMut = useMutation({
    mutationFn: async () => {
      // The API keys sub-issues by the child's internal id; resolve it from the
      // number the user typed.
      const child = await fetchIssueDetail(owner, repo, Number(childNumber.trim()));
      return addSubIssue(owner, repo, number, child.id);
    },
    onSuccess: () => {
      invalidate();
      setChildNumber("");
    },
  });
  const removeMut = useMutation({
    mutationFn: (subIssueId: number) => removeSubIssue(owner, repo, number, subIssueId),
    onSuccess: invalidate,
  });

  const subs = listQ.data ?? [];
  const validNumber = /^\d+$/.test(childNumber.trim());
  return (
    <section aria-label="Sub-issues" className="my-4">
      <h2 style={{ fontSize: "0.9rem", fontWeight: 600, marginBottom: "0.5rem" }}>
        Sub-issues{subs.length > 0 ? ` (${subs.length})` : ""}
      </h2>
      <MutationError of={addMut} />
      <MutationError of={removeMut} />
      {listQ.isLoading && <Spinner label="loading sub-issues" />}
      {listQ.isError && <ErrorBanner>Failed to load sub-issues: {String(listQ.error)}</ErrorBanner>}
      {subs.length > 0 && (
        <Box className="mb-2">
          {subs.map((s, i) => (
            <div
              key={s.id}
              className="flex items-center gap-2"
              style={{ padding: "0.5rem 0.8rem", borderBottom: i === subs.length - 1 ? "none" : "1px solid var(--color-border)" }}
            >
              {s.state === "open" ? <IssueOpenedIcon size={14} /> : <IssueClosedIcon size={14} />}
              <Link
                to={`/ui/repos/${owner}/${repo}/issues/${s.number}`}
                className="min-w-0 flex-1 truncate"
                style={{ color: "var(--color-fg)", textDecoration: "none" }}
              >
                {s.title} <span style={{ color: "var(--color-fg-muted)" }}>#{s.number}</span>
              </Link>
              {canPush && (
                <Button
                  size="sm"
                  variant="ghost"
                  aria-label={`Remove sub-issue #${s.number}`}
                  disabled={removeMut.isPending}
                  onClick={() => removeMut.mutate(s.id)}
                >
                  Remove
                </Button>
              )}
            </div>
          ))}
        </Box>
      )}
      {canPush && (
        <div className="flex items-center gap-2">
          <input
            aria-label="sub-issue number"
            value={childNumber}
            onChange={(e) => setChildNumber(e.target.value)}
            placeholder="Issue number"
            style={{ maxWidth: "10rem", fontSize: "0.85rem", padding: "0.25rem 0.5rem" }}
          />
          <Button size="sm" disabled={!validNumber || addMut.isPending} onClick={() => addMut.mutate()}>
            {addMut.isPending ? "Adding…" : "Add sub-issue"}
          </Button>
        </div>
      )}
    </section>
  );
}

function IssueDetail({ owner, repo, number }: { owner: string; repo: string; number: number }) {
  const counts = useSeededOpenCounts(owner, repo);
  const qc = useQueryClient();
  // One aggregate request replaces the detail's first-paint fan-out: it seeds
  // the exact keys the hooks below (and IssueSidebar's labels / assignees)
  // read, then those hooks are cache hits. On failure nothing is seeded and
  // every hook fetches standalone as before.
  const bootstrapQ = useQuery({
    queryKey: ["issue-bootstrap", owner, repo, number],
    queryFn: async ({ signal }) => {
      const data = await fetchIssueBootstrap(owner, repo, number, signal);
      seedQueryCache(qc, [
        [["issue", owner, repo, number], data.issue],
        [["issue-timeline", owner, repo, number], data.timeline],
        [["labels", owner, repo], data.labels],
        // The aggregate's milestones are state=ALL — the key IssueSidebar
        // reads. NewIssue's state=open key is seeded from the same list
        // filtered client-side, which yields exactly the list the standalone
        // state=open fetch answers.
        [["milestones", owner, repo, "all"], data.milestones],
        [["milestones", owner, repo, "open"], data.milestones.filter((m) => m.state === "open")],
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

  const { data: issue, isLoading, isError, error } = useQuery({
    queryKey: ["issue", owner, repo, number],
    queryFn: ({ signal }) => fetchIssueDetail(owner, repo, number, signal),
    enabled: bootstrapSettled,
  });
  const { data: repoDetail } = useQuery({
    queryKey: ["repo", owner, repo],
    queryFn: ({ signal }) => fetchRepoDetail(owner, repo, signal),
  });
  const { data: timeline = [], isError: commentsError, error: commentsErr } = useQuery({
    queryKey: ["issue-timeline", owner, repo, number],
    queryFn: () => fetchIssueTimeline(owner, repo, number),
    enabled: !!issue,
  });
  // Comments are the "commented" timeline events — used for the count and the
  // participants list; the full timeline drives the interleaved conversation.
  const comments = timeline.filter((i) => i.event === "commented");
  const signedIn = useSignedIn();
  const viewerQ = useQuery({
    queryKey: ["viewer"],
    queryFn: fetchAuthenticatedUser,
    // Wait for the bootstrap: it mirrors the session's cached /api/v3/user
    // response onto this key, so mounting first would fetch redundantly.
    // Anonymous visitors have no viewer to fetch (the read would 401).
    enabled: bootstrapSettled && signedIn,
  });
  const viewerLogin = typeof viewerQ.data?.login === "string" ? viewerQ.data.login : null;

  // github.com hides triage/close controls the viewer cannot use: closing,
  // reopening and title-editing need write access OR issue authorship (GitHub
  // lets authors close/reopen their own issues). While permissions load, the
  // neutral (hidden) state renders so nothing 403-able flashes in.
  const { canPush } = useRepoPermissions(owner, repo);
  const isIssueAuthor = viewerLogin !== null && viewerLogin === issue?.user?.login;
  const canClose = canPush || isIssueAuthor;

  const invalidateIssue = () => {
    qc.invalidateQueries({ queryKey: ["issue", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["issues", owner, repo] });
    qc.invalidateQueries({ queryKey: ["issue-count-exact", owner, repo] });
  };
  const [closeReason, setCloseReason] = useState<"completed" | "not_planned">("completed");
  // The comment box's draft lives here so "Close with comment" can post it
  // together with the state change (github.com's single-click behaviour).
  const [commentDraft, setCommentDraft] = useState("");
  const stateMut = useMutation({
    mutationFn: async () => {
      // github.com posts the pending comment first, then the state change.
      if (issue?.state === "open" && commentDraft.trim()) {
        await createIssueComment(owner, repo, number, commentDraft);
      }
      return updateIssue(
        owner,
        repo,
        number,
        issue?.state === "open"
          ? { state: "closed", state_reason: closeReason }
          : { state: "open", state_reason: "reopened" },
      );
    },
    onSuccess: () => {
      setCommentDraft("");
      invalidateIssue();
      qc.invalidateQueries({ queryKey: ["issue-timeline", owner, repo, number] });
    },
  });
  const toggleTaskMut = useMutation({
    mutationFn: (body: string) => updateIssue(owner, repo, number, { body }),
    // On failure re-fetch so the optimistic checkbox reverts to the persisted body.
    onSettled: invalidateIssue,
  });
  const [editing, setEditing] = useState(false);
  const [editTitle, setEditTitle] = useState("");
  const [editBody, setEditBody] = useState("");
  const editMut = useMutation({
    mutationFn: () => updateIssue(owner, repo, number, { title: editTitle, body: editBody }),
    onSuccess: () => {
      invalidateIssue();
      setEditing(false);
    },
  });

  if (isError) {
    if (isNotFound(error)) {
      return (
        <div>
          <RepoHeader owner={owner} repo={repo} active="issues" {...counts} />
          <Blankslate
            icon={<IssueOpenedIcon size={26} />}
            title={`Issue #${number} not found`}
          >
            It may have been deleted, or the number may be wrong.
          </Blankslate>
        </div>
      );
    }
    return <InlineError title={`Failed to load issue #${number}`} detail={String(error)} />;
  }
  if (isLoading || !issue) return <Spinner label={`loading issue #${number}`} />;

  const open = issue.state === "open";
  // Participants: the opener plus everyone who commented or reviewed —
  // derived from the timeline so PR-style events count too.
  const participants = Array.from(
    new Set(
      [
        issue.user?.login,
        ...timeline
          .filter((t) => t.event === "commented" || t.event === "reviewed")
          .map((t) => t.user?.login ?? t.actor?.login),
      ].filter((l): l is string => typeof l === "string"),
    ),
  );

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="issues" {...counts} />

      <div className="mb-1 flex flex-wrap items-baseline gap-2">
        <h1 style={{ fontSize: "1.5rem", fontWeight: 400, color: "var(--color-fg)", overflowWrap: "anywhere" }}>
          {issue.title}{" "}
          <span style={{ color: "var(--color-fg-muted)" }}>#{issue.number}</span>
        </h1>
        {canClose && (
          <Button
            size="sm"
            onClick={() => {
              setEditTitle(issue.title);
              setEditBody(issue.body ?? "");
              setEditing(true);
            }}
          >
            Edit
          </Button>
        )}
        <IssueActionsMenu owner={owner} repo={repo} issue={issue} repoDetail={repoDetail} />
      </div>

      {editing && (
        <Modal title="Edit issue" onClose={() => setEditing(false)}>
          <FormLabel id="edit-issue-title">Title</FormLabel>
          <input
            id="edit-issue-title"
            autoFocus
            value={editTitle}
            onChange={(e) => setEditTitle(e.target.value)}
            className="mb-3 w-full"
          />
          <FormLabel id="edit-issue-body">Description</FormLabel>
          <textarea
            id="edit-issue-body"
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
      <div
        className="mb-4 flex flex-wrap items-center gap-3 border-b pb-3"
        style={{ borderColor: "var(--color-border)" }}
      >
        <StateLabel
          state={open ? "open" : "closed"}
          icon={open ? <IssueOpenedIcon size={15} /> : <IssueClosedIcon size={15} />}
        >
          {open ? "Open" : "Closed"}
        </StateLabel>
        <span style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          <strong style={{ color: "var(--color-fg)" }}>{issue.user?.login}</strong> opened this on{" "}
          {new Date(issue.created_at).toLocaleDateString()} ·{" "}
          {comments.length} comment{comments.length === 1 ? "" : "s"}
        </span>
      </div>

      <div className="flex flex-col gap-6 lg:flex-row">
        <div className="min-w-0 flex-1">
          {viewerQ.isError && (
            <InlineError inline title="Failed to load current user" detail={String(viewerQ.error)} />
          )}
          <CommentCard
            login={issue.user?.login}
            avatarUrl={issue.user?.avatar_url}
            body={issue.body ?? undefined}
            date={issue.created_at}
            isOp
            onToggleTask={(index, checked) =>
              toggleTaskMut.mutate(toggleTaskInMarkdown(issue.body ?? "", index, checked))
            }
          />
          <ReactionBar
            queryKey={["issue-body-reactions", owner, repo, number]}
            fetchList={() => fetchIssueReactions(owner, repo, number)}
            add={(content) => addIssueReaction(owner, repo, number, content)}
            remove={(reactionId) => removeIssueReaction(owner, repo, number, reactionId)}
            viewerLogin={viewerLogin}
          />
          <MutationError of={toggleTaskMut} />
          <SubIssuesSection owner={owner} repo={repo} number={number} />
          {commentsError ? (
            <InlineError inline title="Failed to load comments" detail={String(commentsErr)} />
          ) : (
            <>
              <EditableCommentList
                owner={owner}
                repo={repo}
                number={number}
                items={timeline}
                viewerLogin={viewerLogin}
                canPush={canPush}
                invalidateKeys={[
                  ["issue-timeline", owner, repo, number],
                  ["issue", owner, repo, number],
                ]}
              />
              {comments.length === 0 && (
                <div style={{ padding: "0.5rem 0", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
                  No comments yet.
                </div>
              )}
            </>
          )}
          <MutationError of={stateMut} />
          {!signedIn && <SignInPrompt action="comment" />}
          {signedIn && <CommentComposer
            owner={owner}
            repo={repo}
            number={number}
            body={commentDraft}
            onBodyChange={setCommentDraft}
            invalidateKeys={[
              ["issue-timeline", owner, repo, number],
              ["issue", owner, repo, number],
            ]}
            extraActions={
              canClose && (
                <div style={{ display: "flex", gap: ".5rem", alignItems: "center" }}>
                  {open && (
                    <select
                      aria-label="Reason for closing"
                      value={closeReason}
                      onChange={(e) => setCloseReason(e.target.value as "completed" | "not_planned")}
                      disabled={stateMut.isPending}
                    >
                      <option value="completed">Close as completed</option>
                      <option value="not_planned">Close as not planned</option>
                    </select>
                  )}
                  <Button
                    size="sm"
                    disabled={stateMut.isPending}
                    onClick={() => stateMut.mutate()}
                  >
                    {open ? (commentDraft.trim() ? "Close with comment" : "Close issue") : "Reopen issue"}
                  </Button>
                </div>
              )
            }
          />}
        </div>
        <div style={{ width: "100%", maxWidth: "16rem", flexShrink: 0 }}>
          <IssueSidebar
            owner={owner}
            repo={repo}
            ownerType={repoDetail?.owner?.type}
            number={number}
            kind="issue"
            assignees={(issue.assignees ?? []).map((a) => a.login)}
            labels={issueLabelPills(issue.labels)}
            milestone={issue.milestone ?? null}
            participants={participants}
            locked={issue.locked ?? false}
            development={<IssueDevelopmentSection owner={owner} repo={repo} number={number} />}
          />
        </div>
      </div>
    </div>
  );
}

// ─── Issue header overflow menu: Pin / Transfer / Delete ─────────────────

const MAX_PINNED = 3;

// One GraphQL round-trip serves both the actions menu (isPinned + pin budget)
// and the sidebar's Development section (closing PRs): the two components
// share this key + fetcher, so the page issues a single request where it used
// to issue two.
interface IssueGraphQLMeta {
  repository?: {
    issue?: {
      isPinned?: boolean | null;
      closedByPullRequestsReferences?: { nodes?: (ClosedByPR | null)[] };
    } | null;
    pinnedIssues?: { totalCount?: number } | null;
  } | null;
}

const fetchIssueGraphQLMeta = (owner: string, repo: string, number: number, signal?: AbortSignal) =>
  ghGraphQL<IssueGraphQLMeta>(
    `query($owner:String!,$repo:String!,$number:Int!){
      repository(owner:$owner,name:$repo){
        issue(number:$number){
          isPinned
          closedByPullRequestsReferences(first:20){ nodes { number title state } }
        }
        pinnedIssues(first:${MAX_PINNED}){ totalCount }
      }
    }`,
    { owner, repo, number },
    signal,
  );

function IssueActionsMenu({
  owner,
  repo,
  issue,
  repoDetail,
}: {
  owner: string;
  repo: string;
  issue: GithubIssue;
  repoDetail?: BleephubRepo | undefined;
}) {
  const qc = useQueryClient();
  const [openMenu, setOpenMenu] = useState(false);
  const [transferring, setTransferring] = useState(false);
  const [converting, setConverting] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const dismissRef = useDismiss<HTMLDivElement>(openMenu, () => setOpenMenu(false));

  // github.com's viewer-role rules for this menu: pin/transfer/convert need
  // write access, delete needs repo admin. When neither applies the whole
  // kebab is hidden (rendered below, after all hooks have run).
  const { isAdmin, canPush } = useRepoPermissions(owner, repo);

  // Convert-to-discussion is offered only when the repo has discussions
  // enabled — derived from the repo payload already loaded for this page,
  // never probed.
  const canConvert = canPush && repoDetail?.has_discussions === true;

  // Pin state + how many pins the repo has left (GitHub caps pins at 3).
  // Shares one GraphQL request with the Development section via the key.
  // GraphQL refuses anonymous callers; the menu is hidden signed-out anyway.
  const signedIn = useSignedIn();
  const pinStateQ = useQuery({
    queryKey: ["issue-pin-state", owner, repo, issue.number],
    queryFn: ({ signal }) => fetchIssueGraphQLMeta(owner, repo, issue.number, signal),
    enabled: signedIn,
  });
  const isPinned = pinStateQ.data?.repository?.issue?.isPinned === true;
  const pinnedCount = pinStateQ.data?.repository?.pinnedIssues?.totalCount ?? null;
  const pinCapReached = !isPinned && pinnedCount !== null && pinnedCount >= MAX_PINNED;

  const pinMut = useMutation({
    mutationFn: () =>
      ghGraphQL(
        isPinned
          ? `mutation($input:UnpinIssueInput!){ unpinIssue(input:$input){ issue { isPinned } } }`
          : `mutation($input:PinIssueInput!){ pinIssue(input:$input){ issue { isPinned } } }`,
        { input: { issueId: issue.node_id } },
      ),
    onSuccess: () => {
      setOpenMenu(false);
      void qc.invalidateQueries({ queryKey: ["issue-pin-state", owner, repo, issue.number] });
      void qc.invalidateQueries({ queryKey: ["pinned-issues", owner, repo] });
    },
  });

  // Delete is repo-admin only (github.com hides it from everyone else).
  const canDelete = isAdmin;

  const itemStyle = {
    display: "block",
    width: "100%",
    textAlign: "left",
    border: "none",
    background: "transparent",
    color: "var(--color-fg)",
    fontSize: "0.85rem",
    padding: "0.45rem 0.85rem",
    cursor: "pointer",
  } as const;

  // No item applies (read-only viewer, or permissions still loading): no
  // kebab at all — github.com shows this menu only to collaborators.
  if (!canPush && !canDelete) return null;

  return (
    <div ref={dismissRef} style={{ position: "relative", display: "inline-block" }}>
      <Button
        size="sm"
        variant="ghost"
        aria-label="Issue actions"
        aria-haspopup="menu"
        aria-expanded={openMenu}
        onClick={() => setOpenMenu((v) => !v)}
      >
        <KebabIcon size={16} />
      </Button>
      {openMenu && (
        <div
          role="menu"
          aria-label="Issue actions"
          style={{
            position: "absolute",
            right: 0,
            top: "calc(100% + 0.25rem)",
            zIndex: 30,
            minWidth: "12rem",
            border: "1px solid var(--color-border)",
            borderRadius: "var(--radius-md)",
            background: "var(--color-surface)",
            boxShadow: "var(--shadow-md, 0 4px 12px rgba(0,0,0,0.15))",
            padding: "0.25rem 0",
          }}
        >
          {canPush && (
            <button
              type="button"
              role="menuitem"
              style={itemStyle}
              disabled={pinMut.isPending || pinCapReached}
              title={pinCapReached ? `A repository can have at most ${MAX_PINNED} pinned issues` : undefined}
              onClick={() => pinMut.mutate()}
            >
              {isPinned ? "Unpin issue" : "Pin issue"}
              {pinCapReached && (
                <span style={{ display: "block", fontSize: "0.72rem", color: "var(--color-fg-muted)" }}>
                  Pin limit of {MAX_PINNED} reached
                </span>
              )}
            </button>
          )}
          {canPush && (
            <button
              type="button"
              role="menuitem"
              style={itemStyle}
              onClick={() => {
                setOpenMenu(false);
                setTransferring(true);
              }}
            >
              Transfer issue
            </button>
          )}
          {canConvert && (
            <button
              type="button"
              role="menuitem"
              style={itemStyle}
              onClick={() => {
                setOpenMenu(false);
                setConverting(true);
              }}
            >
              Convert to discussion
            </button>
          )}
          {canDelete && (
            <button
              type="button"
              role="menuitem"
              style={{ ...itemStyle, color: "var(--color-danger, #cf222e)" }}
              onClick={() => {
                setOpenMenu(false);
                setDeleting(true);
              }}
            >
              Delete issue
            </button>
          )}
        </div>
      )}
      <MutationError of={pinMut} />
      {transferring && (
        <TransferIssueDialog
          owner={owner}
          repo={repo}
          issue={issue}
          ownerType={repoDetail?.owner?.type}
          onClose={() => setTransferring(false)}
        />
      )}
      {converting && (
        <ConvertToDiscussionDialog owner={owner} repo={repo} issue={issue} onClose={() => setConverting(false)} />
      )}
      {deleting && (
        <DeleteIssueDialog owner={owner} repo={repo} issue={issue} onClose={() => setDeleting(false)} />
      )}
    </div>
  );
}

// The GraphQL DiscussionCategory id is the category's node id
// (store.DiscussionCategoryNodeID: "DGC_kgDO%08d"); the /ui-data convert
// endpoint takes the numeric store id, which is the node id's trailing digits.
function discussionCategoryDatabaseId(nodeId: string): number | null {
  const digits = /(\d+)$/.exec(nodeId)?.[1];
  return digits ? parseInt(digits, 10) : null;
}

function ConvertToDiscussionDialog({
  owner,
  repo,
  issue,
  onClose,
}: {
  owner: string;
  repo: string;
  issue: GithubIssue;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [category, setCategory] = useState<string>("");

  // Same key + fetcher DiscussionsPage uses, so a visited Discussions tab
  // makes this list a cache hit.
  const categoriesQ = useQuery({
    queryKey: ["discussion-categories", owner, repo],
    queryFn: ({ signal }) => fetchDiscussionCategories(owner, repo, signal),
    enabled: !!owner && !!repo,
  });
  const categories = categoriesQ.data ?? [];

  const mut = useMutation({
    mutationFn: (categoryId: number) =>
      ghPostJSON<{ number: number }>(
        `/ui-data/repos/${owner}/${repo}/issues/${issue.number}/convert-to-discussion`,
        { category_id: categoryId },
      ),
    onSuccess: (d) => {
      // The conversion closed the issue as not_planned and recorded a
      // converted_to_discussion timeline event server-side.
      void qc.invalidateQueries({ queryKey: ["issue", owner, repo, issue.number] });
      void qc.invalidateQueries({ queryKey: ["issue-timeline", owner, repo, issue.number] });
      void qc.invalidateQueries({ queryKey: ["issues", owner, repo] });
      void qc.invalidateQueries({ queryKey: ["issue-count-exact", owner, repo] });
      void qc.invalidateQueries({ queryKey: ["discussions", owner, repo] });
      onClose();
      navigate(`/ui/repos/${owner}/${repo}/discussions/${d.number}`);
    },
  });

  const categoryId = category ? discussionCategoryDatabaseId(category) : null;

  return (
    <Modal title="Convert to discussion" onClose={onClose}>
      <p className="mb-3" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
        The issue will be closed as not planned and its conversation will move to a new discussion.
        This cannot be undone from here.
      </p>
      {categoriesQ.isError && (
        <ErrorBanner>Failed to load discussion categories: {String(categoriesQ.error)}</ErrorBanner>
      )}
      {categoriesQ.isLoading && <Spinner label="loading discussion categories" />}
      <FormLabel id="convert-category">Category</FormLabel>
      <select
        id="convert-category"
        value={category}
        onChange={(e) => setCategory(e.target.value)}
        className="mb-4 w-full"
        style={{ fontSize: "0.85rem" }}
      >
        <option value="">Select a category…</option>
        {categories.map((cat) => (
          <option key={cat.id} value={cat.id}>
            {renderEmojiShortcodes(cat.emoji)} {cat.name}
          </option>
        ))}
      </select>
      {mut.isError && (
        <ErrorBanner>
          {mut.error instanceof ApiError && mut.error.status === 422
            ? "This issue can't be converted: discussions may be disabled for this repository, or the category is invalid."
            : mut.error.message}
        </ErrorBanner>
      )}
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={mut.isPending}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={categoryId === null || mut.isPending}
          onClick={() => {
            if (categoryId !== null) mut.mutate(categoryId);
          }}
        >
          {mut.isPending ? "Converting…" : "I understand, convert this issue"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

function TransferIssueDialog({
  owner,
  repo,
  issue,
  ownerType,
  onClose,
}: {
  owner: string;
  repo: string;
  issue: GithubIssue;
  ownerType?: string | undefined;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [target, setTarget] = useState("");

  const reposQ = useQuery({
    queryKey: ["same-owner-repos", owner, ownerType],
    queryFn: () => fetchSameOwnerRepos(owner, ownerType),
  });
  // Issues transfer only between repositories of the same user/org.
  const candidates = (reposQ.data ?? []).filter((r) => r.name !== repo);

  const mut = useMutation({
    mutationFn: (repositoryId: string) =>
      ghGraphQL<{
        transferIssue?: {
          issue?: { number: number; repository?: { name?: string; owner?: { login?: string } | null } | null } | null;
        } | null;
      }>(
        `mutation($input:TransferIssueInput!){
          transferIssue(input:$input){
            issue { number repository { name owner { login } } }
          }
        }`,
        { input: { issueId: issue.node_id, repositoryId } },
      ),
    onSuccess: (data) => {
      void qc.invalidateQueries({ queryKey: ["issues", owner, repo] });
      void qc.invalidateQueries({ queryKey: ["issue-count-exact", owner, repo] });
      const moved = data.transferIssue?.issue;
      const newOwner = moved?.repository?.owner?.login ?? owner;
      const newRepo = moved?.repository?.name;
      onClose();
      if (moved && newRepo) {
        navigate(`/ui/repos/${newOwner}/${newRepo}/issues/${moved.number}`);
      }
    },
  });

  return (
    <Modal title="Transfer issue" onClose={onClose}>
      <p className="mb-3" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
        Transfer this issue to another repository owned by <strong>{owner}</strong>. Labels missing in
        the destination are dropped.
      </p>
      {reposQ.isError && <ErrorBanner>Failed to load repositories: {String(reposQ.error)}</ErrorBanner>}
      <FormLabel id="transfer-target">Choose a repository</FormLabel>
      <select
        id="transfer-target"
        value={target}
        onChange={(e) => setTarget(e.target.value)}
        className="mb-4 w-full"
        style={{ fontSize: "0.85rem" }}
      >
        <option value="">Select a repository…</option>
        {candidates.map((r) => (
          <option key={r.id} value={r.node_id}>
            {r.full_name}
          </option>
        ))}
      </select>
      <MutationError of={mut} />
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={mut.isPending}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!target || mut.isPending}
          onClick={() => mut.mutate(target)}
        >
          {mut.isPending ? "Transferring…" : "Transfer issue"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

function DeleteIssueDialog({
  owner,
  repo,
  issue,
  onClose,
}: {
  owner: string;
  repo: string;
  issue: GithubIssue;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [confirmText, setConfirmText] = useState("");
  const expected = `${owner}/${repo}#${issue.number}`;

  const mut = useMutation({
    mutationFn: () =>
      ghGraphQL(
        `mutation($input:DeleteIssueInput!){ deleteIssue(input:$input){ repository { name } } }`,
        { input: { issueId: issue.node_id } },
      ),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["issues", owner, repo] });
      void qc.invalidateQueries({ queryKey: ["issue-count-exact", owner, repo] });
      onClose();
      navigate(`/ui/repos/${owner}/${repo}/issues`);
    },
  });

  return (
    <Modal title="Delete issue" onClose={onClose}>
      <p className="mb-3" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
        This cannot be undone. The issue, its comments and its timeline will be permanently removed.
        Only repository admins can delete issues.
      </p>
      <FormLabel id="delete-confirm">
        Type <strong>{expected}</strong> to confirm
      </FormLabel>
      <input
        id="delete-confirm"
        autoFocus
        value={confirmText}
        onChange={(e) => setConfirmText(e.target.value)}
        className="mb-4 w-full"
        autoComplete="off"
      />
      <MutationError of={mut} />
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={mut.isPending}>
          Cancel
        </Button>
        <Button
          variant="danger"
          size="sm"
          disabled={confirmText !== expected || mut.isPending}
          onClick={() => mut.mutate()}
        >
          {mut.isPending ? "Deleting…" : "Delete this issue"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

// The issue "Development" section: the pull requests that will close this issue
// (github.com's "successfully merging a pull request may close this issue"),
// via the GraphQL Issue.closedByPullRequestsReferences field.
interface ClosedByPR {
  number: number;
  title: string;
  state?: string;
}
function IssueDevelopmentSection({ owner, repo, number }: { owner: string; repo: string; number: number }) {
  // Same key + fetcher as the actions menu's pin-state query: TanStack Query
  // deduplicates the two observers into one GraphQL request per issue.
  // GraphQL refuses anonymous callers, so signed out this renders its empty
  // state without fetching.
  const signedIn = useSignedIn();
  const q = useQuery({
    queryKey: ["issue-pin-state", owner, repo, number],
    queryFn: ({ signal }) => fetchIssueGraphQLMeta(owner, repo, number, signal),
    enabled: signedIn,
  });
  const prs = (q.data?.repository?.issue?.closedByPullRequestsReferences?.nodes ?? []).filter(
    (n): n is ClosedByPR => n != null,
  );
  if (prs.length === 0) {
    return <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>No branches or pull requests</span>;
  }
  return (
    <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: "0.3rem" }}>
      {prs.map((pr) => {
        const open = String(pr.state).toUpperCase() === "OPEN";
        return (
          <li key={pr.number} style={{ fontSize: "0.82rem" }}>
            <Link
              to={`/ui/repos/${owner}/${repo}/pulls/${pr.number}`}
              className="inline-flex items-center gap-1.5"
              style={{ color: "var(--color-accent)", textDecoration: "none" }}
            >
              <PullRequestIcon size={14} style={{ color: open ? "var(--gh-open)" : "var(--gh-merged)" }} />
              #{pr.number} {pr.title}
            </Link>
          </li>
        );
      })}
    </ul>
  );
}

// ─── Repo labels management ─────────────────────────────────────────────

function LabelsView({ owner, repo }: { owner: string; repo: string }) {
  const counts = useSeededOpenCounts(owner, repo);
  const qc = useQueryClient();
  // Label management (create/edit/delete) needs write access — github.com
  // shows read-only labels to everyone else.
  const { canPush } = useRepoPermissions(owner, repo);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<GithubLabel | null>(null);

  const { data: labels, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["labels", owner, repo],
    queryFn: () => fetchRepoLabels(owner, repo),
  });

  const deleteMut = useMutation({
    mutationFn: (name: string) => deleteRepoLabel(owner, repo, name),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["labels", owner, repo] });
    },
    onError: (err: Error) => setError(err.message),
  });

  if (isLoading || !labels) {
    if (isError) return <InlineError title="Failed to load labels" detail={String(loadErr)} />;
    return <Spinner label="loading labels" />;
  }

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="issues" {...counts} />
      <div className="mb-4 flex items-center justify-between gap-3">
        <SectionLabel>Labels</SectionLabel>
        {canPush && (
          <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
            New label
          </Button>
        )}
      </div>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {labels.length === 0 ? (
        <Blankslate icon={<TagIcon size={26} />} title="No labels yet">
          Labels help categorize issues and pull requests.
        </Blankslate>
      ) : (
        <Box>
          {labels.map((label, i) => (
            <div
              key={label.id}
              className="flex flex-wrap items-center gap-3"
              style={{
                padding: "0.7rem 1rem",
                borderBottom: i < labels.length - 1 ? "1px solid var(--color-border)" : "none",
              }}
            >
              <LabelPills labels={[label]} />
              <span className="min-w-0 flex-1" style={{ fontSize: "0.83rem", color: "var(--color-fg-muted)" }}>
                {label.description || "No description"}
              </span>
              {canPush && (
                <>
                  <Button size="sm" variant="ghost" onClick={() => setEditing(label)}>
                    edit
                  </Button>
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={deleteMut.isPending}
                    onClick={async () => {
                      if (await confirmAction(`Delete label ${label.name}?`)) deleteMut.mutate(label.name);
                    }}
                  >
                    delete
                  </Button>
                </>
              )}
            </div>
          ))}
        </Box>
      )}
      {creating && <LabelDialog owner={owner} repo={repo} onClose={() => setCreating(false)} />}
      {editing && (
        <LabelDialog owner={owner} repo={repo} label={editing} onClose={() => setEditing(null)} />
      )}
    </div>
  );
}

function LabelDialog({
  owner,
  repo,
  label,
  onClose,
}: {
  owner: string;
  repo: string;
  label?: GithubLabel;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [name, setName] = useState(label?.name ?? "");
  const [color, setColor] = useState(label?.color ?? "ededed");
  const [description, setDescription] = useState(label?.description ?? "");
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () =>
      label
        ? updateRepoLabel(owner, repo, label.name, {
            new_name: name.trim(),
            color,
            description,
          })
        : createRepoLabel(owner, repo, { name: name.trim(), color, description }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["labels", owner, repo] });
      onClose();
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Modal title={label ? `Edit label ${label.name}` : "New label"} onClose={onClose}>
      <FormLabel id="label-name">Name</FormLabel>
      <input
        id="label-name"
        autoFocus
        value={name}
        onChange={(e) => setName(e.target.value)}
        className="mb-3 w-full"
      />
      <FormLabel id="label-color">Color (hex, without #)</FormLabel>
      <input
        id="label-color"
        value={color}
        onChange={(e) => setColor(e.target.value.replace(/^#/, ""))}
        className="mb-3 w-full"
      />
      <FormLabel id="label-desc">Description (optional)</FormLabel>
      <input
        id="label-desc"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        className="mb-4 w-full"
      />
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={mutation.isPending}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!name.trim() || mutation.isPending}
          onClick={() => {
            setError(null);
            mutation.mutate();
          }}
        >
          {mutation.isPending ? "Saving…" : label ? "Save" : "Create label"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

// ─── Repo milestones management ─────────────────────────────────────────

function MilestonesView({ owner, repo }: { owner: string; repo: string }) {
  const counts = useSeededOpenCounts(owner, repo);
  const qc = useQueryClient();
  // Milestone management (create/close/reopen/delete) needs write access.
  const { canPush } = useRepoPermissions(owner, repo);
  const [state, setState] = useState<"open" | "closed">("open");
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  const { data: milestones, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["milestones", owner, repo, state],
    queryFn: () => fetchRepoMilestones(owner, repo, state),
  });

  const invalidate = () => {
    setError(null);
    qc.invalidateQueries({ queryKey: ["milestones", owner, repo] });
  };
  const stateMut = useMutation({
    mutationFn: ({ number, next }: { number: number; next: "open" | "closed" }) =>
      updateRepoMilestone(owner, repo, number, { state: next }),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const deleteMut = useMutation({
    mutationFn: (number: number) => deleteRepoMilestone(owner, repo, number),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });

  if (isLoading || !milestones) {
    if (isError) return <InlineError title="Failed to load milestones" detail={String(loadErr)} />;
    return <Spinner label="loading milestones" />;
  }

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="issues" {...counts} />
      <div className="mb-4 flex items-center justify-between gap-3">
        <StateToggle
          value={state}
          options={["open", "closed"] as const}
          labels={{ open: "Open", closed: "Closed" }}
          onChange={setState}
        />
        {canPush && (
          <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
            New milestone
          </Button>
        )}
      </div>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {milestones.length === 0 ? (
        <Blankslate icon={<IssueOpenedIcon size={26} />} title={`No ${state} milestones`}>
          Milestones group issues and pull requests toward a target.
        </Blankslate>
      ) : (
        <Box>
          {milestones.map((ms, i) => (
            <MilestoneRow
              key={ms.id}
              owner={owner}
              repo={repo}
              milestone={ms}
              canManage={canPush}
              last={i === milestones.length - 1}
              onToggleState={(next) => stateMut.mutate({ number: ms.number, next })}
              onDelete={async () => {
                if (await confirmAction(`Delete milestone ${ms.title}?`)) deleteMut.mutate(ms.number);
              }}
              busy={stateMut.isPending || deleteMut.isPending}
            />
          ))}
        </Box>
      )}
      {creating && (
        <MilestoneDialog owner={owner} repo={repo} onClose={() => setCreating(false)} />
      )}
    </div>
  );
}

function MilestoneRow({
  owner,
  repo,
  milestone: ms,
  canManage,
  last,
  onToggleState,
  onDelete,
  busy,
}: {
  owner: string;
  repo: string;
  milestone: GithubMilestone;
  /** close/reopen/delete render only with write access. */
  canManage: boolean;
  last: boolean;
  onToggleState: (next: "open" | "closed") => void;
  onDelete: () => void;
  busy: boolean;
}) {
  const total = ms.open_issues + ms.closed_issues;
  const pct = total > 0 ? Math.round((ms.closed_issues / total) * 100) : 0;
  return (
    <div
      className="flex flex-wrap items-center gap-3"
      style={{
        padding: "0.7rem 1rem",
        borderBottom: last ? "none" : "1px solid var(--color-border)",
      }}
    >
      <div className="min-w-0 flex-1">
        <Link
          to={`/ui/repos/${owner}/${repo}/issues?milestone=${encodeURIComponent(ms.title)}`}
          className="inline-block"
          style={{
            fontWeight: 600,
            fontSize: "0.92rem",
            lineHeight: "1.625rem",
            color: "var(--color-fg)",
            textDecoration: "none",
          }}
        >
          {ms.title}
        </Link>
        <div
          className="mt-1"
          role="progressbar"
          aria-label={`${ms.title} progress`}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={pct}
          style={{
            height: "8px",
            maxWidth: "24rem",
            borderRadius: "4px",
            background: "var(--color-bg-subtle)",
            border: "1px solid var(--color-border)",
            overflow: "hidden",
          }}
        >
          <div style={{ width: `${pct}%`, height: "100%", background: "var(--gh-open)" }} />
        </div>
        <div className="mt-0.5" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
          {ms.due_on ? `Due ${new Date(ms.due_on).toLocaleDateString()} · ` : ""}
          {pct}% complete · {ms.open_issues} open · {ms.closed_issues} closed
          {ms.description && ` · ${ms.description}`}
        </div>
      </div>
      {canManage && (
        <>
          <Button
            size="sm"
            variant="ghost"
            disabled={busy}
            onClick={() => onToggleState(ms.state === "open" ? "closed" : "open")}
          >
            {ms.state === "open" ? "close" : "reopen"}
          </Button>
          <Button size="sm" variant="danger" disabled={busy} onClick={onDelete}>
            delete
          </Button>
        </>
      )}
    </div>
  );
}

function MilestoneDialog({
  owner,
  repo,
  onClose,
}: {
  owner: string;
  repo: string;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [dueOn, setDueOn] = useState("");
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () =>
      createRepoMilestone(owner, repo, {
        title: title.trim(),
        description: description || undefined,
        due_on: dueOn ? new Date(dueOn).toISOString() : undefined,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["milestones", owner, repo] });
      onClose();
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Modal title="New milestone" onClose={onClose}>
      <FormLabel id="ms-title">Title</FormLabel>
      <input
        id="ms-title"
        autoFocus
        value={title}
        onChange={(e) => setTitle(e.target.value)}
        className="mb-3 w-full"
      />
      <FormLabel id="ms-desc">Description (optional)</FormLabel>
      <input
        id="ms-desc"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        className="mb-3 w-full"
      />
      <FormLabel id="ms-due">Due date (optional)</FormLabel>
      <input
        id="ms-due"
        type="date"
        value={dueOn}
        onChange={(e) => setDueOn(e.target.value)}
        className="mb-4 w-full"
      />
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={mutation.isPending}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!title.trim() || mutation.isPending}
          onClick={() => {
            setError(null);
            mutation.mutate();
          }}
        >
          {mutation.isPending ? "Creating…" : "Create milestone"}
        </Button>
      </DialogActions>
    </Modal>
  );
}
