import { useQuery } from "@tanstack/react-query";
import { DataTable, InlineError, Spinner, StatusBadge } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@tanstack/react-table";
import { useNavigate } from "react-router";
import { fetchHealth, isForbidden, isRateLimited } from "../api.js";
import { useMetricsData } from "../hooks/useMetricsData.js";
import { OVERVIEW_RUNS_LIMIT, useRecentWorkflows } from "../hooks/useRecentWorkflows.js";
import type { BleephubWorkflow } from "../types.js";
import { PageTitle, StatCard, SectionLabel } from "../components/ui.js";
import { OperatorOnlyStats } from "../components/OperatorOnly.js";

const STAT_TITLES = [
  "Active workflows",
  "Connected runners",
  "Workflow submissions",
  "Job dispatches",
];

const col = createColumnHelper<BleephubWorkflow>();

export function OverviewPage() {
  const navigate = useNavigate();
  const { data: health } = useQuery({
    queryKey: ["health"],
    queryFn: fetchHealth,
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 5000,
  });
  // Shared with MetricsPage: one ["metrics"] query at one interval. This page
  // used to declare the same key with its own 3s interval, so the two fought.
  const { metrics, isLoading, isError, isOperatorOnly } = useMetricsData();
  const { data: workflows, isError: workflowsError } = useRecentWorkflows(OVERVIEW_RUNS_LIMIT);

  // A transport fault is a failure; a refusal is not. Only the first gets an
  // error surface — the second is explained in place of the figures below.
  if (isError) return <InlineError title="Failed to load overview" />;
  if (isLoading || (!metrics && !isOperatorOnly)) return <Spinner label="loading overview" />;

  const recent = workflows ?? [];

  const columns = [
    col.accessor("name", {
      header: "Name",
      cell: (info) => (
        <span style={{ color: "var(--color-fg)", fontWeight: 500 }}>{info.getValue()}</span>
      ),
    }),
    col.accessor("status", {
      header: "Status",
      cell: (info) => <StatusBadge status={info.getValue()} />,
    }),
    col.accessor("result", {
      header: "Result",
      cell: (info) => {
        const v = info.getValue();
        return v ? <StatusBadge status={v} /> : null;
      },
    }),
    col.accessor("eventName", {
      header: "Event",
      cell: (info) => (
        <span style={{ color: "var(--color-fg-muted)" }}>{info.getValue() ?? "—"}</span>
      ),
    }),
    col.display({
      id: "jobs",
      header: "Jobs",
      cell: (info) => (
        <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
          {Object.keys(info.row.original.jobs).length}
        </span>
      ),
    }),
  ];

  return (
    <div>
      <PageTitle
        title="System status"
        meta={
          <span className="inline-flex items-center gap-2">
            {health ? (
              <StatusBadge status={health.status === "ok" ? "ok" : "error"} />
            ) : (
              <span style={{ color: "var(--color-fg-subtle)" }}>health unknown</span>
            )}
          </span>
        }
      />

      <div className="mb-6">
        {metrics ? (
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
            <StatCard
              title="Active workflows"
              value={metrics.active_workflows}
              emphasized={metrics.active_workflows > 0}
            />
            <StatCard title="Connected runners" value={metrics.connected_runners} />
            <StatCard title="Workflow submissions" value={metrics.workflow_submissions} />
            <StatCard title="Job dispatches" value={metrics.job_dispatches} />
          </div>
        ) : (
          <OperatorOnlyStats titles={STAT_TITLES} />
        )}
      </div>

      <SectionLabel>Recent workflows</SectionLabel>
      {workflowsError ? (
        <InlineError title="Failed to load workflows" />
      ) : (
        <DataTable
          data={recent}
          columns={columns}
          filterPlaceholder="Filter recent workflows…"
          emptyMessage="No workflow runs yet."
          onRowClick={(row) => navigate(`/ui/workflows/${row.id}`)}
        />
      )}
    </div>
  );
}
