import { useMemo, useState, type ReactNode } from "react";
import type { ListFilterState } from "../types.js";
import { SearchIcon, IssueOpenedIcon, IssueClosedIcon } from "./octicons.js";

/**
 * Accessors that pull the filterable dimensions out of an issue or PR list
 * item so ListControls can stay generic across both list pages.
 */
export interface ListItemAccessors<T> {
  labels: (item: T) => { name: string }[];
  author: (item: T) => string | null;
  assignees: (item: T) => string[];
  milestone: (item: T) => string | null;
  comments: (item: T) => number;
  createdAt: (item: T) => string;
  updatedAt: (item: T) => string;
  /** Haystack for the free-text filter (title + body). Optional: when the
   * caller doesn't provide it, free-text terms are kept in the box but the
   * client-side substring filter is skipped. */
  text?: (item: T) => string;
}

export const emptyFilters: ListFilterState = {
  label: null,
  author: null,
  assignee: null,
  milestone: null,
  sort: "newest",
  text: null,
  noLabel: false,
  noMilestone: false,
  noAssignee: false,
};

/** Map the UI sort choice to the server's sort + direction query params. */
export function sortToServerParams(sort: ListFilterState["sort"]): { sort?: string; direction?: string } {
  switch (sort) {
    case "newest":
      return { sort: "created", direction: "desc" };
    case "oldest":
      return { sort: "created", direction: "asc" };
    case "comments":
      return { sort: "comments", direction: "desc" };
    case "updated":
      return { sort: "updated", direction: "desc" };
    default:
      return {};
  }
}

/** Apply the client-side dimensions (label/author/assignee/milestone) + sort. */
export function filterAndSortItems<T>(
  items: T[],
  filters: ListFilterState,
  acc: ListItemAccessors<T>,
): T[] {
  let out = items;
  if (filters.label) {
    out = out.filter((i) => acc.labels(i).some((l) => l.name === filters.label));
  }
  if (filters.author) {
    out = out.filter((i) => acc.author(i) === filters.author);
  }
  if (filters.assignee) {
    out = out.filter((i) => acc.assignees(i).includes(filters.assignee as string));
  }
  if (filters.milestone) {
    out = out.filter((i) => acc.milestone(i) === filters.milestone);
  }
  if (filters.noLabel) {
    out = out.filter((i) => acc.labels(i).length === 0);
  }
  if (filters.noMilestone) {
    out = out.filter((i) => acc.milestone(i) === null);
  }
  if (filters.noAssignee) {
    out = out.filter((i) => acc.assignees(i).length === 0);
  }
  const text = (filters.text ?? "").trim().toLowerCase();
  if (text && acc.text) {
    // Every free-text term must appear somewhere in the haystack, the way
    // github.com ANDs search terms.
    const terms = text.split(/\s+/);
    out = out.filter((i) => {
      const hay = acc.text!(i).toLowerCase();
      return terms.every((t) => hay.includes(t));
    });
  }
  const sorted = [...out];
  sorted.sort((a, b) => {
    switch (filters.sort) {
      case "oldest":
        return acc.createdAt(a).localeCompare(acc.createdAt(b));
      case "comments":
        return acc.comments(b) - acc.comments(a);
      case "updated":
        return acc.updatedAt(b).localeCompare(acc.updatedAt(a));
      default:
        return acc.createdAt(b).localeCompare(acc.createdAt(a));
    }
  });
  return sorted;
}

/** Compose the GitHub-style search token string from the active filters. */
function composeQuery(kind: "issue" | "pr", state: "open" | "closed" | "all", f: ListFilterState): string {
  const tokens = [`is:${kind === "pr" ? "pr" : "issue"}`];
  // github.com has no `is:all` qualifier — "all" is simply the absence of an
  // open/closed filter.
  if (state !== "all") tokens.push(`is:${state}`);
  if (f.label) tokens.push(`label:${quoteToken(f.label)}`);
  if (f.author) tokens.push(`author:${f.author}`);
  if (f.assignee) tokens.push(`assignee:${f.assignee}`);
  if (f.milestone) tokens.push(`milestone:${quoteToken(f.milestone)}`);
  if (f.noLabel) tokens.push("no:label");
  if (f.noMilestone) tokens.push("no:milestone");
  if (f.noAssignee) tokens.push("no:assignee");
  if (f.text?.trim()) tokens.push(f.text.trim());
  return tokens.join(" ");
}

function quoteToken(v: string): string {
  return /\s/.test(v) ? `"${v}"` : v;
}

/**
 * Parse a GitHub-style query back into filter state (state + dimensions).
 * Anything that isn't a recognised qualifier — bare words and unknown
 * `key:value` tokens alike — is kept as free text rather than silently
 * dropped, and applied as a title/body substring filter.
 */
export function parseQuery(
  raw: string,
): { state?: "open" | "closed" | undefined; filters: Partial<ListFilterState> } {
  const filters: Partial<ListFilterState> = {
    label: null,
    author: null,
    assignee: null,
    milestone: null,
    text: null,
    noLabel: false,
    noMilestone: false,
    noAssignee: false,
  };
  let state: "open" | "closed" | undefined;
  const freeText: string[] = [];
  // Tokenise on whitespace, keeping quoted qualifier values intact.
  const tokenRe = /(?:(\w+):("[^"]*"|\S+))|(\S+)/g;
  let m: RegExpExecArray | null;
  while ((m = tokenRe.exec(raw)) !== null) {
    if (m[3] !== undefined) {
      freeText.push(m[3]);
      continue;
    }
    const key = m[1];
    const val = m[2]!.replace(/^"|"$/g, "");
    switch (key) {
      case "is":
      case "state":
        if (val === "open" || val === "closed") state = val;
        // is:issue / is:pr restate the page, so they are neither filters
        // nor free text.
        break;
      case "label":
        filters.label = val;
        break;
      case "author":
        filters.author = val;
        break;
      case "assignee":
        filters.assignee = val;
        break;
      case "milestone":
        filters.milestone = val;
        break;
      case "no":
        if (val === "label") filters.noLabel = true;
        else if (val === "milestone") filters.noMilestone = true;
        else if (val === "assignee") filters.noAssignee = true;
        else freeText.push(m[0]!);
        break;
      default:
        // Unknown qualifier — keep it as free text, never drop input.
        freeText.push(m[0]!);
    }
  }
  filters.text = freeText.length > 0 ? freeText.join(" ") : null;
  return { state, filters };
}

function Select({
  label,
  value,
  options,
  onChange,
}: {
  label: string;
  value: string | null;
  options: { value: string; label: string }[];
  onChange: (v: string | null) => void;
}) {
  return (
    <label
      className="inline-flex items-center gap-1"
      style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}
    >
      <span className="sr-only">{label}</span>
      <select
        aria-label={label}
        value={value ?? ""}
        onChange={(e) => onChange(e.target.value === "" ? null : e.target.value)}
        style={{
          background: "transparent",
          border: "none",
          color: value ? "var(--color-fg)" : "var(--color-fg-muted)",
          fontSize: "0.82rem",
          fontWeight: 600,
          cursor: "pointer",
        }}
      >
        <option value="">{label}</option>
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  );
}

/**
 * GitHub's issues/PRs list filter bar + Open/Closed count header. The search
 * box reflects the active filter tokens (and re-parses them on submit); the
 * dropdowns drive the client-side label/author/assignee/milestone filters plus
 * the sort. `state` (open/closed) is server-driven by the page.
 */
export function ListControls<T>({
  kind,
  state,
  onState,
  openCount,
  closedCount,
  items,
  filters,
  onFilters,
  accessors,
  actions,
  resultCount,
}: {
  kind: "issue" | "pr";
  state: "open" | "closed" | "all";
  onState: (s: "open" | "closed" | "all") => void;
  openCount?: number | string | undefined;
  closedCount?: number | string | undefined;
  items: T[];
  filters: ListFilterState;
  onFilters: (f: ListFilterState) => void;
  accessors: ListItemAccessors<T>;
  actions?: ReactNode;
  /** Count of the currently-visible (client-filtered) list, announced to
   * screen readers — the client-side dropdown filters re-narrow the list with
   * no refetch/spinner, so without this a blind user gets no feedback. */
  resultCount?: number | undefined;
}) {
  const [queryDraft, setQueryDraft] = useState<string | null>(null);
  const query = queryDraft ?? composeQuery(kind, state, filters);

  const options = useMemo(() => {
    const labels = new Set<string>();
    const authors = new Set<string>();
    const assignees = new Set<string>();
    const milestones = new Set<string>();
    for (const it of items) {
      for (const l of accessors.labels(it)) labels.add(l.name);
      const a = accessors.author(it);
      if (a) authors.add(a);
      for (const s of accessors.assignees(it)) assignees.add(s);
      const ms = accessors.milestone(it);
      if (ms) milestones.add(ms);
    }
    const opt = (s: Set<string>) => [...s].sort().map((v) => ({ value: v, label: v }));
    return {
      labels: opt(labels),
      authors: opt(authors),
      assignees: opt(assignees),
      milestones: opt(milestones),
    };
  }, [items, accessors]);

  const submitQuery = () => {
    const { state: parsedState, filters: parsed } = parseQuery(query);
    if (parsedState && parsedState !== state) onState(parsedState);
    onFilters({ ...filters, ...parsed });
    setQueryDraft(null);
  };

  const countPill = (active: boolean) =>
    ({
      display: "inline-flex",
      alignItems: "center",
      gap: "0.35rem",
      fontSize: "0.85rem",
      fontWeight: active ? 600 : 500,
      color: active ? "var(--color-fg)" : "var(--color-fg-muted)",
      background: "transparent",
      border: "none",
      cursor: "pointer",
    }) as const;

  return (
    <div className="mb-4">
      {typeof resultCount === "number" && (
        <span className="sr-only" role="status">
          {resultCount} {kind === "pr" ? (resultCount === 1 ? "pull request" : "pull requests") : resultCount === 1 ? "issue" : "issues"}
        </span>
      )}
      {/* Search + actions row */}
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <form
          className="flex min-w-0 flex-1 items-center gap-2"
          style={{
            border: "1px solid var(--color-border)",
            borderRadius: "var(--radius-md)",
            background: "var(--color-bg-subtle)",
            padding: "0.3rem 0.6rem",
          }}
          onSubmit={(e) => {
            e.preventDefault();
            submitQuery();
          }}
        >
          <SearchIcon size={14} style={{ color: "var(--color-fg-muted)", flexShrink: 0 }} />
          <input
            aria-label="Search issues and pull requests"
            value={query}
            onChange={(e) => setQueryDraft(e.target.value)}
            className="min-w-0 flex-1"
            style={{ background: "transparent", border: "none", fontSize: "0.85rem" }}
          />
        </form>
        {actions}
      </div>

      {/* Count header + filter dropdowns */}
      <div
        className="flex flex-wrap items-center justify-between gap-3"
        style={{
          border: "1px solid var(--color-border)",
          borderTopLeftRadius: "var(--radius-md)",
          borderTopRightRadius: "var(--radius-md)",
          background: "var(--color-bg-subtle)",
          padding: "0.55rem 0.9rem",
        }}
      >
        <div className="flex items-center gap-4">
          <button type="button" style={countPill(state === "open")} onClick={() => onState("open")}>
            <IssueOpenedIcon size={14} />
            {`${openCount ?? "–"} Open`}
          </button>
          <button
            type="button"
            style={countPill(state === "closed")}
            onClick={() => onState("closed")}
          >
            <IssueClosedIcon size={14} />
            {`${closedCount ?? "–"} Closed`}
          </button>
          <button type="button" style={countPill(state === "all")} onClick={() => onState("all")}>
            {`${typeof openCount === "number" && typeof closedCount === "number" ? openCount + closedCount : "–"} All`}
          </button>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          <Select
            label="Author"
            value={filters.author}
            options={options.authors}
            onChange={(v) => onFilters({ ...filters, author: v })}
          />
          <Select
            label="Label"
            value={filters.label}
            options={options.labels}
            onChange={(v) => onFilters({ ...filters, label: v })}
          />
          <Select
            label="Milestones"
            value={filters.milestone}
            options={options.milestones}
            onChange={(v) => onFilters({ ...filters, milestone: v })}
          />
          <Select
            label="Assignee"
            value={filters.assignee}
            options={options.assignees}
            onChange={(v) => onFilters({ ...filters, assignee: v })}
          />
          <Select
            label="Sort"
            value={filters.sort === "newest" ? null : filters.sort}
            options={[
              { value: "newest", label: "Newest" },
              { value: "oldest", label: "Oldest" },
              { value: "comments", label: "Most commented" },
              { value: "updated", label: "Recently updated" },
            ]}
            onChange={(v) =>
              onFilters({ ...filters, sort: (v as ListFilterState["sort"]) ?? "newest" })
            }
          />
        </div>
      </div>
    </div>
  );
}
