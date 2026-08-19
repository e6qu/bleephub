import { parseLinkLast } from "../api.js";

/*
 * Link-header pagination helpers. The parser itself already lives in api.ts
 * (parseLinkLast / parseLinkNext, used by ghFetchPage) — re-exported here
 * under the name list pages reach for, not duplicated.
 */

/** Page number of the rel="last" target in a GitHub-style Link header; null when absent (single page). */
export const lastPageFromLink: (link: string | null) => number | null = parseLinkLast;

/**
 * Upper bound on the total item count implied by a Link header: the last
 * page can be partially filled, so the real total is in
 * ((lastPage-1)*perPage, lastPage*perPage]. Null when the header has no
 * rel="last" (single page — the caller already holds every item).
 */
export function totalUpperBoundFromLink(link: string | null, perPage: number): number | null {
  const last = lastPageFromLink(link);
  if (last === null || perPage <= 0) return null;
  return last * perPage;
}
