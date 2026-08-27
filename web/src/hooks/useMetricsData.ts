import { useQuery } from "@tanstack/react-query";
import { fetchMetrics, isForbidden } from "../api.js";
import type { BleephubMetrics, BleephubStatus } from "../types.js";

// Server serves these counters to site admins only; a refusal is reported
// via isOperatorOnly (not isError), with no retry and no polling once refused.
export function useMetricsData(): {
  metrics: BleephubMetrics | undefined;
  status: BleephubStatus | undefined;
  isLoading: boolean;
  isError: boolean;
  isOperatorOnly: boolean;
} {
  const { data: metrics, isLoading, isError, error } = useQuery({
    queryKey: ["metrics"],
    queryFn: ({ signal }) => fetchMetrics(signal),
    retry: (failureCount, err) => !isForbidden(err) && failureCount < 1,
    refetchInterval: (query) => (isForbidden(query.state.error) ? false : 5000),
  });
  const isOperatorOnly = isForbidden(error);
  const status = metrics
    ? {
      active_workflows: metrics.active_workflows,
      jobs_by_status: metrics.jobs_by_status,
      connected_runners: metrics.connected_runners,
    }
    : undefined;
  return {
    metrics,
    status,
    isLoading,
    isError: isError && !isOperatorOnly,
    isOperatorOnly,
  };
}
