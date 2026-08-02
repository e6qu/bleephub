import { useQuery } from "@tanstack/react-query";
import { fetchWorkflows, isForbidden, isRateLimited } from "../api.js";

/** Rows the Operations console's "Recent workflows" panel shows. */
export const OVERVIEW_RUNS_LIMIT = 10;

/**
 * Rows the Workflows page's Runs tab shows. The list is bounded because each
 * row's job count costs a request; see fetchWorkflows for why that cannot be
 * folded into the runs query.
 */
export const RUNS_TAB_LIMIT = 50;

/**
 * Shared hook so the two callers cannot declare the same key with different
 * poll intervals. The limit is part of the key: a 10-row and a 50-row request
 * are different queries, and both are invalidated by `["workflows"]`.
 */
export function useRecentWorkflows(limit: number) {
  return useQuery({
    queryKey: ["workflows", limit],
    queryFn: () => fetchWorkflows(limit),
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 10_000,
  });
}
