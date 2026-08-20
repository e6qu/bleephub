import { useEffect, useRef, useState, lazy, Suspense } from "react";
import { Link, useLocation, useNavigate, useParams, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import Markdown from "../components/Markdown";
import {
  fetchRepoDetail,
  fetchRepoBranches,
  fetchRepoCommit,
  syncFork,
  fetchCombinedStatus,
  createCommitStatus,
  fetchCommitComments,
  createCommitComment,
  fetchCommitCommentReactions,
  addCommitCommentReaction,
  removeCommitCommentReaction,
  fetchAuthenticatedUser,
  fetchRepoCommits,
  fetchRepoComparison,
  fetchRepoContents,
  fetchRepoFile,
  fetchRepoLanguages,
  fetchRepoReadme,
  fetchRepoSocialCounts,
  fetchRepoTags,
  fetchRepoTopics,
  fetchWebhooks,
  fetchSecrets,
  fetchEnvironments,
  fetchReleases,
  fetchPackages,
  fetchRepoContributors,
  putFile,
  uploadFile,
  deleteFile,
  createRef,
  deleteRef,
  ghFetch,
  ghPostJSON,
} from "../api.js";
import { Avatar } from "../components/Avatar.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
import { useRepoPermissions } from "../hooks/useRepoPermissions.js";
import {
  fetchRepoBootstrap,
  fetchTreeMeta,
  repoBootstrapKey,
  seedQueryCache,
  useSeededOpenCounts,
  SEED_STALE_TIME,
  type TreeMetaLatest,
} from "../utils/bootstrap.js";
import { decodeContentsBase64 } from "../utils/contents.js";
import { confirmAction } from "../components/confirmAction.js";
import { MutationError } from "../components/MutationError.js";
import { RelativeTime } from "../components/RelativeTime.js";
import { RefSwitcher } from "../components/RefSwitcher.js";
import { PathBreadcrumbs } from "../components/PathBreadcrumbs.js";
import { DiffText } from "../components/DiffText.js";
import { BlobContent, decodeBlobText, parseLineHash } from "../components/BlobContent.js";
import { repoCodeRoute, accountRoute } from "../routes.js";
import type {
  BleephubRepo,
  GithubBranch,
  GithubCommit,
  GithubCommitStatusState,
  GithubComparison,
  GithubContentFile,
  GithubContentItem,
  GithubTag,
  GithubWebhook,
  GithubSecret,
  GithubEnvironment,
  GithubRelease,
  GithubRepoSocialCounts,
} from "../types.js";
import { RepoHeader } from "../components/PageHeader.js";
import { Box, Blankslate, Button, ButtonLink, CodeBlock, SectionLabel, Modal, DialogActions, FormLabel } from "../components/ui.js";
import { CommentCard } from "../components/CommentCard.js";
import { ReactionBar } from "../components/ReactionBar.js";
import {
  BranchIcon,
  TagIcon,
  LockIcon,
  CommentIcon,
  FileIcon,
  DirectoryIcon,
  StarIcon,
  EyeIcon,
  RepoForkedIcon,
  GlobeIcon,
  CodeIcon,
  CopyIcon,
  CheckIcon,
  ChevronDownIcon,
  GearIcon,
  LawIcon,
} from "../components/octicons.js";

// Lazy so the fuzzy finder (and its recursive-tree fetch) stay out of the entry
// bundle; loaded on first "Go to file".
const GoToFile = lazy(() => import("../components/GoToFile.js").then((m) => ({ default: m.GoToFile })));

/** Read a File's bytes as base64 (no data: prefix) for the contents API. */
function fileToBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const result = String(reader.result);
      resolve(result.slice(result.indexOf(",") + 1));
    };
    reader.onerror = () => reject(reader.error ?? new Error("could not read file"));
    reader.readAsDataURL(file);
  });
}

/*
 * Per-row metadata fetches (latest commit per file, head commit per branch or
 * tag, ahead/behind per branch) fan out across many rows, so they run through
 * a small shared semaphore and a session cache: one request per key, at most
 * MAX_META_FETCHES in flight at a time.
 */
const MAX_META_FETCHES = 6;
let metaInFlight = 0;
const metaWaiters: Array<() => void> = [];
const metaAcquire = () =>
  new Promise<void>((resolve) => {
    if (metaInFlight < MAX_META_FETCHES) {
      metaInFlight++;
      resolve();
    } else {
      metaWaiters.push(() => {
        metaInFlight++;
        resolve();
      });
    }
  });
const metaRelease = () => {
  metaInFlight--;
  metaWaiters.shift()?.();
};

const metaCache = new Map<string, Promise<unknown>>();
function cachedMeta<T>(key: string, fetcher: () => Promise<T>): Promise<T> {
  let p = metaCache.get(key) as Promise<T> | undefined;
  if (!p) {
    p = (async () => {
      await metaAcquire();
      try {
        return await fetcher();
      } finally {
        metaRelease();
      }
    })();
    metaCache.set(key, p);
    // Do not cache failures — a retry should re-fetch.
    p.catch(() => metaCache.delete(key));
  }
  return p;
}

/** Latest commit touching `path` at `ref` (GitHub's file-table columns). */
const latestCommitForPath = (owner: string, repo: string, ref: string, path: string): Promise<GithubCommit | null> =>
  cachedMeta(`file:${owner}/${repo}@${ref}:${path}`, async () => {
    const commits = await fetchRepoCommits(owner, repo, { sha: ref, path, perPage: 1 });
    const c = Array.isArray(commits) ? commits[0] : undefined;
    return c && typeof c === "object" && "commit" in c ? c : null;
  });

/** Full commit object for a sha (branch/tag head dates). Answers null when the
 * response is not commit-shaped, so metadata rows degrade to em-dashes. */
const commitBySha = (owner: string, repo: string, sha: string): Promise<GithubCommit | null> =>
  cachedMeta(`commit:${owner}/${repo}@${sha}`, async () => {
    const c = await fetchRepoCommit(owner, repo, sha);
    return c && typeof c === "object" && "commit" in c ? c : null;
  });

/** Ahead/behind counts for a branch against the default branch. */
const aheadBehind = (owner: string, repo: string, base: string, head: string) =>
  cachedMeta(`cmp:${owner}/${repo}@${base}...${head}`, async () => {
    const cmp = await fetchRepoComparison(owner, repo, base, head);
    return { ahead: cmp.ahead_by, behind: cmp.behind_by };
  });

type SubTab = "code" | "commits" | "branches" | "tags" | "activity" | "releases" | "webhooks" | "secrets" | "environments";

const SUB_TABS: { key: SubTab; label: string }[] = [
  { key: "code", label: "Code" },
  { key: "commits", label: "Commits" },
  { key: "branches", label: "Branches" },
  { key: "tags", label: "Tags" },
  { key: "activity", label: "Activity" },
  { key: "releases", label: "Releases" },
  { key: "webhooks", label: "Webhooks" },
  { key: "secrets", label: "Secrets" },
  { key: "environments", label: "Environments" },
];

const CONTENT_TABS = SUB_TABS.slice(0, 6);
const ADMIN_TABS = SUB_TABS.slice(6);

/** A recorded repository ref-update, as served by GET /repos/{o}/{r}/activity. */
interface RepoActivityItem {
  id: number;
  activity_type: string;
  ref: string;
  before: string;
  after: string;
  timestamp: string;
  actor: { login?: string; avatar_url?: string } | null;
}

// Inline fetch (per the repo's ghFetch convention) — the activity feed is the
// only caller, so it lives here rather than in the shared api module.
const fetchRepoActivity = (owner: string, repo: string) =>
  ghFetch<RepoActivityItem[]>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/activity`,
  );

const ACTIVITY_LABELS: Record<string, string> = {
  push: "pushed to",
  force_push: "force-pushed",
  branch_creation: "created branch",
  branch_deletion: "deleted branch",
  pr_merge: "merged a pull request into",
  merge_queue_merge: "merged via merge queue into",
};

function UseThisTemplateBanner({ owner, repo }: { owner: string; repo: string }) {
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [includeAllBranches, setIncludeAllBranches] = useState(false);
  const [isPrivate, setIsPrivate] = useState(false);

  const generateMut = useMutation({
    // POST /repos/{template_owner}/{template_repo}/generate — owner omitted so
    // the new repository is created under the authenticated viewer's account.
    mutationFn: () =>
      ghPostJSON<BleephubRepo>(`/api/v3/repos/${owner}/${repo}/generate`, {
        name: name.trim(),
        description: description.trim(),
        include_all_branches: includeAllBranches,
        private: isPrivate,
      }),
    onSuccess: (created) => {
      setOpen(false);
      navigate(`/ui/repos/${created.full_name}`);
    },
  });

  return (
    <div
      className="mb-4 flex flex-wrap items-center gap-3"
      style={{
        border: "1px solid var(--color-border)",
        borderRadius: "var(--radius-md)",
        padding: "0.75rem 1rem",
        background: "var(--color-bg-subtle)",
      }}
    >
      <span style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
        This repository is a template. Generate a new repository with the same directory structure and files.
      </span>
      <div style={{ marginLeft: "auto" }}>
        <Button variant="primary" size="sm" onClick={() => setOpen(true)}>
          Use this template
        </Button>
      </div>
      {open && (
        <Modal title="Create a new repository from this template" onClose={() => setOpen(false)}>
          <FormLabel id="tmpl-name">Repository name</FormLabel>
          <input
            id="tmpl-name"
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. my-new-project"
            className="mb-3 w-full"
          />
          <FormLabel id="tmpl-description">Description (optional)</FormLabel>
          <input
            id="tmpl-description"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            className="mb-3 w-full"
          />
          <label className="mb-2 flex items-center gap-2" style={{ fontSize: "0.85rem" }}>
            <input type="checkbox" checked={isPrivate} onChange={(e) => setIsPrivate(e.target.checked)} />
            Private
          </label>
          <label className="mb-3 flex items-center gap-2" style={{ fontSize: "0.85rem" }}>
            <input
              type="checkbox"
              checked={includeAllBranches}
              onChange={(e) => setIncludeAllBranches(e.target.checked)}
            />
            Include all branches
          </label>
          <MutationError of={generateMut} />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="sm"
              disabled={!name.trim() || generateMut.isPending}
              onClick={() => generateMut.mutate()}
            >
              {generateMut.isPending ? "Creating…" : "Create repository"}
            </Button>
          </DialogActions>
        </Modal>
      )}
    </div>
  );
}

export function RepoDetailPage({ initialTab = "code" }: { initialTab?: SubTab }) {
  const params = useParams<{ owner: string; repo: string; ref?: string; "*": string }>();
  const owner = params.owner ?? "";
  const repo = params.repo ?? "";
  const routeRef = params.ref ?? "";
  const routePath = params["*"] ?? "";
  const [tab, setTab] = useState<SubTab>(initialTab);

  useEffect(() => {
    setTab(initialTab);
  }, [initialTab]);

  const mainQc = useQueryClient();
  // One aggregate request replaces the page's first-paint fan-out: on success
  // it seeds the exact query keys the hooks below (and RepoHeader / the About
  // sidebar / CodeView) read, so those hooks become cache hits. On failure
  // nothing is seeded and every hook fetches standalone as before.
  const bootstrapQ = useQuery({
    queryKey: repoBootstrapKey(owner, repo),
    queryFn: async ({ signal }) => {
      const data = await fetchRepoBootstrap(owner, repo, signal);
      const defaultBranch = data.repo.default_branch;
      // Decode the README the same way the readme hook does; a corrupt
      // payload just skips the seed so that hook fetches and errors itself.
      let readmeSeed: { name: string; text: string } | null = null;
      try {
        readmeSeed = data.readme
          ? { name: data.readme.name, text: decodeContentsBase64(data.readme.content) }
          : null;
      } catch {
        readmeSeed = null;
      }
      seedQueryCache(mainQc, [
        [["repo", owner, repo], data.repo],
        // Same endpoint, counter-typed subset — RepoHeader's social counters.
        [["repo-social-counts", owner, repo], data.repo],
        [["branches", owner, repo], data.branches.first_page],
        [["repo-tags", owner, repo], data.tags.first_page],
        [["repo-languages", owner, repo], data.languages],
        [["repo-contributors", owner, repo], data.contributors],
        [["repo-topics", owner, repo], Array.isArray(data.repo.topics) ? { names: data.repo.topics } : null],
        [["contents", owner, repo, "", defaultBranch], data.root_entries],
        [["readme", owner, repo, defaultBranch], readmeSeed],
      ]);
      return data;
    },
    enabled: !!owner && !!repo,
    staleTime: SEED_STALE_TIME,
  });
  const bootstrapSettled = bootstrapQ.isSuccess || bootstrapQ.isError;

  const { data: repoData, isLoading, isError, error } = useQuery({
    queryKey: ["repo", owner, repo],
    queryFn: ({ signal }) => fetchRepoDetail(owner, repo, signal),
    enabled: !!owner && !!repo && bootstrapSettled,
  });
  const { data: branches = [] } = useQuery({
    queryKey: ["branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
    enabled: !!owner && !!repo && bootstrapSettled,
  });
  const {
    data: commits = [],
    isLoading: commitsLoading,
    isError: commitsError,
    error: commitsErr,
  } = useQuery({
    queryKey: ["commits", owner, repo, routeRef],
    queryFn: () => fetchRepoCommits(owner, repo, routeRef ? { sha: routeRef } : {}),
    // GitHub returns 409 for the commits endpoint on an empty repository.
    // `pushed_at` is the reliable emptiness signal here; `size` is not,
    // because in-memory and S3-backed repositories legitimately report zero.
    enabled:
      tab === "code"
      && repoData !== undefined
      && repoData.pushed_at !== null,
  });
  // Tab badges from the bootstrap's exact open counts; the standalone count
  // fetches only run when the bootstrap itself failed.
  const counts = useSeededOpenCounts(owner, repo, { fallbackEnabled: bootstrapQ.isError });
  // github.com hides what the viewer cannot do: write affordances need push,
  // the administration menu needs admin. Read straight off the page's own
  // repo query (NOT useRepoPermissions): that hook's query is ungated, and
  // mounting it here would race the bootstrap seed with a standalone
  // /repos/{owner}/{repo} fetch. Child components mount after the seed, so
  // they use the hook as a pure cache hit.
  const isAdmin = repoData?.permissions?.admin === true;
  const canPush = repoData?.permissions?.push === true;
  const syncForkMut = useMutation({
    mutationFn: () => syncFork(owner, repo, repoData?.default_branch ?? ""),
    onSuccess: () => {
      void mainQc.invalidateQueries({ queryKey: ["commits", owner, repo] });
      void mainQc.invalidateQueries({ queryKey: ["repo", owner, repo] });
    },
  });
  const { data: webhooks = [], isError: webhooksError, error: webhooksErr } = useQuery({
    queryKey: ["webhooks", owner, repo],
    queryFn: () => fetchWebhooks(owner, repo),
    enabled: tab === "webhooks" && !!owner && !!repo,
  });
  const { data: secrets = [], isError: secretsError, error: secretsErr } = useQuery({
    queryKey: ["secrets", owner, repo],
    queryFn: () => fetchSecrets(owner, repo),
    enabled: tab === "secrets" && !!owner && !!repo,
  });
  const { data: environments = [], isError: environmentsError, error: environmentsErr } = useQuery({
    queryKey: ["environments", owner, repo],
    queryFn: () => fetchEnvironments(owner, repo),
    enabled: tab === "environments" && !!owner && !!repo,
  });
  const { data: releases = [], isError: releasesError, error: releasesErr } = useQuery({
    queryKey: ["releases", owner, repo],
    queryFn: () => fetchReleases(owner, repo),
    enabled: tab === "releases" && !!owner && !!repo,
  });
  const { data: tags = [], isError: tagsError, error: tagsErr } = useQuery({
    queryKey: ["repo-tags", owner, repo],
    queryFn: () => fetchRepoTags(owner, repo),
    // bootstrapSettled: the bootstrap seeds this key, so a deep link straight
    // to the Tags tab must not race it with a duplicate standalone fetch.
    enabled: tab === "tags" && !!owner && !!repo && bootstrapSettled,
  });
  const { data: activity = [], isLoading: activityLoading, isError: activityError, error: activityErr } = useQuery({
    queryKey: ["repo-activity", owner, repo],
    queryFn: () => fetchRepoActivity(owner, repo),
    enabled: tab === "activity" && !!owner && !!repo,
  });
  const { data: socialCounts } = useQuery({
    queryKey: ["repo-social-counts", owner, repo],
    queryFn: () => fetchRepoSocialCounts(owner, repo),
    enabled: !!owner && !!repo && bootstrapSettled,
  });
  const { data: languages } = useQuery({
    queryKey: ["repo-languages", owner, repo],
    queryFn: () => fetchRepoLanguages(owner, repo),
    enabled: !!owner && !!repo && bootstrapSettled,
  });

  if (bootstrapQ.isPending || isLoading) return <Spinner label={`loading ${owner}/${repo}`} />;
  if (isError || !repoData)
    return <InlineError title={`Failed to load ${owner}/${repo}`} detail={String(error)} />;

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="code" {...counts} />

      {repoData?.is_template && <UseThisTemplateBanner owner={owner} repo={repo} />}

      {/* GitHub keeps content destinations close to the Code view and puts
          administrative resources behind Settings/overflow navigation. */}
      <nav className="repo-utility-bar mb-4" aria-label="Repository content">
        <div className="flex min-w-0 flex-wrap gap-1">
        {CONTENT_TABS.map((t) => {
          if (t.key === "releases") {
            return (
              <button
                key={t.key}
                type="button"
                onClick={() => setTab(t.key)}
                className="repo-utility-tab"
                aria-current={tab === t.key ? "page" : undefined}
                style={{
                  fontWeight: tab === t.key ? 600 : 500,
                  color: tab === t.key ? "var(--color-fg)" : "var(--color-fg-muted)",
                  background: tab === t.key ? "var(--color-accent-soft)" : "transparent",
                  borderColor: tab === t.key ? "color-mix(in srgb, var(--color-accent) 30%, var(--color-border))" : "transparent",
                }}
              >
                {t.label}
              </button>
            );
          }
          const destination =
            t.key === "commits" || t.key === "branches" || t.key === "tags"
              ? t.key
              : "root";
          const to =
            t.key === "activity"
              ? `/ui/repos/${owner}/${repo}/activity`
              : repoCodeRoute(owner, repo, { kind: destination });
          return (
          <Link
            key={t.key}
            to={to}
            className="repo-utility-tab"
            aria-current={tab === t.key ? "page" : undefined}
            style={{
              fontWeight: tab === t.key ? 600 : 500,
              color: tab === t.key ? "var(--color-fg)" : "var(--color-fg-muted)",
              background: tab === t.key ? "var(--color-accent-soft)" : "transparent",
              borderColor: tab === t.key ? "color-mix(in srgb, var(--color-accent) 30%, var(--color-border))" : "transparent",
            }}
          >
            {t.label}
          </Link>
          );
        })}
        </div>
        {isAdmin && (
        <details className="repo-more-menu">
          <summary className="repo-more-trigger">
            <GearIcon size={14} />
            {ADMIN_TABS.find((item) => item.key === tab)?.label ?? "More"}
            <ChevronDownIcon size={13} />
          </summary>
          <div className="repo-more-popover">
            <div className="repo-more-heading">Repository administration</div>
            {ADMIN_TABS.map((item) => (
              <button
                key={item.key}
                type="button"
                onClick={(event) => {
                  setTab(item.key);
                  event.currentTarget.closest("details")?.removeAttribute("open");
                }}
                aria-current={tab === item.key ? "page" : undefined}
              >
                {item.label}
              </button>
            ))}
            <Link to={`/ui/repos/${owner}/${repo}/settings`}>All repository settings</Link>
          </div>
        </details>
        )}
      </nav>

      {/* GitHub's two-column Code page: file browser + README on the left,
          the About sidebar (description, topics, releases, packages,
          languages, social counts) on the right. */}
      {tab === "code" && (
        commitsError ? (
          <InlineError title="Failed to load repository contents" detail={String(commitsErr)} />
        ) : (
          <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_296px]">
            <div className="min-w-0">
              {repoData.fork && canPush && (
                <div className="mb-3 flex items-center gap-2">
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={syncForkMut.isPending}
                    onClick={() => syncForkMut.mutate()}
                  >
                    {syncForkMut.isPending ? "Syncing…" : "Sync fork"}
                  </Button>
                  {syncForkMut.isError && (
                    <span style={{ color: "var(--color-status-error)", fontSize: "0.8rem" }}>
                      Sync failed: {syncForkMut.error instanceof Error ? syncForkMut.error.message : "unknown error"}
                    </span>
                  )}
                </div>
              )}
              <CodeView
                owner={owner}
                repo={repo}
                commits={commits}
                loading={commitsLoading}
                branches={branches.map((b) => b.name)}
                defaultBranch={repoData.default_branch}
                sshUrl={repoData.ssh_url}
                initialRef={routeRef}
                initialPath={routePath}
              />
            </div>
            <AboutSidebar
              owner={owner}
              repo={repo}
              repoData={repoData}
              languages={languages}
              socialCounts={socialCounts}
            />
          </div>
        )
      )}
      {tab === "commits" &&
        <CommitHistory
          owner={owner}
          repo={repo}
          branches={branches.map((branch) => branch.name)}
          defaultBranch={repoData.default_branch}
        />}
      {tab === "branches" && (
        <BranchesList
          owner={owner}
          repo={repo}
          branches={branches}
          defaultBranch={repoData.default_branch}
        />
      )}
      {tab === "tags" &&
        (tagsError ? (
          <InlineError title="Failed to load tags" detail={String(tagsErr)} />
        ) : (
          <TagsList owner={owner} repo={repo} tags={tags} branches={branches} defaultBranch={repoData.default_branch} />
        ))}
      {tab === "activity" &&
        (activityError ? (
          <InlineError title="Failed to load activity" detail={String(activityErr)} />
        ) : (
          <ActivityFeed items={activity} loading={activityLoading} />
        ))}
      {tab === "releases" &&
        (releasesError ? (
          <InlineError title="Failed to load releases" detail={String(releasesErr)} />
        ) : (
          <ReleasesList owner={owner} repo={repo} releases={releases} />
        ))}
      {tab === "webhooks" &&
        (webhooksError ? (
          <InlineError title="Failed to load webhooks" detail={String(webhooksErr)} />
        ) : (
          <WebhooksList owner={owner} repo={repo} hooks={webhooks} />
        ))}
      {tab === "secrets" &&
        (secretsError ? (
          <InlineError title="Failed to load secrets" detail={String(secretsErr)} />
        ) : (
          <SecretsList secrets={secrets} />
        ))}
      {tab === "environments" &&
        (environmentsError ? (
          <InlineError title="Failed to load environments" detail={String(environmentsErr)} />
        ) : (
          <>
            <div className="mb-3" style={{ fontSize: "0.85rem" }}>
              <Link
                to={`/ui/repos/${owner}/${repo}/deployments`}
                style={{ color: "var(--color-accent)", textDecoration: "none" }}
              >
                View deployments, protection rules, and pending approvals →
              </Link>
            </div>
            <EnvironmentsList environments={environments} />
          </>
        ))}
    </div>
  );
}

function CodeView({
  owner,
  repo,
  commits,
  loading,
  branches,
  defaultBranch,
  sshUrl,
  initialRef,
  initialPath,
}: {
  owner: string;
  repo: string;
  commits: GithubCommit[];
  loading: boolean;
  branches: string[];
  defaultBranch: string;
  sshUrl?: string | undefined;
  initialRef?: string;
  initialPath?: string;
}) {
  const navigate = useNavigate();
  // File writes need push access; read affordances (Go to file, clone box)
  // stay for everyone.
  const { canPush } = useRepoPermissions(owner, repo);
  const [branch, setBranch] = useState(initialRef || defaultBranch);
  const [path, setPath] = useState(initialPath ?? "");

  useEffect(() => {
    setBranch(initialRef || defaultBranch);
    setPath(initialPath ?? "");
  }, [defaultBranch, initialPath, initialRef]);

  const {
    data: items,
    isLoading: itemsLoading,
    isError: itemsError,
    error: itemsErr,
  } = useQuery({
    queryKey: ["contents", owner, repo, path, branch],
    queryFn: () => fetchRepoContents(owner, repo, path, branch),
    enabled: commits.length > 0,
  });

  const {
    data: readme,
    isLoading: readmeLoading,
    isError: readmeError,
  } = useQuery({
    queryKey: ["readme", owner, repo, branch],
    // Decode here so a corrupt base64 payload surfaces as readmeError
    // instead of throwing mid-render.
    queryFn: async () => {
      const file = await fetchRepoReadme(owner, repo, branch);
      return { name: file.name, text: decodeContentsBase64(file.content) };
    },
    enabled: commits.length > 0 && path === "",
  });

  const qc = useQueryClient();
  const [adding, setAdding] = useState(false);
  const [goToFileOpen, setGoToFileOpen] = useState(false);

  // GitHub's `t` shortcut opens "Go to file" (ignored while typing in a field).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "t" || e.metaKey || e.ctrlKey || e.altKey) return;
      const el = document.activeElement;
      const tag = el?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || (el as HTMLElement | null)?.isContentEditable) return;
      e.preventDefault();
      setGoToFileOpen(true);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);
  const [newName, setNewName] = useState("");
  const [newContent, setNewContent] = useState("");
  const [newMessage, setNewMessage] = useState("");
  const createFileMut = useMutation({
    mutationFn: () => {
      const fullPath = path ? `${path}/${newName}` : newName;
      return putFile(owner, repo, fullPath, {
        message: newMessage || `Create ${fullPath}`,
        content: newContent,
        branch,
      });
    },
    onSuccess: () => {
      const fullPath = path ? `${path}/${newName}` : newName;
      qc.invalidateQueries({ queryKey: ["contents", owner, repo, path, branch] });
      setAdding(false);
      setNewName("");
      setNewContent("");
      setNewMessage("");
      navigate(repoCodeRoute(owner, repo, { kind: "blob", ref: branch, path: fullPath }));
    },
  });

  const [uploading, setUploading] = useState(false);
  const [uploadItems, setUploadItems] = useState<File[]>([]);
  const [uploadMessage, setUploadMessage] = useState("");
  const uploadFilesMut = useMutation({
    mutationFn: async () => {
      // Sequential so a mid-batch failure surfaces the file that failed and the
      // earlier commits still land (matching github.com's per-file commits).
      for (const file of uploadItems) {
        const contentBase64 = await fileToBase64(file);
        const fullPath = path ? `${path}/${file.name}` : file.name;
        await uploadFile(owner, repo, fullPath, {
          message: uploadMessage || `Add ${file.name}`,
          contentBase64,
          branch,
        });
      }
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["contents", owner, repo, path, branch] });
      setUploading(false);
      setUploadItems([]);
      setUploadMessage("");
    },
  });

  // One tree-meta call per (ref, path) supplies every file row's
  // latest-commit column and the subdirectory banner, replacing the old
  // per-entry `commits?path=&per_page=1` fan-out.
  const treeMetaQ = useQuery({
    queryKey: ["tree-meta", owner, repo, branch, path],
    queryFn: ({ signal }) => fetchTreeMeta(owner, repo, branch, path, signal),
    enabled: commits.length > 0,
    staleTime: SEED_STALE_TIME,
  });
  const treeLatestByPath = new Map(
    (treeMetaQ.data?.entries ?? []).map((entry) => [entry.path, entry.latest]),
  );
  const treeMetaStatus: "pending" | "error" | "success" = treeMetaQ.isError
    ? "error"
    : treeMetaQ.isSuccess
      ? "success"
      : "pending";

  // GitHub shows the latest-commit banner in subdirectories too, scoped to
  // the commits that touched the directory. Per-path fallback only when
  // tree-meta failed or could not attribute the directory.
  const dirCommitQ = useQuery({
    queryKey: ["dir-latest-commit", owner, repo, branch, path],
    queryFn: () => latestCommitForPath(owner, repo, branch, path),
    enabled:
      commits.length > 0
      && path !== ""
      && (treeMetaQ.isError || (treeMetaQ.isSuccess && !treeMetaQ.data.latest_commit)),
  });

  if (loading || itemsLoading || readmeLoading) return <Spinner label="loading code" />;
  if (commits.length === 0) {
    return <EmptyRepoSetup owner={owner} repo={repo} defaultBranch={defaultBranch} sshUrl={sshUrl} />;
  }
  if (itemsError) return <InlineError title="Failed to load files" detail={String(itemsErr)} />;

  const fileList = Array.isArray(items) ? items : [];
  const latestCommit =
    path === ""
      ? commits[0]
      : treeMetaQ.data?.latest_commit ?? dirCommitQ.data ?? undefined;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      <div className="flex flex-wrap items-center gap-2">
        <RefSwitcher
          owner={owner}
          repo={repo}
          current={branch}
          branches={branches}
          defaultBranch={defaultBranch}
          onSelect={(nextRef) =>
            navigate(repoCodeRoute(owner, repo, { kind: "tree", ref: nextRef, ...(path ? { path } : {}) }))
          }
        />
        <div className="min-w-0 flex-1">
          {path && <PathBreadcrumbs owner={owner} repo={repo} gitRef={branch} path={path} />}
        </div>
        <Button size="sm" onClick={() => setGoToFileOpen(true)}>
          Go to file
        </Button>
        {canPush && (
          <Button size="sm" onClick={() => setAdding(true)}>
            Add file
          </Button>
        )}
        {canPush && (
          <Button size="sm" onClick={() => setUploading(true)}>
            Upload files
          </Button>
        )}
        <CloneButton owner={owner} repo={repo} sshUrl={sshUrl} archiveRef={branch} />
      </div>

      {goToFileOpen && (
        <Suspense fallback={null}>
          <GoToFile owner={owner} repo={repo} gitRef={branch} onClose={() => setGoToFileOpen(false)} />
        </Suspense>
      )}

      {adding && (
        <Modal title="Add a new file" onClose={() => setAdding(false)}>
          <FormLabel id="new-file-name">
            File path{path ? ` (under ${path}/)` : ""}
          </FormLabel>
          <input
            id="new-file-name"
            autoFocus
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            placeholder="e.g. docs/README.md"
            className="mb-3 w-full"
          />
          <FormLabel id="new-file-content">Contents</FormLabel>
          <textarea
            id="new-file-content"
            value={newContent}
            onChange={(e) => setNewContent(e.target.value)}
            rows={12}
            className="font-mono mb-3 w-full"
            style={{ resize: "vertical", fontSize: ".8rem" }}
          />
          <FormLabel id="new-file-message">Commit message</FormLabel>
          <input
            id="new-file-message"
            value={newMessage}
            onChange={(e) => setNewMessage(e.target.value)}
            placeholder={`Create ${newName || "file"}`}
            className="mb-2 w-full"
          />
          <MutationError of={createFileMut} />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setAdding(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="sm"
              disabled={!newName.trim() || createFileMut.isPending}
              onClick={() => createFileMut.mutate()}
            >
              {createFileMut.isPending ? "Committing…" : "Commit new file"}
            </Button>
          </DialogActions>
        </Modal>
      )}

      {uploading && (
        <Modal title="Upload files" onClose={() => setUploading(false)}>
          <FormLabel id="upload-input">Choose files{path ? ` (into ${path}/)` : ""}</FormLabel>
          <input
            id="upload-input"
            type="file"
            multiple
            className="mb-3 w-full"
            onChange={(e) => setUploadItems(Array.from(e.target.files ?? []))}
          />
          {uploadItems.length > 0 && (
            <ul className="mb-3" style={{ listStyle: "none", margin: 0, padding: 0, fontSize: ".82rem" }}>
              {uploadItems.map((f) => (
                <li key={f.name} style={{ display: "flex", justifyContent: "space-between", padding: ".15rem 0" }}>
                  <span>{f.name}</span>
                  <span style={{ color: "var(--color-fg-muted)" }}>{(f.size / 1024).toFixed(1)} KB</span>
                </li>
              ))}
            </ul>
          )}
          <FormLabel id="upload-message">Commit message</FormLabel>
          <input
            id="upload-message"
            value={uploadMessage}
            onChange={(e) => setUploadMessage(e.target.value)}
            placeholder={`Add ${uploadItems.length || ""} file${uploadItems.length === 1 ? "" : "s"}`.replace("  ", " ")}
            className="mb-2 w-full"
          />
          <MutationError of={uploadFilesMut} />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setUploading(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="sm"
              disabled={uploadItems.length === 0 || uploadFilesMut.isPending}
              onClick={() => uploadFilesMut.mutate()}
            >
              {uploadFilesMut.isPending ? "Uploading…" : `Commit ${uploadItems.length} file${uploadItems.length === 1 ? "" : "s"}`}
            </Button>
          </DialogActions>
        </Modal>
      )}

      {fileList.length > 0 && (
        <Box
          header={
            latestCommit ? (
              <LatestCommitBanner
                owner={owner}
                repo={repo}
                commit={latestCommit}
                {...(path === ""
                  ? { total: commits.length, hasMore: commits.length >= 100 }
                  : { historyPath: path })}
              />
            ) : undefined
          }
        >
          {fileList.map((item, i) => (
            <FileRow
              key={item.sha}
              owner={owner}
              repo={repo}
              gitRef={branch}
              item={item}
              treeMetaStatus={treeMetaStatus}
              treeLatest={treeLatestByPath.get(item.path) ?? null}
              isLast={i === fileList.length - 1}
              href={repoCodeRoute(owner, repo, item.type === "dir"
                ? { kind: "tree", ref: branch, path: item.path }
                : { kind: "blob", ref: branch, path: item.path })}
            />
          ))}
        </Box>
      )}

      {path === "" && !readmeError && readme ? (
        <Box
          header={
            <span style={{ fontSize: "0.9rem", fontWeight: 600 }}>
              {readme.name}
            </span>
          }
        >
          <div
            style={{ padding: "1.5rem", fontSize: "0.9rem" }}
            className="markdown-body"
          >
            <Markdown>
              {readme.text}
            </Markdown>
          </div>
        </Box>
      ) : null}
    </div>
  );
}

/** GitHub's "latest commit" strip at the top of the file listing. */
function LatestCommitBanner({
  owner,
  repo,
  commit,
  total,
  hasMore = false,
  historyPath,
}: {
  owner: string;
  repo: string;
  commit: GithubCommit;
  /** Repo-root banner: total commit count. Absent in subdirectories. */
  total?: number;
  // The commits query fetches a single page (per_page=100); when it comes back
  // full there are likely more, so render "100+" rather than assert an exact
  // count we did not fetch. A precise total would need a dedicated count endpoint.
  hasMore?: boolean;
  /** Subdirectory banner: link to the path-scoped history instead of a count. */
  historyPath?: string;
}) {
  return (
    <div className="flex w-full min-w-0 items-center gap-2">
      <Avatar login={commit.author?.login ?? commit.commit.author?.name ?? "?"} src={commit.author?.avatar_url} size={20} />
      <Link
        to={`/ui/repos/${owner}/${repo}/commits/${commit.sha}`}
        className="min-w-0 flex-1 truncate"
        style={{ color: "var(--color-fg)", textDecoration: "none" }}
        title={commit.commit.message}
      >
        <span style={{ fontWeight: 600 }}>{commit.author?.login ?? commit.commit.author?.name}</span>{" "}
        {commit.commit.message.split("\n")[0]}
      </Link>
      <Link
        to={`/ui/repos/${owner}/${repo}/commits/${commit.sha}`}
        className="font-mono"
        style={{ color: "var(--color-fg-muted)", textDecoration: "none" }}
      >
        {commit.sha.slice(0, 7)}
      </Link>
      <span style={{ color: "var(--color-fg-muted)" }}>
        · <RelativeTime iso={commit.commit.author?.date} />
      </span>
      {total !== undefined ? (
        <Link
          to={`/ui/repos/${owner}/${repo}/commits`}
          className="inline-flex items-center gap-1"
          style={{ color: "var(--color-fg-muted)", textDecoration: "none", whiteSpace: "nowrap" }}
        >
          <CommentIcon size={14} /> {total}
          {hasMore ? "+" : ""} {total === 1 ? "commit" : "commits"}
        </Link>
      ) : (
        <Link
          to={`/ui/repos/${owner}/${repo}/commits?path=${encodeURIComponent(historyPath ?? "")}`}
          className="inline-flex items-center gap-1"
          style={{ color: "var(--color-fg-muted)", textDecoration: "none", whiteSpace: "nowrap" }}
        >
          <CommentIcon size={14} /> History
        </Link>
      )}
    </div>
  );
}

/** GitHub's green "Code" clone dropdown — HTTPS and configured SSH URLs. */
function CloneButton({
  owner,
  repo,
  sshUrl,
  archiveRef,
}: {
  owner: string;
  repo: string;
  sshUrl?: string | undefined;
  archiveRef: string;
}) {
  const [open, setOpen] = useState(false);
  const [copied, setCopied] = useState(false);
  const [copyError, setCopyError] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);
  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const httpsUrl = `${origin}/${owner}/${repo}.git`;
  const [transport, setTransport] = useState<"https" | "ssh">("https");
  const cloneUrl = transport === "ssh" ? (sshUrl ?? "") : httpsUrl;

  useEffect(() => {
    if (!open) return;
    const onDocClick = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDocClick);
    return () => document.removeEventListener("mousedown", onDocClick);
  }, [open]);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(cloneUrl);
      setCopied(true);
      setCopyError(false);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      setCopied(false);
      setCopyError(true);
    }
  };

  return (
    <div ref={wrapRef} style={{ position: "relative" }}>
      <button
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
        className="inline-flex items-center gap-1.5"
        style={{
          background: "var(--gh-open-solid)",
          color: "#ffffff",
          border: "1px solid color-mix(in srgb, #000 12%, var(--gh-open-solid))",
          borderRadius: "var(--radius-md)",
          padding: "0.34rem 0.7rem",
          fontSize: "0.82rem",
          fontWeight: 600,
        }}
      >
        <CodeIcon size={15} /> Code <ChevronDownIcon size={14} />
      </button>
      {open && (
        <div
          role="dialog"
          aria-label="Clone this repository"
          style={{
            position: "absolute",
            top: "calc(100% + 6px)",
            right: 0,
            zIndex: 20,
            width: 320,
            padding: "0.85rem",
            background: "var(--color-surface-raised)",
            border: "1px solid var(--color-border)",
            borderRadius: "var(--radius-md)",
            boxShadow: "0 8px 24px rgba(31,35,40,0.2)",
          }}
        >
          <div style={{ fontSize: "0.82rem", fontWeight: 600, marginBottom: "0.4rem" }}>Clone</div>
          <div className="flex gap-2" style={{ fontSize: "0.72rem", marginBottom: "0.5rem" }}>
            <button type="button" onClick={() => setTransport("https")} style={{ border: 0, background: "transparent", color: transport === "https" ? "var(--color-accent)" : "var(--color-fg-muted)", fontWeight: 600 }}>HTTPS</button>
            <button type="button" onClick={() => setTransport("ssh")} style={{ border: 0, background: "transparent", color: transport === "ssh" ? "var(--color-accent)" : "var(--color-fg-muted)", fontWeight: 600 }}>SSH</button>
          </div>
          {transport === "ssh" && !sshUrl ? (
            <p role="note" style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem", margin: 0 }}>
              SSH cloning is not enabled on this server. An operator can configure
              <code> BLEEPHUB_SSH_ADDR</code>, a host key, and <code>BLEEPHUB_SSH_HOST</code>.
            </p>
          ) : (
          <div className="flex items-center gap-1.5">
            <input
              type="text"
              readOnly
              value={cloneUrl}
              aria-label={`${transport.toUpperCase()} clone URL`}
              onFocus={(e) => e.currentTarget.select()}
              style={{
                flex: 1,
                minWidth: 0,
                fontSize: "0.78rem",
                fontFamily: "var(--font-mono)",
                padding: "0.35rem 0.5rem",
                border: "1px solid var(--color-border)",
                borderRadius: "var(--radius-sm)",
                background: "var(--color-bg-subtle)",
                color: "var(--color-fg)",
              }}
            />
            <button
              type="button"
              onClick={copy}
              aria-label="Copy clone URL"
              title="Copy clone URL"
              className="inline-flex items-center justify-center"
              style={{
                flexShrink: 0,
                width: 30,
                height: 30,
                background: "var(--color-bg-subtle)",
                border: "1px solid var(--color-border)",
                borderRadius: "var(--radius-sm)",
                color: copied ? "var(--color-status-ok)" : "var(--color-fg-muted)",
                cursor: "pointer",
              }}
            >
              {copied ? <CheckIcon size={15} /> : <CopyIcon size={15} />}
            </button>
          </div>
          )}
          {copyError && (
            <p role="alert" className="mt-2" style={{ color: "var(--color-danger-fg)", fontSize: "0.75rem" }}>
              Clipboard access failed. Select the URL above and copy it manually.
            </p>
          )}
          <div
            className="mt-3 flex gap-3"
            style={{ paddingTop: "0.7rem", borderTop: "1px solid var(--color-border)", fontSize: "0.78rem" }}
          >
            <a
              href={`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/zipball/${encodeURIComponent(archiveRef)}`}
              style={{ color: "var(--color-accent)", textDecoration: "none" }}
            >
              Download ZIP
            </a>
            <a
              href={`/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/tarball/${encodeURIComponent(archiveRef)}`}
              style={{ color: "var(--color-accent)", textDecoration: "none" }}
            >
              Download TAR.GZ
            </a>
          </div>
        </div>
      )}
    </div>
  );
}

/** GitHub's right-hand "About" column on the repo Code page. */
function AboutSidebar({
  owner,
  repo,
  repoData,
  languages,
  socialCounts,
}: {
  owner: string;
  repo: string;
  repoData: BleephubRepo;
  languages: Record<string, number> | undefined;
  socialCounts: GithubRepoSocialCounts | undefined;
}) {
  const { data: topics, isError: topicsError } = useQuery({
    queryKey: ["repo-topics", owner, repo],
    queryFn: () => fetchRepoTopics(owner, repo),
    enabled: !!owner && !!repo,
  });
  const { data: releases, isError: releasesError } = useQuery({
    queryKey: ["releases", owner, repo],
    queryFn: () => fetchReleases(owner, repo),
    enabled: !!owner && !!repo,
  });
  const { data: packages, isError: packagesError } = useQuery({
    queryKey: ["repo-packages", owner, repo],
    queryFn: () => fetchPackages({ kind: "repo", owner, repo }),
    enabled: !!owner && !!repo,
  });
  const { data: contributors, isError: contributorsError } = useQuery({
    queryKey: ["repo-contributors", owner, repo],
    queryFn: () => fetchRepoContributors(owner, repo),
    enabled: !!owner && !!repo,
  });

  const base = `/ui/repos/${owner}/${repo}`;
  const topicNames = topics?.names ?? [];
  const divider = { border: "none", borderTop: "1px solid var(--color-border)", margin: 0 } as const;
  const mutedLink = { color: "var(--color-fg-muted)", textDecoration: "none" } as const;

  return (
    <aside className="flex min-w-0 flex-col gap-4" style={{ fontSize: "0.85rem" }} aria-label="About">
      <section>
        <SectionLabel>About</SectionLabel>
        <p
          className="mb-2"
          style={{ color: repoData.description ? "var(--color-fg)" : "var(--color-fg-muted)" }}
        >
          {repoData.description || "No description provided."}
        </p>
        {repoData.homepage && (
          <a
            href={repoData.homepage}
            target="_blank"
            rel="noreferrer noopener"
            className="mb-2 flex items-center gap-1.5"
            style={{ color: "var(--color-accent)", textDecoration: "none", fontWeight: 600 }}
          >
            <GlobeIcon size={15} />
            <span className="truncate">{repoData.homepage.replace(/^https?:\/\//, "")}</span>
          </a>
        )}
        {topicsError ? (
          <InlineError title="Failed to load topics" />
        ) : topicNames.length > 0 ? (
          <div className="mb-1 mt-1 flex flex-wrap gap-1.5">
            {topicNames.map((t) => (
              <Link
                key={t}
                to={`/ui/search?q=${encodeURIComponent(`topic:${t}`)}`}
                style={{
                  display: "inline-block",
                  padding: "0.1rem 0.6rem",
                  fontSize: "0.75rem",
                  fontWeight: 500,
                  color: "var(--color-accent)",
                  background: "var(--color-accent-soft)",
                  borderRadius: "2rem",
                  textDecoration: "none",
                }}
              >
                {t}
              </Link>
            ))}
          </div>
        ) : null}
        {socialCounts && (
          <div className="mt-2 flex flex-col gap-1.5">
            <Link to={`${base}/stargazers`} className="inline-flex items-center gap-1.5" style={mutedLink}>
              <StarIcon size={15} /> {socialCounts.stargazers_count}{" "}
              {socialCounts.stargazers_count === 1 ? "star" : "stars"}
            </Link>
            <Link to={`${base}/watchers`} className="inline-flex items-center gap-1.5" style={mutedLink}>
              <EyeIcon size={15} /> {socialCounts.subscribers_count}{" "}
              {socialCounts.subscribers_count === 1 ? "watcher" : "watchers"}
            </Link>
            <Link to={`${base}/forks`} className="inline-flex items-center gap-1.5" style={mutedLink}>
              <RepoForkedIcon size={15} /> {socialCounts.forks_count}{" "}
              {socialCounts.forks_count === 1 ? "fork" : "forks"}
            </Link>
          </div>
        )}
        {repoData.license && (
          <div className="mt-2 inline-flex items-center gap-1.5" style={mutedLink}>
            <LawIcon size={15} /> {repoData.license.name}
          </div>
        )}
      </section>

      <hr style={divider} />

      <section>
        <SectionLabel>Releases</SectionLabel>
        {releasesError ? (
          <InlineError title="Failed to load releases" />
        ) : releases && releases.length > 0 ? (
          <div className="flex flex-col gap-1">
            <Link
              to={`${base}/releases/${releases[0]!.id}`}
              className="inline-flex items-center gap-1.5"
              style={{ fontWeight: 600, color: "var(--color-fg)", textDecoration: "none" }}
            >
              <TagIcon size={15} style={{ color: "var(--color-status-ok)" }} />
              {releases[0]!.name || releases[0]!.tag_name}
              <span
                style={{
                  fontSize: "0.68rem",
                  fontWeight: 600,
                  color: "#ffffff",
                  background: "var(--gh-open-solid)",
                  borderRadius: "2rem",
                  padding: "0.05rem 0.5rem",
                }}
              >
                Latest
              </span>
            </Link>
            {releases.length > 1 && (
              <Link to={`${base}/releases`} style={mutedLink}>
                + {releases.length - 1} {releases.length - 1 === 1 ? "release" : "releases"}
              </Link>
            )}
          </div>
        ) : (
          <span style={{ color: "var(--color-fg-muted)" }}>No releases published</span>
        )}
      </section>

      <hr style={divider} />

      <section>
        <SectionLabel>Packages</SectionLabel>
        {packagesError ? (
          <InlineError title="Failed to load packages" />
        ) : packages && packages.length > 0 ? (
          <Link
            to={`/ui/repos/${owner}/${repo}/packages`}
            style={{ color: "var(--color-accent)", textDecoration: "none" }}
          >
            {packages.length} {packages.length === 1 ? "package" : "packages"}
          </Link>
        ) : (
          <span style={{ color: "var(--color-fg-muted)" }}>No packages published</span>
        )}
      </section>

      {!contributorsError && (contributors?.length ?? 0) > 0 && (
        <>
          <hr style={divider} />
          <section>
            <SectionLabel>
              <Link
                to={`/ui/repos/${owner}/${repo}/insights`}
                style={{ color: "inherit", textDecoration: "none" }}
              >
                Contributors {contributors!.length}
              </Link>
            </SectionLabel>
            <div className="mt-1 flex flex-wrap gap-1.5">
              {contributors!.slice(0, 14).map((c, i) =>
                c.login ? (
                  <Link
                    key={c.login}
                    to={accountRoute(c.login, c.type)}
                    aria-label={c.login}
                    title={c.login}
                    style={{ display: "inline-flex" }}
                  >
                    <Avatar login={c.login} src={c.avatar_url} size={28} />
                  </Link>
                ) : (
                  <Avatar key={`anon-${i}`} login={c.name ?? "?"} size={28} />
                ),
              )}
            </div>
          </section>
        </>
      )}

      {languages && Object.keys(languages).length > 0 && (
        <>
          <hr style={divider} />
          <section>
            <SectionLabel>Languages</SectionLabel>
            <LanguagesBar languages={languages} />
          </section>
        </>
      )}
    </aside>
  );
}

function FileRow({
  owner,
  repo,
  gitRef,
  item,
  treeMetaStatus,
  treeLatest,
  isLast,
  href,
}: {
  owner: string;
  repo: string;
  gitRef: string;
  item: GithubContentItem;
  /** State of the directory's single tree-meta call (shared by all rows). */
  treeMetaStatus: "pending" | "error" | "success";
  /** This entry's latest commit from tree-meta; null when unattributed. */
  treeLatest: TreeMetaLatest | null;
  isLast: boolean;
  href: string;
}) {
  const isDir = item.type === "dir";
  // GitHub's per-file columns come from the directory's ONE tree-meta call.
  // The old per-path commits fetch survives strictly as a fallback: when
  // tree-meta failed outright, or answered but could not attribute this
  // entry (latest: null). While tree-meta is pending the row shows em-dashes.
  const fallbackEnabled =
    treeMetaStatus === "error" || (treeMetaStatus === "success" && treeLatest === null);
  const commitQ = useQuery({
    queryKey: ["file-latest-commit", owner, repo, gitRef, item.path],
    queryFn: () => latestCommitForPath(owner, repo, gitRef, item.path),
    staleTime: 5 * 60_000,
    enabled: fallbackEnabled,
  });
  const fallback = fallbackEnabled ? commitQ.data ?? null : null;
  const latest: { sha: string; headline: string; title: string; date?: string | undefined } | null =
    treeLatest
      ? {
          sha: treeLatest.sha,
          headline: treeLatest.message_headline,
          title: treeLatest.message_headline,
          date: treeLatest.author_date,
        }
      : fallback
        ? {
            sha: fallback.sha,
            headline: fallback.commit.message.split("\n")[0] ?? "",
            title: fallback.commit.message,
            date: fallback.commit.author?.date,
          }
        : null;
  const nameLink = (
    <>
      <span style={{ color: isDir ? "var(--color-accent)" : "var(--color-fg-muted)", display: "flex" }}>
        {isDir ? <DirectoryIcon size={16} /> : <FileIcon size={16} />}
      </span>
      <span style={{ color: "var(--color-fg)", fontWeight: 400, textAlign: "left" }} className="truncate">
        {item.name}
      </span>
      {(item.type === "symlink" || item.type === "submodule") && (
        <span style={{ color: "var(--color-fg-muted)", fontSize: "0.72rem" }}>{item.type}</span>
      )}
    </>
  );
  const linkStyle = {
    minWidth: 0,
    flex: "0 1 34%",
    border: "none",
    cursor: "pointer",
    background: "transparent",
    textDecoration: "none",
    lineHeight: "1.625rem",
  } as const;
  return (
    <div
      className="flex w-full items-center gap-2"
      style={{
        padding: "0.35rem 1rem",
        fontSize: "0.85rem",
        borderBottom: isLast ? "none" : "1px solid var(--color-border)",
      }}
    >
      {item.type === "submodule" && item.submodule_git_url ? (
        <a href={item.submodule_git_url} className="flex items-center gap-2" style={linkStyle}>{nameLink}</a>
      ) : (
        <Link to={href} className="flex items-center gap-2" style={linkStyle}>{nameLink}</Link>
      )}
      <span className="min-w-0 flex-1 truncate" style={{ color: "var(--color-fg-muted)" }}>
        {latest ? (
          <Link
            to={`/ui/repos/${owner}/${repo}/commits/${latest.sha}`}
            title={latest.title}
            style={{ color: "var(--color-fg-muted)", textDecoration: "none", display: "inline-block", lineHeight: "1.625rem" }}
          >
            {latest.headline}
          </Link>
        ) : (
          <span aria-hidden>—</span>
        )}
      </span>
      <span style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem", whiteSpace: "nowrap" }}>
        {latest ? <RelativeTime iso={latest.date} /> : <span aria-hidden>—</span>}
      </span>
    </div>
  );
}

function EmptyRepoSetup({
  owner,
  repo,
  defaultBranch,
  sshUrl,
}: {
  owner: string;
  repo: string;
  defaultBranch: string;
  sshUrl?: string | undefined;
}) {
  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const [activeTab, setActiveTab] = useState<"https" | "ssh" | "gh">("https");
  const tabs: { key: "https" | "ssh" | "gh"; label: string }[] = [
    { key: "https", label: "HTTPS" },
    { key: "ssh", label: "SSH" },
    { key: "gh", label: "GitHub CLI" },
  ];

  const snippets: Record<typeof activeTab, string> = {
    https: `git remote add origin ${origin}/${owner}/${repo}.git\ngit branch -M ${defaultBranch}\ngit push -u origin ${defaultBranch}`,
    ssh: `git remote add origin ${sshUrl ?? ""}\ngit branch -M ${defaultBranch}\ngit push -u origin ${defaultBranch}`,
    gh: `gh repo clone ${owner}/${repo}\ncd ${repo}`,
  };

  return (
    <Blankslate title="This repository is empty">
      <p className="mb-3">Get started by creating a new file or cloning an existing repository.</p>

      <div
        className="mb-3 flex gap-1"
        style={{ borderBottom: "1px solid var(--color-border)" }}
      >
        {tabs.map((t) => (
          <button
            key={t.key}
            type="button"
            onClick={() => setActiveTab(t.key)}
            style={{
              padding: "0.4rem 0.7rem",
              marginBottom: "-1px",
              fontSize: "0.84rem",
              fontWeight: activeTab === t.key ? 600 : 500,
              color: activeTab === t.key ? "var(--color-fg)" : "var(--color-fg-muted)",
              background: "transparent",
              border: "none",
              borderBottom: `2px solid ${activeTab === t.key ? "var(--color-accent)" : "transparent"}`,
            }}
          >
            {t.label}
          </button>
        ))}
      </div>
      {activeTab === "ssh" && !sshUrl ? (
        <p role="note" style={{ color: "var(--color-fg-muted)" }}>
          SSH cloning is not enabled on this server. Configure the SSH listener and advertised
          host to publish an SSH clone URL.
        </p>
      ) : (
        <CodeBlock>{snippets[activeTab]}</CodeBlock>
      )}
    </Blankslate>
  );
}

interface CommitHistoryFilters {
  sha: string;
  path: string;
  author: string;
  since: string;
  until: string;
}

const commitDateBoundary = (date: string, endOfDay: boolean) =>
  date ? new Date(`${date}T${endOfDay ? "23:59:59.999" : "00:00:00.000"}Z`).toISOString() : undefined;

function CommitHistory({
  owner,
  repo,
  branches,
  defaultBranch,
}: {
  owner: string;
  repo: string;
  branches: string[];
  defaultBranch: string;
}) {
  const emptyFilters: CommitHistoryFilters = {
    sha: defaultBranch,
    path: "",
    author: "",
    since: "",
    until: "",
  };
  // Deep-link support for the per-file "History" control: ?path=… (&sha=…)
  // pre-fills the path filter so the commits tab lands scoped to that file.
  const [searchParams] = useSearchParams();
  const initialFilters: CommitHistoryFilters = {
    ...emptyFilters,
    path: searchParams.get("path") ?? "",
    sha: searchParams.get("sha") || defaultBranch,
  };
  const [draft, setDraft] = useState<CommitHistoryFilters>(initialFilters);
  const [filters, setFilters] = useState<CommitHistoryFilters>(initialFilters);
  const [page, setPage] = useState(1);
  const perPage = 30;
  const query = useQuery({
    queryKey: ["commit-history", owner, repo, filters, page],
    queryFn: () => fetchRepoCommits(owner, repo, {
      sha: filters.sha || undefined,
      path: filters.path.trim() || undefined,
      author: filters.author.trim() || undefined,
      since: commitDateBoundary(filters.since, false),
      until: commitDateBoundary(filters.until, true),
      page,
      perPage,
    }),
  });

  return (
    <div className="flex flex-col gap-4">
      <Box header={<span style={{ fontWeight: 600 }}>Filter commit history</span>}>
        <form
          onSubmit={(event) => {
            event.preventDefault();
            setFilters(draft);
            setPage(1);
          }}
          className="grid gap-3 md:grid-cols-2 xl:grid-cols-5"
          style={{ padding: "1rem" }}
        >
          <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
            Branch or ref
            <input
              list="commit-history-refs"
              value={draft.sha}
              onChange={(event) => setDraft((current) => ({ ...current, sha: event.target.value }))}
              placeholder={defaultBranch}
            />
            <datalist id="commit-history-refs">
              {branches.map((branch) => <option key={branch} value={branch} />)}
            </datalist>
          </label>
          <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
            Path
            <input
              value={draft.path}
              onChange={(event) => setDraft((current) => ({ ...current, path: event.target.value }))}
              placeholder="src/server.go"
            />
          </label>
          <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
            Author
            <input
              value={draft.author}
              onChange={(event) => setDraft((current) => ({ ...current, author: event.target.value }))}
              placeholder="login or email"
            />
          </label>
          <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
            Since
            <input
              type="date"
              value={draft.since}
              onChange={(event) => setDraft((current) => ({ ...current, since: event.target.value }))}
            />
          </label>
          <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
            Until
            <input
              type="date"
              value={draft.until}
              onChange={(event) => setDraft((current) => ({ ...current, until: event.target.value }))}
            />
          </label>
          <div className="flex gap-2 md:col-span-2 xl:col-span-5">
            <Button type="submit" variant="primary">Apply filters</Button>
            <Button
              type="button"
              onClick={() => {
                setDraft(emptyFilters);
                setFilters(emptyFilters);
                setPage(1);
              }}
            >
              Clear
            </Button>
          </div>
        </form>
      </Box>
      {query.isError ? (
        <InlineError title="Failed to load commits" detail={String(query.error)} />
      ) : (
        <CommitsList owner={owner} repo={repo} commits={query.data ?? []} loading={query.isLoading} />
      )}
      {!query.isError && !query.isLoading && ((query.data?.length ?? 0) > 0 || page > 1) && (
        <nav className="flex items-center justify-between" aria-label="Commit history pages">
          <Button type="button" disabled={page === 1} onClick={() => setPage((current) => Math.max(1, current - 1))}>
            Previous
          </Button>
          <span style={{ color: "var(--color-fg-muted)", fontSize: "0.8rem" }}>Page {page}</span>
          <Button
            type="button"
            disabled={(query.data?.length ?? 0) < perPage}
            onClick={() => setPage((current) => current + 1)}
          >
            Next
          </Button>
        </nav>
      )}
    </div>
  );
}

/** Small clipboard button with GitHub's check-mark copy feedback. */
function CopyShaButton({ sha }: { sha: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      aria-label={`Copy full SHA for ${sha.slice(0, 7)}`}
      title="Copy full SHA"
      className="inline-flex items-center justify-center"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(sha);
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1500);
        } catch {
          // Clipboard denied — leave the icon unchanged.
        }
      }}
      style={{
        flexShrink: 0,
        width: 28,
        height: 28,
        background: "var(--color-bg-subtle)",
        border: "1px solid var(--color-border)",
        borderRadius: "var(--radius-sm)",
        color: copied ? "var(--color-status-ok)" : "var(--color-fg-muted)",
        cursor: "pointer",
      }}
    >
      {copied ? <CheckIcon size={14} /> : <CopyIcon size={14} />}
    </button>
  );
}

function CommitsList({
  owner,
  repo,
  commits,
  loading,
}: {
  owner: string;
  repo: string;
  commits: GithubCommit[];
  loading: boolean;
}) {
  if (loading) return <Spinner label="loading commits" />;
  if (commits.length === 0) return <Blankslate title="No commits yet" />;

  // GitHub groups the history under "Commits on {date}" headers.
  const groups: { date: string; items: GithubCommit[] }[] = [];
  for (const c of commits) {
    const date = new Date(c.commit.author?.date ?? c.commit.committer?.date ?? "").toLocaleDateString("en-US", {
      month: "short",
      day: "numeric",
      year: "numeric",
    });
    const last = groups[groups.length - 1];
    if (last && last.date === date) last.items.push(c);
    else groups.push({ date, items: [c] });
  }

  return (
    <div className="flex flex-col gap-3">
      {groups.map((group) => (
        <section key={group.date} aria-label={`Commits on ${group.date}`}>
          <div className="mb-1.5" style={{ fontSize: "0.8rem", fontWeight: 600, color: "var(--color-fg-muted)" }}>
            Commits on {group.date}
          </div>
          <Box>
            {group.items.map((c, i) => {
              const login = c.author?.login ?? c.commit.author?.name ?? "unknown";
              return (
                <div
                  key={c.sha}
                  className="flex items-center gap-3"
                  style={{
                    padding: "0.65rem 1rem",
                    borderBottom: i < group.items.length - 1 ? "1px solid var(--color-border)" : "none",
                  }}
                >
                  <div className="min-w-0 flex-1">
                    <Link
                      to={`/ui/repos/${owner}/${repo}/commits/${c.sha}`}
                      style={{
                        fontSize: "0.88rem",
                        color: "var(--color-fg)",
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        whiteSpace: "nowrap",
                        textDecoration: "none",
                      }}
                    >
                      {c.commit.message.split("\n")[0]}
                    </Link>
                    <div className="mt-0.5 flex items-center gap-1.5" style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}>
                      <Avatar login={login} src={c.author?.avatar_url} size={16} />
                      <span style={{ fontWeight: 600, color: "var(--color-fg)" }}>{login}</span>
                      <span>
                        committed <RelativeTime iso={c.commit.author?.date} />
                      </span>
                    </div>
                  </div>
                  <Link
                    to={`/ui/repos/${owner}/${repo}/commits/${c.sha}`}
                    className="font-mono"
                    style={{
                      fontSize: "0.74rem",
                      color: "var(--color-fg-muted)",
                      background: "var(--color-bg-subtle)",
                      border: "1px solid var(--color-border)",
                      padding: "0.28rem 0.4rem",
                      borderRadius: "var(--radius-sm)",
                      textDecoration: "none",
                    }}
                  >
                    {c.sha.slice(0, 7)}
                  </Link>
                  <CopyShaButton sha={c.sha} />
                  <Link
                    to={repoCodeRoute(owner, repo, { kind: "tree", ref: c.sha })}
                    aria-label={`Browse repository at ${c.sha.slice(0, 7)}`}
                    title="Browse repository at this commit"
                    className="inline-flex items-center justify-center"
                    style={{
                      flexShrink: 0,
                      width: 28,
                      height: 28,
                      border: "1px solid var(--color-border)",
                      borderRadius: "var(--radius-sm)",
                      color: "var(--color-fg-muted)",
                      background: "var(--color-bg-subtle)",
                    }}
                  >
                    <CodeIcon size={14} />
                  </Link>
                </div>
              );
            })}
          </Box>
        </section>
      ))}
    </div>
  );
}

/** GitHub's repo /activity feed: recorded ref updates, newest first. */
function ActivityFeed({ items, loading }: { items: RepoActivityItem[]; loading: boolean }) {
  if (loading) return <Spinner label="loading activity" />;
  if (items.length === 0) return <Blankslate icon={<BranchIcon size={26} />} title="No activity yet" />;
  return (
    <Box>
      {items.map((a, i) => {
        const login = a.actor?.login ?? "ghost";
        const shortRef = a.ref.replace(/^refs\/(heads|tags)\//, "");
        const label = ACTIVITY_LABELS[a.activity_type] ?? a.activity_type.replace(/_/g, " ");
        return (
          <div
            key={a.id}
            className="flex items-center gap-3"
            style={{
              padding: "0.65rem 1rem",
              borderBottom: i < items.length - 1 ? "1px solid var(--color-border)" : "none",
            }}
          >
            <Avatar login={login} src={a.actor?.avatar_url} size={28} />
            <div className="min-w-0 flex-1" style={{ fontSize: "0.85rem", color: "var(--color-fg)" }}>
              <span style={{ fontWeight: 600 }}>{login}</span>{" "}
              <span style={{ color: "var(--color-fg-muted)" }}>{label}</span>{" "}
              <span className="font-mono" style={{ color: "var(--color-fg)" }}>{shortRef}</span>
            </div>
            <span style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)", whiteSpace: "nowrap" }}>
              <RelativeTime iso={a.timestamp} />
            </span>
          </div>
        );
      })}
    </Box>
  );
}

function WebhooksList({
  owner,
  repo,
  hooks,
}: {
  owner: string;
  repo: string;
  hooks: GithubWebhook[];
}) {
  if (hooks.length === 0) return <Blankslate icon={<CommentIcon size={26} />} title="No webhooks configured" />;
  return (
    <Box>
      {hooks.map((h, i) => (
        <div
          key={h.id}
          className="flex items-center gap-3"
          style={{
            padding: "0.7rem 1rem",
            borderBottom: i < hooks.length - 1 ? "1px solid var(--color-border)" : "none",
          }}
        >
          <span
            aria-hidden
            style={{
              width: 8,
              height: 8,
              borderRadius: "999px",
              background: h.active ? "var(--gh-open)" : "var(--color-fg-subtle)",
              flexShrink: 0,
            }}
          />
          <div className="min-w-0 flex-1">
            <div style={{ fontSize: "0.88rem", fontWeight: 500, color: "var(--color-fg)" }}>
              {h.name}{" "}
              <span style={{ color: "var(--color-fg-subtle)", fontWeight: 400 }}>#{h.id}</span>
            </div>
            <div className="font-mono" style={{ fontSize: "0.74rem", color: "var(--color-fg-muted)" }}>
              {h.config?.url || "no url"} · events: {h.events?.join(", ") || "none"}
            </div>
          </div>
          <Link
            to={`/ui/repos/${owner}/${repo}/hooks/${h.id}/deliveries`}
            style={{ color: "var(--color-accent)", fontSize: "0.8rem", textDecoration: "none", flexShrink: 0 }}
          >
            Deliveries
          </Link>
        </div>
      ))}
    </Box>
  );
}

function SecretsList({ secrets }: { secrets: GithubSecret[] }) {
  if (secrets.length === 0) return <Blankslate icon={<LockIcon size={26} />} title="No secrets configured" />;
  return (
    <Box>
      {secrets.map((s, i) => (
        <div
          key={s.name}
          className="flex items-center gap-2 font-mono"
          style={{
            padding: "0.65rem 1rem",
            fontSize: "0.85rem",
            color: "var(--color-fg)",
            borderBottom: i < secrets.length - 1 ? "1px solid var(--color-border)" : "none",
          }}
        >
          <LockIcon size={14} style={{ color: "var(--color-fg-muted)" }} /> {s.name}
        </div>
      ))}
    </Box>
  );
}

function EnvironmentsList({ environments }: { environments: GithubEnvironment[] }) {
  if (environments.length === 0) return <Blankslate title="No environments" />;
  return (
    <Box>
      {environments.map((e, i) => (
        <div
          key={e.name}
          style={{
            padding: "0.65rem 1rem",
            fontSize: "0.85rem",
            color: "var(--color-fg)",
            borderBottom: i < environments.length - 1 ? "1px solid var(--color-border)" : "none",
          }}
        >
          {e.name}
        </div>
      ))}
    </Box>
  );
}

function ReleasesList({ owner, repo, releases }: { owner: string; repo: string; releases: GithubRelease[] }) {
  // Creating releases needs push access; the feed stays readable for everyone.
  const { canPush } = useRepoPermissions(owner, repo);
  if (releases.length === 0) return (
    <Blankslate icon={<TagIcon size={26} />} title="No releases">
      {canPush
        ? <Link to={`/ui/repos/${owner}/${repo}/releases/new`}>Create the first release</Link>
        : <p>There aren’t any releases here.</p>}
    </Blankslate>
  );
  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end">
        <Link to={`/ui/repos/${owner}/${repo}/releases`} style={{ color: "var(--color-accent)", fontSize: "0.82rem" }}>
          Manage releases and assets
        </Link>
      </div>
      <Box>{releases.map((r, i) => (
        <div
          key={r.id}
          className="flex items-center gap-3"
          style={{
            padding: "0.7rem 1rem",
            borderBottom: i < releases.length - 1 ? "1px solid var(--color-border)" : "none",
          }}
        >
          <span
            className="inline-flex items-center gap-1 font-mono"
            style={{
              fontSize: "0.74rem",
              color: "var(--color-accent)",
              background: "var(--color-accent-soft)",
              padding: "0.1rem 0.45rem",
              borderRadius: "var(--radius-sm)",
            }}
          >
            <TagIcon size={12} /> {r.tag_name}
          </span>
          <Link className="min-w-0 flex-1" to={`/ui/repos/${owner}/${repo}/releases/${r.id}`} style={{ color: "inherit", textDecoration: "none" }}>
            <div style={{ fontSize: "0.88rem", fontWeight: 500, color: "var(--color-fg)" }}>
              {r.name || r.tag_name}
            </div>
            <div style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}>
              {r.published_at === null
                ? "draft"
                : <>published <RelativeTime iso={r.published_at} /></>}
            </div>
          </Link>
        </div>
      ))}</Box>
    </div>
  );
}

export function RepoCommitPage() {
  const { owner = "", repo = "", sha = "" } = useParams<{
    owner: string;
    repo: string;
    sha: string;
  }>();
  const counts = useOpenCounts(owner, repo);
  const query = useQuery({
    queryKey: ["commit", owner, repo, sha],
    queryFn: () => fetchRepoCommit(owner, repo, sha),
    enabled: !!owner && !!repo && !!sha,
  });

  if (query.isLoading) return <Spinner label="loading commit" />;
  if (query.isError || !query.data) {
    return <InlineError title="Failed to load commit" detail={String(query.error)} />;
  }

  const commit = query.data;
  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="code" {...counts} />
      <div className="mb-4 flex flex-wrap items-center justify-between gap-2">
        <Link
          to={`/ui/repos/${owner}/${repo}/commits`}
          style={{ color: "var(--color-accent)", textDecoration: "none" }}
        >
          ← Commit history
        </Link>
        <Link
          to={repoCodeRoute(owner, repo, { kind: "tree", ref: commit.sha })}
          style={{ color: "var(--color-accent)", textDecoration: "none" }}
        >
          Browse repository at this commit
        </Link>
      </div>
      <Box
        header={
          <div>
            <h1 style={{ fontSize: "1rem", fontWeight: 650 }}>
              {commit.commit.message.split("\n")[0]}
            </h1>
            <div className="mt-1" style={{ color: "var(--color-fg-muted)", fontSize: ".78rem" }}>
              {commit.commit.author?.name} committed <RelativeTime iso={commit.commit.author?.date} />
            </div>
          </div>
        }
      >
        <div style={{ padding: "1rem" }}>
          <div className="font-mono" style={{ fontSize: ".8rem", wordBreak: "break-all" }}>
            {commit.sha}
          </div>
          {commit.commit.message.includes("\n") && (
            <p className="mt-4 whitespace-pre-wrap" style={{ color: "var(--color-fg-muted)" }}>
              {commit.commit.message.split("\n").slice(1).join("\n").trim()}
            </p>
          )}
          {commit.stats && (
            <div className="mt-4" style={{ fontSize: ".82rem" }}>
              <b>{commit.stats.total} changes</b>{" "}
              <span style={{ color: "var(--color-status-ok)" }}>+{commit.stats.additions}</span>{" "}
              <span style={{ color: "var(--color-status-error)" }}>−{commit.stats.deletions}</span>
            </div>
          )}
          {commit.parents && commit.parents.length > 0 && (
            <div className="mt-4 flex flex-wrap items-center gap-1.5" style={{ fontSize: ".82rem" }}>
              <span style={{ color: "var(--color-fg-muted)" }}>
                {commit.parents.length} {commit.parents.length === 1 ? "parent" : "parents"}
              </span>
              {commit.parents.map((parent) => (
                <Link
                  key={parent.sha}
                  className="font-mono"
                  to={`/ui/repos/${owner}/${repo}/commits/${parent.sha}`}
                  style={{ color: "var(--color-accent)", textDecoration: "none" }}
                >
                  {parent.sha.slice(0, 7)}
                </Link>
              ))}
            </div>
          )}
        </div>
      </Box>
      {commit.files && commit.files.length > 0 && (
        <div className="mt-4 flex flex-col gap-3">
          {commit.files.map((file) => (
            <Box
              key={file.filename}
              header={
                <div className="flex w-full items-center gap-2">
                  {file.status === "removed" ? (
                    <span className="font-mono min-w-0 flex-1 truncate">{file.filename}</span>
                  ) : (
                    <Link
                      className="font-mono min-w-0 flex-1 truncate"
                      to={repoCodeRoute(owner, repo, {
                        kind: "blob",
                        ref: commit.sha,
                        path: file.filename,
                      })}
                      style={{ color: "var(--color-accent)", textDecoration: "none" }}
                    >
                      {file.filename}
                    </Link>
                  )}
                  <span style={{ color: "var(--color-status-ok)" }}>+{file.additions}</span>
                  <span style={{ color: "var(--color-status-error)" }}>−{file.deletions}</span>
                </div>
              }
            >
              {file.patch ? (
                <DiffText patch={file.patch} />
              ) : (
                <div style={{ padding: "1rem", color: "var(--color-fg-muted)" }}>
                  Binary file or patch unavailable.
                </div>
              )}
            </Box>
          ))}
        </div>
      )}
      <CommitPullsSection owner={owner} repo={repo} sha={sha} />
      <CommitStatusesSection owner={owner} repo={repo} sha={sha} />
      <CommitCommentsSection owner={owner} repo={repo} sha={sha} />
    </div>
  );
}

// Pull requests that introduce a commit — GET /commits/{sha}/pulls returns an
// array of pull-request-simple objects. Defined inline per the ghFetch
// convention; RepoCommitPage is the only caller.
interface CommitPullSimple {
  number: number;
  title: string;
  state: string;
  merged_at?: string | null;
  html_url?: string;
}

const fetchCommitPulls = (owner: string, repo: string, sha: string) =>
  ghFetch<CommitPullSimple[]>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/commits/${encodeURIComponent(sha)}/pulls`,
  );

function CommitPullsSection({ owner, repo, sha }: { owner: string; repo: string; sha: string }) {
  const pullsQ = useQuery({
    queryKey: ["commit-pulls", owner, repo, sha],
    queryFn: () => fetchCommitPulls(owner, repo, sha),
    enabled: !!owner && !!repo && !!sha,
  });

  const pulls = Array.isArray(pullsQ.data) ? pullsQ.data : [];
  if (pulls.length === 0) return null;

  return (
    <section aria-label="Associated pull requests" className="mt-5">
      <SectionLabel>Associated pull requests</SectionLabel>
      <Box>
        {pulls.map((pr, i) => {
          const merged = pr.state === "closed" && !!pr.merged_at;
          const label = merged ? "Merged" : pr.state === "open" ? "Open" : "Closed";
          const color = merged
            ? "var(--color-accent)"
            : pr.state === "open"
              ? "var(--color-status-ok)"
              : "var(--color-status-error)";
          return (
            <div
              key={pr.number}
              className="flex items-center gap-2"
              style={{ padding: "0.5rem 0.8rem", borderBottom: i === pulls.length - 1 ? "none" : "1px solid var(--color-border)" }}
            >
              <span
                aria-hidden
                style={{ width: ".55rem", height: ".55rem", borderRadius: "50%", background: color, flexShrink: 0 }}
              />
              <Link
                to={`/ui/repos/${owner}/${repo}/pulls/${pr.number}`}
                className="min-w-0 flex-1 truncate"
                style={{ color: "var(--color-accent)", textDecoration: "none" }}
              >
                #{pr.number} {pr.title}
              </Link>
              <span style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>{label}</span>
            </div>
          );
        })}
      </Box>
    </section>
  );
}

const COMMIT_STATUS_STATES: GithubCommitStatusState[] = ["success", "pending", "failure", "error"];

function CommitStatusesSection({ owner, repo, sha }: { owner: string; repo: string; sha: string }) {
  const qc = useQueryClient();
  const [state, setState] = useState<GithubCommitStatusState>("success");
  const [context, setContext] = useState("");
  const [description, setDescription] = useState("");
  const statusQ = useQuery({
    queryKey: ["commit-status", owner, repo, sha],
    queryFn: () => fetchCombinedStatus(owner, repo, sha),
    enabled: !!owner && !!repo && !!sha,
  });
  const createMut = useMutation({
    mutationFn: () =>
      createCommitStatus(owner, repo, sha, {
        state,
        ...(context.trim() ? { context: context.trim() } : {}),
        ...(description.trim() ? { description: description.trim() } : {}),
      }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["commit-status", owner, repo, sha] });
      setContext("");
      setDescription("");
    },
  });

  const statuses = statusQ.data?.statuses ?? [];
  return (
    <section aria-label="Commit statuses" className="mt-5">
      <SectionLabel>Statuses</SectionLabel>
      {statusQ.data && statuses.length > 0 && (
        <Box className="mb-3">
          {statuses.map((s, i) => (
            <div
              key={`${s.context}-${i}`}
              className="flex items-center gap-2"
              style={{ padding: "0.5rem 0.8rem", borderBottom: i === statuses.length - 1 ? "none" : "1px solid var(--color-border)" }}
            >
              <span className="min-w-0 flex-1">
                <span style={{ fontWeight: 500 }}>{s.context}</span>
                {s.description && (
                  <span style={{ color: "var(--color-fg-muted)", fontSize: "0.8rem" }}> — {s.description}</span>
                )}
              </span>
              <span style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>{s.state}</span>
            </div>
          ))}
        </Box>
      )}
      {createMut.error && (
        <InlineError inline title="Failed to create status" detail={String(createMut.error)} />
      )}
      <div className="flex flex-wrap items-end gap-2">
        <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
          State
          <select aria-label="status state" value={state} onChange={(e) => setState(e.target.value as GithubCommitStatusState)}>
            {COMMIT_STATUS_STATES.map((s) => (
              <option key={s} value={s}>{s}</option>
            ))}
          </select>
        </label>
        <label className="flex min-w-0 flex-1 flex-col gap-1" style={{ fontSize: "0.78rem" }}>
          Context
          <input aria-label="status context" value={context} onChange={(e) => setContext(e.target.value)} placeholder="ci/build" className="w-full" />
        </label>
        <label className="flex min-w-0 flex-1 flex-col gap-1" style={{ fontSize: "0.78rem" }}>
          Description
          <input aria-label="status description" value={description} onChange={(e) => setDescription(e.target.value)} placeholder="optional" className="w-full" />
        </label>
        <Button
          variant="primary"
          disabled={createMut.isPending || !context.trim()}
          onClick={() => createMut.mutate()}
        >
          Create status
        </Button>
      </div>
    </section>
  );
}

function CommitCommentsSection({ owner, repo, sha }: { owner: string; repo: string; sha: string }) {
  const qc = useQueryClient();
  const [body, setBody] = useState("");
  const listQ = useQuery({
    queryKey: ["commit-comments", owner, repo, sha],
    queryFn: () => fetchCommitComments(owner, repo, sha),
    enabled: !!owner && !!repo && !!sha,
  });
  const createMut = useMutation({
    mutationFn: () => createCommitComment(owner, repo, sha, body.trim()),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["commit-comments", owner, repo, sha] });
      setBody("");
    },
  });
  const viewerQ = useQuery({ queryKey: ["viewer"], queryFn: fetchAuthenticatedUser });
  const viewerLogin = typeof viewerQ.data?.login === "string" ? viewerQ.data.login : null;

  const comments = listQ.data ?? [];
  return (
    <section aria-label="Commit comments" className="mt-5">
      <SectionLabel>Comments</SectionLabel>
      {createMut.error && (
        <InlineError inline title="Failed to add comment" detail={String(createMut.error)} />
      )}
      {comments.map((c) => (
        <div key={c.id}>
          <CommentCard login={c.user?.login} body={c.body} date={c.created_at} />
          <ReactionBar
            queryKey={["commit-comment-reactions", owner, repo, c.id]}
            fetchList={() => fetchCommitCommentReactions(owner, repo, c.id)}
            add={(content) => addCommitCommentReaction(owner, repo, c.id, content)}
            remove={(reactionId) => removeCommitCommentReaction(owner, repo, c.id, reactionId)}
            viewerLogin={viewerLogin}
          />
        </div>
      ))}
      {comments.length === 0 && (
        <div style={{ padding: "0.25rem 0 0.75rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
          No comments yet.
        </div>
      )}
      <div className="flex flex-col gap-2">
        <textarea
          aria-label="commit comment"
          value={body}
          onChange={(e) => setBody(e.target.value)}
          placeholder="Leave a comment on this commit"
          rows={3}
          className="w-full"
          style={{ fontSize: "0.88rem", padding: "0.5rem" }}
        />
        <div className="flex justify-end">
          <Button
            variant="primary"
            disabled={createMut.isPending || !body.trim()}
            onClick={() => createMut.mutate()}
          >
            Comment
          </Button>
        </div>
      </div>
    </section>
  );
}

export function RepoComparePage() {
  const { owner = "", repo = "", range = "" } = useParams<{
    owner: string;
    repo: string;
    range: string;
  }>();
  const navigate = useNavigate();
  const separator = range.indexOf("...");
  const base = separator >= 0 ? range.slice(0, separator) : "";
  const head = separator >= 0 ? range.slice(separator + 3) : "";
  const [draftBase, setDraftBase] = useState(base);
  const [draftHead, setDraftHead] = useState(head);
  const counts = useOpenCounts(owner, repo);
  const branchesQuery = useQuery({
    queryKey: ["branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
    enabled: !!owner && !!repo,
  });
  const query = useQuery<GithubComparison>({
    queryKey: ["comparison", owner, repo, base, head],
    queryFn: () => fetchRepoComparison(owner, repo, base, head),
    enabled: !!owner && !!repo && !!base && !!head,
  });

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="code" {...counts} />
      <Box header={<span style={{ fontWeight: 600 }}>Compare changes</span>}>
        <form
          className="flex flex-wrap items-end gap-3"
          style={{ padding: "1rem" }}
          onSubmit={(event) => {
            event.preventDefault();
            if (draftBase && draftHead) {
              navigate(repoCodeRoute(owner, repo, { kind: "compare", base: draftBase, head: draftHead }));
            }
          }}
        >
          <label className="flex flex-col gap-1" style={{ fontSize: "0.8rem" }}>
            Base
            <input list="compare-refs" value={draftBase} onChange={(event) => setDraftBase(event.target.value)} />
          </label>
          <span style={{ paddingBottom: "0.45rem", color: "var(--color-fg-muted)" }}>…</span>
          <label className="flex flex-col gap-1" style={{ fontSize: "0.8rem" }}>
            Compare
            <input list="compare-refs" value={draftHead} onChange={(event) => setDraftHead(event.target.value)} />
          </label>
          <datalist id="compare-refs">
            {(branchesQuery.data ?? []).map((branch) => <option key={branch.name} value={branch.name} />)}
          </datalist>
          <Button type="submit" variant="primary" disabled={!draftBase || !draftHead}>Compare</Button>
        </form>
      </Box>
      {query.isLoading && <Spinner label={`comparing ${base} and ${head}`} />}
      {query.isError && <div className="mt-4"><InlineError title="Failed to compare refs" detail={String(query.error)} /></div>}
      {query.data && (
        <div className="mt-4 flex flex-col gap-4">
          <Box>
            <div className="flex flex-wrap items-center gap-3" style={{ padding: "1rem" }}>
              <span className="min-w-0 flex-1">
                <b>{query.data.status}</b>
                <span style={{ color: "var(--color-fg-muted)" }}>
                  {" "}· {query.data.ahead_by} ahead · {query.data.behind_by} behind · {query.data.total_commits} commits
                </span>
              </span>
              {/* PullsPage has no query-param prefill yet; ?compare={base}...{head}
                  is the agreed hand-off contract for its create flow. */}
              <ButtonLink
                variant="primary"
                size="sm"
                to={`/ui/repos/${owner}/${repo}/pulls?compare=${encodeURIComponent(base)}...${encodeURIComponent(head)}`}
              >
                Create pull request
              </ButtonLink>
            </div>
          </Box>
          <CommitsList owner={owner} repo={repo} commits={query.data.commits} loading={false} />
          {query.data.files?.map((file) => (
            <Box
              key={file.filename}
              header={
                <div className="flex w-full items-center gap-2">
                  <span className="font-mono min-w-0 flex-1 truncate">{file.filename}</span>
                  <span style={{ color: "var(--color-status-ok)" }}>+{file.additions}</span>
                  <span style={{ color: "var(--color-status-error)" }}>−{file.deletions}</span>
                </div>
              }
            >
              {file.patch ? (
                <DiffText patch={file.patch} />
              ) : (
                <div style={{ padding: "1rem", color: "var(--color-fg-muted)" }}>
                  Binary file or patch unavailable.
                </div>
              )}
            </Box>
          ))}
        </div>
      )}
    </div>
  );
}

export function RepoFilePage() {
  const params = useParams<{
    owner: string;
    repo: string;
    ref: string;
    "*": string;
  }>();
  const owner = params.owner ?? "";
  const repo = params.repo ?? "";
  const ref = params.ref ?? "";
  const path = params["*"] ?? "";
  const counts = useOpenCounts(owner, repo);
  // Blob Edit/Delete are push affordances; Raw/History/Blame/permalink stay
  // for everyone.
  const { canPush } = useRepoPermissions(owner, repo);
  const location = useLocation();
  const query = useQuery<GithubContentFile>({
    queryKey: ["file", owner, repo, ref, path],
    queryFn: () => fetchRepoFile(owner, repo, path, ref),
    enabled: !!owner && !!repo && !!ref && !!path,
  });
  const { data: branchList = [] } = useQuery({
    queryKey: ["branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
    enabled: !!owner && !!repo,
  });
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [message, setMessage] = useState("");
  const [copied, setCopied] = useState(false);
  // "Copy permalink" pins the URL to the ref's current commit SHA (github.com's
  // `y` shortcut), so the link keeps pointing at this exact revision. The
  // current #L… selection rides along.
  const copyPermalink = async () => {
    try {
      const commits = await fetchRepoCommits(owner, repo, { sha: ref, perPage: 1 });
      const sha = commits[0]?.sha ?? ref;
      const hash = parseLineHash(location.hash) ? location.hash : "";
      await navigator.clipboard.writeText(
        `${window.location.origin}/ui/repos/${owner}/${repo}/blob/${sha}/${path}${hash}`,
      );
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard denied or offline — leave the button label unchanged.
    }
  };
  const editMut = useMutation({
    mutationFn: () =>
      putFile(owner, repo, path, {
        message: message || `Update ${path}`,
        content: draft,
        sha: query.data?.sha,
        branch: ref,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["file", owner, repo, ref, path] });
      setEditing(false);
    },
  });
  const deleteMut = useMutation({
    mutationFn: () =>
      deleteFile(owner, repo, path, {
        message: `Delete ${path}`,
        sha: query.data?.sha ?? "",
        branch: ref,
      }),
    onSuccess: () => navigate(`/ui/repos/${owner}/${repo}`),
  });

  if (query.isLoading) return <Spinner label={`loading ${path}`} />;
  if (query.isError || !query.data) {
    return <InlineError title={`Failed to load ${path}`} detail={String(query.error)} />;
  }

  // null means the bytes are not UTF-8 text (image or other binary); the
  // BlobContent viewer picks the right rendering, and text-only affordances
  // (Raw, Edit) hide themselves.
  const content = decodeBlobText(query.data.content);

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="code" {...counts} />
      <div className="mb-4 flex flex-wrap items-center gap-2" style={{ fontSize: ".84rem" }}>
        <RefSwitcher
          owner={owner}
          repo={repo}
          current={ref}
          branches={branchList.map((b) => b.name)}
          onSelect={(nextRef) => navigate(repoCodeRoute(owner, repo, { kind: "blob", ref: nextRef, path }))}
        />
        <PathBreadcrumbs owner={owner} repo={repo} gitRef={ref} path={path} />
      </div>
      <Box
        header={
          <div className="flex w-full items-center gap-2">
            <FileIcon size={15} />
            <span className="font-mono min-w-0 flex-1 truncate">{path}</span>
            <span style={{ color: "var(--color-fg-muted)", fontSize: ".76rem" }}>
              {query.data.sha.slice(0, 7)}
            </span>
            {!editing && (
              <>
                {content !== null && (
                <Button
                  size="sm"
                  aria-label="View raw file"
                  onClick={() => {
                    // The decoded file text is already loaded; open it as a raw
                    // text blob, mirroring github.com's "Raw" view.
                    const url = URL.createObjectURL(new Blob([content], { type: "text/plain;charset=utf-8" }));
                    window.open(url, "_blank", "noopener");
                    setTimeout(() => URL.revokeObjectURL(url), 30_000);
                  }}
                >
                  Raw
                </Button>
                )}
                <ButtonLink size="sm" to={`/ui/repos/${owner}/${repo}/commits?path=${encodeURIComponent(path)}&sha=${encodeURIComponent(ref)}`}>
                  History
                </ButtonLink>
                <ButtonLink size="sm" to={`/ui/repos/${owner}/${repo}/blame/${ref}/${path}`}>
                  Blame
                </ButtonLink>
                <Button size="sm" onClick={copyPermalink}>
                  {copied ? "Copied!" : "Copy permalink"}
                </Button>
                {content !== null && canPush && (
                <Button
                  size="sm"
                  onClick={() => {
                    setDraft(content);
                    setMessage("");
                    setEditing(true);
                  }}
                >
                  Edit
                </Button>
                )}
                {canPush && (
                <Button
                  size="sm"
                  aria-label="Delete file"
                  disabled={deleteMut.isPending}
                  onClick={async () => {
                    if (
                      await confirmAction(`Delete ${path}?`, {
                        title: "Delete file",
                        confirmLabel: "Delete",
                      })
                    ) {
                      deleteMut.mutate();
                    }
                  }}
                >
                  Delete
                </Button>
                )}
              </>
            )}
          </div>
        }
      >
        {editing ? (
          <div style={{ padding: "1rem" }}>
            <textarea
              aria-label={`Edit ${path}`}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              rows={18}
              className="font-mono w-full"
              style={{ resize: "vertical", fontSize: ".8rem" }}
            />
            <div className="mt-3">
              <FormLabel id="commit-message">Commit message</FormLabel>
              <input
                id="commit-message"
                value={message}
                onChange={(e) => setMessage(e.target.value)}
                placeholder={`Update ${path}`}
                className="mb-2 w-full"
              />
            </div>
            <MutationError of={editMut} />
            <DialogActions>
              <Button variant="ghost" size="sm" onClick={() => setEditing(false)}>
                Cancel
              </Button>
              <Button
                variant="primary"
                size="sm"
                disabled={editMut.isPending}
                onClick={() => editMut.mutate()}
              >
                {editMut.isPending ? "Committing…" : "Commit changes"}
              </Button>
            </DialogActions>
          </div>
        ) : (
          <BlobContent
            path={path}
            name={query.data.name || path.split("/").pop() || path}
            base64={query.data.content}
            size={query.data.size}
          />
        )}
      </Box>
      <MutationError of={deleteMut} />
    </div>
  );
}

/** Fixed palette cycled across languages, largest share first. */
const LANGUAGE_BAR_COLORS = ["#3572A5", "#F1E05A", "#E34C26", "#563D7C", "#00ADD8", "#B07219", "#701516", "#178600"];

function LanguagesBar({ languages }: { languages: Record<string, number> }) {
  const entries = Object.entries(languages);
  const total = entries.reduce((sum, [, bytes]) => sum + bytes, 0);
  if (total === 0) return null;
  return (
    <div className="mb-4">
      <div
        className="flex overflow-hidden"
        style={{ height: 8, borderRadius: "var(--radius-md)", border: "1px solid var(--color-border)" }}
      >
        {entries.map(([lang, bytes], i) => (
          <span
            key={lang}
            title={`${lang} ${((bytes / total) * 100).toFixed(1)}%`}
            style={{
              width: `${(bytes / total) * 100}%`,
              background: LANGUAGE_BAR_COLORS[i % LANGUAGE_BAR_COLORS.length],
            }}
          />
        ))}
      </div>
      <div className="mt-1.5 flex flex-wrap gap-x-4 gap-y-1" style={{ fontSize: "0.78rem" }}>
        {entries.map(([lang, bytes], i) => (
          <span key={lang} className="inline-flex items-center gap-1.5">
            <span
              aria-hidden
              style={{
                width: 8,
                height: 8,
                borderRadius: "999px",
                background: LANGUAGE_BAR_COLORS[i % LANGUAGE_BAR_COLORS.length],
              }}
            />
            <span style={{ fontWeight: 500 }}>{lang}</span>
            <span style={{ color: "var(--color-fg-muted)" }}>
              {((bytes / total) * 100).toFixed(1)}%
            </span>
          </span>
        ))}
      </div>
    </div>
  );
}

function BranchesList({
  owner,
  repo,
  branches,
  defaultBranch,
}: {
  owner: string;
  repo: string;
  branches: GithubBranch[];
  defaultBranch: string;
}) {
  const qc = useQueryClient();
  // Creating and deleting branches needs push access (github.com hides both
  // from read-only viewers).
  const { canPush } = useRepoPermissions(owner, repo);
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [source, setSource] = useState(defaultBranch);
  const createMut = useMutation({
    mutationFn: () => {
      const sha = branches.find((b) => b.name === source)?.commit.sha ?? "";
      return createRef(owner, repo, `refs/heads/${name}`, sha);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["branches", owner, repo] });
      setCreating(false);
      setName("");
    },
  });
  const deleteMut = useMutation({
    mutationFn: (branch: string) => deleteRef(owner, repo, `heads/${branch}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["branches", owner, repo] }),
  });

  const newBranchButton = branches.length > 0 && canPush && (
    <Button size="sm" onClick={() => setCreating(true)}>
      New branch
    </Button>
  );
  const newBranchModal = creating && (
    <Modal title="Create a branch" onClose={() => setCreating(false)}>
      <FormLabel id="new-branch-name">Branch name</FormLabel>
      <input
        id="new-branch-name"
        autoFocus
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder="e.g. feature/login"
        className="mb-3 w-full"
      />
      <FormLabel id="new-branch-source">Create from</FormLabel>
      <select
        id="new-branch-source"
        value={source}
        onChange={(e) => setSource(e.target.value)}
        className="mb-3 w-full"
      >
        {branches.map((b) => (
          <option key={b.name} value={b.name}>
            {b.name}
          </option>
        ))}
      </select>
      <MutationError of={createMut} />
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={() => setCreating(false)}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!name.trim() || createMut.isPending}
          onClick={() => createMut.mutate()}
        >
          {createMut.isPending ? "Creating…" : "Create branch"}
        </Button>
      </DialogActions>
    </Modal>
  );

  // Head commits for every branch (author + date), concurrency-capped and
  // cached; used both for the row metadata and the Active/Stale bucketing.
  const headsQ = useQuery({
    queryKey: ["branch-heads", owner, repo, branches.map((b) => b.commit.sha).join(",")],
    queryFn: async () => {
      const entries = await Promise.all(
        branches.map(async (b) => {
          try {
            return [b.name, await commitBySha(owner, repo, b.commit.sha)] as const;
          } catch {
            return [b.name, null] as const;
          }
        }),
      );
      return new Map(entries);
    },
    enabled: branches.length > 0,
    staleTime: 60_000,
  });
  const heads = headsQ.data;

  if (branches.length === 0) return <Blankslate icon={<BranchIcon size={26} />} title="No branches" />;

  // GitHub's branches page: Default / Active / Stale (no commit in 3 months).
  const staleCutoff = Date.now() - 90 * 24 * 3600 * 1000;
  const defaultRows = branches.filter((b) => b.name === defaultBranch);
  const rest = branches.filter((b) => b.name !== defaultBranch);
  const isStale = (name: string) => {
    const date = heads?.get(name)?.commit.author?.date;
    return !!date && new Date(date).getTime() < staleCutoff;
  };
  const sections: { title: string; rows: GithubBranch[] }[] = [
    { title: "Default", rows: defaultRows },
    { title: "Active branches", rows: rest.filter((b) => !isStale(b.name)) },
    { title: "Stale branches", rows: rest.filter((b) => isStale(b.name)) },
  ].filter((s) => s.rows.length > 0);

  return (
    <>
      <div className="mb-3 flex justify-end">{newBranchButton}</div>
      {newBranchModal}
      <MutationError of={deleteMut} />
      <div className="flex flex-col gap-4">
        {sections.map((section) => (
          <section key={section.title} aria-label={section.title}>
            <div className="mb-1.5" style={{ fontSize: "0.8rem", fontWeight: 600, color: "var(--color-fg-muted)" }}>
              {section.title}
            </div>
            <Box>
              {section.rows.map((b, i) => (
                <BranchRow
                  key={b.name}
                  owner={owner}
                  repo={repo}
                  branch={b}
                  head={heads?.get(b.name) ?? null}
                  defaultBranch={defaultBranch}
                  isLast={i === section.rows.length - 1}
                  canPush={canPush}
                  deletePending={deleteMut.isPending}
                  onDelete={async () => {
                    if (await confirmAction(`Delete branch "${b.name}"?`, { title: "Delete branch", confirmLabel: "Delete" })) {
                      deleteMut.mutate(b.name);
                    }
                  }}
                />
              ))}
            </Box>
          </section>
        ))}
      </div>
    </>
  );
}

function BranchRow({
  owner,
  repo,
  branch: b,
  head,
  defaultBranch,
  isLast,
  canPush,
  deletePending,
  onDelete,
}: {
  owner: string;
  repo: string;
  branch: GithubBranch;
  head: GithubCommit | null;
  defaultBranch: string;
  isLast: boolean;
  canPush: boolean;
  deletePending: boolean;
  onDelete: () => void;
}) {
  const isDefault = b.name === defaultBranch;
  // Ahead/behind vs the default branch, computed lazily per rendered row and
  // cached (cachedMeta), so the fan-out stays bounded.
  const cmpQ = useQuery({
    queryKey: ["branch-ahead-behind", owner, repo, defaultBranch, b.name],
    queryFn: () => aheadBehind(owner, repo, defaultBranch, b.name),
    enabled: !isDefault && !!defaultBranch,
    staleTime: 60_000,
  });
  const authorLogin = head?.author?.login ?? head?.commit.author?.name;
  return (
    <div
      className="flex flex-wrap items-center gap-3"
      style={{
        padding: "0.65rem 1rem",
        borderBottom: isLast ? "none" : "1px solid var(--color-border)",
      }}
    >
      <BranchIcon size={14} style={{ color: "var(--color-fg-muted)" }} />
      <span className="font-mono min-w-0" style={{ fontSize: "0.85rem", fontWeight: 500, flex: "1 1 12rem" }}>
        {b.name}
        {isDefault && (
          <span
            style={{
              marginLeft: "0.6rem",
              fontSize: "0.72rem",
              fontWeight: 600,
              color: "var(--color-accent)",
              border: "1px solid var(--color-accent)",
              borderRadius: "2rem",
              padding: "0.05rem 0.5rem",
            }}
          >
            default
          </span>
        )}
        {b.protected && (
          // The protection editor is an admin settings surface; read-only
          // viewers get the informational badge without the link affordance.
          canPush ? (
            <Link
              to={`/ui/repos/${owner}/${repo}/settings/branch-protection`}
              style={{
                marginLeft: "0.45rem",
                fontSize: "0.72rem",
                fontWeight: 600,
                color: "var(--color-success-fg)",
                textDecoration: "none",
              }}
            >
              protected
            </Link>
          ) : (
            <span
              style={{
                marginLeft: "0.45rem",
                fontSize: "0.72rem",
                fontWeight: 600,
                color: "var(--color-success-fg)",
              }}
            >
              protected
            </span>
          )
        )}
      </span>
      <span className="inline-flex items-center gap-1.5" style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}>
        {authorLogin ? (
          <>
            <Avatar login={authorLogin} src={head?.author?.avatar_url} size={16} />
            <span>{authorLogin}</span>
            <RelativeTime iso={head?.commit.author?.date} />
          </>
        ) : (
          <span aria-hidden>—</span>
        )}
      </span>
      {!isDefault && (
        <span className="tabular-nums" style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)", whiteSpace: "nowrap" }}>
          {cmpQ.data ? `${cmpQ.data.ahead} ahead · ${cmpQ.data.behind} behind` : <span aria-hidden>—</span>}
        </span>
      )}
      <span className="font-mono" style={{ fontSize: "0.74rem", color: "var(--color-fg-muted)" }}>
        {b.commit.sha.slice(0, 7)}
      </span>
      {!isDefault && (
        <Link
          to={repoCodeRoute(owner, repo, { kind: "compare", base: defaultBranch, head: b.name })}
          style={{ color: "var(--color-accent)", fontSize: "0.78rem", textDecoration: "none", display: "inline-block", lineHeight: "1.625rem" }}
        >
          Compare
        </Link>
      )}
      {!isDefault && (
        <ButtonLink
          size="sm"
          to={repoCodeRoute(owner, repo, { kind: "compare", base: defaultBranch, head: b.name })}
        >
          New pull request
        </ButtonLink>
      )}
      {!isDefault && !b.protected && canPush && (
        <Button
          size="sm"
          variant="danger"
          aria-label={`Delete branch ${b.name}`}
          disabled={deletePending}
          onClick={onDelete}
        >
          Delete
        </Button>
      )}
    </div>
  );
}

function TagsList({
  owner,
  repo,
  tags,
  branches,
  defaultBranch,
}: {
  owner: string;
  repo: string;
  tags: GithubTag[];
  branches: GithubBranch[];
  defaultBranch: string;
}) {
  const qc = useQueryClient();
  // Creating and deleting tags needs push access.
  const { canPush } = useRepoPermissions(owner, repo);
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [source, setSource] = useState(defaultBranch);
  // A tag with a same-named release links through to it (GitHub's tag list).
  const { data: releases = [] } = useQuery({
    queryKey: ["releases", owner, repo],
    queryFn: () => fetchReleases(owner, repo),
    enabled: !!owner && !!repo,
  });
  const createMut = useMutation({
    mutationFn: () => {
      const sha = branches.find((b) => b.name === source)?.commit.sha ?? "";
      return createRef(owner, repo, `refs/tags/${name}`, sha);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["repo-tags", owner, repo] });
      setCreating(false);
      setName("");
    },
  });
  const deleteMut = useMutation({
    mutationFn: (tag: string) => deleteRef(owner, repo, `tags/${tag}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["repo-tags", owner, repo] }),
  });

  const newTagModal = creating && (
    <Modal title="Create a tag" onClose={() => setCreating(false)}>
      <FormLabel id="new-tag-name">Tag name</FormLabel>
      <input
        id="new-tag-name"
        autoFocus
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder="e.g. v1.0.0"
        className="mb-3 w-full"
      />
      <FormLabel id="new-tag-source">Create from</FormLabel>
      <select
        id="new-tag-source"
        value={source}
        onChange={(e) => setSource(e.target.value)}
        className="mb-3 w-full"
      >
        {branches.map((b) => (
          <option key={b.name} value={b.name}>
            {b.name}
          </option>
        ))}
      </select>
      <MutationError of={createMut} />
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={() => setCreating(false)}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!name.trim() || !branches.length || createMut.isPending}
          onClick={() => createMut.mutate()}
        >
          {createMut.isPending ? "Creating…" : "Create tag"}
        </Button>
      </DialogActions>
    </Modal>
  );

  return (
    <>
      {canPush && (
        <div className="mb-3 flex justify-end">
          <Button size="sm" disabled={!branches.length} onClick={() => setCreating(true)}>
            New tag
          </Button>
        </div>
      )}
      {newTagModal}
      <MutationError of={deleteMut} />
      {tags.length === 0 ? (
        <Blankslate icon={<TagIcon size={26} />} title="No tags" />
      ) : (
    <Box>
      {tags.map((t, i) => (
        <TagRow
          key={t.name}
          owner={owner}
          repo={repo}
          tag={t}
          release={releases.find((r) => r.tag_name === t.name)}
          isLast={i === tags.length - 1}
          canPush={canPush}
          deletePending={deleteMut.isPending}
          onDelete={async () => {
            if (await confirmAction(`Delete tag "${t.name}"?`, { title: "Delete tag", confirmLabel: "Delete" })) {
              deleteMut.mutate(t.name);
            }
          }}
        />
      ))}
    </Box>
      )}
    </>
  );
}

const smallLink = {
  fontSize: "0.78rem",
  color: "var(--color-accent)",
  textDecoration: "none",
  display: "inline-block",
  lineHeight: "1.625rem",
} as const;

function TagRow({
  owner,
  repo,
  tag: t,
  release,
  isLast,
  canPush,
  deletePending,
  onDelete,
}: {
  owner: string;
  repo: string;
  tag: GithubTag;
  release: GithubRelease | undefined;
  isLast: boolean;
  canPush: boolean;
  deletePending: boolean;
  onDelete: () => void;
}) {
  // The tags REST payload carries only the commit sha; GitHub's tag list also
  // shows when the tagged commit landed, so resolve it (capped + cached).
  const commitQ = useQuery({
    queryKey: ["tag-commit", owner, repo, t.commit.sha],
    queryFn: () => commitBySha(owner, repo, t.commit.sha),
    staleTime: 5 * 60_000,
  });
  return (
    <div
      className="flex flex-wrap items-center gap-3"
      style={{
        padding: "0.65rem 1rem",
        borderBottom: isLast ? "none" : "1px solid var(--color-border)",
      }}
    >
      <TagIcon size={14} style={{ color: "var(--color-fg-muted)" }} />
      <span className="font-mono min-w-0" style={{ fontSize: "0.85rem", fontWeight: 500, flex: "1 1 10rem" }}>
        {t.name}
      </span>
      <span style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)", whiteSpace: "nowrap" }}>
        {commitQ.data ? <RelativeTime iso={commitQ.data.commit.author?.date} /> : <span aria-hidden>—</span>}
      </span>
      <Link
        to={`/ui/repos/${owner}/${repo}/commits/${t.commit.sha}`}
        className="font-mono"
        style={{ ...smallLink, fontSize: "0.74rem", color: "var(--color-fg-muted)" }}
      >
        {t.commit.sha.slice(0, 7)}
      </Link>
      {release && (
        <Link to={`/ui/repos/${owner}/${repo}/releases/${release.id}`} style={smallLink}>
          Release
        </Link>
      )}
      <a href={t.zipball_url} style={smallLink}>
        zip
      </a>
      <a href={t.tarball_url} style={smallLink}>
        tar.gz
      </a>
      {canPush && (
        <Button
          size="sm"
          variant="danger"
          aria-label={`Delete tag ${t.name}`}
          disabled={deletePending}
          onClick={onDelete}
        >
          Delete
        </Button>
      )}
    </div>
  );
}
