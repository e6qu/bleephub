import { useState } from "react";
import { useParams, Link } from "react-router";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { DataTable, InlineError, Spinner } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@bleephub/ui-core/components";
import {
  fetchOrgRulesets,
  createOrgRuleset,
  updateOrgRuleset,
  deleteOrgRuleset,
  fetchOrgRulesetSuites,
  fetchOrgRulesetSuite,
} from "../api.js";
import type {
  GithubRuleset,
  GithubRulesetTarget,
  GithubRulesetEnforcement,
  GithubRulesetSuite,
} from "../types.js";
import { confirmAction } from "../components/confirmAction.js";
import { RulesetEditor, type RulesetRuleConfig } from "../components/RulesetEditor.js";
import { OrgHeader } from "../components/PageHeader.js";
import {
  Box,
  Button,
  Modal,
  FormLabel,
  ErrorBanner,
  DialogActions,
  PageTitle,
} from "../components/ui.js";

const col = createColumnHelper<GithubRuleset>();
const suiteCol = createColumnHelper<GithubRulesetSuite>();

const TARGETS: GithubRulesetTarget[] = ["branch", "tag", "push"];
const ENFORCEMENTS: GithubRulesetEnforcement[] = ["disabled", "active", "evaluate"];

export function RulesetsPage() {
  const { org } = useParams<{ org: string }>();
  if (!org) {
    return <InlineError title="Missing organization" detail="No organization login provided." />;
  }

  return (
    <div>
      <OrgHeader org={org} active="rulesets" />
      <RulesetsContent org={org} />
    </div>
  );
}

function RulesetsContent({ org }: { org: string }) {
  const queryClient = useQueryClient();
  const [view, setView] = useState<"rulesets" | "insights">("rulesets");
  const [selected, setSelected] = useState<GithubRuleset | null>(null);
  const [editing, setEditing] = useState<GithubRuleset | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [mutationError, setMutationError] = useState<string | null>(null);

  const {
    data: rulesets = [],
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ["org-rulesets", org],
    queryFn: () => fetchOrgRulesets(org),
    enabled: !!org && view === "rulesets",
  });

  const createMutation = useMutation({
    mutationFn: (payload: Parameters<typeof createOrgRuleset>[1]) => createOrgRuleset(org, payload),
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["org-rulesets", org] });
      setShowCreate(false);
    },
    onError: (err: Error) => setMutationError(err.message),
  });

  const updateMutation = useMutation({
    mutationFn: ({ id, payload }: { id: number; payload: Parameters<typeof updateOrgRuleset>[2] }) => updateOrgRuleset(org, id, payload),
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["org-rulesets", org] });
      setEditing(null);
    },
    onError: (err: Error) => setMutationError(err.message),
  });

  const deleteMutation = useMutation({
    mutationFn: (id: number) => deleteOrgRuleset(org, id),
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["org-rulesets", org] });
      setSelected(null);
    },
    onError: (err: Error) => setMutationError(err.message),
  });

  if (view === "rulesets" && isLoading) return <Spinner label={`loading ${org} rulesets`} />;
  if (view === "rulesets" && isError) return <InlineError title="Failed to load rulesets" detail={String(error)} />;

  const columns = [
    col.accessor("id", {
      header: "ID",
      cell: (info) => (
        <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
          {info.getValue()}
        </span>
      ),
    }),
    col.accessor("name", {
      header: "Name",
      cell: (info) => (
        <button
          type="button"
          onClick={() => setSelected(info.row.original)}
          className="font-medium"
          style={{
            background: "transparent",
            border: "none",
            padding: 0,
            color: "var(--color-accent)",
            cursor: "pointer",
          }}
        >
          {info.getValue()}
        </button>
      ),
    }),
    col.accessor("target", { header: "Target" }),
    col.accessor("enforcement", { header: "Enforcement" }),
    col.accessor("source", { header: "Source" }),
    col.display({
      id: "rules",
      header: "Rules",
      cell: (info) => info.row.original.rules?.length ?? 0,
    }),
    col.display({
      id: "actions",
      header: "Actions",
      cell: (info) => {
        const ruleset = info.row.original;
        return (
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" variant="ghost" onClick={() => setEditing(ruleset)}>
              Edit
            </Button>
            <Button
              size="sm"
              variant="danger"
              onClick={async () => {
                if (await confirmAction(`Delete ruleset "${ruleset.name}"?`)) {
                  deleteMutation.mutate(ruleset.id);
                }
              }}
              disabled={deleteMutation.isPending}
            >
              Delete
            </Button>
          </div>
        );
      },
    }),
  ];

  return (
    <div className="mt-4">
      <PageTitle
        title="Rulesets"
        meta={
          <Link to={`/ui/orgs/${org}/repos`} style={{ color: "var(--color-accent)", textDecoration: "none" }}>
            ← Back to repositories
          </Link>
        }
        actions={
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant={view === "rulesets" ? "primary" : "ghost"}
              size="sm"
              onClick={() => setView("rulesets")}
            >
              Rulesets
            </Button>
            <Button
              variant={view === "insights" ? "primary" : "ghost"}
              size="sm"
              onClick={() => setView("insights")}
            >
              Rule insights
            </Button>
            {view === "rulesets" && (
              <Button variant="primary" size="sm" onClick={() => setShowCreate(true)}>
                New ruleset
              </Button>
            )}
          </div>
        }
      />

      {mutationError && <ErrorBanner>{mutationError}</ErrorBanner>}

      {view === "rulesets" ? (
        <Box>
          <DataTable
            data={rulesets}
            columns={columns}
            emptyMessage="No rulesets configured for this organization."
          />
        </Box>
      ) : (
        <RulesetInsights org={org} />
      )}

      {selected && <RulesetDetailDialog ruleset={selected} onClose={() => setSelected(null)} />}

      {(showCreate || editing) && (
        <RulesetFormModal
          ruleset={editing}
          onClose={() => {
            setShowCreate(false);
            setEditing(null);
          }}
          onSubmit={(payload) => {
            if (editing) {
              updateMutation.mutate({ id: editing.id, payload });
            } else {
              createMutation.mutate(payload);
            }
          }}
          pending={createMutation.isPending || updateMutation.isPending}
          error={createMutation.error ?? updateMutation.error}
        />
      )}
    </div>
  );
}

function RulesetInsights({ org }: { org: string }) {
  const [repositoryName, setRepositoryName] = useState("");
  const [ref, setRef] = useState("");
  const [result, setResult] = useState<"all" | "pass" | "fail" | "bypass">("all");
  const [evaluateStatus, setEvaluateStatus] = useState<"all" | "active" | "evaluate">("all");
  const [timePeriod, setTimePeriod] = useState<"hour" | "day" | "week" | "month">("day");
  const [selectedID, setSelectedID] = useState<number | null>(null);

  const filters = { repositoryName, ref, result, evaluateStatus, timePeriod };
  const suitesQuery = useQuery({
    queryKey: ["org-ruleset-suites", org, filters],
    queryFn: () => fetchOrgRulesetSuites(org, filters),
    placeholderData: (previous) => previous,
  });
  const detailQuery = useQuery({
    queryKey: ["org-ruleset-suite", org, selectedID],
    queryFn: () => fetchOrgRulesetSuite(org, selectedID!),
    enabled: selectedID !== null,
  });

  if (suitesQuery.isLoading) return <Spinner label={`loading ${org} rule insights`} />;
  if (suitesQuery.isError) {
    return <InlineError title="Failed to load rule insights" detail={String(suitesQuery.error)} />;
  }

  const columns = [
    suiteCol.accessor("id", { header: "ID" }),
    suiteCol.accessor("repository_name", { header: "Repository" }),
    suiteCol.accessor("ref", {
      header: "Ref",
      cell: (info) => info.getValue().replace(/^refs\/(heads|tags)\//, ""),
    }),
    suiteCol.accessor("actor_name", {
      header: "Actor",
      cell: (info) => info.getValue() ?? "GitHub App",
    }),
    suiteCol.accessor("result", {
      header: "Enforced",
      cell: (info) => <RuleResult value={info.getValue()} />,
    }),
    suiteCol.accessor("evaluation_result", {
      header: "Evaluate",
      cell: (info) => info.getValue() ? <RuleResult value={info.getValue()!} /> : "—",
    }),
    suiteCol.accessor("pushed_at", {
      header: "Pushed",
      cell: (info) => new Date(info.getValue()).toLocaleString(),
    }),
    suiteCol.display({
      id: "details",
      header: "Details",
      cell: (info) => (
        <Button size="sm" variant="ghost" onClick={() => setSelectedID(info.row.original.id)}>
          View
        </Button>
      ),
    }),
  ];

  return (
    <>
      <Box>
        <div className="flex flex-wrap gap-3 p-3" aria-label="Rule insight filters">
          <label className="flex flex-col gap-1">
            <span className="text-xs">Repository</span>
            <input
              aria-label="Repository filter"
              value={repositoryName}
              onChange={(event) => setRepositoryName(event.target.value)}
              placeholder="repository name"
            />
          </label>
          <label className="flex flex-col gap-1">
            <span className="text-xs">Ref</span>
            <input
              aria-label="Ref filter"
              value={ref}
              onChange={(event) => setRef(event.target.value)}
              placeholder="main"
            />
          </label>
          <label className="flex flex-col gap-1">
            <span className="text-xs">Result</span>
            <select aria-label="Result filter" value={result} onChange={(event) => setResult(event.target.value as typeof result)}>
              {["all", "pass", "fail", "bypass"].map((value) => <option key={value} value={value}>{value}</option>)}
            </select>
          </label>
          <label className="flex flex-col gap-1">
            <span className="text-xs">Mode</span>
            <select
              aria-label="Evaluate status filter"
              value={evaluateStatus}
              onChange={(event) => setEvaluateStatus(event.target.value as typeof evaluateStatus)}
            >
              {["all", "active", "evaluate"].map((value) => <option key={value} value={value}>{value}</option>)}
            </select>
          </label>
          <label className="flex flex-col gap-1">
            <span className="text-xs">Period</span>
            <select aria-label="Time period filter" value={timePeriod} onChange={(event) => setTimePeriod(event.target.value as typeof timePeriod)}>
              {["hour", "day", "week", "month"].map((value) => <option key={value} value={value}>{value}</option>)}
            </select>
          </label>
        </div>
        <DataTable
          data={suitesQuery.data ?? []}
          columns={columns}
          emptyMessage="No rule evaluations match these filters."
        />
      </Box>
      {selectedID !== null && (
        <Modal title={`Rule suite #${selectedID}`} onClose={() => setSelectedID(null)}>
          {detailQuery.isLoading && <Spinner label="loading rule suite details" />}
          {detailQuery.isError && <InlineError title="Failed to load rule suite" detail={String(detailQuery.error)} />}
          {detailQuery.data && (
            <div className="flex flex-col gap-3">
              <div>
                <strong>{detailQuery.data.repository_name}</strong>{" "}
                {detailQuery.data.ref.replace(/^refs\/(heads|tags)\//, "")}
              </div>
              <div>
                Enforced result: <RuleResult value={detailQuery.data.result} />
                {" · "}Evaluate result:{" "}
                {detailQuery.data.evaluation_result ? <RuleResult value={detailQuery.data.evaluation_result} /> : "not run"}
              </div>
              <ul className="m-0 list-none p-0">
                {(detailQuery.data.rule_evaluations ?? []).map((evaluation, index) => (
                  <li key={`${evaluation.rule_source.id ?? "source"}-${evaluation.rule_type}-${index}`} className="py-2" style={{ borderBottom: "1px solid var(--color-border)" }}>
                    <RuleResult value={evaluation.result} />{" "}
                    <strong>{evaluation.rule_type}</strong>
                    {" · "}{evaluation.rule_source.name ?? evaluation.rule_source.type}
                    {" · "}{evaluation.enforcement}
                    {evaluation.details && <div style={{ color: "var(--color-fg-muted)" }}>{evaluation.details}</div>}
                  </li>
                ))}
              </ul>
            </div>
          )}
          <DialogActions>
            <Button onClick={() => setSelectedID(null)} variant="ghost">Close</Button>
          </DialogActions>
        </Modal>
      )}
    </>
  );
}

function RuleResult({ value }: { value: "pass" | "fail" | "bypass" }) {
  const color = value === "pass" ? "var(--color-status-ok)" : value === "fail" ? "var(--color-status-error)" : "var(--color-status-warn)";
  return <span style={{ color, fontWeight: 600 }}>{value}</span>;
}

function RulesetDetailDialog({ ruleset, onClose }: { ruleset: GithubRuleset; onClose: () => void }) {
  return (
    <Modal title={ruleset.name} onClose={onClose}>
      <div style={{ fontSize: "0.85rem", display: "flex", flexDirection: "column", gap: "0.5rem" }}>
        <div>
          <strong>ID:</strong> {ruleset.id}
        </div>
        <div>
          <strong>Target:</strong> {ruleset.target}
        </div>
        <div>
          <strong>Enforcement:</strong> {ruleset.enforcement}
        </div>
        <div>
          <strong>Source:</strong> {ruleset.source} ({ruleset.source_type})
        </div>
        {ruleset.created_at && (
          <div>
            <strong>Created:</strong> {new Date(ruleset.created_at).toLocaleString()}
          </div>
        )}
        {ruleset.updated_at && (
          <div>
            <strong>Updated:</strong> {new Date(ruleset.updated_at).toLocaleString()}
          </div>
        )}
        <div style={{ marginTop: "0.5rem" }}>
          <strong>Rules ({ruleset.rules?.length ?? 0})</strong>
          {ruleset.rules && ruleset.rules.length > 0 ? (
            <ul style={{ listStyle: "none", padding: 0, margin: "0.5rem 0 0" }}>
              {ruleset.rules.map((rule, idx) => (
                <li key={idx} style={{ padding: "0.25rem 0", borderBottom: "1px solid var(--color-border)" }}>
                  {rule.type}
                </li>
              ))}
            </ul>
          ) : (
            <p style={{ color: "var(--color-fg-muted)" }}>No rules.</p>
          )}
        </div>
      </div>
      <DialogActions>
        <Button onClick={onClose} variant="ghost">Close</Button>
      </DialogActions>
    </Modal>
  );
}

function RulesetFormModal({
  ruleset,
  onClose,
  onSubmit,
  pending,
  error,
}: {
  ruleset: GithubRuleset | null;
  onClose: () => void;
  onSubmit: (payload: Parameters<typeof createOrgRuleset>[1]) => void;
  pending: boolean;
  error: Error | null;
}) {
  const [name, setName] = useState(ruleset?.name ?? "");
  const [target, setTarget] = useState<GithubRulesetTarget>(ruleset?.target ?? "branch");
  const [enforcement, setEnforcement] = useState<GithubRulesetEnforcement>(
    ruleset?.enforcement ?? "active",
  );
  const [ruleConfig, setRuleConfig] = useState<RulesetRuleConfig>({ rules: [], bypass_actors: [] });
  const [validationError, setValidationError] = useState<string | null>(null);

  const handleSubmit = () => {
    setValidationError(null);
    if (!name.trim()) {
      setValidationError("Name is required.");
      return;
    }
    const payload: Parameters<typeof createOrgRuleset>[1] = {
      name: name.trim(),
      target,
      enforcement,
      rules: ruleConfig.rules,
      ...(ruleConfig.conditions ? { conditions: ruleConfig.conditions } : {}),
      bypass_actors: ruleConfig.bypass_actors,
    };
    onSubmit(payload);
  };

  return (
    <Modal title={ruleset ? "Edit ruleset" : "Create ruleset"} onClose={onClose}>
      <FormLabel id="ruleset-name">Name</FormLabel>
      <input
        id="ruleset-name"
        type="text"
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder="e.g. main-branch-protection"
        className="mb-4 w-full"
      />

      <FormLabel id="ruleset-target">Target</FormLabel>
      <select
        id="ruleset-target"
        value={target}
        onChange={(e) => setTarget(e.target.value as GithubRulesetTarget)}
        className="mb-4 w-full"
      >
        {TARGETS.map((t) => (
          <option key={t} value={t}>
            {t}
          </option>
        ))}
      </select>

      <FormLabel id="ruleset-enforcement">Enforcement</FormLabel>
      <select
        id="ruleset-enforcement"
        value={enforcement}
        onChange={(e) => setEnforcement(e.target.value as GithubRulesetEnforcement)}
        className="mb-4 w-full"
      >
        {ENFORCEMENTS.map((e) => (
          <option key={e} value={e}>
            {e}
          </option>
        ))}
      </select>

      <div className="mb-4">
        <RulesetEditor target={target} initial={ruleset} onChange={setRuleConfig} />
      </div>

      {(validationError || error) && <ErrorBanner>{validationError ?? (error instanceof Error ? error.message : String(error))}</ErrorBanner>}

      <DialogActions>
        <Button onClick={onClose} disabled={pending} variant="ghost">
          Cancel
        </Button>
        <Button onClick={handleSubmit} disabled={pending} variant="primary">
          {pending ? "Saving…" : ruleset ? "Save ruleset" : "Create ruleset"}
        </Button>
      </DialogActions>
    </Modal>
  );
}
