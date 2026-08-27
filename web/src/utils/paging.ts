import { parseLinkLast } from "../api.js";

// Link-header pagination helpers, re-exporting api.ts's parser.

export const lastPageFromLink: (link: string | null) => number | null = parseLinkLast;

// Upper bound: the last page can be partial, so the real total is in
// ((lastPage-1)*perPage, lastPage*perPage]. Null when there's no rel="last".
export function totalUpperBoundFromLink(link: string | null, perPage: number): number | null {
  const last = lastPageFromLink(link);
  if (last === null || perPage <= 0) return null;
  return last * perPage;
}
