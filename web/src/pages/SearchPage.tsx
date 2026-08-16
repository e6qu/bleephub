import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchRepoDetail,
  isNotFound,
  searchCode,
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
  GithubSearchTopicItem,
  GithubSearchUserItem,
} from "../types.js";
import { PageTitle, Box, Blankslate, Button, Tabs, StateLabel } from "../components/ui.js";
import { SearchIcon } from "../components/octicons.js";

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
      <Tabs
        items={TABS}
        active={tab}
        onChange={(next) => update({ type: next, page: "" })}
      />
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
          tab={tab}
          q={q}
          page={page}
          labelsRepo={labelsRepo}
          repositoryFilters={repositoryFilters}
          issuesSort={issuesSort}
          usersSort={usersSort}
          order={order}
          onPage={(p) => update({ page: String(p) })}
        />
      )}
    </div>
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
}) {
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
          queryFn={() => searchCode(q, page)}
          page={page}
          onPage={onPage}
          noun={{ singular: "code result", plural: "code results" }}
          render={(item: GithubSearchCodeItem) => (
            <div>
              <span style={{ fontWeight: 600 }}>{item.repository.full_name}</span>
              <span className="font-mono" style={{ marginLeft: "0.5rem", fontSize: "0.82rem" }}>
                {item.path}
              </span>
              <span style={{ marginLeft: "0.5rem", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                {item.language ?? "unknown language"}
              </span>
            </div>
          )}
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
          noun={{ singular: "user", plural: "users" }}
          render={(u: GithubSearchUserItem) => (
            <div>
              <span style={{ fontWeight: 600 }}>{u.login}</span>
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
          noun={{ singular: "commit", plural: "commits" }}
          render={(c: GithubSearchCommitItem) => (
            <div>
              <div style={{ fontWeight: 600, fontSize: "0.88rem" }}>
                {c.commit.message.split("\n")[0]}
              </div>
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
      return <LabelResults q={q} page={page} labelsRepo={labelsRepo} onPage={onPage} />;
    case "topics":
      return (
        <ResultList
          queryKey={["search", "topics", q, page]}
          queryFn={() => searchTopics(q, page)}
          page={page}
          onPage={onPage}
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
}: {
  q: string;
  page: number;
  labelsRepo: string;
  onPage: (page: number) => void;
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

function ResultList<T>({
  queryKey,
  queryFn,
  page,
  onPage,
  noun,
  render,
}: {
  queryKey: (string | number)[];
  queryFn: () => Promise<SearchResultPage<T>>;
  page: number;
  onPage: (page: number) => void;
  noun: { singular: string; plural: string };
  render: (item: T) => React.ReactNode;
}) {
  const { data, isLoading, isError, error } = useQuery({ queryKey, queryFn });

  if (isLoading) return <Spinner label={`searching ${noun.plural}`} />;
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
