import { useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router";
import { useQueries, useQuery } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  ApiError,
  authHeaders,
  fetchRepoDetail,
  ghFetch,
  isNotFound,
  isRateLimited,
  searchCommits,
  searchIssues,
  searchLabels,
  searchRepositories,
  searchTopics,
  searchUsers,
  SEARCH_PER_PAGE,
  type SearchResultPage,
} from "../api.js";
import type {
  BleephubRepo,
  GithubSearchCodeItem,
  GithubSearchCommitItem,
  GithubSearchIssueItem,
  GithubSearchLabelItem,
  GithubSearchTextMatch,
  GithubSearchTextMatchSpan,
  GithubSearchTopicItem,
  GithubSearchUserItem,
} from "../types.js";
import { accountRoute, repoCodeRoute } from "../routes.js";
import { PageTitle, Box, Blankslate, Button, StateLabel } from "../components/ui.js";
import { SearchIcon } from "../components/octicons.js";
import { SignInPrompt } from "../components/SignInPrompt.js";
import { useSignedIn } from "../session.js";

type SearchTab = "repositories" | "code" | "issues" | "users" | "commits" | "labels" | "topics";
type RepositorySearchFilters = {
  visibility: string;
  archived: string;
  fork: string;
  topic: string;
  excludeTopic: string;
  language: string;
  sort: string;
  order: string;
};

const TABS: { key: SearchTab; label: string }[] = [
  { key: "repositories", label: "Repositories" },
  { key: "code", label: "Code" },
  { key: "issues", label: "Issues & PRs" },
  { key: "users", label: "Users" },
  { key: "commits", label: "Commits" },
  { key: "labels", label: "Labels" },
  { key: "topics", label: "Topics" },
];

// Types whose count can be probed with just the free-text query. Labels are
// excluded (the endpoint requires a repository_id) and code is skipped when
// the query has no free-text term (the server 422s qualifier-only code
// queries).
const COUNTABLE_TABS: SearchTab[] = ["repositories", "code", "issues", "users", "commits", "topics"];

/** Auto-search debounce: URL-driven re-queries (filter keystrokes, sort
 *  changes) wait this long before hitting the 30/min search budget. */
const SEARCH_DEBOUNCE_MS = 250;

// Sort keys the server actually honors (gh_search.go: issue rows sort on
// created/updated plus the comments render-all path; user results sort on
// followers/created/updated). Only these are offered so no control is a no-op.
const ISSUE_SORT_OPTIONS: { value: string; label: string }[] = [
  { value: "created", label: "Newest" },
  { value: "updated", label: "Recently updated" },
  { value: "comments", label: "Most commented" },
];
const USER_SORT_OPTIONS: { value: string; label: string }[] = [
  { value: "followers", label: "Most followers" },
  { value: "created", label: "Recently joined" },
  { value: "updated", label: "Recently active" },
];

/** True if the query has at least one non-qualifier free-text token. */
function hasFreeTextTerm(query: string): boolean {
  return query
    .trim()
    .split(/\s+/)
    .some((token) => token !== "" && !/^-?[A-Za-z_]+:/.test(token));
}

// ─── page-local search helpers ───────────────────────────────────────────────

/** GET /search/code item plus the opt-in text-match payload; the repository is
 *  the full REST repo shape, so default_branch is available for blob links. */
export type CodeSearchItem = GithubSearchCodeItem & {
  repository: GithubSearchCodeItem["repository"] & { default_branch?: string };
  text_matches?: GithubSearchTextMatch[];
};

/**
 * Code search with the text-match media type: the plain api.ts search wrappers
 * cannot set an Accept header, so this page-local fetcher opts into
 * application/vnd.github.text-match+json itself and mirrors the module's
 * rate-limit ApiError contract (retryAfterSeconds on a throttled 403).
 */
async function searchCodeWithMatches(q: string, page: number): Promise<SearchResultPage<CodeSearchItem>> {
  const params = new URLSearchParams({ q, page: String(page), per_page: String(SEARCH_PER_PAGE) });
  const res = await fetch(`/api/v3/search/code?${params}`, {
    headers: { ...authHeaders(), Accept: "application/vnd.github.text-match+json" },
  });
  if (!res.ok) {
    throw new ApiError(res.status, `${res.status} ${res.statusText}`, rateLimitOptions(res));
  }
  const body = (await res.json()) as { total_count: number; incomplete_results: boolean; items: CodeSearchItem[] };
  if (!Array.isArray(body.items)) throw new Error(`malformed response: missing "items" array`);
  return { totalCount: body.total_count, incompleteResults: body.incomplete_results, items: body.items };
}

/** retryAfterSeconds for a throttled 403, mirroring api.ts's classification. */
function rateLimitOptions(res: Response): { retryAfterSeconds: number } | undefined {
  if (res.status !== 403) return undefined;
  const retryAfter = Number(res.headers.get("Retry-After"));
  if (Number.isFinite(retryAfter) && retryAfter > 0) return { retryAfterSeconds: retryAfter };
  if (res.headers.get("X-RateLimit-Remaining") === "0") return { retryAfterSeconds: 60 };
  return undefined;
}

/** Exact result count for one type: search envelopes carry total_count, so a
 *  per_page=1 probe answers the sidebar count in one cheap request. */
const fetchSearchCount = (endpoint: SearchTab, q: string) =>
  ghFetch<{ total_count: number }>(
    `/api/v3/search/${endpoint}?${new URLSearchParams({ q, per_page: "1" })}`,
  ).then((body) => body.total_count);

/** The ref segment of a …/{owner}/{repo}/blob/{ref}/{path} html_url. */
export function blobRefFromHtmlUrl(htmlUrl: string): string | null {
  const m = htmlUrl.match(/\/blob\/([^/]+)\//);
  return m ? m[1]! : null;
}

/**
 * Splits a text-match fragment into plain/matched runs. Match indices are
 * fragment-relative BYTE offsets (the server is Go), so the fragment is
 * sliced on its UTF-8 bytes, not on UTF-16 code units.
 */
export function splitTextMatchFragment(
  fragment: string,
  matches: GithubSearchTextMatchSpan[],
): { text: string; matched: boolean }[] {
  const bytes = new TextEncoder().encode(fragment);
  const spans = matches
    .map((m) => m.indices)
    .filter(
      (pair): pair is [number, number] =>
        Array.isArray(pair) && pair.length === 2 && pair[0] >= 0 && pair[1] > pair[0] && pair[1] <= bytes.length,
    )
    .sort((a, b) => a[0] - b[0]);
  const merged: [number, number][] = [];
  for (const [start, end] of spans) {
    const last = merged[merged.length - 1];
    if (last && start <= last[1]) last[1] = Math.max(last[1], end);
    else merged.push([start, end]);
  }
  const decoder = new TextDecoder();
  const parts: { text: string; matched: boolean }[] = [];
  let pos = 0;
  for (const [start, end] of merged) {
    if (start > pos) parts.push({ text: decoder.decode(bytes.subarray(pos, start)), matched: false });
    parts.push({ text: decoder.decode(bytes.subarray(start, end)), matched: true });
    pos = end;
  }
  if (pos < bytes.length) parts.push({ text: decoder.decode(bytes.subarray(pos)), matched: false });
  return parts.filter((p) => p.text !== "");
}

// ─── page ─────────────────────────────────────────────────────────────────────

export function SearchPage() {
  const [params, setParams] = useSearchParams();
  const q = params.get("q") ?? "";
  const tab = (TABS.some((t) => t.key === params.get("type"))
    ? params.get("type")
    : "repositories") as SearchTab;
  const page = Math.max(1, parseInt(params.get("page") ?? "1", 10) || 1);
  const labelsRepo = params.get("repo") ?? "";
  const [draft, setDraft] = useState(q);
  const [labelsRepoDraft, setLabelsRepoDraft] = useState(labelsRepo);
  const [showAdvanced, setShowAdvanced] = useState(false);
  // The active type's count is reported up by its ResultList; the other types'
  // counts are probed lazily (below) once the active search has answered.
  const [activeCount, setActiveCount] = useState<number | null>(null);
  const repositoryFilters: RepositorySearchFilters = {
    visibility: params.get("visibility") ?? "",
    archived: params.get("archived") ?? "",
    fork: params.get("fork") ?? "",
    topic: params.get("topic") ?? "",
    excludeTopic: params.get("exclude_topic") ?? "",
    language: params.get("language") ?? "",
    sort: params.get("sort") ?? "",
    order: params.get("order") ?? "desc",
  };
  // Issues and Users share the sort/order URL params with Repositories, so a
  // sort carried across tabs is normalized to "best match" unless it is valid
  // for the active tab.
  const rawSort = params.get("sort") ?? "";
  const order = params.get("order") === "asc" ? "asc" : "desc";
  const issuesSort = ISSUE_SORT_OPTIONS.some((o) => o.value === rawSort) ? rawSort : "";
  const usersSort = USER_SORT_OPTIONS.some((o) => o.value === rawSort) ? rawSort : "";
  const hasSearch =
    !!q.trim() ||
    (tab === "repositories" && !!buildRepositoryQuery("", repositoryFilters));

  useEffect(() => setDraft(q), [q]);
  useEffect(() => setLabelsRepoDraft(labelsRepo), [labelsRepo]);
  useEffect(() => setActiveCount(null), [q, tab]);

  // Lazy per-type counts for the sidebar: probed only after the active type's
  // search has answered (so a throttled session doesn't fan out further), one
  // cheap per_page=1 request per type, cached for a minute per (type, query).
  const trimmedQ = q.trim();
  // Code search requires authentication (the server, like real GitHub, 401s
  // anonymous code queries), so its count probe is signed-in only.
  const signedIn = useSignedIn();
  const countQueries = useQueries({
    queries: COUNTABLE_TABS.map((key) => ({
      queryKey: ["search-count", key, trimmedQ],
      queryFn: () => fetchSearchCount(key, trimmedQ),
      enabled:
        !!trimmedQ &&
        activeCount !== null &&
        key !== tab &&
        (key !== "code" || (signedIn && hasFreeTextTerm(trimmedQ))),
      staleTime: 60_000,
      retry: false,
    })),
  });
  const counts: Partial<Record<SearchTab, number | null>> = {};
  COUNTABLE_TABS.forEach((key, i) => {
    counts[key] = countQueries[i]?.data ?? null;
  });
  counts[tab] = activeCount;

  const update = (next: Record<string, string>) => {
    const merged = new URLSearchParams(params);
    for (const [k, v] of Object.entries(next)) {
      if (v) merged.set(k, v);
      else merged.delete(k);
    }
    setParams(merged);
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    update({ q: draft.trim(), repo: labelsRepoDraft.trim(), page: "" });
  };

  return (
    <div>
      <PageTitle title="Search" />
      <form onSubmit={submit} className="mb-1 flex flex-wrap items-center gap-2">
        <input
          type="search"
          aria-label="Search query"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder="Search bleephub…"
          style={{ fontSize: "0.9rem", padding: "0.45rem 0.6rem", minWidth: "20rem", flex: 1 }}
        />
        {tab === "labels" && (
          <input
            type="text"
            aria-label="Repository for label search"
            value={labelsRepoDraft}
            onChange={(e) => setLabelsRepoDraft(e.target.value)}
            placeholder="owner/repo (required for labels)"
            style={{ fontSize: "0.9rem", padding: "0.45rem 0.6rem", minWidth: "14rem" }}
          />
        )}
        <Button type="submit" variant="primary">
          <span className="inline-flex items-center gap-1.5">
            <SearchIcon size={14} /> Search
          </span>
        </Button>
        <Button type="button" variant="secondary" aria-expanded={showAdvanced} onClick={() => setShowAdvanced((v) => !v)}>
          Advanced
        </Button>
      </form>
      {showAdvanced && (
        <AdvancedSearchForm
          onBuild={(query) => {
            setDraft(query);
            update({ q: query, page: "" });
            setShowAdvanced(false);
          }}
        />
      )}
      <p className="mb-4" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
        Qualifiers: <code>repo:owner/name</code> <code>user:login</code> <code>org:name</code>{" "}
        <code>language:go</code> <code>topic:web</code> <code>-topic:legacy</code>{" "}
        <code>archived:false</code> <code>fork:only</code> <code>stars:&gt;10</code>{" "}
        <code>label:bug</code> <code>state:open</code> <code>is:pr</code> <code>in:title</code>{" "}
        <code>path:dir</code> <code>extension:go</code> <code>author:login</code> — quote multi-word
        terms and prefix a qualifier with <code>-</code> to exclude matches.
      </p>
      <div className="grid items-start gap-6 md:grid-cols-[13rem_minmax(0,1fr)]">
        {/* github.com's search sidebar: result types with per-type counts. */}
        <SearchTypeNav active={tab} counts={hasSearch ? counts : {}} onChange={(next) => update({ type: next, page: "" })} />
        <div className="min-w-0">
          {tab === "repositories" && (
            <RepositoryFilters
              filters={repositoryFilters}
              onChange={(next) => update({ ...next, page: "" })}
            />
          )}
          {tab === "issues" && (
            <SortControls
              title="Issue sort"
              sortLabel="Issue search sort"
              orderLabel="Issue search order"
              sort={issuesSort}
              order={order}
              options={ISSUE_SORT_OPTIONS}
              onChange={(next) => update({ ...next, page: "" })}
            />
          )}
          {tab === "users" && (
            <SortControls
              title="User sort"
              sortLabel="User search sort"
              orderLabel="User search order"
              sort={usersSort}
              order={order}
              options={USER_SORT_OPTIONS}
              onChange={(next) => update({ ...next, page: "" })}
            />
          )}
          {!hasSearch ? (
            <Blankslate
              icon={<SearchIcon size={26} />}
              title="Search bleephub"
            >
              <p>Type a query above to search {TABS.find((t) => t.key === tab)?.label.toLowerCase()}.</p>
            </Blankslate>
          ) : (
            <SearchResults
              key={tab}
              tab={tab}
              q={q}
              page={page}
              labelsRepo={labelsRepo}
              repositoryFilters={repositoryFilters}
              issuesSort={issuesSort}
              usersSort={usersSort}
              order={order}
              onPage={(p) => update({ page: String(p) })}
              onCount={setActiveCount}
            />
          )}
        </div>
      </div>
    </div>
  );
}

/** Left sidebar of result types with per-type counts (github.com's search
 *  layout). Counts render as "—" until known. */
function SearchTypeNav({
  active,
  counts,
  onChange,
}: {
  active: SearchTab;
  counts: Partial<Record<SearchTab, number | null>>;
  onChange: (next: SearchTab) => void;
}) {
  return (
    <nav aria-label="Search result types">
      <ul style={{ listStyle: "none", margin: 0, padding: 0 }} className="flex flex-row flex-wrap gap-1 md:flex-col">
        {TABS.map((t) => {
          const isActive = t.key === active;
          const count = counts[t.key];
          return (
            <li key={t.key}>
              <button
                type="button"
                aria-current={isActive ? "page" : undefined}
                onClick={() => onChange(t.key)}
                className="flex w-full items-center justify-between gap-3"
                style={{
                  padding: "0.4rem 0.65rem",
                  minHeight: "1.75rem",
                  fontSize: "0.85rem",
                  fontWeight: isActive ? 600 : 400,
                  color: "var(--color-fg)",
                  background: isActive ? "var(--color-bg-subtle)" : "transparent",
                  border: "none",
                  borderLeft: `2px solid ${isActive ? "var(--color-accent)" : "transparent"}`,
                  borderRadius: "var(--radius-sm)",
                  cursor: "pointer",
                  textAlign: "left",
                }}
              >
                <span>{t.label}</span>
                <span
                  className="tabular-nums"
                  style={{
                    fontSize: "0.72rem",
                    color: "var(--color-fg-muted)",
                    background: "var(--color-bg-subtle)",
                    border: "1px solid var(--color-border)",
                    borderRadius: "999px",
                    padding: "0.05rem 0.45rem",
                  }}
                >
                  {typeof count === "number" ? count.toLocaleString() : "—"}
                </span>
              </button>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}

interface AdvancedSearchFields {
  keywords: string;
  language: string;
  repo: string;
  user: string;
  org: string;
  topic: string;
  stars: string;
}

/** Assemble a GitHub qualifier query from the advanced-search fields. Multi-word
 *  values are quoted; `stars` becomes a `>=N` range. Empty fields are dropped. */
export function buildAdvancedQuery(f: AdvancedSearchFields): string {
  const parts: string[] = [];
  if (f.keywords.trim()) parts.push(f.keywords.trim());
  if (f.language.trim()) parts.push(`language:${quotedQualifierValue(f.language.trim())}`);
  if (f.repo.trim()) parts.push(`repo:${f.repo.trim()}`);
  if (f.user.trim()) parts.push(`user:${f.user.trim()}`);
  if (f.org.trim()) parts.push(`org:${f.org.trim()}`);
  if (f.topic.trim()) parts.push(`topic:${quotedQualifierValue(f.topic.trim())}`);
  if (f.stars.trim() && !Number.isNaN(Number(f.stars))) parts.push(`stars:>=${Number(f.stars)}`);
  return parts.join(" ");
}

// GitHub's /search/advanced query builder, inlined so it needs no extra route.
function AdvancedSearchForm({ onBuild }: { onBuild: (query: string) => void }) {
  const [fields, setFields] = useState<AdvancedSearchFields>({
    keywords: "", language: "", repo: "", user: "", org: "", topic: "", stars: "",
  });
  const set = (k: keyof AdvancedSearchFields) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setFields((prev) => ({ ...prev, [k]: e.target.value }));
  const rows: { key: keyof AdvancedSearchFields; label: string; placeholder: string; type?: string }[] = [
    { key: "keywords", label: "With these words", placeholder: "exact wording or free text" },
    { key: "language", label: "Written in language", placeholder: "go" },
    { key: "repo", label: "In repository", placeholder: "owner/name" },
    { key: "user", label: "From user", placeholder: "login" },
    { key: "org", label: "In organization", placeholder: "org login" },
    { key: "topic", label: "With topic", placeholder: "web" },
    { key: "stars", label: "With at least this many stars", placeholder: "10", type: "number" },
  ];
  const preview = buildAdvancedQuery(fields);
  return (
    <Box header={<span style={{ fontWeight: 600 }}>Advanced search</span>}>
      <form
        className="flex flex-col gap-3 p-3"
        onSubmit={(e) => { e.preventDefault(); if (preview) onBuild(preview); }}
      >
        <div className="grid gap-3" style={{ gridTemplateColumns: "repeat(auto-fill, minmax(16rem, 1fr))" }}>
          {rows.map((r) => (
            <label key={r.key} className="flex flex-col gap-1" style={{ fontSize: "0.82rem" }}>
              <span>{r.label}</span>
              <input
                type={r.type ?? "text"}
                aria-label={r.label}
                value={fields[r.key]}
                onChange={set(r.key)}
                placeholder={r.placeholder}
                style={{ fontSize: "0.85rem", padding: "0.4rem 0.5rem" }}
              />
            </label>
          ))}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button type="submit" variant="primary" disabled={!preview}>Build query</Button>
          {preview && <code style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>{preview}</code>}
        </div>
      </form>
    </Box>
  );
}

function SearchResults({
  tab,
  q,
  page,
  labelsRepo,
  repositoryFilters,
  issuesSort,
  usersSort,
  order,
  onPage,
  onCount,
}: {
  tab: SearchTab;
  q: string;
  page: number;
  labelsRepo: string;
  repositoryFilters: RepositorySearchFilters;
  issuesSort: string;
  usersSort: string;
  order: "asc" | "desc";
  onPage: (page: number) => void;
  onCount: (count: number) => void;
}) {
  // Code search is signed-in only (see the "code" case below).
  const signedIn = useSignedIn();
  switch (tab) {
    case "repositories": {
      const repositoryQuery = buildRepositoryQuery(q, repositoryFilters);
      return (
        <ResultList
          queryKey={[
            "search",
            "repositories",
            repositoryQuery,
            repositoryFilters.sort,
            repositoryFilters.order,
            page,
          ]}
          queryFn={() =>
            searchRepositories(repositoryQuery, page, {
              sort: repositoryFilters.sort as "stars" | "forks" | "help-wanted-issues" | "updated" | undefined,
              order: repositoryFilters.sort
                ? repositoryFilters.order as "asc" | "desc"
                : undefined,
            })
          }
          page={page}
          onPage={onPage}
          onCount={onCount}
          noun={{ singular: "repository", plural: "repositories" }}
          render={(r: BleephubRepo) => (
            <div>
              <Link
                to={`/ui/repos/${r.full_name}`}
                style={{ color: "var(--color-accent)", fontWeight: 600, textDecoration: "none" }}
              >
                {r.full_name}
              </Link>
              <div style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
                {r.description || "No description."}
              </div>
              <div
                className="mt-1 flex flex-wrap gap-2"
                style={{ fontSize: "0.74rem", color: "var(--color-fg-muted)" }}
              >
                <span>{r.visibility}</span>
                {r.archived && <span>archived</span>}
                {r.fork && <span>fork</span>}
                {r.language && <span>{r.language}</span>}
                {typeof r.stargazers_count === "number" && <span>★ {r.stargazers_count}</span>}
                {typeof r.forks_count === "number" && <span>{r.forks_count} forks</span>}
              </div>
            </div>
          )}
        />
      );
    }
    case "code":
      // Code search requires authentication on the server (and on real
      // GitHub, which asks anonymous visitors to sign in for code search).
      if (!signedIn) {
        return <SignInPrompt action="search code" />;
      }
      // The server (and real GitHub) 422 a code query with no free-text term
      // (qualifiers only). Guide instead of firing a request that errors.
      if (!hasFreeTextTerm(q)) {
        return (
          <Blankslate title="Enter a search term">
            Code search needs at least one word to match; qualifiers like <code>language:go</code> only narrow the results.
          </Blankslate>
        );
      }
      return (
        <ResultList
          queryKey={["search", "code", q, page]}
          queryFn={() => searchCodeWithMatches(q, page)}
          page={page}
          onPage={onPage}
          onCount={onCount}
          noun={{ singular: "code result", plural: "code results" }}
          render={(item: CodeSearchItem) => <CodeResultRow item={item} />}
        />
      );
    case "issues":
      return (
        <ResultList
          queryKey={["search", "issues", q, issuesSort, order, page]}
          queryFn={() =>
            searchIssues(q, page, {
              sort: (issuesSort || undefined) as "comments" | "created" | "updated" | undefined,
              order: issuesSort ? order : undefined,
            })
          }
          page={page}
          onPage={onPage}
          onCount={onCount}
          noun={{ singular: "issue or pull request", plural: "issues and pull requests" }}
          render={(item: GithubSearchIssueItem) => (
            <div className="flex items-center gap-2">
              <StateLabel state={item.state === "open" ? "open" : "closed"}>{item.state}</StateLabel>
              <div className="min-w-0 flex-1">
                <Link
                  to={`/ui/repos/${item.repository.full_name}/${item.pull_request ? "pulls" : "issues"}/${item.number}`}
                  style={{ color: "var(--color-fg)", fontWeight: 600, textDecoration: "none" }}
                >
                  {item.title}
                </Link>
                <div style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                  {item.pull_request ? "pull request" : "issue"} · {item.repository.full_name}#
                  {item.number} · {item.user?.login ?? "ghost"} · {item.comments} comments
                </div>
              </div>
            </div>
          )}
        />
      );
    case "users":
      return (
        <ResultList
          queryKey={["search", "users", q, usersSort, order, page]}
          queryFn={() =>
            searchUsers(q, page, {
              sort: (usersSort || undefined) as "followers" | "created" | "updated" | undefined,
              order: usersSort ? order : undefined,
            })
          }
          page={page}
          onPage={onPage}
          onCount={onCount}
          noun={{ singular: "user", plural: "users" }}
          render={(u: GithubSearchUserItem) => (
            <div>
              <Link
                to={accountRoute(u.login, u.type)}
                // inline-block + ≥24px line-height: a standalone list link must
                // clear WCAG 2.5.8 target-size.
                style={{
                  display: "inline-block",
                  color: "var(--color-accent)",
                  fontWeight: 600,
                  lineHeight: "1.625rem",
                  textDecoration: "none",
                }}
              >
                {u.login}
              </Link>
              <span style={{ marginLeft: "0.5rem", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                {u.type}
                {u.name ? ` · ${u.name}` : ""}
                {u.bio ? ` · ${u.bio}` : ""}
              </span>
            </div>
          )}
        />
      );
    case "commits":
      return (
        <ResultList
          queryKey={["search", "commits", q, page]}
          queryFn={() => searchCommits(q, page)}
          page={page}
          onPage={onPage}
          onCount={onCount}
          noun={{ singular: "commit", plural: "commits" }}
          render={(c: GithubSearchCommitItem) => (
            <div>
              <Link
                to={`/ui/repos/${c.repository.full_name}/commits/${c.sha}`}
                style={{
                  display: "inline-block",
                  color: "var(--color-fg)",
                  fontWeight: 600,
                  fontSize: "0.88rem",
                  lineHeight: "1.625rem",
                  textDecoration: "none",
                }}
              >
                {c.commit.message.split("\n")[0]}
              </Link>
              <div style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                {c.repository.full_name} ·{" "}
                <span className="font-mono">{c.sha.slice(0, 7)}</span> ·{" "}
                {c.author?.login ?? c.commit.author.name} ·{" "}
                {new Date(c.commit.author.date).toLocaleDateString()}
              </div>
            </div>
          )}
        />
      );
    case "labels":
      return <LabelResults q={q} page={page} labelsRepo={labelsRepo} onPage={onPage} onCount={onCount} />;
    case "topics":
      return (
        <ResultList
          queryKey={["search", "topics", q, page]}
          queryFn={() => searchTopics(q, page)}
          page={page}
          onPage={onPage}
          onCount={onCount}
          noun={{ singular: "topic", plural: "topics" }}
          render={(t: GithubSearchTopicItem) => (
            <div>
              <span style={{ fontWeight: 600 }}>{t.name}</span>
              <span style={{ marginLeft: "0.5rem", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                {t.repository_count} {t.repository_count === 1 ? "repository" : "repositories"}
              </span>
            </div>
          )}
        />
      );
  }
}

/** One code search result: the file links to its blob view and the opt-in
 *  text-match fragments render with the matched spans highlighted. */
function CodeResultRow({ item }: { item: CodeSearchItem }) {
  const [owner = "", repo = ""] = item.repository.full_name.split("/");
  const ref = blobRefFromHtmlUrl(item.html_url) ?? item.repository.default_branch ?? "main";
  const fragments = (item.text_matches ?? []).filter((m) => m.property === "content").slice(0, 2);
  return (
    <div>
      <div>
        <Link
          to={repoCodeRoute(owner, repo, { kind: "blob", ref, path: item.path })}
          // inline-block + ≥24px line-height: a standalone list link must
          // clear WCAG 2.5.8 target-size.
          style={{
            display: "inline-block",
            color: "var(--color-accent)",
            textDecoration: "none",
            lineHeight: "1.625rem",
          }}
        >
          <span style={{ fontWeight: 600 }}>{item.repository.full_name}</span>
          <span className="font-mono" style={{ marginLeft: "0.5rem", fontSize: "0.82rem" }}>
            {item.path}
          </span>
        </Link>
        <span style={{ marginLeft: "0.5rem", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
          {item.language ?? "unknown language"}
        </span>
      </div>
      {fragments.map((m, i) => (
        <pre
          key={i}
          className="mt-1"
          style={{
            margin: 0,
            padding: "0.4rem 0.6rem",
            fontSize: "0.76rem",
            lineHeight: 1.5,
            background: "var(--color-bg-subtle)",
            border: "1px solid var(--color-border)",
            borderRadius: "var(--radius-sm)",
            overflowX: "auto",
            whiteSpace: "pre-wrap",
            wordBreak: "break-word",
          }}
        >
          {splitTextMatchFragment(m.fragment, m.matches).map((part, j) =>
            part.matched ? (
              <mark
                key={j}
                style={{
                  background: "color-mix(in srgb, var(--color-accent) 22%, transparent)",
                  color: "inherit",
                  borderRadius: "2px",
                }}
              >
                {part.text}
              </mark>
            ) : (
              <span key={j}>{part.text}</span>
            ),
          )}
        </pre>
      ))}
    </div>
  );
}

function quotedQualifierValue(value: string) {
  const trimmed = value.trim();
  if (!trimmed) return "";
  return /\s/.test(trimmed) ? `"${trimmed.replaceAll('"', '\\"')}"` : trimmed;
}

export function buildRepositoryQuery(q: string, filters: RepositorySearchFilters) {
  const qualifiers = [
    filters.visibility && `is:${filters.visibility}`,
    filters.archived && `archived:${filters.archived}`,
    filters.fork && `fork:${filters.fork}`,
    filters.topic && `topic:${quotedQualifierValue(filters.topic)}`,
    filters.excludeTopic && `-topic:${quotedQualifierValue(filters.excludeTopic)}`,
    filters.language && `language:${quotedQualifierValue(filters.language)}`,
  ].filter(Boolean);
  return [q.trim(), ...qualifiers].filter(Boolean).join(" ");
}

/** Sort + order controls for the Issues and Users result tabs, mirroring the
 *  Repositories tab. The Order menu appears only once a sort key is chosen
 *  (best-match ordering has no direction). */
function SortControls({
  title,
  sortLabel,
  orderLabel,
  sort,
  order,
  options,
  onChange,
}: {
  title: string;
  sortLabel: string;
  orderLabel: string;
  sort: string;
  order: string;
  options: { value: string; label: string }[];
  onChange: (next: Record<string, string>) => void;
}) {
  const selectStyle = { fontSize: "0.78rem", padding: "0.32rem 0.45rem" };
  return (
    <Box className="mb-4">
      <div style={{ padding: "0.75rem 1rem" }}>
        <div className="mb-2" style={{ fontSize: "0.8rem", fontWeight: 650 }}>
          {title}
        </div>
        <div className="flex flex-wrap items-end gap-2">
          <label style={{ fontSize: "0.72rem" }}>
            <span className="mb-1 block">Sort</span>
            <select
              aria-label={sortLabel}
              value={sort}
              onChange={(event) => onChange({ sort: event.target.value })}
              style={selectStyle}
            >
              <option value="">Best match</option>
              {options.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </select>
          </label>
          {sort && (
            <label style={{ fontSize: "0.72rem" }}>
              <span className="mb-1 block">Order</span>
              <select
                aria-label={orderLabel}
                value={order}
                onChange={(event) => onChange({ order: event.target.value })}
                style={selectStyle}
              >
                <option value="desc">Descending</option>
                <option value="asc">Ascending</option>
              </select>
            </label>
          )}
        </div>
      </div>
    </Box>
  );
}

function RepositoryFilters({
  filters,
  onChange,
}: {
  filters: RepositorySearchFilters;
  onChange: (next: Record<string, string>) => void;
}) {
  const selectStyle = { fontSize: "0.78rem", padding: "0.32rem 0.45rem" };
  const inputStyle = { ...selectStyle, minWidth: "8.5rem" };
  return (
    <Box className="mb-4">
      <div style={{ padding: "0.75rem 1rem" }}>
        <div className="mb-2" style={{ fontSize: "0.8rem", fontWeight: 650 }}>
          Repository filters
        </div>
        <div className="flex flex-wrap items-end gap-2">
          <label style={{ fontSize: "0.72rem" }}>
            <span className="mb-1 block">Visibility</span>
            <select
              aria-label="Repository visibility"
              value={filters.visibility}
              onChange={(event) => onChange({ visibility: event.target.value })}
              style={selectStyle}
            >
              <option value="">Any visibility</option>
              <option value="public">Public</option>
              <option value="private">Private</option>
              <option value="internal">Internal</option>
            </select>
          </label>
          <label style={{ fontSize: "0.72rem" }}>
            <span className="mb-1 block">Archive state</span>
            <select
              aria-label="Repository archive state"
              value={filters.archived}
              onChange={(event) => onChange({ archived: event.target.value })}
              style={selectStyle}
            >
              <option value="">Active and archived</option>
              <option value="false">Active</option>
              <option value="true">Archived</option>
            </select>
          </label>
          <label style={{ fontSize: "0.72rem" }}>
            <span className="mb-1 block">Forks</span>
            <select
              aria-label="Repository forks"
              value={filters.fork}
              onChange={(event) => onChange({ fork: event.target.value })}
              style={selectStyle}
            >
              <option value="">Sources only</option>
              <option value="true">Include forks</option>
              <option value="only">Only forks</option>
            </select>
          </label>
          <label style={{ fontSize: "0.72rem" }}>
            <span className="mb-1 block">Has topic</span>
            <input
              aria-label="Required repository topic"
              value={filters.topic}
              onChange={(event) => onChange({ topic: event.target.value })}
              placeholder="web"
              style={inputStyle}
            />
          </label>
          <label style={{ fontSize: "0.72rem" }}>
            <span className="mb-1 block">Without topic</span>
            <input
              aria-label="Excluded repository topic"
              value={filters.excludeTopic}
              onChange={(event) => onChange({ exclude_topic: event.target.value })}
              placeholder="legacy"
              style={inputStyle}
            />
          </label>
          <label style={{ fontSize: "0.72rem" }}>
            <span className="mb-1 block">Language</span>
            <input
              aria-label="Repository language"
              value={filters.language}
              onChange={(event) => onChange({ language: event.target.value })}
              placeholder="Go"
              style={inputStyle}
            />
          </label>
          <label style={{ fontSize: "0.72rem" }}>
            <span className="mb-1 block">Sort</span>
            <select
              aria-label="Repository search sort"
              value={filters.sort}
              onChange={(event) => onChange({ sort: event.target.value })}
              style={selectStyle}
            >
              <option value="">Best match</option>
              <option value="updated">Recently updated</option>
              <option value="stars">Most stars</option>
              <option value="forks">Most forks</option>
              <option value="help-wanted-issues">Help wanted issues</option>
            </select>
          </label>
          {filters.sort && (
            <label style={{ fontSize: "0.72rem" }}>
              <span className="mb-1 block">Order</span>
              <select
                aria-label="Repository search order"
                value={filters.order}
                onChange={(event) => onChange({ order: event.target.value })}
                style={selectStyle}
              >
                <option value="desc">Descending</option>
                <option value="asc">Ascending</option>
              </select>
            </label>
          )}
          {Object.values(filters).some((value) => value && value !== "desc") && (
            <Button
              size="sm"
              variant="secondary"
              onClick={() =>
                onChange({
                  visibility: "",
                  archived: "",
                  fork: "",
                  topic: "",
                  exclude_topic: "",
                  language: "",
                  sort: "",
                  order: "",
                })
              }
            >
              Clear filters
            </Button>
          )}
        </div>
      </div>
    </Box>
  );
}

/** Label search requires a repository_id; resolve the typed owner/repo first. */
function LabelResults({
  q,
  page,
  labelsRepo,
  onPage,
  onCount,
}: {
  q: string;
  page: number;
  labelsRepo: string;
  onPage: (page: number) => void;
  onCount: (count: number) => void;
}) {
  const [owner = "", repo = ""] = labelsRepo.split("/");
  const valid = !!owner && !!repo && labelsRepo.split("/").length === 2;
  const repoQuery = useQuery({
    queryKey: ["repo", owner, repo],
    queryFn: () => fetchRepoDetail(owner, repo),
    enabled: valid,
  });

  if (!valid) {
    return (
      <Blankslate title="Pick a repository">
        <p>Label search is scoped to one repository — enter it as owner/repo above.</p>
      </Blankslate>
    );
  }
  if (repoQuery.isLoading) return <Spinner label={`resolving ${labelsRepo}`} />;
  if (repoQuery.isError || !repoQuery.data) {
    return isNotFound(repoQuery.error) ? (
      <Blankslate title={`Repository ${labelsRepo} not found`} />
    ) : (
      <InlineError title={`Failed to resolve ${labelsRepo}`} detail={String(repoQuery.error)} />
    );
  }

  const repositoryId = repoQuery.data.id;
  return (
    <ResultList
      queryKey={["search", "labels", q, repositoryId, page]}
      queryFn={() => searchLabels(q, repositoryId, page)}
      page={page}
      onPage={onPage}
      onCount={onCount}
      noun={{ singular: "label", plural: "labels" }}
      render={(l: GithubSearchLabelItem) => (
        <div className="flex items-center gap-2">
          <span
            aria-hidden
            style={{
              width: 12,
              height: 12,
              borderRadius: "999px",
              background: `#${l.color}`,
              border: "1px solid var(--color-border)",
              flexShrink: 0,
            }}
          />
          <span style={{ fontWeight: 600 }}>{l.name}</span>
          <span style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
            {l.description || (l.default ? "default label" : "")}
          </span>
        </div>
      )}
    />
  );
}

/** Friendly throttle state: countdown from the server's Retry-After, one
 *  automatic retry when it elapses, then a manual "Try again". */
function SearchRateLimitNotice({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const seconds =
    error instanceof ApiError && error.retryAfterSeconds !== undefined ? error.retryAfterSeconds : 60;
  const [left, setLeft] = useState(seconds);
  const autoRetriedRef = useRef(false);

  // A fresh throttle answer (the auto-retry got throttled again) restarts
  // the countdown.
  useEffect(() => setLeft(seconds), [error, seconds]);
  useEffect(() => {
    if (left <= 0) return;
    const t = setTimeout(() => setLeft((s) => s - 1), 1_000);
    return () => clearTimeout(t);
  }, [left]);
  useEffect(() => {
    if (left > 0 || autoRetriedRef.current) return;
    autoRetriedRef.current = true;
    onRetry();
  }, [left, onRetry]);

  return (
    <Blankslate icon={<SearchIcon size={26} />} title="You're searching too fast">
      <p role="status">
        {left > 0
          ? `The search rate limit is exhausted — retrying in ${left}s.`
          : "Retrying now…"}
      </p>
      {left <= 0 && (
        <Button size="sm" variant="secondary" onClick={onRetry}>
          Try again
        </Button>
      )}
    </Blankslate>
  );
}

function ResultList<T>({
  queryKey,
  queryFn,
  page,
  onPage,
  onCount,
  noun,
  render,
}: {
  queryKey: (string | number)[];
  queryFn: () => Promise<SearchResultPage<T>>;
  page: number;
  onPage: (page: number) => void;
  onCount?: (count: number) => void;
  noun: { singular: string; plural: string };
  render: (item: T) => React.ReactNode;
}) {
  // Auto-search debounce: the first render queries immediately, but URL-driven
  // key changes (filter keystrokes, sort switches) settle for a beat before a
  // request goes out — search is budgeted at 30/min per user.
  const signature = JSON.stringify(queryKey);
  const [debounced, setDebounced] = useState({ signature, queryKey });
  const queryFnRef = useRef(queryFn);
  queryFnRef.current = queryFn;
  useEffect(() => {
    if (debounced.signature === signature) return;
    const t = setTimeout(() => setDebounced({ signature, queryKey }), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [signature]);

  const { data, isLoading, isError, error, refetch } = useQuery({
    queryKey: debounced.queryKey,
    queryFn: () => queryFnRef.current(),
    // A throttled 403 is final for the current window — retrying only deepens
    // the exhaustion. The friendly notice below owns the single retry. (Same
    // guard pattern as AppHeader's current-user query.)
    retry: (failureCount, err) => !isRateLimited(err) && failureCount < 1,
  });

  const totalCount = data?.totalCount;
  useEffect(() => {
    if (typeof totalCount === "number") onCount?.(totalCount);
  }, [totalCount, onCount]);

  if (isLoading || debounced.signature !== signature) return <Spinner label={`searching ${noun.plural}`} />;
  if (isError && isRateLimited(error))
    return <SearchRateLimitNotice error={error} onRetry={() => void refetch()} />;
  if (isError || !data)
    return <InlineError title={`Search failed`} detail={String(error)} />;
  if (data.totalCount === 0)
    return (
      <Blankslate icon={<SearchIcon size={26} />} title={`No matching ${noun.plural}`}>
        <p>Try different terms or qualifiers.</p>
      </Blankslate>
    );

  const lastPage = Math.max(1, Math.ceil(data.totalCount / SEARCH_PER_PAGE));
  return (
    <div>
      <div role="status" className="mb-2" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
        {data.totalCount} {data.totalCount === 1 ? noun.singular : noun.plural}
        {data.incompleteResults ? " (incomplete)" : ""}
      </div>
      <Box>
        {data.items.map((item, i) => (
          <div
            key={i}
            style={{
              padding: "0.6rem 1rem",
              fontSize: "0.88rem",
              borderBottom: i < data.items.length - 1 ? "1px solid var(--color-border)" : "none",
            }}
          >
            {render(item)}
          </div>
        ))}
      </Box>
      {lastPage > 1 && (
        <div className="mt-3 flex items-center justify-center gap-2">
          <Button size="sm" variant="secondary" disabled={page <= 1} onClick={() => onPage(page - 1)}>
            Previous
          </Button>
          <span style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
            Page {page} of {lastPage}
          </span>
          <Button
            size="sm"
            variant="secondary"
            disabled={page >= lastPage}
            onClick={() => onPage(page + 1)}
          >
            Next
          </Button>
        </div>
      )}
    </div>
  );
}
