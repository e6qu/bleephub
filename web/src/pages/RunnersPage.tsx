import { useMemo, useState } from "react";
import { useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { DataTable, InlineError, Spinner, StatusBadge } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@bleephub/ui-core/components";
import {
  fetchRepos,
  fetchAuthenticatedUserOrgs,
  ghFetch,
  ghPostJSON,
  ghSend,
  isForbidden,
  isRateLimited,
} from "../api.js";
import type { GithubRunner } from "../types.js";
import { PageTitle, StatCard, Button, Modal, CodeBlock, Tabs } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { confirmAction } from "../components/confirmAction.js";

// ─── Scoped runner-registry fetchers ─────────────────────────────────────
// The runner registry exists at BOTH the repository and the organization
// level (GET/DELETE /api/v3/{repos/{o}/{r}|orgs/{org}}/actions/runners…);
// these page-local helpers target whichever scope the picker selects.

type RunnerScope = { kind: "repo"; fullName: string } | { kind: "org"; org: string };

function runnersBase(scope: RunnerScope): string {
  if (scope.kind === "org") return `/api/v3/orgs/${encodeURIComponent(scope.org)}/actions/runners`;
  const [owner = "", repo = ""] = scope.fullName.split("/");
  return `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/actions/runners`;
}

const fetchScopedRunners = (scope: RunnerScope) =>
  ghFetch<{ total_count: number; runners: GithubRunner[] }>(runnersBase(scope));

const createScopedRegistrationToken = (scope: RunnerScope) =>
  ghPostJSON<{ token: string; expires_at: string }>(`${runnersBase(scope)}/registration-token`, {});

const deleteScopedRunner = (scope: RunnerScope, runnerId: number) =>
  ghSend("DELETE", `${runnersBase(scope)}/${runnerId}`);

/** The `--url` target `config.sh` registers against for this scope. */
function scopeUrl(scope: RunnerScope): string {
  return `${window.location.origin}/${scope.kind === "org" ? scope.org : scope.fullName}`;
}

function scopeLabel(scope: RunnerScope): string {
  return scope.kind === "org" ? `organization ${scope.org}` : scope.fullName;
}

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
  const [searchParams, setSearchParams] = useSearchParams();
  const reposQ = useQuery({ queryKey: ["repos"], queryFn: fetchRepos });
  const orgsQ = useQuery({ queryKey: ["user-orgs"], queryFn: ({ signal }) => fetchAuthenticatedUserOrgs(signal) });
  const repos = reposQ.data ?? [];
  const orgs = orgsQ.data ?? [];

  // The selection is URL-addressable (?repo=owner/name or ?org=login) so a
  // specific registry view is linkable; with no query the first repository
  // the viewer can see is managed, never silently repos[0]-only.
  const orgParam = searchParams.get("org");
  const repoParam = searchParams.get("repo");
  const scope: RunnerScope | null = orgParam
    ? { kind: "org", org: orgParam }
    : repoParam && repoParam.includes("/")
      ? { kind: "repo", fullName: repoParam }
      : repos[0]?.full_name
        ? { kind: "repo", fullName: repos[0].full_name }
        : null;

  const selectRepo = (fullName: string) => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      next.delete("org");
      next.set("repo", fullName);
      return next;
    });
  };
  const selectOrg = (login: string) => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      next.delete("repo");
      next.set("org", login);
      return next;
    });
  };

  if (reposQ.isError) {
    return <InlineError title="Failed to load repositories for the runner registry" />;
  }
  if (reposQ.isLoading || !reposQ.data) return <Spinner label="loading runners" />;

  return (
    <div>
      <PageTitle
        title="Registered runners"
        meta="Self-hosted runners registered with the GitHub Actions service."
      />
      <ScopePicker
        scope={scope}
        repos={repos.map((r) => r.full_name)}
        orgs={orgs.map((o) => o.login)}
        onSelectRepo={selectRepo}
        onSelectOrg={selectOrg}
      />
      {scope === null ? (
        <DataTable
          data={[]}
          columns={columns}
          filterPlaceholder="Filter runners…"
          emptyMessage="Create a repository to query the GitHub Actions runner registry."
        />
      ) : (
        <ScopedRunners scope={scope} />
      )}
    </div>
  );
}

// ─── Scope picker: repo search + select, plus an org scope option ────────

function ScopePicker({
  scope,
  repos,
  orgs,
  onSelectRepo,
  onSelectOrg,
}: {
  scope: RunnerScope | null;
  repos: string[];
  orgs: string[];
  onSelectRepo: (fullName: string) => void;
  onSelectOrg: (login: string) => void;
}) {
  const [repoFilter, setRepoFilter] = useState("");
  const kind = scope?.kind ?? "repo";
  const matches = useMemo(() => {
    const q = repoFilter.trim().toLowerCase();
    return q ? repos.filter((r) => r.toLowerCase().includes(q)) : repos;
  }, [repos, repoFilter]);
  const selectedRepo = scope?.kind === "repo" ? scope.fullName : "";
  const options =
    selectedRepo && !matches.includes(selectedRepo) ? [selectedRepo, ...matches] : matches;

  return (
    <div className="mb-4 flex flex-wrap items-end gap-3">
      <div className="flex flex-col gap-1">
        <label htmlFor="runner-scope-kind" style={pickerLabelStyle}>
          Scope
        </label>
        <select
          id="runner-scope-kind"
          value={kind}
          onChange={(e) => {
            if (e.target.value === "org") {
              if (orgs[0]) onSelectOrg(orgs[0]);
            } else if (repos[0]) {
              onSelectRepo(selectedRepo || repos[0]);
            }
          }}
          style={pickerControlStyle}
        >
          <option value="repo">Repository</option>
          <option value="org" disabled={orgs.length === 0}>
            Organization{orgs.length === 0 ? " (none)" : ""}
          </option>
        </select>
      </div>
      {kind === "repo" ? (
        <>
          <div className="flex flex-col gap-1">
            <label htmlFor="runner-repo-search" style={pickerLabelStyle}>
              Find a repository
            </label>
            <input
              id="runner-repo-search"
              type="search"
              placeholder="Search repositories…"
              value={repoFilter}
              onChange={(e) => setRepoFilter(e.target.value)}
              style={{ ...pickerControlStyle, minWidth: "13rem" }}
            />
          </div>
          <div className="flex flex-col gap-1">
            <label htmlFor="runner-repo-select" style={pickerLabelStyle}>
              Repository
            </label>
            <select
              id="runner-repo-select"
              value={selectedRepo}
              onChange={(e) => onSelectRepo(e.target.value)}
              style={{ ...pickerControlStyle, minWidth: "13rem" }}
            >
              {options.length === 0 && <option value="">No repositories matched</option>}
              {options.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
          </div>
        </>
      ) : (
        <div className="flex flex-col gap-1">
          <label htmlFor="runner-org-select" style={pickerLabelStyle}>
            Organization
          </label>
          <select
            id="runner-org-select"
            value={scope?.kind === "org" ? scope.org : ""}
            onChange={(e) => onSelectOrg(e.target.value)}
            style={{ ...pickerControlStyle, minWidth: "13rem" }}
          >
            {orgs.map((login) => (
              <option key={login} value={login}>
                {login}
              </option>
            ))}
          </select>
        </div>
      )}
    </div>
  );
}

const pickerLabelStyle: React.CSSProperties = {
  fontSize: "0.72rem",
  fontWeight: 600,
  color: "var(--color-fg-muted)",
};

const pickerControlStyle: React.CSSProperties = {
  padding: "0.3rem 0.55rem",
  fontSize: "0.82rem",
  background: "var(--color-bg-subtle)",
  color: "var(--color-fg)",
  border: "1px solid var(--color-border)",
  borderRadius: "var(--radius-md)",
};

// ─── Registry for the selected scope ─────────────────────────────────────

function ScopedRunners({ scope }: { scope: RunnerScope }) {
  const scopeKey = scope.kind === "org" ? `org:${scope.org}` : scope.fullName;
  const runnersQ = useQuery({
    queryKey: ["gh-runners", scopeKey],
    queryFn: () => fetchScopedRunners(scope),
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 5000,
  });
  const tokenMut = useMutation({
    mutationFn: () => createScopedRegistrationToken(scope),
  });
  const qc = useQueryClient();
  const removeMut = useMutation({
    mutationFn: (runnerId: number) => deleteScopedRunner(scope, runnerId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["gh-runners", scopeKey] }),
  });
  const tableColumns = useMemo(
    () => [
      ...columns,
      col.display({
        id: "actions",
        header: "",
        cell: (info) => (
          <Button
            variant="danger"
            size="sm"
            aria-label={`Remove runner ${info.row.original.name}`}
            disabled={removeMut.isPending}
            onClick={async () => {
              if (
                await confirmAction(`Remove runner "${info.row.original.name}"?`, {
                  title: "Remove runner",
                  confirmLabel: "Remove",
                })
              ) {
                removeMut.mutate(info.row.original.id);
              }
            }}
          >
            Remove
          </Button>
        ),
      }),
    ],
    [removeMut],
  );

  if (runnersQ.isError) return <InlineError title="Failed to load registered runners" />;
  if (runnersQ.isLoading || !runnersQ.data) return <Spinner label="loading runners" />;

  const runners = runnersQ.data.runners ?? [];
  const totalCount = runnersQ.data.total_count ?? runners.length;
  const online = runners.filter((runner) => runner.status === "online").length;
  const busy = runners.filter((runner) => runner.busy).length;

  return (
    <div>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div style={{ fontSize: "0.84rem", color: "var(--color-fg-muted)" }}>
          {totalCount} runner{totalCount === 1 ? "" : "s"} · {scopeLabel(scope)}
        </div>
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
        <AddRunnerModal
          scope={scope}
          token={tokenMut.data.token}
          expiresAt={tokenMut.data.expires_at}
          onClose={() => tokenMut.reset()}
        />
      )}

      <div className="mb-6 mt-3 grid grid-cols-3 gap-3">
        <StatCard
          title="Registered runners"
          value={totalCount}
          emphasized={runners.length > 0}
        />
        <StatCard title="Online runners" value={online} emphasized={online > 0} />
        <StatCard title="Busy runners" value={busy} emphasized={busy > 0} />
      </div>

      <MutationError of={removeMut} />
      <DataTable
        data={runners}
        columns={tableColumns}
        filterPlaceholder="Filter runners…"
        emptyMessage="No runners registered."
      />
    </div>
  );
}

// ─── GitHub-style add-runner instructions ────────────────────────────────

type RunnerOS = "linux" | "macos" | "windows";

function runnerScript(os: RunnerOS, url: string, token: string): string {
  const archive =
    os === "windows" ? "actions-runner-win-x64.zip" : `actions-runner-${os === "macos" ? "osx" : "linux"}-x64.tar.gz`;
  if (os === "windows") {
    return [
      "# Create a folder under the drive root",
      "mkdir actions-runner; cd actions-runner",
      "# Download the latest runner package for your architecture",
      `# (from your runner distribution) ${archive}`,
      "# Extract the installer",
      `Expand-Archive -Path ${archive} -DestinationPath .`,
      "# Configure the runner",
      `./config.cmd --url ${url} --token ${token}`,
      "# Run it!",
      "./run.cmd",
    ].join("\n");
  }
  return [
    "# Create a folder",
    "mkdir actions-runner && cd actions-runner",
    "# Download the latest runner package for your architecture",
    `# (from your runner distribution) ${archive}`,
    "# Extract the installer",
    `tar xzf ./${archive}`,
    "# Configure the runner",
    `./config.sh --url ${url} --token ${token}`,
    "# Run it!",
    "./run.sh",
  ].join("\n");
}

function AddRunnerModal({
  scope,
  token,
  expiresAt,
  onClose,
}: {
  scope: RunnerScope;
  token: string;
  expiresAt: string;
  onClose: () => void;
}) {
  const [os, setOs] = useState<RunnerOS>("linux");
  return (
    <Modal title="Register a self-hosted runner" onClose={onClose}>
      <p style={{ marginTop: 0, fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
        Run this on the runner host to register it with {scopeLabel(scope)}. The token expires{" "}
        {new Date(expiresAt).toLocaleString()}.
      </p>
      <Tabs<RunnerOS>
        items={[
          { key: "linux", label: "Linux" },
          { key: "macos", label: "macOS" },
          { key: "windows", label: "Windows" },
        ]}
        active={os}
        onChange={setOs}
      />
      <CodeBlock>{runnerScript(os, scopeUrl(scope), token)}</CodeBlock>
    </Modal>
  );
}
