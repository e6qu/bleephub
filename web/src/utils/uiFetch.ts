import { ghFetch } from "../api.js";
import type { Page } from "../api.js";
import type { BleephubRepo, RepoListFilters } from "../types.js";

// Page-local fetch helpers kept out of api.ts so they ride the lazy page
// chunks, keeping the entry bundle under budget.

// Cap in-flight lazy row hydrations so a page of rows doesn't stampede the
// server. React-query dedupes per queryKey, so each resource is fetched once.
const MAX_CONCURRENT_HYDRATIONS = 4;
let active = 0;
const waiters: (() => void)[] = [];

async function acquire(): Promise<void> {
  if (active >= MAX_CONCURRENT_HYDRATIONS) {
    await new Promise<void>((resolve) => waiters.push(resolve));
  }
  active++;
}

function release(): void {
  active--;
  waiters.shift()?.();
}

/** ghFetch with a global concurrency cap, for per-row lazy hydration. */
export async function limitedGhFetch<T>(path: string): Promise<T> {
  await acquire();
  try {
    return await ghFetch<T>(path);
  } finally {
    release();
  }
}

/** Max Link-header pages a client-side search/sort walks before giving up. */
export const WALK_PAGE_CAP = 10;

export interface WalkResult<T> {
  items: T[];
  /** True when the cap was hit before the last page — the set is incomplete. */
  truncated: boolean;
}

// Walk every page of a Link-paginated repo list (capped) so a client-only
// filter (search text, archived, star sort) sees all repos.
export async function walkRepoPages(
  fetchPage: (filters: RepoListFilters, pageUrl?: string) => Promise<Page<BleephubRepo>>,
  filters: RepoListFilters,
  maxPages: number = WALK_PAGE_CAP,
): Promise<WalkResult<BleephubRepo>> {
  const items: BleephubRepo[] = [];
  let pageUrl: string | undefined;
  for (let i = 0; i < maxPages; i++) {
    const page = await fetchPage(filters, pageUrl);
    items.push(...page.items);
    if (!page.nextUrl) return { items, truncated: false };
    pageUrl = page.nextUrl;
  }
  return { items, truncated: true };
}

// Walk a paginated array endpoint by page number. Stops at the first short
// page or the cap.
export async function walkNumberedPages<T>(
  basePath: string,
  perPage = 100,
  maxPages: number = WALK_PAGE_CAP,
): Promise<WalkResult<T>> {
  const sep = basePath.includes("?") ? "&" : "?";
  const items: T[] = [];
  for (let page = 1; page <= maxPages; page++) {
    const chunk = await ghFetch<T[]>(`${basePath}${sep}per_page=${perPage}&page=${page}`);
    if (!Array.isArray(chunk)) break;
    items.push(...chunk);
    if (chunk.length < perPage) return { items, truncated: false };
  }
  return { items, truncated: true };
}

// Viewer's org membership role from GET /user/memberships/orgs/{org}: "admin"
// for owners, "member" otherwise, null when not a member (404) or signed out.
// Gates org-admin controls and the pinned-repos editor.
export async function fetchViewerOrgRole(org: string): Promise<"admin" | "member" | null> {
  try {
    const m = await ghFetch<{ role?: string }>(
      `/api/v3/user/memberships/orgs/${encodeURIComponent(org)}`,
    );
    if (m && typeof m === "object" && !Array.isArray(m)) {
      if (m.role === "admin") return "admin";
      if (m.role === "member") return "member";
    }
    return null;
  } catch {
    return null;
  }
}
