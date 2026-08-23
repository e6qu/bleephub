import { useMemo, useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router";
import { DataTable, InlineError, Spinner } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@bleephub/ui-core/components";
import {
  createRepoCodespace,
  createUserCodespace,
  deleteCodespace,
  fetchCodespace,
  fetchCodespaceMachines,
  fetchRepoCodespaces,
  fetchRepos,
  fetchUserCodespaces,
  startCodespace,
  stopCodespace,
  updateCodespace,
  isForbidden,
  isRateLimited,
  ghPostJSON,
} from "../api.js";
import type { BleephubRepo, GithubCodespace, GithubCodespaceState } from "../types.js";
import { confirmAction } from "../components/confirmAction.js";
import {
  Blankslate,
  Box,
  Button,
  DialogActions,
  ErrorBanner,
  FormLabel,
  Modal,
  PageTitle,
  StateLabel,
} from "../components/ui.js";
import { CodespaceIcon, PlayIcon, SquareIcon, TrashIcon } from "../components/octicons.js";

const col = createColumnHelper<GithubCodespace>();

function stateLabel(state: GithubCodespaceState): { state: "open" | "closed" | "draft"; label: string } {
  switch (state) {
    case "Available":
      return { state: "open", label: "available" };
    case "Shutdown":
      return { state: "closed", label: "shutdown" };
    case "Unavailable":
      return { state: "closed", label: "unavailable" };
    case "Failed":
    case "Deleted":
    case "Archived":
      return { state: "closed", label: state.toLowerCase() };
    case "Unknown":
    case "Created":
    case "Queued":
    case "Provisioning":
    case "Awaiting":
    case "Moved":
    case "Starting":
    case "ShuttingDown":
    case "Exporting":
    case "Updating":
    case "Rebuilding":
      return { state: "draft", label: state.replace(/([a-z])([A-Z])/g, "$1 $2").toLowerCase() };
  }
}

const canStart = (state: GithubCodespaceState) =>
  state === "Shutdown" || state === "Unavailable" || state === "Failed";
const canStop = (state: GithubCodespaceState) => state === "Available";

export function CodespacesPage() {
  const { owner, repo, codespaceName } = useParams<{
    owner?: string;
    repo?: string;
    codespaceName?: string;
  }>();
  if (codespaceName) return <CodespaceDetail name={codespaceName} />;
  const repoScope = owner && repo ? `${owner}/${repo}` : null;
  const [showCreate, setShowCreate] = useState(false);

  return (
    <div>
      <PageTitle
        icon={<CodespaceIcon size={20} />}
        title="Codespaces"
        meta={repoScope ? `Codespaces for ${repoScope}.` : "Your personal codespaces."}
        actions={
          <Button variant="primary" size="sm" onClick={() => setShowCreate(true)}>
            New codespace
          </Button>
        }
      />

      <CodespacesList repoScope={repoScope} />

      {showCreate && (
        <CreateCodespaceDialog repoScope={repoScope} onClose={() => setShowCreate(false)} />
      )}
    </div>
  );
}

function CodespacesList({ repoScope }: { repoScope: string | null }) {
  const queryClient = useQueryClient();
  const queryKey = repoScope ? ["codespaces", "repo", repoScope] : ["codespaces", "user"];

  const {
    data,
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey,
    queryFn: () =>
      repoScope
        ? fetchRepoCodespaces(repoScope.split("/")[0]!, repoScope.split("/")[1]!)
        : fetchUserCodespaces(),
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 10000,
  });

  const startMut = useMutation({
    mutationFn: (name: string) => startCodespace(name),
    onSuccess: () => queryClient.invalidateQueries({ queryKey }),
  });
  const stopMut = useMutation({
    mutationFn: (name: string) => stopCodespace(name),
    onSuccess: () => queryClient.invalidateQueries({ queryKey }),
  });
  const deleteMut = useMutation({
    mutationFn: (name: string) => deleteCodespace(name),
    onSuccess: () => queryClient.invalidateQueries({ queryKey }),
  });

  const columns = useMemo(
    () => [
      col.accessor("name", {
        header: "Name",
        cell: (info) => (
          <Link
            to={`/ui/codespaces/${encodeURIComponent(info.getValue())}`}
            className="font-medium"
            style={{ color: "var(--color-accent)", textDecoration: "none" }}
          >
            {info.getValue()}
          </Link>
        ),
      }),
      col.accessor("display_name", {
        header: "Display name",
      }),
      col.accessor("state", {
        header: "State",
        cell: (info) => {
          const s = stateLabel(info.getValue<GithubCodespaceState>());
          return <StateLabel state={s.state}>{s.label}</StateLabel>;
        },
      }),
      col.accessor("machine", {
        header: "Machine",
        cell: (info) => info.getValue<GithubCodespace["machine"]>()?.display_name ?? "Not assigned",
      }),
      col.accessor("last_used_at", {
        header: "Last used",
        cell: (info) => new Date(info.getValue<string>()).toLocaleString(),
      }),
      col.display({
        id: "actions",
        header: "Actions",
        cell: (info) => {
          const cs = info.row.original;
          return (
            <div className="flex flex-wrap items-center gap-2">
              {canStart(cs.state) ? (
                <Button size="sm" variant="secondary" onClick={() => startMut.mutate(cs.name)}>
                  <PlayIcon size={14} /> Start
                </Button>
              ) : canStop(cs.state) ? (
                <Button size="sm" variant="secondary" onClick={() => stopMut.mutate(cs.name)}>
                  <SquareIcon size={14} /> Stop
                </Button>
              ) : null}
              <Button
                size="sm"
                variant="ghost"
                onClick={async () => {
                  if (await confirmAction(`Delete codespace ${cs.name}?`)) {
                    deleteMut.mutate(cs.name);
                  }
                }}
              >
                <TrashIcon size={14} /> Delete
              </Button>
            </div>
          );
        },
      }),
    ],
    [startMut, stopMut, deleteMut],
  );

  if (isLoading) return <Spinner />;
  if (isError) return <InlineError title="Failed to load codespaces" detail={error instanceof Error ? error : String(error)} />;

  const items = data?.items ?? [];
  if (items.length === 0) {
    return (
      <Blankslate title="No codespaces" icon={<CodespaceIcon size={32} />}>
        {repoScope
          ? `There are no codespaces for ${repoScope} yet.`
          : "You don't have any codespaces yet."}
      </Blankslate>
    );
  }

  return (
    <Box>
      <DataTable columns={columns} data={items} />
      {startMut.error && <ErrorBanner>{String(startMut.error)}</ErrorBanner>}
      {stopMut.error && <ErrorBanner>{String(stopMut.error)}</ErrorBanner>}
      {deleteMut.error && <ErrorBanner>{String(deleteMut.error)}</ErrorBanner>}
    </Box>
  );
}

function CodespaceDetail({ name }: { name: string }) {
  const navigate = useNavigate();
  const client = useQueryClient();
  const queryKey = ["codespaces", "detail", name] as const;
  const query = useQuery({
    queryKey,
    queryFn: () => fetchCodespace(name),
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 10_000,
  });
  const start = useMutation({
    mutationFn: () => startCodespace(name),
    onSuccess: (codespace) => client.setQueryData(queryKey, codespace),
  });
  const stop = useMutation({
    mutationFn: () => stopCodespace(name),
    onSuccess: (codespace) => client.setQueryData(queryKey, codespace),
  });
  const remove = useMutation({
    mutationFn: () => deleteCodespace(name),
    onSuccess: () => navigate("/ui/codespaces"),
  });
  const [editingMachine, setEditingMachine] = useState(false);
  const [machine, setMachine] = useState("");
  const changeMachine = useMutation({
    mutationFn: () => updateCodespace(name, { machine }),
    onSuccess: (codespace) => {
      client.setQueryData(queryKey, codespace);
      setEditingMachine(false);
    },
  });
  const exportBranch = useMutation({
    mutationFn: () =>
      ghPostJSON<unknown>(`/api/v3/user/codespaces/${encodeURIComponent(name)}/exports`, {}),
    onSuccess: () => client.invalidateQueries({ queryKey }),
  });
  const [publishName, setPublishName] = useState("");
  const [publishPrivate, setPublishPrivate] = useState(true);
  const publish = useMutation({
    mutationFn: () =>
      ghPostJSON<unknown>(`/api/v3/user/codespaces/${encodeURIComponent(name)}/publish`, {
        name: publishName,
        private: publishPrivate,
      }),
    onSuccess: () => {
      client.invalidateQueries({ queryKey });
      client.invalidateQueries({ queryKey: ["codespaces", "user"] });
    },
  });

  if (query.isLoading) return <Spinner label={`Loading ${name}`} />;
  if (query.isError || !query.data) {
    return <InlineError title="Failed to load codespace" detail={String(query.error)} />;
  }

  const cs = query.data;
  const label = stateLabel(cs.state);
  return (
    <div>
      <div className="mb-4">
        <Link to="/ui/codespaces" style={{ color: "var(--color-accent)", textDecoration: "none" }}>
          ← Codespaces
        </Link>
      </div>
      <PageTitle
        icon={<CodespaceIcon size={20} />}
        title={cs.display_name || cs.name}
        meta={cs.name}
        actions={
          <>
            {canStop(cs.state) ? (
              <Button onClick={() => stop.mutate()} disabled={stop.isPending}>
                <SquareIcon size={14} /> Stop
              </Button>
            ) : canStart(cs.state) ? (
              <Button onClick={() => start.mutate()} disabled={start.isPending}>
                <PlayIcon size={14} /> Start
              </Button>
            ) : null}
            <Button
              variant="secondary"
              disabled={exportBranch.isPending}
              onClick={() => exportBranch.mutate()}
            >
              Export to branch
            </Button>
            <Button
              variant="danger"
              disabled={remove.isPending}
              onClick={async () => {
                if (await confirmAction(`Delete codespace ${cs.name}?`)) remove.mutate();
              }}
            >
              <TrashIcon size={14} /> Delete
            </Button>
          </>
        }
      />
      {(start.error || stop.error || remove.error || changeMachine.error || exportBranch.error || publish.error) && (
        <ErrorBanner>{String(start.error || stop.error || remove.error || changeMachine.error || exportBranch.error || publish.error)}</ErrorBanner>
      )}
      <Box>
        <dl className="grid gap-4 sm:grid-cols-2" style={{ padding: "1rem" }}>
          <CodespaceFact label="State">
            <StateLabel state={label.state}>{label.label}</StateLabel>
          </CodespaceFact>
          <CodespaceFact label="Repository">
            {cs.repository ? (
              <Link
                to={`/ui/${cs.repository.full_name}`}
                style={{ color: "var(--color-accent)", textDecoration: "none" }}
              >
                {cs.repository.full_name}
              </Link>
            ) : (
              <div className="flex flex-col gap-2">
                <span>Unpublished</span>
                <div className="flex flex-wrap items-center gap-2">
                  <input
                    type="text"
                    aria-label="New repository name"
                    value={publishName}
                    onChange={(e) => setPublishName(e.target.value)}
                    placeholder="new-repo-name"
                  />
                  <label className="flex items-center gap-1" style={{ fontSize: ".82rem" }}>
                    <input
                      type="checkbox"
                      checked={publishPrivate}
                      onChange={(e) => setPublishPrivate(e.target.checked)}
                    />
                    Private
                  </label>
                  <Button
                    variant="primary"
                    disabled={publish.isPending || !publishName}
                    onClick={() => publish.mutate()}
                  >
                    Publish
                  </Button>
                </div>
              </div>
            )}
          </CodespaceFact>
          <CodespaceFact label="Machine">
            {editingMachine && cs.repository ? (
              <div className="flex items-center gap-2">
                <MachineSelect
                  owner={cs.repository.full_name.split("/")[0] ?? ""}
                  repo={cs.repository.full_name.split("/")[1] ?? ""}
                  value={machine}
                  onChange={setMachine}
                />
                <Button
                  variant="primary"
                  disabled={changeMachine.isPending || !machine}
                  onClick={() => changeMachine.mutate()}
                >
                  Save
                </Button>
                <Button variant="ghost" onClick={() => setEditingMachine(false)}>Cancel</Button>
              </div>
            ) : (
              <span className="flex items-center gap-2">
                {cs.machine?.display_name ?? "Not assigned"}
                {cs.repository && (
                  <Button
                    variant="ghost"
                    onClick={() => {
                      setMachine(cs.machine?.name ?? "");
                      setEditingMachine(true);
                    }}
                  >
                    Change
                  </Button>
                )}
              </span>
            )}
          </CodespaceFact>
          <CodespaceFact label="Git ref">{cs.git_status.ref || "Default branch"}</CodespaceFact>
          <CodespaceFact label="Created">{new Date(cs.created_at).toLocaleString()}</CodespaceFact>
          <CodespaceFact label="Last used">{new Date(cs.last_used_at).toLocaleString()}</CodespaceFact>
        </dl>
      </Box>
    </div>
  );
}

function CodespaceFact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <dt style={{ color: "var(--color-fg-muted)", fontSize: ".76rem" }}>{label}</dt>
      <dd className="mt-1" style={{ fontSize: ".88rem" }}>{children}</dd>
    </div>
  );
}

function CreateCodespaceDialog({
  repoScope,
  onClose,
}: {
  repoScope: string | null;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const { data: repos } = useQuery({
    queryKey: ["repos"],
    queryFn: fetchRepos,
  });
  const [selectedRepo, setSelectedRepo] = useState<string>(repoScope ?? "");
  const [machine, setMachine] = useState("basicLinux32");
  const [displayName, setDisplayName] = useState("");
  const [error, setError] = useState<unknown>(null);

  const createMut = useMutation({
    mutationFn: async () => {
      if (repoScope) {
        const [owner = "", repo = ""] = repoScope.split("/");
        return createRepoCodespace(owner, repo, { machine, display_name: displayName });
      }
      const repo = repos?.find((r: BleephubRepo) => r.full_name === selectedRepo);
      if (!repo) throw new Error("Select a repository.");
      return createUserCodespace({ repository_id: repo.id, machine, display_name: displayName });
    },
    onSuccess: () => {
      queryClient.invalidateQueries({
        queryKey: repoScope ? ["codespaces", "repo", repoScope] : ["codespaces", "user"],
      });
      onClose();
    },
    onError: (err) => setError(err),
  });

  return (
    <Modal title="New codespace" onClose={onClose}>
      <div className="flex flex-col gap-4">
        {error ? <ErrorBanner>{error instanceof Error ? error.message : String(error)}</ErrorBanner> : null}

        {!repoScope && (
          <div>
            <FormLabel id="cs-repo">Repository</FormLabel>
            <select
              id="cs-repo"
              value={selectedRepo}
              onChange={(e) => setSelectedRepo(e.target.value)}
              className="w-full"
            >
              <option value="">Select a repository</option>
              {repos?.map((r: BleephubRepo) => (
                <option key={r.id} value={r.full_name}>
                  {r.full_name}
                </option>
              ))}
            </select>
          </div>
        )}

        <div>
          <FormLabel id="cs-machine">Machine</FormLabel>
          <MachineSelect owner={repoScope?.split("/")[0] ?? ""} repo={repoScope?.split("/")[1] ?? ""} value={machine} onChange={setMachine} />
        </div>

        <div>
          <FormLabel id="cs-display">Display name</FormLabel>
          <input
            id="cs-display"
            type="text"
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
            placeholder="Optional display name"
            className="w-full"
          />
        </div>
      </div>

      <DialogActions>
        <Button variant="secondary" onClick={onClose}>
          Cancel
        </Button>
        <Button variant="primary" onClick={() => createMut.mutate()} disabled={createMut.isPending}>
          Create codespace
        </Button>
      </DialogActions>
    </Modal>
  );
}

function MachineSelect({
  owner,
  repo,
  value,
  onChange,
}: {
  owner: string;
  repo: string;
  value: string;
  onChange: (v: string) => void;
}) {
  const { data, isLoading } = useQuery({
    queryKey: ["codespaces", "machines", owner, repo],
    queryFn: () => fetchCodespaceMachines(owner, repo),
    enabled: !!owner && !!repo,
  });

  if (!owner || !repo) {
    return (
      <select aria-label="Machine type" value={value} onChange={(e) => onChange(e.target.value)} className="w-full">
        <option value="basicLinux32">basicLinux32</option>
        <option value="standardLinux32">standardLinux32</option>
        <option value="premiumLinux64">premiumLinux64</option>
      </select>
    );
  }

  if (isLoading) return <Spinner label="Loading machines" />;

  return (
    <select aria-label="Machine type" value={value} onChange={(e) => onChange(e.target.value)} className="w-full">
      {data?.items.map((m) => (
        <option key={m.name} value={m.name}>
          {m.display_name}
        </option>
      ))}
    </select>
  );
}
