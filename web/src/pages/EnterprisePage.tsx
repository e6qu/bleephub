import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { confirmAction } from "../components/confirmAction.js";
import { MutationError } from "../components/MutationError.js";
import { ghFetch, ghPostJSON, ghSend } from "../api.js";
import {
  fetchEnterpriseSlug,
  fetchEnterpriseTeams,
  createEnterpriseTeam,
  updateEnterpriseTeam,
  deleteEnterpriseTeam,
  fetchEnterpriseTeamMembers,
  addEnterpriseTeamMember,
  removeEnterpriseTeamMember,
  fetchEnterpriseTeamOrgs,
  assignEnterpriseTeamOrg,
  unassignEnterpriseTeamOrg,
  fetchEnterpriseActionsCacheLimit,
  setEnterpriseActionsCacheLimit,
  fetchEnterpriseDependabotAccess,
  updateEnterpriseDependabotAccess,
  setEnterpriseDependabotDefaultLevel,
} from "../api.js";
import type { GithubEnterpriseTeam } from "../types.js";
import {
  Box,
  Button,
  DialogActions,
  ErrorBanner,
  FormLabel,
  Modal,
  PageTitle,
  Tabs,
} from "../components/ui.js";
import { GlobeIcon, GraphIcon, TeamIcon } from "../components/octicons.js";
import { Blankslate } from "../components/ui.js";

const enc = encodeURIComponent;

type EnterpriseTab = "teams" | "settings" | "billing";

export function EnterprisePage() {
  const [tab, setTab] = useState<EnterpriseTab>("teams");
  const { data: enterpriseSlug } = useQuery({
    queryKey: ["enterprise-slug"],
    queryFn: ({ signal }) => fetchEnterpriseSlug(signal),
  });

  return (
    <div>
      <PageTitle
        icon={<GlobeIcon size={22} />}
        title="Enterprise"
        meta={
          enterpriseSlug
            ? `Instance-wide administration for the "${enterpriseSlug}" enterprise.`
            : "Instance-wide enterprise administration."
        }
      />
      <Tabs
        items={[
          { key: "teams" as const, label: "Teams" },
          { key: "billing" as const, label: "Billing" },
          { key: "settings" as const, label: "Settings" },
        ]}
        active={tab}
        onChange={setTab}
      />
      {tab === "teams" && <EnterpriseTeamsPanel />}
      {tab === "billing" && <EnterpriseBillingPanel enterpriseSlug={enterpriseSlug} />}
      {tab === "settings" && <EnterpriseSettingsPanel />}
    </div>
  );
}

// ─── Enterprise teams ───────────────────────────────────────────────────

function EnterpriseTeamsPanel() {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<GithubEnterpriseTeam | null>(null);
  const [viewingMembers, setViewingMembers] = useState<GithubEnterpriseTeam | null>(null);
  const [viewingOrgs, setViewingOrgs] = useState<GithubEnterpriseTeam | null>(null);

  const { data: teams, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["enterprise-teams"],
    queryFn: fetchEnterpriseTeams,
  });

  const deleteMut = useMutation({
    mutationFn: (slug: string) => deleteEnterpriseTeam(slug),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["enterprise-teams"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  if (isLoading) return <Spinner label="loading enterprise teams" />;
  if (isError || !teams) {
    return <InlineError title="Failed to load enterprise teams" detail={String(loadErr)} />;
  }

  return (
    <div>
      <div className="mb-3 flex items-center justify-between gap-3">
        <span style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          Teams that span every organization on this instance.
        </span>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
          New enterprise team
        </Button>
      </div>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {teams.length === 0 ? (
        <Blankslate icon={<TeamIcon size={26} />} title="No enterprise teams yet" />
      ) : (
        <Box>
          {teams.map((team, i) => (
            <div
              key={team.id}
              className="flex flex-wrap items-center gap-3"
              style={{
                padding: "0.7rem 1rem",
                borderBottom: i < teams.length - 1 ? "1px solid var(--color-border)" : "none",
              }}
            >
              <div className="min-w-0 flex-1">
                <div style={{ fontWeight: 600, fontSize: "0.92rem" }}>
                  {team.name} <span style={{ color: "var(--color-fg-muted)", fontWeight: 400 }}>@{team.slug}</span>
                </div>
                <div className="mt-0.5" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                  {team.description || "No description"} · organizations: {team.organization_selection_type} ·
                  created {new Date(team.created_at).toLocaleDateString()}
                </div>
              </div>
              <Button size="sm" variant="ghost" onClick={() => setViewingMembers(team)}>
                members
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setViewingOrgs(team)}>
                organizations
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setEditing(team)}>
                edit
              </Button>
              <Button
                size="sm"
                variant="danger"
                disabled={deleteMut.isPending}
                onClick={async () => {
                  if (await confirmAction(`Delete enterprise team ${team.slug}?`)) deleteMut.mutate(team.slug);
                }}
              >
                delete
              </Button>
            </div>
          ))}
        </Box>
      )}
      {(creating || editing) && (
        <EnterpriseTeamDialog
          team={editing ?? undefined}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
        />
      )}
      {viewingMembers && (
        <EnterpriseTeamMembersDialog team={viewingMembers} onClose={() => setViewingMembers(null)} />
      )}
      {viewingOrgs && (
        <EnterpriseTeamOrgsDialog team={viewingOrgs} onClose={() => setViewingOrgs(null)} />
      )}
    </div>
  );
}

function EnterpriseTeamDialog({
  team,
  onClose,
}: {
  team?: GithubEnterpriseTeam | undefined;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [name, setName] = useState(team?.name ?? "");
  const [description, setDescription] = useState(team?.description ?? "");
  const [selectionType, setSelectionType] = useState(team?.organization_selection_type ?? "disabled");
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () =>
      team
        ? updateEnterpriseTeam(team.slug, {
            name: name.trim(),
            description,
            organization_selection_type: selectionType,
          })
        : createEnterpriseTeam({
            name: name.trim(),
            description: description || undefined,
            organization_selection_type: selectionType,
          }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["enterprise-teams"] });
      onClose();
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Modal title={team ? `Edit ${team.slug}` : "New enterprise team"} onClose={onClose}>
      <FormLabel id="ent-team-name">Name</FormLabel>
      <input
        id="ent-team-name"
        autoFocus
        value={name}
        onChange={(e) => setName(e.target.value)}
        className="mb-3 w-full"
      />
      <FormLabel id="ent-team-desc">Description (optional)</FormLabel>
      <input
        id="ent-team-desc"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        className="mb-3 w-full"
      />
      <FormLabel id="ent-team-orgs">Organization selection</FormLabel>
      <select
        id="ent-team-orgs"
        value={selectionType}
        onChange={(e) => setSelectionType(e.target.value)}
        className="mb-4 w-full"
      >
        <option value="disabled">disabled</option>
        <option value="selected">selected</option>
        <option value="all">all</option>
      </select>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={mutation.isPending}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!name.trim() || mutation.isPending}
          onClick={() => {
            setError(null);
            mutation.mutate();
          }}
        >
          {mutation.isPending ? "Saving…" : team ? "Save" : "Create team"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

function EnterpriseTeamMembersDialog({
  team,
  onClose,
}: {
  team: GithubEnterpriseTeam;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [username, setUsername] = useState("");

  const { data: members, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["enterprise-team-members", team.slug],
    queryFn: () => fetchEnterpriseTeamMembers(team.slug),
  });

  const addMut = useMutation({
    mutationFn: () => addEnterpriseTeamMember(team.slug, username.trim()),
    onSuccess: () => {
      setError(null);
      setUsername("");
      qc.invalidateQueries({ queryKey: ["enterprise-team-members", team.slug] });
    },
    onError: (err: Error) => setError(err.message),
  });
  const removeMut = useMutation({
    mutationFn: (login: string) => removeEnterpriseTeamMember(team.slug, login),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["enterprise-team-members", team.slug] });
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Modal title={`${team.name} members`} onClose={onClose}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <div className="mb-3 flex gap-2">
        <input
          aria-label="Username to add"
          placeholder="username"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          className="w-full"
        />
        <Button
          variant="primary"
          size="sm"
          disabled={addMut.isPending || !username.trim()}
          onClick={() => {
            setError(null);
            addMut.mutate();
          }}
        >
          Add
        </Button>
      </div>
      {isLoading && <Spinner label="loading team members" />}
      {isError && <InlineError title="Failed to load team members" detail={String(loadErr)} />}
      {members &&
        (members.length === 0 ? (
          <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>No members.</div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {members.map((m) => (
              <li
                key={m.id}
                className="flex items-center justify-between gap-2"
                style={{ padding: "0.5rem 0", borderBottom: "1px solid var(--color-border)", fontSize: "0.88rem" }}
              >
                <span style={{ fontWeight: 500 }}>@{m.login}</span>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={removeMut.isPending}
                  onClick={async () => {
                    if (await confirmAction(`Remove ${m.login} from ${team.slug}?`)) removeMut.mutate(m.login);
                  }}
                >
                  remove
                </Button>
              </li>
            ))}
          </ul>
        ))}
    </Modal>
  );
}

function EnterpriseTeamOrgsDialog({
  team,
  onClose,
}: {
  team: GithubEnterpriseTeam;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [orgSlug, setOrgSlug] = useState("");

  // The endpoints 422 unless the team's organization selection type is
  // "selected" — "all" and "disabled" derive the assignment set instead.
  const editable = team.organization_selection_type === "selected";

  const { data: orgs, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["enterprise-team-orgs", team.slug],
    queryFn: () => fetchEnterpriseTeamOrgs(team.slug),
  });

  const invalidate = () => {
    setError(null);
    qc.invalidateQueries({ queryKey: ["enterprise-team-orgs", team.slug] });
  };
  const assignMut = useMutation({
    mutationFn: () => assignEnterpriseTeamOrg(team.slug, orgSlug.trim()),
    onSuccess: () => {
      invalidate();
      setOrgSlug("");
    },
    onError: (err: Error) => setError(err.message),
  });
  const unassignMut = useMutation({
    mutationFn: (org: string) => unassignEnterpriseTeamOrg(team.slug, org),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Modal title={`${team.name} organizations`} onClose={onClose}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <div className="mb-3" style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
        Organization selection type: <strong>{team.organization_selection_type}</strong>
        {!editable &&
          " — assignments can only be edited when the selection type is \"selected\"."}
      </div>
      <div className="mb-3 flex gap-2">
        <input
          aria-label="Organization to assign"
          placeholder="organization login"
          value={orgSlug}
          onChange={(e) => setOrgSlug(e.target.value)}
          disabled={!editable}
          className="w-full"
        />
        <Button
          variant="primary"
          size="sm"
          disabled={!editable || assignMut.isPending || !orgSlug.trim()}
          onClick={() => {
            setError(null);
            assignMut.mutate();
          }}
        >
          Assign
        </Button>
      </div>
      {isLoading && <Spinner label="loading team organizations" />}
      {isError && <InlineError title="Failed to load team organizations" detail={String(loadErr)} />}
      {orgs &&
        (orgs.length === 0 ? (
          <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
            No organizations assigned.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {orgs.map((org) => (
              <li
                key={org.id}
                className="flex items-center justify-between gap-2"
                style={{ padding: "0.5rem 0", borderBottom: "1px solid var(--color-border)", fontSize: "0.88rem" }}
              >
                <span>
                  <span style={{ fontWeight: 500 }}>@{org.login}</span>
                  {org.description && (
                    <span style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem" }}>
                      {" "}
                      · {org.description}
                    </span>
                  )}
                </span>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={!editable || unassignMut.isPending}
                  onClick={async () => {
                    if (await confirmAction(`Unassign ${org.login} from ${team.slug}?`)) unassignMut.mutate(org.login);
                  }}
                >
                  unassign
                </Button>
              </li>
            ))}
          </ul>
        ))}
    </Modal>
  );
}

// ─── Enterprise billing ─────────────────────────────────────────────────

interface EnterpriseBudget {
  id: string;
  budget_scope: string;
  budget_entity_name: string;
  budget_amount: number;
  prevent_further_usage: boolean;
  budget_product_sku: string;
  budget_type: string;
  budget_alerting: { will_alert: boolean; alert_recipients: string[] };
  user?: string;
}

interface EnterpriseBudgetList {
  budgets: EnterpriseBudget[];
  total_count: number;
  has_next_page: boolean;
}

// Same request family as the org budgets — budget_alerting is required on
// create even when alerting is disabled, so send an explicit empty envelope.
interface EnterpriseBudgetBody {
  budget_amount: number;
  prevent_further_usage: boolean;
  budget_scope: string;
  budget_type: string;
  budget_product_sku: string;
  budget_entity_name?: string;
  budget_alerting: { will_alert: boolean; alert_recipients: string[] };
}

const BUDGET_SCOPES = [
  "enterprise",
  "organization",
  "repository",
  "cost_center",
  "user",
  "multi_user_customer",
  "multi_user_cost_center",
] as const;

const budgetsPath = (slug: string) => `/api/v3/enterprises/${enc(slug)}/settings/billing/budgets`;

const fetchEnterpriseBudgets = (slug: string) =>
  ghFetch<EnterpriseBudgetList>(budgetsPath(slug));
const createEnterpriseBudget = (slug: string, body: EnterpriseBudgetBody) =>
  ghPostJSON<{ budget: EnterpriseBudget }>(budgetsPath(slug), body);
const updateEnterpriseBudget = (slug: string, id: string, body: Partial<EnterpriseBudgetBody>) =>
  ghSend("PATCH", `${budgetsPath(slug)}/${enc(id)}`, body);
const deleteEnterpriseBudget = (slug: string, id: string) =>
  ghSend("DELETE", `${budgetsPath(slug)}/${enc(id)}`);

function EnterpriseBillingPanel({ enterpriseSlug }: { enterpriseSlug: string | undefined }) {
  const qc = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<EnterpriseBudget | null>(null);

  const { data, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["enterprise-budgets", enterpriseSlug],
    queryFn: () => fetchEnterpriseBudgets(enterpriseSlug!),
    enabled: !!enterpriseSlug,
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => deleteEnterpriseBudget(enterpriseSlug!, id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["enterprise-budgets", enterpriseSlug] }),
  });

  if (!enterpriseSlug || isLoading) return <Spinner label="loading spending budgets" />;
  if (isError || !data) {
    return <InlineError title="Failed to load spending budgets" detail={String(loadErr)} />;
  }

  const budgets = data.budgets ?? [];

  return (
    <div>
      <div className="mb-3 flex items-center justify-between gap-3">
        <span style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          Spending budgets cap metered usage across this enterprise.
        </span>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
          New budget
        </Button>
      </div>
      <MutationError of={deleteMut} />
      {budgets.length === 0 ? (
        <Blankslate icon={<GraphIcon size={26} />} title="No spending budgets yet" />
      ) : (
        <Box>
          {budgets.map((budget, i) => (
            <div
              key={budget.id}
              className="flex flex-wrap items-center gap-3"
              style={{
                padding: "0.7rem 1rem",
                borderBottom: i < budgets.length - 1 ? "1px solid var(--color-border)" : "none",
              }}
            >
              <div className="min-w-0 flex-1">
                <div style={{ fontWeight: 600, fontSize: "0.92rem" }}>
                  {budget.budget_product_sku}{" "}
                  <span style={{ color: "var(--color-fg-muted)", fontWeight: 400 }}>
                    ${budget.budget_amount}
                  </span>
                </div>
                <div className="mt-0.5" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                  scope: {budget.budget_scope}
                  {budget.budget_entity_name ? ` (${budget.budget_entity_name})` : ""} ·{" "}
                  {budget.prevent_further_usage ? "stops usage at limit" : "alert only"}
                </div>
              </div>
              <Button size="sm" variant="ghost" onClick={() => setEditing(budget)}>
                edit
              </Button>
              <Button
                size="sm"
                variant="danger"
                disabled={deleteMut.isPending}
                onClick={async () => {
                  if (await confirmAction(`Delete the ${budget.budget_product_sku} budget?`))
                    deleteMut.mutate(budget.id);
                }}
              >
                delete
              </Button>
            </div>
          ))}
        </Box>
      )}
      {(creating || editing) && (
        <EnterpriseBudgetDialog
          enterpriseSlug={enterpriseSlug}
          budget={editing ?? undefined}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
        />
      )}
    </div>
  );
}

function EnterpriseBudgetDialog({
  enterpriseSlug,
  budget,
  onClose,
}: {
  enterpriseSlug: string;
  budget?: EnterpriseBudget | undefined;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [productSku, setProductSku] = useState(budget?.budget_product_sku ?? "");
  const [amount, setAmount] = useState(String(budget?.budget_amount ?? ""));
  const [scope, setScope] = useState(budget?.budget_scope ?? "enterprise");
  const [entityName, setEntityName] = useState(budget?.budget_entity_name ?? "");
  const [preventUsage, setPreventUsage] = useState(budget?.prevent_further_usage ?? false);

  const parsedAmount = parseInt(amount, 10);
  const validAmount = Number.isFinite(parsedAmount) && parsedAmount >= 0;
  // enterprise / multi_user_customer scopes reject a non-empty entity name.
  const entityless = scope === "enterprise" || scope === "multi_user_customer";

  const mutation = useMutation({
    mutationFn: async () => {
      if (budget) {
        await updateEnterpriseBudget(enterpriseSlug, budget.id, {
          budget_amount: parsedAmount,
          prevent_further_usage: preventUsage,
          budget_scope: scope,
          budget_product_sku: productSku.trim(),
          budget_entity_name: entityless ? "" : entityName.trim(),
        });
        return;
      }
      await createEnterpriseBudget(enterpriseSlug, {
        budget_amount: parsedAmount,
        prevent_further_usage: preventUsage,
        budget_scope: scope,
        budget_type: "ProductPricing",
        budget_product_sku: productSku.trim(),
        budget_entity_name: entityless ? "" : entityName.trim(),
        budget_alerting: { will_alert: false, alert_recipients: [] },
      });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["enterprise-budgets", enterpriseSlug] });
      onClose();
    },
  });

  return (
    <Modal title={budget ? "Edit spending budget" : "New spending budget"} onClose={onClose}>
      <FormLabel id="ent-budget-sku">Product SKU</FormLabel>
      <input
        id="ent-budget-sku"
        autoFocus
        placeholder="e.g. actions"
        value={productSku}
        onChange={(e) => setProductSku(e.target.value)}
        className="mb-3 w-full"
      />
      <FormLabel id="ent-budget-amount">Budget amount (USD)</FormLabel>
      <input
        id="ent-budget-amount"
        type="number"
        min={0}
        value={amount}
        onChange={(e) => setAmount(e.target.value)}
        className="mb-3 w-full"
      />
      <FormLabel id="ent-budget-scope">Scope</FormLabel>
      <select
        id="ent-budget-scope"
        value={scope}
        onChange={(e) => setScope(e.target.value)}
        className="mb-3 w-full"
      >
        {BUDGET_SCOPES.map((s) => (
          <option key={s} value={s}>
            {s}
          </option>
        ))}
      </select>
      {!entityless && (
        <>
          <FormLabel id="ent-budget-entity">Entity name</FormLabel>
          <input
            id="ent-budget-entity"
            placeholder="organization / repository / login"
            value={entityName}
            onChange={(e) => setEntityName(e.target.value)}
            className="mb-3 w-full"
          />
        </>
      )}
      <label className="mb-4 flex items-center gap-2" style={{ fontSize: "0.88rem" }}>
        <input
          type="checkbox"
          checked={preventUsage}
          onChange={(e) => setPreventUsage(e.target.checked)}
        />
        Prevent further usage once the budget is exceeded
      </label>
      <MutationError of={mutation} />
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={mutation.isPending}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!productSku.trim() || !validAmount || mutation.isPending}
          onClick={() => mutation.mutate()}
        >
          {mutation.isPending ? "Saving…" : budget ? "Save" : "Create budget"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

// ─── Enterprise settings ────────────────────────────────────────────────

function EnterpriseSettingsPanel() {
  return (
    <div className="flex flex-col gap-4">
      <ActionsCacheSettings />
      <DependabotAccessSettings />
    </div>
  );
}

function ActionsCacheSettings() {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [sizeInput, setSizeInput] = useState("");

  const { data, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["enterprise-actions-cache-limit"],
    queryFn: fetchEnterpriseActionsCacheLimit,
  });

  const setMut = useMutation({
    mutationFn: (gb: number) => setEnterpriseActionsCacheLimit(gb),
    onSuccess: () => {
      setError(null);
      setSizeInput("");
      qc.invalidateQueries({ queryKey: ["enterprise-actions-cache-limit"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const parsed = parseInt(sizeInput, 10);
  const valid = Number.isFinite(parsed) && parsed > 0;

  return (
    <Box header={<span style={{ fontWeight: 600 }}>GitHub Actions cache storage limit</span>}>
      <div style={{ padding: "0.9rem 1rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        {isLoading && <Spinner label="loading Actions cache limit" />}
        {isError && <InlineError title="Failed to load Actions cache limit" detail={String(loadErr)} />}
        {data && (
          <div className="flex flex-wrap items-center gap-3">
            <span style={{ fontSize: "0.88rem" }}>
              Current limit: <strong>{data.max_cache_size_gb} GB</strong> per repository
            </span>
            <input
              aria-label="New cache size limit in GB"
              type="number"
              min={1}
              placeholder="GB"
              value={sizeInput}
              onChange={(e) => setSizeInput(e.target.value)}
              style={{ width: "6rem" }}
            />
            <Button
              size="sm"
              variant="primary"
              disabled={setMut.isPending || !valid}
              onClick={() => {
                setError(null);
                setMut.mutate(parsed);
              }}
            >
              Update limit
            </Button>
          </div>
        )}
      </div>
    </Box>
  );
}

function DependabotAccessSettings() {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [repoIdInput, setRepoIdInput] = useState("");

  const { data, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["enterprise-dependabot-access"],
    queryFn: fetchEnterpriseDependabotAccess,
  });

  const invalidate = () => {
    setError(null);
    qc.invalidateQueries({ queryKey: ["enterprise-dependabot-access"] });
  };
  const levelMut = useMutation({
    mutationFn: (level: "public" | "internal") => setEnterpriseDependabotDefaultLevel(level),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const addMut = useMutation({
    mutationFn: (id: number) => updateEnterpriseDependabotAccess({ repository_ids_to_add: [id] }),
    onSuccess: () => {
      invalidate();
      setRepoIdInput("");
    },
    onError: (err: Error) => setError(err.message),
  });
  const removeMut = useMutation({
    mutationFn: (id: number) => updateEnterpriseDependabotAccess({ repository_ids_to_remove: [id] }),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });

  const parsedId = parseInt(repoIdInput, 10);
  const validId = Number.isFinite(parsedId) && parsedId > 0;

  return (
    <Box header={<span style={{ fontWeight: 600 }}>Dependabot repository access</span>}>
      <div style={{ padding: "0.9rem 1rem" }}>
        {error && <ErrorBanner>{error}</ErrorBanner>}
        {isLoading && <Spinner label="loading Dependabot repository access" />}
        {isError && (
          <InlineError title="Failed to load Dependabot repository access" detail={String(loadErr)} />
        )}
        {data && (
          <>
            <div className="mb-3 flex flex-wrap items-center gap-3">
              <span style={{ fontSize: "0.88rem" }}>Default access level:</span>
              <select
                aria-label="Dependabot default access level"
                value={data.default_level ?? ""}
                onChange={(e) => {
                  const v = e.target.value;
                  if (v === "public" || v === "internal") levelMut.mutate(v);
                }}
                disabled={levelMut.isPending}
              >
                {data.default_level == null && <option value="">not set</option>}
                <option value="public">public</option>
                <option value="internal">internal</option>
              </select>
            </div>
            <div className="mb-2 flex flex-wrap items-center gap-2">
              <input
                aria-label="Repository ID to grant Dependabot access"
                type="number"
                min={1}
                placeholder="repository ID"
                value={repoIdInput}
                onChange={(e) => setRepoIdInput(e.target.value)}
                style={{ width: "9rem" }}
              />
              <Button
                size="sm"
                disabled={addMut.isPending || !validId}
                onClick={() => {
                  setError(null);
                  addMut.mutate(parsedId);
                }}
              >
                Grant access
              </Button>
            </div>
            {data.accessible_repositories.length === 0 ? (
              <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
                No repositories granted private-repository Dependabot access.
              </div>
            ) : (
              <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
                {data.accessible_repositories.map((repo) => (
                  <li
                    key={repo.id}
                    className="flex items-center justify-between gap-2"
                    style={{
                      padding: "0.45rem 0",
                      borderBottom: "1px solid var(--color-border)",
                      fontSize: "0.88rem",
                    }}
                  >
                    <span>
                      {repo.full_name}{" "}
                      <span style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem" }}>#{repo.id}</span>
                    </span>
                    <Button
                      size="sm"
                      variant="danger"
                      disabled={removeMut.isPending}
                      onClick={() => removeMut.mutate(repo.id)}
                    >
                      revoke
                    </Button>
                  </li>
                ))}
              </ul>
            )}
          </>
        )}
      </div>
    </Box>
  );
}
