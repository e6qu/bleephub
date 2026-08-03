import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { useMetricsData } from "../hooks/useMetricsData.js";
import { PageTitle, StatCard, SectionLabel } from "../components/ui.js";
import { OperatorOnlyStats } from "../components/OperatorOnly.js";

const STAT_TITLES = [
  "Workflow submissions",
  "Job dispatches",
  "Active workflows",
  "Connected runners",
];

export function MetricsPage() {
  const { metrics, status, isLoading, isError, isOperatorOnly } = useMetricsData();

  // A transport fault is a failure; a refusal is not. Only the first gets an
  // error surface — the second is explained in place of the figures below.
  if (isError) return <InlineError title="Failed to load metrics" />;
  if (isLoading && !metrics) return <Spinner label="loading metrics" />;

  return (
    <div>
      <PageTitle
        title="GitHub Actions throughput"
        meta={metrics ? `${metrics.workflow_submissions} workflow submissions · ${metrics.connected_runners} connected runners` : undefined}
      />

      {isOperatorOnly && (
        <section className="mb-8">
          <SectionLabel>Counters</SectionLabel>
          <OperatorOnlyStats titles={STAT_TITLES} />
        </section>
      )}

      {metrics && (
        <section className="mb-8">
          <SectionLabel>Counters</SectionLabel>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard title="Workflow submissions" value={metrics.workflow_submissions} />
            <StatCard title="Job dispatches" value={metrics.job_dispatches} />
            <StatCard
              title="Active workflows"
              value={metrics.active_workflows}
              emphasized={metrics.active_workflows > 0}
            />
            <StatCard
              title="Connected runners"
              value={metrics.connected_runners}
              emphasized={metrics.connected_runners > 0}
            />
          </div>
        </section>
      )}

      {metrics && (
        <section className="mb-8">
          <SectionLabel>Job latency</SectionLabel>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard title="p50 duration" value={formatSeconds(metrics.job_duration_p50_seconds)} />
            <StatCard title="p95 duration" value={formatSeconds(metrics.job_duration_p95_seconds)} />
            <StatCard title="p99 duration" value={formatSeconds(metrics.job_duration_p99_seconds)} />
          </div>
        </section>
      )}

      {metrics && (
        <section className="mb-8">
          <SectionLabel>Runtime</SectionLabel>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard title="Uptime" value={formatUptime(metrics.uptime_seconds)} />
            <StatCard title="Goroutines" value={metrics.goroutines} />
            <StatCard title="Heap allocated" value={`${metrics.heap_alloc_mb.toFixed(1)} MB`} />
          </div>
        </section>
      )}

      {status && (
        <section className="mb-8">
          <SectionLabel>Jobs by status</SectionLabel>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            {Object.keys(status.jobs_by_status).length === 0 ? (
              <EmptyCell>no jobs in flight</EmptyCell>
            ) : (
              Object.entries(status.jobs_by_status).map(([s, count]) => (
                <StatCard key={s} title={s} value={count} emphasized={s === "running" || s === "queued"} />
              ))
            )}
          </div>
        </section>
      )}

      {metrics && (
        <section>
          <SectionLabel>Job completions</SectionLabel>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            {Object.keys(metrics.job_completions).length === 0 ? (
              <EmptyCell>no completed jobs yet</EmptyCell>
            ) : (
              Object.entries(metrics.job_completions).map(([result, count]) => (
                <StatCard key={result} title={result} value={count} emphasized={result === "failure"} />
              ))
            )}
          </div>
        </section>
      )}
    </div>
  );
}

function formatSeconds(s: number): string {
  if (!s || s <= 0) return "—";
  if (s < 1) return `${Math.round(s * 1000)} ms`;
  return `${s.toFixed(2)} s`;
}

function formatUptime(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const parts: string[] = [];
  if (d) parts.push(`${d}d`);
  if (h || d) parts.push(`${h}h`);
  parts.push(`${m}m`);
  return parts.join(" ");
}

function EmptyCell({ children }: { children: React.ReactNode }) {
  return (
    <div
      className="col-span-full"
      style={{
        padding: "1.25rem",
        textAlign: "center",
        background: "var(--color-surface)",
        border: "1px solid var(--color-border)",
        borderRadius: "var(--radius-md)",
        color: "var(--color-fg-muted)",
        fontSize: "0.85rem",
      }}
    >
      {children}
    </div>
  );
}
