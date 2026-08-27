import { useQuery } from "@tanstack/react-query";
import { fetchWorkflows, isForbidden, isRateLimited } from "../api.js";

export const OVERVIEW_RUNS_LIMIT = 10;

// Bounded because each row's job count costs a request; see fetchWorkflows.
export const RUNS_TAB_LIMIT = 50;

// Shared hook so both callers use the same key/poll interval. The limit is
// part of the key; both are invalidated by ["workflows"].
export function useRecentWorkflows(limit: number) {
  return useQuery({
    queryKey: ["workflows", limit],
    queryFn: ({ signal }) => fetchWorkflows(limit, signal),
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 10_000,
  });
}
