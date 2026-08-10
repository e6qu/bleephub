import { useMutation, useQuery } from "@tanstack/react-query";
import { DataTable, InlineError, Spinner, StatusBadge } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@bleephub/ui-core/components";
import {
  fetchRepos,
  fetchActionsRunners,
  createRunnerRegistrationToken,
  isForbidden,
  isRateLimited,
} from "../api.js";
import type { GithubRunner } from "../types.js";
import { PageTitle, StatCard, Button, Modal, CodeBlock } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";

const col = createColumnHelper<GithubRunner>();

const columns = [
  col.display({
    id: "name",
    header: "Runner name",
    cell: (info) => (
      <span style={{ color: "var(--color-fg)", fontWeight: 500 }}>
        {info.row.original.name}
      </span>
    ),
  }),
  col.display({
    id: "id",
    header: "Runner identifier",
    cell: (info) => (
      <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
        {info.row.original.id}
      </span>
    ),
  }),
  col.accessor("os", {
    header: "Operating system",
    cell: (info) => <span style={{ color: "var(--color-fg-muted)" }}>{info.getValue()}</span>,
  }),
  col.display({
    id: "status",
    header: "Status",
    cell: (info) => <StatusBadge status={info.row.original.status} />,
  }),
  col.display({
    id: "busy",
    header: "Busy",
    cell: (info) => (
      <span
        style={{
          color: info.row.original.busy ? "var(--color-status-warn)" : "var(--color-fg-subtle)",
          fontSize: "0.78rem",
          fontWeight: info.row.original.busy ? 600 : 400,
        }}
      >
        {info.row.original.busy ? "yes" : "no"}
      </span>
    ),
  }),
  col.display({
    id: "labels",
    header: "Labels",
    cell: (info) => {
      const names = info.row.original.labels.map((l) => l.name);
      if (names.length === 0) return <span style={{ color: "var(--color-fg-subtle)" }}>—</span>;
      return (
        <span
          className="font-mono"
          style={{ color: "var(--color-fg-muted)", fontSize: "0.7rem" }}
        >
          {names.join(", ")}
        </span>
      );
    },
  }),
];

export function RunnersPage() {
  const reposQ = useQuery({ queryKey: ["repos"], queryFn: fetchRepos });
  const firstRepo = reposQ.data?.[0]?.full_name;
  const [owner = "", repo = ""] = firstRepo ? firstRepo.split("/") : ["", ""];

  const runnersQ = useQuery({
    queryKey: ["gh-runners", firstRepo],
    queryFn: () => fetchActionsRunners(owner, repo),
    enabled: !!firstRepo,
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 5000,
  });
  const tokenMut = useMutation({
    mutationFn: () => createRunnerRegistrationToken(owner, repo),
  });

  if (reposQ.isError) {
    return <InlineError title="Failed to load repositories for the runner registry" />;
  }
  if (reposQ.isLoading || !reposQ.data) return <Spinner label="loading runners" />;

  if (!firstRepo) {
    return (
      <div>
        <PageTitle title="Registered runners" meta="0 runners" />
        <DataTable
          data={[]}
          columns={columns}
          filterPlaceholder="Filter runners…"
          emptyMessage="Create a repository to query the GitHub Actions runner registry."
        />
      </div>
    );
  }

  if (runnersQ.isError) return <InlineError title="Failed to load registered runners" />;
  if (runnersQ.isLoading || !runnersQ.data) return <Spinner label="loading runners" />;

  const runners = runnersQ.data.items;
  const totalCount = runnersQ.data.totalCount;
  const online = runners.filter((runner) => runner.status === "online").length;
  const busy = runners.filter((runner) => runner.busy).length;

  return (
    <div>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <PageTitle
          title="Registered runners"
          meta={`${totalCount} runner${totalCount === 1 ? "" : "s"} · ${firstRepo}`}
        />
        <Button
          variant="primary"
          size="sm"
          disabled={tokenMut.isPending}
          onClick={() => tokenMut.mutate()}
        >
          {tokenMut.isPending ? "Generating…" : "Add runner"}
        </Button>
      </div>
      <MutationError of={tokenMut} />

      {tokenMut.data && (
        <Modal title="Register a self-hosted runner" onClose={() => tokenMut.reset()}>
          <p style={{ marginTop: 0, fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
            Run this on the runner host. The token expires{" "}
            {new Date(tokenMut.data.expires_at).toLocaleString()}.
          </p>
          <CodeBlock>
            {`./config.sh --url ${window.location.origin}/${firstRepo} --token ${tokenMut.data.token}`}
          </CodeBlock>
        </Modal>
      )}

      <div className="mb-6 grid grid-cols-3 gap-3">
        <StatCard
          title="Registered runners"
          value={totalCount}
          emphasized={runners.length > 0}
        />
        <StatCard title="Online runners" value={online} emphasized={online > 0} />
        <StatCard title="Busy runners" value={busy} emphasized={busy > 0} />
      </div>

      <DataTable
        data={runners}
        columns={columns}
        filterPlaceholder="Filter runners…"
        emptyMessage="No runners registered."
      />
    </div>
  );
}
