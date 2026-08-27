import { useState } from "react";
import { Link, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { OrgHeader } from "../components/PageHeader.js";
import { PageTitle, Box, Button, FormLabel, ErrorBanner, SectionLabel, Blankslate } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { confirmAction } from "../components/confirmAction.js";
import { GraphIcon, TrashIcon } from "../components/octicons.js";
import { fetchOrgProfile, updateOrg, ghFetch, ghPostJSON, ghSend } from "../api.js";
import {
  GearIcon,
  PeopleIcon,
  WebhookIcon,
  CommentIcon,
  TeamIcon,
} from "../components/octicons.js";

const enc = encodeURIComponent;

// Org Settings landing: edits the org profile (PATCH /orgs/{org}) and links to
// the other settings surfaces.
export function OrgSettingsPage() {
  const { org = "" } = useParams<{ org: string }>();
  const qc = useQueryClient();
  const profile = useQuery({ queryKey: ["org-profile", org], queryFn: () => fetchOrgProfile(org), enabled: !!org });

  const [form, setForm] = useState<{ name: string; description: string; billing_email: string } | null>(null);
  const current = form ?? {
    name: profile.data?.name ?? "",
    description: profile.data?.description ?? "",
    billing_email: profile.data?.email ?? "",
  };
  const set = (k: keyof typeof current, v: string) => setForm({ ...current, [k]: v });

  const saveMut = useMutation({
    mutationFn: () => updateOrg(org, { name: current.name, description: current.description, billing_email: current.billing_email }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["org-profile", org] }),
  });

  const base = `/ui/orgs/${org}`;
  const sections: { to: string; icon: React.ReactNode; label: string; hint: string }[] = [
    { to: `${base}/governance?tab=member-privileges`, icon: <PeopleIcon size={16} />, label: "Member privileges", hint: "Base permissions, repository creation, and governance" },
    { to: `${base}/rulesets`, icon: <GearIcon size={16} />, label: "Repository rulesets", hint: "Branch and tag protection rules across repositories" },
    { to: `${base}/hooks`, icon: <WebhookIcon size={16} />, label: "Webhooks", hint: "Organization webhooks and deliveries" },
    { to: `${base}/copilot`, icon: <CommentIcon size={16} />, label: "Copilot", hint: "Copilot access and policies" },
    { to: `${base}/people`, icon: <PeopleIcon size={16} />, label: "People", hint: "Members and outside collaborators" },
    { to: `${base}/teams`, icon: <TeamIcon size={16} />, label: "Teams", hint: "Team structure and membership" },
  ];

  return (
    <div>
      <OrgHeader org={org} active="settings" />
      <PageTitle title="Settings" />
      {profile.isError ? (
        <InlineError title="Failed to load organization" detail={String(profile.error)} />
      ) : profile.isLoading ? (
        <Spinner label="loading organization" />
      ) : (
        <div className="flex flex-col gap-5" style={{ maxWidth: "48rem" }}>
          <Box header={<span style={{ fontWeight: 600 }}>Organization profile</span>}>
            <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
              {saveMut.error && <ErrorBanner>{String(saveMut.error)}</ErrorBanner>}
              <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                <FormLabel id="org-name">Display name</FormLabel>
                <input id="org-name" type="text" value={current.name} onChange={(e) => set("name", e.target.value)} className="w-full" />
              </div>
              <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                <FormLabel id="org-description">Description</FormLabel>
                <textarea id="org-description" value={current.description} rows={3} onChange={(e) => set("description", e.target.value)} className="w-full" />
              </div>
              <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                <FormLabel id="org-billing-email">Billing email</FormLabel>
                <input id="org-billing-email" type="email" value={current.billing_email} onChange={(e) => set("billing_email", e.target.value)} className="w-full" />
              </div>
              <div className="flex items-center justify-end gap-3">
                {saveMut.isSuccess && <span style={{ fontSize: "0.82rem", color: "var(--gh-open)" }}>Saved.</span>}
                <Button variant="primary" disabled={saveMut.isPending} onClick={() => saveMut.mutate()}>
                  {saveMut.isPending ? "Saving…" : "Save"}
                </Button>
              </div>
            </div>
          </Box>

          <BillingSection org={org} />

          <section>
            <SectionLabel>Access and governance</SectionLabel>
            <Box>
              {sections.map((s, i) => (
                <Link
                  key={s.to}
                  to={s.to}
                  className="flex items-center gap-3"
                  style={{
                    padding: "0.75rem 1rem",
                    borderBottom: i < sections.length - 1 ? "1px solid var(--color-border)" : "none",
                    textDecoration: "none",
                    color: "var(--color-fg)",
                  }}
                >
                  <span style={{ color: "var(--color-fg-muted)" }}>{s.icon}</span>
                  <span className="flex-1">
                    <span style={{ fontWeight: 600, fontSize: "0.9rem", color: "var(--color-accent)" }}>{s.label}</span>
                    <span style={{ display: "block", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>{s.hint}</span>
                  </span>
                </Link>
              ))}
            </Box>
          </section>
        </div>
      )}
    </div>
  );
}

// Billing: read-only usage summary plus CRUD over spending budgets.

interface OrgBudget {
  id: string;
  budget_scope: string;
  budget_entity_name: string;
  budget_amount: number;
  prevent_further_usage: boolean;
  budget_product_sku: string;
  budget_type: string;
}

interface BudgetsResponse {
  budgets: OrgBudget[];
  total_count: number;
  has_next_page: boolean;
}

interface UsageSummaryItem {
  product: string;
  sku: string;
  unitType: string;
  netQuantity: number;
  netAmount: number;
}

interface UsageSummaryResponse {
  timePeriod: { year: number; month?: number; day?: number };
  organization: string;
  usageItems: UsageSummaryItem[];
}

const budgetScopes = ["organization", "repository", "multi_user_customer", "user"] as const;

function BillingSection({ org }: { org: string }) {
  const qc = useQueryClient();
  const budgetsKey = ["org-budgets", org];

  const budgets = useQuery({
    queryKey: budgetsKey,
    queryFn: () => ghFetch<BudgetsResponse>(`/api/v3/organizations/${enc(org)}/settings/billing/budgets`),
    enabled: !!org,
  });
  const usage = useQuery({
    queryKey: ["org-billing-usage", org],
    queryFn: () => ghFetch<UsageSummaryResponse>(`/api/v3/organizations/${enc(org)}/settings/billing/usage/summary`),
    enabled: !!org,
  });

  const [sku, setSku] = useState("");
  const [amount, setAmount] = useState("");
  const [scope, setScope] = useState<(typeof budgetScopes)[number]>("organization");
  const [entity, setEntity] = useState("");
  const [prevent, setPrevent] = useState(false);

  const invalidate = () => qc.invalidateQueries({ queryKey: budgetsKey });

  const createMut = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = {
        budget_product_sku: sku.trim(),
        budget_scope: scope,
        prevent_further_usage: prevent,
      };
      if (amount.trim() !== "") body.budget_amount = Number(amount);
      if (entity.trim() !== "") body.budget_entity_name = entity.trim();
      return ghPostJSON(`/api/v3/organizations/${enc(org)}/settings/billing/budgets`, body);
    },
    onSuccess: () => {
      setSku("");
      setAmount("");
      setEntity("");
      setPrevent(false);
      invalidate();
    },
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => ghSend("DELETE", `/api/v3/organizations/${enc(org)}/settings/billing/budgets/${enc(id)}`),
    onSuccess: invalidate,
  });

  return (
    <section>
      <SectionLabel>Billing and plans</SectionLabel>
      <div className="flex flex-col gap-4">
        <Box header={<span style={{ fontWeight: 600 }}>Usage this period</span>}>
          <div style={{ padding: "1rem" }}>
            {usage.isError ? (
              <InlineError title="Failed to load usage" detail={String(usage.error)} />
            ) : usage.isLoading ? (
              <Spinner label="loading usage" />
            ) : usage.data?.usageItems && usage.data.usageItems.length > 0 ? (
              <table style={{ width: "100%", fontSize: "0.85rem", borderCollapse: "collapse" }}>
                <thead>
                  <tr style={{ textAlign: "left", color: "var(--color-fg-muted)" }}>
                    <th style={{ padding: "0.35rem 0" }}>Product</th>
                    <th style={{ padding: "0.35rem 0" }}>SKU</th>
                    <th style={{ padding: "0.35rem 0" }}>Quantity</th>
                    <th style={{ padding: "0.35rem 0" }}>Net amount</th>
                  </tr>
                </thead>
                <tbody>
                  {usage.data.usageItems.map((u) => (
                    <tr key={`${u.product}/${u.sku}`} style={{ borderTop: "1px solid var(--color-border)" }}>
                      <td style={{ padding: "0.35rem 0" }}>{u.product}</td>
                      <td style={{ padding: "0.35rem 0" }}>{u.sku}</td>
                      <td style={{ padding: "0.35rem 0" }}>{u.netQuantity} {u.unitType}</td>
                      <td style={{ padding: "0.35rem 0" }}>${u.netAmount.toFixed(2)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <Blankslate icon={<GraphIcon size={24} />} title="No billable usage this period" />
            )}
          </div>
        </Box>

        <Box header={<span style={{ fontWeight: 600 }}>Spending budgets</span>}>
          <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.9rem" }}>
            <MutationError of={[createMut, deleteMut]} />

            <form
              className="flex flex-col gap-3"
              onSubmit={(e) => {
                e.preventDefault();
                if (sku.trim() !== "") createMut.mutate();
              }}
              style={{
                border: "1px solid var(--color-border)",
                borderRadius: "var(--radius-md)",
                padding: "0.85rem",
              }}
            >
              <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                <FormLabel id="budget-sku">Product SKU</FormLabel>
                <input id="budget-sku" type="text" value={sku} placeholder="actions_linux" onChange={(e) => setSku(e.target.value)} className="w-full" />
              </div>
              <div className="flex gap-3">
                <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem", flex: 1 }}>
                  <FormLabel id="budget-amount">Amount (USD)</FormLabel>
                  <input id="budget-amount" type="number" min={0} value={amount} onChange={(e) => setAmount(e.target.value)} className="w-full" />
                </div>
                <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem", flex: 1 }}>
                  <FormLabel id="budget-scope">Scope</FormLabel>
                  <select
                    id="budget-scope"
                    value={scope}
                    onChange={(e) => setScope(e.target.value as (typeof budgetScopes)[number])}
                    className="w-full"
                  >
                    {budgetScopes.map((s) => (
                      <option key={s} value={s}>{s}</option>
                    ))}
                  </select>
                </div>
              </div>
              <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
                <FormLabel id="budget-entity">Entity name (optional)</FormLabel>
                <input id="budget-entity" type="text" value={entity} placeholder={scope === "repository" ? "owner/repo" : ""} onChange={(e) => setEntity(e.target.value)} className="w-full" />
              </div>
              <label className="flex items-center gap-2" style={{ fontSize: "0.85rem", color: "var(--color-fg)" }}>
                <input type="checkbox" checked={prevent} onChange={(e) => setPrevent(e.target.checked)} />
                Prevent further usage when the budget is exceeded
              </label>
              <div className="flex justify-end">
                <Button type="submit" variant="primary" disabled={createMut.isPending || sku.trim() === ""}>
                  {createMut.isPending ? "Creating…" : "Create budget"}
                </Button>
              </div>
            </form>

            {budgets.isError ? (
              <InlineError title="Failed to load budgets" detail={String(budgets.error)} />
            ) : budgets.isLoading ? (
              <Spinner label="loading budgets" />
            ) : budgets.data?.budgets && budgets.data.budgets.length > 0 ? (
              <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: "0.5rem" }}>
                {budgets.data.budgets.map((b) => (
                  <BudgetRow
                    key={b.id}
                    org={org}
                    budget={b}
                    onChanged={invalidate}
                    onDelete={async () => {
                      if (await confirmAction(`Delete the ${b.budget_product_sku} budget?`, { confirmLabel: "Delete" })) {
                        deleteMut.mutate(b.id);
                      }
                    }}
                  />
                ))}
              </ul>
            ) : (
              <Blankslate title="No spending budgets" />
            )}
          </div>
        </Box>
      </div>
    </section>
  );
}

function BudgetRow({
  org,
  budget,
  onChanged,
  onDelete,
}: {
  org: string;
  budget: OrgBudget;
  onChanged: () => void;
  onDelete: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [amount, setAmount] = useState(String(budget.budget_amount));
  const [prevent, setPrevent] = useState(budget.prevent_further_usage);

  const patchMut = useMutation({
    mutationFn: () =>
      ghSend("PATCH", `/api/v3/organizations/${enc(org)}/settings/billing/budgets/${enc(budget.id)}`, {
        budget_amount: Number(amount),
        prevent_further_usage: prevent,
      }),
    onSuccess: () => {
      setEditing(false);
      onChanged();
    },
  });

  return (
    <li
      style={{
        border: "1px solid var(--color-border)",
        borderRadius: "var(--radius-md)",
        padding: "0.7rem 0.85rem",
      }}
    >
      <div className="flex items-center justify-between gap-3">
        <div>
          <span style={{ fontWeight: 600, fontSize: "0.9rem" }}>{budget.budget_product_sku}</span>
          <span style={{ display: "block", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
            {budget.budget_scope}
            {budget.budget_entity_name ? ` · ${budget.budget_entity_name}` : ""} · ${budget.budget_amount}
            {budget.prevent_further_usage ? " · blocks usage" : ""}
          </span>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="ghost" onClick={() => setEditing((v) => !v)}>{editing ? "Cancel" : "Edit"}</Button>
          <Button variant="ghost" aria-label={`Delete ${budget.budget_product_sku} budget`} onClick={onDelete}>
            <TrashIcon size={15} />
          </Button>
        </div>
      </div>
      {editing && (
        <div className="flex flex-col gap-2" style={{ marginTop: "0.7rem" }}>
          <MutationError of={patchMut} />
          <div className="flex items-end gap-3">
            <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
              <FormLabel id={`budget-amount-${budget.id}`}>Amount (USD)</FormLabel>
              <input id={`budget-amount-${budget.id}`} type="number" min={0} value={amount} onChange={(e) => setAmount(e.target.value)} />
            </div>
            <label className="flex items-center gap-2" style={{ fontSize: "0.85rem", color: "var(--color-fg)", paddingBottom: "0.4rem" }}>
              <input type="checkbox" checked={prevent} onChange={(e) => setPrevent(e.target.checked)} />
              Prevent further usage
            </label>
            <Button variant="primary" disabled={patchMut.isPending} onClick={() => patchMut.mutate()}>
              {patchMut.isPending ? "Saving…" : "Save"}
            </Button>
          </div>
        </div>
      )}
    </li>
  );
}
