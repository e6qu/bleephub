import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { DataTable, InlineError, Spinner, StatusBadge } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@bleephub/ui-core/components";
import { useNavigate } from "react-router";
import { useEffect, useMemo, useState } from "react";
import {
  dispatchWorkflow,
  fetchFileContent,
  fetchRepoBranches,
  fetchRepoDetail,
  fetchWorkflowFiles,
  isForbidden,
  isRateLimited,
} from "../api.js";
import { decodeContentsBase64 } from "../utils/contents.js";
import { parseWorkflowDispatch } from "../utils/workflowDispatch.js";
import { RUNS_TAB_LIMIT, useRecentWorkflows } from "../hooks/useRecentWorkflows.js";
import type { BleephubWorkflow, BleephubWorkflowFile, WorkflowDispatchInput } from "../types.js";
import {
  PageTitle,
  Tabs,
  Button,
  Modal,
  FormLabel,
  ErrorBanner,
  DialogActions,
} from "../components/ui.js";

type Tab = "workflows" | "runs";

export function WorkflowsPage() {
  const [tab, setTab] = useState<Tab>("workflows");
  return (
    <div>
      <PageTitle
        title="Workflows & runs"
        meta={
          tab === "workflows"
            ? "GitHub Actions workflow files discovered from repository storage."
            : "Run-level history. Click a row for the per-job timeline."
        }
      />
      <Tabs
        items={[
          { key: "workflows", label: "Workflows" },
          { key: "runs", label: "Runs" },
        ]}
        active={tab}
        onChange={setTab}
      />
      {tab === "workflows" ? <WorkflowsTab /> : <RunsTab />}
    </div>
  );
}

const filesCol = createColumnHelper<BleephubWorkflowFile>();

function WorkflowsTab() {
  const { data, isLoading, isError } = useQuery({
    queryKey: ["workflow_files"],
    queryFn: ({ signal }) => fetchWorkflowFiles(signal),
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 5000,
  });
  const [dispatchTarget, setDispatchTarget] = useState<BleephubWorkflowFile | null>(null);

  if (isError) return <InlineError title="Failed to load workflows" />;
  if (isLoading || !data) return <Spinner label="loading workflows" />;

  const columns = [
    filesCol.accessor("name", {
      header: "Name",
      cell: (info) => (
        <span style={{ color: "var(--color-fg)", fontWeight: 500 }}>{info.getValue()}</span>
      ),
    }),
    filesCol.accessor("path", {
      header: "Path",
      cell: (info) => <span style={{ color: "var(--color-fg-muted)" }}>{info.getValue()}</span>,
    }),
    filesCol.accessor("repoFullName", { header: "Repo" }),
    filesCol.accessor("state", {
      header: "State",
      cell: (info) => <StatusBadge status={info.getValue()} />,
    }),
    filesCol.accessor("updatedAt", {
      header: "Updated",
      cell: (info) => (
        <span style={{ color: "var(--color-fg-muted)" }}>{new Date(info.getValue()).toLocaleString()}</span>
      ),
    }),
    filesCol.display({
      id: "actions",
      header: "",
      cell: (info) => (
        <Button
          variant="ghost"
          size="sm"
          onClick={(e: React.MouseEvent) => {
            e.stopPropagation();
            setDispatchTarget(info.row.original);
          }}
        >
          Run
        </Button>
      ),
    }),
  ];

  return (
    <>
      <DataTable
        data={data}
        columns={columns}
        filterPlaceholder="Filter workflow files…"
        emptyMessage="No workflow files yet. Push a workflow file under .github/workflows."
      />
      {dispatchTarget && (
        <DispatchDialog target={dispatchTarget} onClose={() => setDispatchTarget(null)} />
      )}
    </>
  );
}

const runsCol = createColumnHelper<BleephubWorkflow>();

function RunsTab() {
  const navigate = useNavigate();
  const { data, isLoading, isError } = useRecentWorkflows(RUNS_TAB_LIMIT);

  if (isError) return <InlineError title="Failed to load runs" />;
  if (isLoading || !data) return <Spinner label="loading runs" />;

  const columns = [
    runsCol.accessor("name", {
      header: "Name",
      cell: (info) => (
        <span style={{ color: "var(--color-fg)", fontWeight: 500 }}>{info.getValue()}</span>
      ),
    }),
    runsCol.accessor("runId", {
      header: "Run #",
      cell: (info) => <span style={{ color: "var(--color-fg-muted)" }}>#{info.getValue()}</span>,
    }),
    runsCol.accessor("status", {
      header: "Status",
      cell: (info) => <StatusBadge status={info.getValue()} />,
    }),
    runsCol.accessor("result", {
      header: "Result",
      cell: (info) => {
        const v = info.getValue();
        return v ? <StatusBadge status={v} /> : null;
      },
    }),
    runsCol.accessor("eventName", {
      header: "Event",
      cell: (info) => <span style={{ color: "var(--color-fg-muted)" }}>{info.getValue()}</span>,
    }),
    runsCol.accessor("repoFullName", { header: "Repo" }),
    runsCol.accessor("createdAt", {
      header: "Created",
      cell: (info) => new Date(info.getValue()).toLocaleString(),
    }),
    runsCol.display({
      id: "jobCount",
      header: "Jobs",
      cell: (info) => (
        <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
          {Object.keys(info.row.original.jobs).length}
        </span>
      ),
    }),
  ];

  return (
    <DataTable
      data={data}
      columns={columns}
      filterPlaceholder="Filter runs…"
      emptyMessage="No runs yet. Dispatch a GitHub Actions workflow from the Workflows tab."
      onRowClick={(row) => navigate(`/ui/workflows/${row.id}`)}
    />
  );
}

function dispatchDefaultFor(def: WorkflowDispatchInput): string {
  if (def.type === "boolean") {
    return def.default === true || def.default === "true" ? "true" : "false";
  }
  if (typeof def.default === "string") return def.default;
  if (def.type === "choice" && def.options && def.options.length > 0) return def.options[0]!;
  return "";
}

function DispatchDialog({
  target,
  onClose,
}: {
  target: BleephubWorkflowFile;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const [owner = "", repo = ""] = target.repoFullName.split("/");
  const [ref, setRef] = useState("main");
  const [refTouched, setRefTouched] = useState(false);
  const [inputsJSON, setInputsJSON] = useState("{}");
  const [values, setValues] = useState<Record<string, string>>({});
  const [error, setError] = useState<string | null>(null);

  // Branch selector seeded from the repository's branches, the default
  // branch preselected; a free-text ref input remains the fallback for
  // repositories whose branch list can't be read.
  const branchesQ = useQuery({
    queryKey: ["branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
    retry: false,
  });
  const repoQ = useQuery({
    queryKey: ["repo-detail", owner, repo],
    queryFn: ({ signal }) => fetchRepoDetail(owner, repo, signal),
    retry: false,
  });
  const branches = Array.isArray(branchesQ.data) ? branchesQ.data : [];
  const defaultBranch = repoQ.data?.default_branch;
  useEffect(() => {
    if (refTouched || branches.length === 0) return;
    const preferred =
      defaultBranch && branches.some((b) => b.name === defaultBranch)
        ? defaultBranch
        : branches[0]!.name;
    setRef(preferred);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [defaultBranch, branchesQ.data]);

  // Typed inputs from the workflow file's on.workflow_dispatch.inputs;
  // an unreadable file falls back to the raw JSON inputs textarea.
  const yamlQ = useQuery({
    queryKey: ["workflow-yaml", owner, repo, target.path],
    queryFn: () => fetchFileContent(owner, repo, target.path),
    retry: false,
  });
  const dispatchSpec = useMemo(() => {
    const content = yamlQ.data?.content;
    if (typeof content !== "string") return null;
    try {
      return parseWorkflowDispatch(decodeContentsBase64(content));
    } catch {
      return null;
    }
  }, [yamlQ.data]);
  const typedInputs = dispatchSpec?.hasDispatch ? dispatchSpec.inputs : null;
  const inputNames = typedInputs ? Object.keys(typedInputs) : [];
  useEffect(() => {
    if (!typedInputs) return;
    setValues((prev) => {
      const next = { ...prev };
      for (const name of Object.keys(typedInputs)) {
        if (!(name in next)) next[name] = dispatchDefaultFor(typedInputs[name]!);
      }
      return next;
    });
  }, [typedInputs]);

  const mutation = useMutation({
    mutationFn: async () => {
      let inputs: Record<string, string> = {};
      if (typedInputs) {
        for (const name of inputNames) {
          if (typedInputs[name]!.required && !(values[name] ?? "")) {
            throw new Error(`Input "${name}" is required`);
          }
        }
        inputs = Object.fromEntries(inputNames.map((n) => [n, values[n] ?? ""]));
      } else {
        try {
          inputs = JSON.parse(inputsJSON || "{}");
        } catch {
          throw new Error("inputs must be valid JSON");
        }
      }
      await dispatchWorkflow(target.repoFullName, target.id, { ref, inputs });
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["workflows"] });
      onClose();
    },
    onError: (err: Error) => setError(err.message),
  });

  const setValue = (name: string, v: string) => setValues((prev) => ({ ...prev, [name]: v }));

  return (
    <Modal title={`Run ${target.name}`} onClose={onClose}>
      <div className="mb-4" style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
        {target.path} · {target.repoFullName}
      </div>

      <FormLabel id="dispatch-ref">Use workflow from branch</FormLabel>
      {branches.length > 0 ? (
        <select
          id="dispatch-ref"
          value={ref}
          onChange={(e) => {
            setRefTouched(true);
            setRef(e.target.value);
          }}
          className="mb-4 w-full"
        >
          {branches.map((b) => (
            <option key={b.name} value={b.name}>
              {b.name}
              {b.name === defaultBranch ? " (default)" : ""}
            </option>
          ))}
        </select>
      ) : (
        <input
          id="dispatch-ref"
          type="text"
          value={ref}
          onChange={(e) => {
            setRefTouched(true);
            setRef(e.target.value);
          }}
          className="mb-4 w-full"
        />
      )}

      {typedInputs ? (
        inputNames.map((name) => {
          const def = typedInputs[name]!;
          const fieldId = `dispatch-input-${name}`;
          if (def.type === "boolean") {
            return (
              <div key={name} className="mb-4 flex items-center gap-2">
                <input
                  id={fieldId}
                  type="checkbox"
                  checked={values[name] === "true"}
                  onChange={(e) => setValue(name, e.target.checked ? "true" : "false")}
                />
                <label htmlFor={fieldId} style={{ fontSize: "0.84rem", color: "var(--color-fg)" }}>
                  {def.description || name}
                </label>
              </div>
            );
          }
          if (def.type === "choice") {
            return (
              <div key={name} className="mb-4">
                <FormLabel id={fieldId}>
                  {def.description || name}
                  {def.required ? " *" : ""}
                </FormLabel>
                <select
                  id={fieldId}
                  value={values[name] ?? ""}
                  onChange={(e) => setValue(name, e.target.value)}
                  className="w-full"
                >
                  {(def.options ?? []).map((opt) => (
                    <option key={opt} value={opt}>
                      {opt}
                    </option>
                  ))}
                </select>
              </div>
            );
          }
          return (
            <div key={name} className="mb-4">
              <FormLabel id={fieldId}>
                {def.description || name}
                {def.required ? " *" : ""}
              </FormLabel>
              <input
                id={fieldId}
                type="text"
                value={values[name] ?? ""}
                onChange={(e) => setValue(name, e.target.value)}
                className="w-full"
              />
            </div>
          );
        })
      ) : (
        <>
          <FormLabel id="dispatch-inputs">Inputs (JSON)</FormLabel>
          <textarea
            id="dispatch-inputs"
            value={inputsJSON}
            onChange={(e) => setInputsJSON(e.target.value)}
            rows={5}
            className="mb-4 w-full"
            style={{ resize: "vertical", fontFamily: "var(--font-mono)" }}
          />
        </>
      )}

      {error && <ErrorBanner>{error}</ErrorBanner>}

      <DialogActions>
        <Button onClick={onClose} disabled={mutation.isPending} variant="ghost">
          Cancel
        </Button>
        <Button
          onClick={() => {
            setError(null);
            mutation.mutate();
          }}
          disabled={mutation.isPending}
          variant="primary"
        >
          {mutation.isPending ? "Dispatching…" : "Run workflow"}
        </Button>
      </DialogActions>
    </Modal>
  );
}
