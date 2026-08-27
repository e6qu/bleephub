import { useState } from "react";
import { useParams, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { confirmAction } from "../components/confirmAction.js";
import {
  ghFetch,
  ghSend,
  ghPostJSON,
  fetchOrgInvitations,
  fetchFailedOrgInvitations,
  createOrgInvitation,
  cancelOrgInvitation,
  fetchOutsideCollaborators,
  removeOutsideCollaborator,
  fetchOrgBlocks,
  blockOrgUser,
  unblockOrgUser,
  fetchOrgCustomProperties,
  upsertOrgCustomProperties,
  deleteOrgCustomProperty,
  fetchOrgRepoCustomPropertyValues,
  setOrgRepoCustomPropertyValues,
  fetchOrgIssueTypes,
  createOrgIssueType,
  updateOrgIssueType,
  deleteOrgIssueType,
  fetchOrgRoles,
  fetchOrgRoleTeams,
  fetchOrgRoleUsers,
  assignOrgRoleToTeam,
  revokeOrgRoleFromTeam,
  assignOrgRoleToUser,
  revokeOrgRoleFromUser,
} from "../api.js";
import type {
  GithubAccount,
  GithubCustomProperty,
  GithubCustomPropertyValueType,
  GithubIssueType,
  GithubOrgInvitation,
  GithubOrgRole,
  GithubOrgRepoCustomPropertyValues,
} from "../types.js";
import type { SecretsScope } from "../api.js";
import { SecretsSection, VariablesSection } from "../components/SecretsManager.js";
import { InteractionLimitsCard } from "../components/InteractionLimitsCard.js";
import { OrgHeader } from "../components/PageHeader.js";
import {
  Box,
  Button,
  ErrorBanner,
  FormLabel,
  Modal,
  DialogActions,
  Tabs,
} from "../components/ui.js";
import { ChevronDownIcon, ChevronRightIcon } from "../components/octicons.js";

type GovernanceTab = "people" | "roles" | "member-privileges" | "actions" | "secrets" | "code-security" | "properties" | "issue-types";

const GOVERNANCE_TABS: GovernanceTab[] = ["people", "roles", "member-privileges", "actions", "secrets", "code-security", "properties", "issue-types"];

export function OrgGovernancePage() {
  const { org = "" } = useParams<{ org: string }>();
  const [params, setParams] = useSearchParams();
  const urlTab = params.get("tab") ?? "";
  const [tab, setTab] = useState<GovernanceTab>(
    GOVERNANCE_TABS.includes(urlTab as GovernanceTab) ? (urlTab as GovernanceTab) : "people",
  );
  const selectTab = (next: GovernanceTab) => {
    setTab(next);
    const merged = new URLSearchParams(params);
    merged.set("tab", next);
    setParams(merged);
  };

  return (
    <div>
      <OrgHeader org={org} active="governance" />
      <Tabs
        items={[
          { key: "people" as const, label: "People" },
          { key: "roles" as const, label: "Roles" },
          { key: "member-privileges" as const, label: "Member privileges" },
          { key: "actions" as const, label: "Actions" },
          { key: "secrets" as const, label: "Secrets and variables" },
          { key: "code-security" as const, label: "Code security" },
          { key: "properties" as const, label: "Custom properties" },
          { key: "issue-types" as const, label: "Issue types" },
        ]}
        active={tab}
        onChange={selectTab}
      />
      {tab === "people" && <PeoplePanel org={org} />}
      {tab === "roles" && <RolesPanel org={org} />}
      {tab === "member-privileges" && <MemberPrivilegesPanel org={org} />}
      {tab === "actions" && <OrgActionsPanel org={org} />}
      {tab === "secrets" && <OrgSecretsPanel org={org} />}
      {tab === "code-security" && <OrgCodeSecurityPanel org={org} />}
      {tab === "properties" && <PropertiesPanel org={org} />}
      {tab === "issue-types" && <IssueTypesPanel org={org} />}
    </div>
  );
}

interface OrgMemberPrivileges {
  default_repository_permission: string;
  members_can_create_repositories: boolean;
  members_can_create_public_repositories: boolean;
  members_can_create_private_repositories: boolean;
  members_can_create_pages: boolean;
  members_can_fork_private_repositories: boolean;
  members_can_create_teams: boolean;
  web_commit_signoff_required: boolean;
}

// Org Settings › Member privileges — all via PATCH /orgs/{org}.
function MemberPrivilegesPanel({ org }: { org: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState<OrgMemberPrivileges | null>(null);
  const detail = useQuery({
    queryKey: ["org-member-privileges", org],
    queryFn: () => ghFetch<OrgMemberPrivileges>(`/api/v3/orgs/${encodeURIComponent(org)}`),
  });
  const current = form ?? detail.data ?? null;
  const save = useMutation({
    mutationFn: (payload: OrgMemberPrivileges) => ghSend("PATCH", `/api/v3/orgs/${encodeURIComponent(org)}`, payload),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["org-member-privileges", org] }); },
    onError: (e: Error) => setError(e.message),
  });

  if (detail.isLoading) return <Spinner label="loading member privileges" />;
  if (detail.isError || !current) return <InlineError title="Failed to load member privileges" />;

  const update = (patch: Partial<OrgMemberPrivileges>) => setForm({ ...current, ...patch });

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1.25rem", maxWidth: "44rem", marginTop: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <div>
        <FormLabel id="base-perm">Base permissions</FormLabel>
        <p style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)", margin: "0.2rem 0 0.4rem" }}>
          The default permission every member has on organization repositories.
        </p>
        <select id="base-perm" aria-label="Base permissions" value={current.default_repository_permission}
          onChange={(e) => update({ default_repository_permission: e.target.value })}
          style={{ padding: "0.4rem 0.6rem", fontSize: "0.9rem" }}>
          {[
            { value: "none", label: "No permission" },
            { value: "read", label: "Read" },
            { value: "write", label: "Write" },
            { value: "admin", label: "Admin" },
          ].map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
        </select>
      </div>
      {(
        [
          ["members_can_create_repositories", "Allow members to create repositories"],
          ["members_can_create_public_repositories", "Allow members to create public repositories"],
          ["members_can_create_private_repositories", "Allow members to create private repositories"],
          ["members_can_create_pages", "Allow members to publish GitHub Pages sites"],
          ["members_can_fork_private_repositories", "Allow forking of private repositories"],
          ["members_can_create_teams", "Allow members to create teams"],
          ["web_commit_signoff_required", "Require contributors to sign off on web-based commits"],
        ] as [keyof OrgMemberPrivileges, string][]
      ).map(([key, label]) => (
        <label key={key} style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.9rem", minHeight: "1.625rem" }}>
          <input type="checkbox" checked={!!current[key]}
            onChange={(e) => update({ [key]: e.target.checked })} />
          {label}
        </label>
      ))}
      <div>
        <Button variant="primary" disabled={save.isPending || !form} onClick={() => form && save.mutate(form)}>
          {save.isPending ? "Saving…" : "Save"}
        </Button>
      </div>
    </div>
  );
}

interface OrgActionsPermissions { enabled_repositories: string; allowed_actions: string }
interface OrgWorkflowPermissions { default_workflow_permissions: string; can_approve_pull_request_reviews: boolean }
const orgActionsBase = (org: string) => `/api/v3/orgs/${encodeURIComponent(org)}/actions/permissions`;

// Org Settings › Actions — mirrors the repo Actions settings against the org endpoints.
function OrgActionsPanel({ org }: { org: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const perms = useQuery({ queryKey: ["org-actions-perms", org], queryFn: () => ghFetch<OrgActionsPermissions>(orgActionsBase(org)) });
  const wf = useQuery({ queryKey: ["org-workflow-perms", org], queryFn: () => ghFetch<OrgWorkflowPermissions>(`${orgActionsBase(org)}/workflow`) });
  const permMut = useMutation({
    mutationFn: (body: Partial<OrgActionsPermissions>) => ghSend("PUT", orgActionsBase(org), body),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["org-actions-perms", org] }); },
    onError: (e: Error) => setError(e.message),
  });
  const wfMut = useMutation({
    mutationFn: (body: OrgWorkflowPermissions) => ghSend("PUT", `${orgActionsBase(org)}/workflow`, body),
    onSuccess: () => { setError(null); void qc.invalidateQueries({ queryKey: ["org-workflow-perms", org] }); },
    onError: (e: Error) => setError(e.message),
  });

  const ENABLED = [
    { value: "all", label: "All repositories" },
    { value: "selected", label: "Selected repositories" },
    { value: "none", label: "Disabled for all repositories" },
  ];
  const ALLOWED = [
    { value: "all", label: "Allow all actions and reusable workflows" },
    { value: "local_only", label: "Allow local actions only" },
    { value: "selected", label: "Allow select actions and reusable workflows" },
  ];

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1.5rem", maxWidth: "44rem", marginTop: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <div>
        <h3 style={{ fontSize: "1rem", fontWeight: 600, margin: "0 0 0.5rem" }}>Policies</h3>
        {perms.isLoading && <Spinner label="loading org actions permissions" />}
        {perms.isError && <InlineError title="Failed to load organization Actions permissions" />}
        {perms.data && (
          <div style={{ display: "flex", flexDirection: "column", gap: "0.4rem" }}>
            {ENABLED.map((o) => (
              <label key={o.value} style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.9rem", minHeight: "1.625rem" }}>
                <input type="radio" name="org-enabled-repos" checked={perms.data!.enabled_repositories === o.value} disabled={permMut.isPending}
                  onChange={() => permMut.mutate({ enabled_repositories: o.value })} />
                {o.label}
              </label>
            ))}
            <h3 style={{ fontSize: "1rem", fontWeight: 600, margin: "0.6rem 0 0.3rem" }}>Allowed actions</h3>
            {ALLOWED.map((o) => (
              <label key={o.value} style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.9rem", minHeight: "1.625rem" }}>
                <input type="radio" name="org-allowed-actions" checked={perms.data!.allowed_actions === o.value} disabled={permMut.isPending}
                  onChange={() => permMut.mutate({ allowed_actions: o.value })} />
                {o.label}
              </label>
            ))}
          </div>
        )}
      </div>
      <div>
        <h3 style={{ fontSize: "1rem", fontWeight: 600, margin: "0 0 0.5rem" }}>Workflow permissions</h3>
        {wf.isLoading && <Spinner label="loading org workflow permissions" />}
        {wf.isError && <InlineError title="Failed to load organization workflow permissions" />}
        {wf.data && (
          <div style={{ display: "flex", flexDirection: "column", gap: "0.4rem" }}>
            {(["read", "write"] as const).map((v) => (
              <label key={v} style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.9rem", minHeight: "1.625rem" }}>
                <input type="radio" name="org-wf-perm" checked={wf.data!.default_workflow_permissions === v} disabled={wfMut.isPending}
                  onChange={() => wfMut.mutate({ default_workflow_permissions: v, can_approve_pull_request_reviews: wf.data!.can_approve_pull_request_reviews })} />
                {v === "read" ? "Read repository contents and packages permissions" : "Read and write permissions"}
              </label>
            ))}
            <label style={{ display: "flex", alignItems: "center", gap: "0.5rem", fontSize: "0.9rem", minHeight: "1.625rem" }}>
              <input type="checkbox" checked={wf.data!.can_approve_pull_request_reviews ?? false} disabled={wfMut.isPending}
                onChange={(e) => wfMut.mutate({ default_workflow_permissions: wf.data!.default_workflow_permissions, can_approve_pull_request_reviews: e.target.checked })} />
              Allow GitHub Actions to create and approve pull requests
            </label>
          </div>
        )}
      </div>
    </div>
  );
}

// Org Settings › Secrets and variables — reuses the shared secrets manager at org scope.
function OrgSecretsPanel({ org }: { org: string }) {
  const scope: SecretsScope = { kind: "org", org };
  return (
    <div style={{ marginTop: "1rem" }}>
      <p className="mb-4" style={{ fontSize: "0.84rem", color: "var(--color-fg-muted)" }}>
        Secrets are encrypted in the browser with the organization&apos;s public key before upload
        and are never readable again from this page. Variables are stored as plain text. Each can be
        scoped to all, private, or selected repositories.
      </p>
      <div className="grid gap-6 lg:grid-cols-2">
        <SecretsSection scope={scope} />
        <VariablesSection scope={scope} />
      </div>
    </div>
  );
}

interface CodeSecurityConfig {
  id: number;
  name: string;
  description: string;
  enforcement?: string;
  default_for_new_repos?: string | null;
  advanced_security?: string;
  dependency_graph?: string;
  dependabot_alerts?: string;
  dependabot_security_updates?: string;
  code_scanning_default_setup?: string;
  secret_scanning?: string;
  secret_scanning_push_protection?: string;
  private_vulnerability_reporting?: string;
}
const cscBase = (org: string) => `/api/v3/orgs/${encodeURIComponent(org)}/code-security/configurations`;
// Feature toggles (enabled/disabled/not_set), each a code-security-configuration field.
const CSC_TOGGLES: { key: keyof CodeSecurityConfig; label: string }[] = [
  { key: "dependency_graph", label: "Dependency graph" },
  { key: "dependabot_alerts", label: "Dependabot alerts" },
  { key: "dependabot_security_updates", label: "Dependabot security updates" },
  { key: "code_scanning_default_setup", label: "Code scanning default setup" },
  { key: "secret_scanning", label: "Secret scanning" },
  { key: "secret_scanning_push_protection", label: "Secret scanning push protection" },
  { key: "private_vulnerability_reporting", label: "Private vulnerability reporting" },
];
const CSC_STATES = ["not_set", "enabled", "disabled"];

// Org Settings › Code security › Configurations. Server: /orgs/{org}/code-security/configurations.
function OrgCodeSecurityPanel({ org }: { org: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState<Record<string, string>>({ name: "", description: "", enforcement: "enforced", advanced_security: "disabled" });
  const list = useQuery({ queryKey: ["code-security-configs", org], queryFn: () => ghFetch<CodeSecurityConfig[]>(cscBase(org)) });
  const invalidate = () => void qc.invalidateQueries({ queryKey: ["code-security-configs", org] });
  const createMut = useMutation({
    mutationFn: (body: Record<string, string>) => ghPostJSON<CodeSecurityConfig>(cscBase(org), body),
    onSuccess: () => { setError(null); setCreating(false); setForm({ name: "", description: "", enforcement: "enforced", advanced_security: "disabled" }); invalidate(); },
    onError: (e: Error) => setError(e.message),
  });
  const delMut = useMutation({
    mutationFn: (id: number) => ghSend("DELETE", `${cscBase(org)}/${id}`),
    onSuccess: () => { setError(null); invalidate(); },
    onError: (e: Error) => setError(e.message),
  });
  const defaultMut = useMutation({
    mutationFn: (id: number) => ghSend("PUT", `${cscBase(org)}/${id}/defaults`, { default_for_new_repos: "all" }),
    onSuccess: () => { setError(null); invalidate(); },
    onError: (e: Error) => setError(e.message),
  });

  const configs: CodeSecurityConfig[] = list.data ?? [];

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem", marginTop: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <div className="flex items-center justify-between">
        <p style={{ fontSize: "0.84rem", color: "var(--color-fg-muted)", margin: 0 }}>
          Reusable security configurations you can apply as the default for new repositories.
        </p>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>New configuration</Button>
      </div>
      {list.isLoading && <Spinner label="loading code security configurations" />}
      {list.isError && <InlineError title="Failed to load code security configurations" />}
      {list.data && (configs.length === 0 ? (
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>No configurations yet.</p>
      ) : (
        <Box>
          {configs.map((c, i) => (
            <div key={c.id} style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "0.75rem", padding: "0.7rem 1rem", borderBottom: i < configs.length - 1 ? "1px solid var(--color-border)" : "none" }}>
              <div style={{ minWidth: 0 }}>
                <div style={{ fontSize: "0.9rem" }}>
                  <strong>{c.name}</strong>
                  {c.default_for_new_repos && c.default_for_new_repos !== "none" && (
                    <span style={{ marginLeft: "0.5rem", fontSize: "0.72rem", color: "var(--color-fg-subtle)" }}>default: {c.default_for_new_repos}</span>
                  )}
                </div>
                <div style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>{c.description}</div>
              </div>
              <div className="flex items-center gap-2">
                <Button size="sm" variant="ghost" disabled={defaultMut.isPending} onClick={() => defaultMut.mutate(c.id)}>Set as default</Button>
                <Button size="sm" variant="danger" aria-label={`Delete configuration ${c.name}`} disabled={delMut.isPending}
                  onClick={async () => { if (await confirmAction(`Delete configuration "${c.name}"?`, { title: "Delete configuration", confirmLabel: "Delete" })) delMut.mutate(c.id); }}>Delete</Button>
              </div>
            </div>
          ))}
        </Box>
      ))}

      {creating && (
        <Modal title="New code security configuration" onClose={() => setCreating(false)}>
          <div style={{ display: "flex", flexDirection: "column", gap: "0.75rem" }}>
            <label><FormLabel id="csc-name">Name</FormLabel>
              <input id="csc-name" aria-label="Configuration name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} className="w-full" /></label>
            <label><FormLabel id="csc-desc">Description</FormLabel>
              <input id="csc-desc" aria-label="Configuration description" value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} className="w-full" /></label>
            <label style={{ display: "flex", flexDirection: "column", gap: "0.2rem", fontSize: "0.85rem" }}>
              <span>Enforcement</span>
              <select aria-label="Enforcement" value={form.enforcement} onChange={(e) => setForm({ ...form, enforcement: e.target.value })}>
                <option value="enforced">Enforced</option>
                <option value="unenforced">Unenforced</option>
              </select>
            </label>
            <label style={{ display: "flex", flexDirection: "column", gap: "0.2rem", fontSize: "0.85rem" }}>
              <span>GitHub Advanced Security</span>
              <select aria-label="GitHub Advanced Security" value={form.advanced_security} onChange={(e) => setForm({ ...form, advanced_security: e.target.value })}>
                <option value="enabled">Enabled</option>
                <option value="disabled">Disabled</option>
              </select>
            </label>
            {CSC_TOGGLES.map((t) => (
              <label key={t.key} style={{ display: "flex", flexDirection: "column", gap: "0.2rem", fontSize: "0.85rem" }}>
                <span>{t.label}</span>
                <select aria-label={t.label} value={form[t.key] ?? "not_set"} onChange={(e) => setForm({ ...form, [t.key]: e.target.value })}>
                  {CSC_STATES.map((s) => <option key={s} value={s}>{s}</option>)}
                </select>
              </label>
            ))}
          </div>
          <DialogActions>
            <Button variant="ghost" onClick={() => setCreating(false)} disabled={createMut.isPending}>Cancel</Button>
            <Button variant="primary" disabled={!(form.name ?? "").trim() || !(form.description ?? "").trim() || createMut.isPending}
              onClick={() => createMut.mutate(form)}>{createMut.isPending ? "Creating…" : "Create configuration"}</Button>
          </DialogActions>
        </Modal>
      )}
    </div>
  );
}

// ─── People: invitations, outside collaborators, blocks ─────────────────

function invitee(inv: GithubOrgInvitation): string {
  return inv.login ? `@${inv.login}` : (inv.email ?? `#${inv.id}`);
}

function PeoplePanel({ org }: { org: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState("direct_member");
  const [blockUsername, setBlockUsername] = useState("");

  const invitations = useQuery({
    queryKey: ["org-invitations", org],
    queryFn: () => fetchOrgInvitations(org),
  });
  const failed = useQuery({
    queryKey: ["org-failed-invitations", org],
    queryFn: () => fetchFailedOrgInvitations(org),
  });
  const outside = useQuery({
    queryKey: ["org-outside-collaborators", org],
    queryFn: () => fetchOutsideCollaborators(org),
  });
  const blocks = useQuery({
    queryKey: ["org-blocks", org],
    queryFn: () => fetchOrgBlocks(org),
  });

  const inviteMut = useMutation({
    mutationFn: () => createOrgInvitation(org, { email: inviteEmail.trim(), role: inviteRole }),
    onSuccess: () => {
      setError(null);
      setInviteEmail("");
      qc.invalidateQueries({ queryKey: ["org-invitations", org] });
    },
    onError: (err: Error) => setError(err.message),
  });
  const cancelMut = useMutation({
    mutationFn: (id: number) => cancelOrgInvitation(org, id),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["org-invitations", org] });
    },
    onError: (err: Error) => setError(err.message),
  });
  const removeOutsideMut = useMutation({
    mutationFn: (username: string) => removeOutsideCollaborator(org, username),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["org-outside-collaborators", org] });
    },
    onError: (err: Error) => setError(err.message),
  });
  const blockMut = useMutation({
    mutationFn: () => blockOrgUser(org, blockUsername.trim()),
    onSuccess: () => {
      setError(null);
      setBlockUsername("");
      qc.invalidateQueries({ queryKey: ["org-blocks", org] });
    },
    onError: (err: Error) => setError(err.message),
  });
  const unblockMut = useMutation({
    mutationFn: (username: string) => unblockOrgUser(org, username),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["org-blocks", org] });
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <div className="flex flex-col gap-4">
      {error && <ErrorBanner>{error}</ErrorBanner>}

      <Box header={<span style={{ fontWeight: 600 }}>Pending invitations</span>}>
        <div className="flex gap-2" style={{ padding: "0.75rem 1rem", borderBottom: "1px solid var(--color-border)" }}>
          <input
            aria-label="Invitee email"
            type="email"
            placeholder="person@example.com"
            value={inviteEmail}
            onChange={(e) => setInviteEmail(e.target.value)}
            className="w-full"
          />
          <select aria-label="Invitation role" value={inviteRole} onChange={(e) => setInviteRole(e.target.value)}>
            <option value="direct_member">direct_member</option>
            <option value="admin">admin</option>
            <option value="billing_manager">billing_manager</option>
          </select>
          <Button
            variant="primary"
            size="sm"
            disabled={inviteMut.isPending || !inviteEmail.trim()}
            onClick={() => {
              setError(null);
              inviteMut.mutate();
            }}
          >
            Invite
          </Button>
        </div>
        {invitations.isLoading && <Spinner label="loading invitations" />}
        {invitations.isError && (
          <InlineError title="Failed to load invitations" detail={String(invitations.error)} />
        )}
        {invitations.data &&
          (invitations.data.length === 0 ? (
            <EmptyRow>No pending invitations.</EmptyRow>
          ) : (
            invitations.data.map((inv, i) => (
              <PersonRow key={inv.id} last={i === invitations.data.length - 1}>
                <span style={{ fontWeight: 500 }}>{invitee(inv)}</span>
                <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>
                  {inv.role} · invited by {inv.inviter ? `@${inv.inviter.login}` : "—"} ·{" "}
                  {new Date(inv.created_at).toLocaleDateString()}
                </span>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={cancelMut.isPending}
                  onClick={() => cancelMut.mutate(inv.id)}
                >
                  cancel
                </Button>
              </PersonRow>
            ))
          ))}
      </Box>

      <Box header={<span style={{ fontWeight: 600 }}>Failed invitations</span>}>
        {failed.isLoading && <Spinner label="loading failed invitations" />}
        {failed.isError && (
          <InlineError title="Failed to load failed invitations" detail={String(failed.error)} />
        )}
        {failed.data &&
          (failed.data.length === 0 ? (
            <EmptyRow>No failed invitations.</EmptyRow>
          ) : (
            failed.data.map((inv, i) => (
              <PersonRow key={inv.id} last={i === failed.data.length - 1}>
                <span style={{ fontWeight: 500 }}>{invitee(inv)}</span>
                <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>
                  {inv.failed_reason ?? "failed"} ·{" "}
                  {inv.failed_at ? new Date(inv.failed_at).toLocaleDateString() : "—"}
                </span>
                <span />
              </PersonRow>
            ))
          ))}
      </Box>

      <Box header={<span style={{ fontWeight: 600 }}>Outside collaborators</span>}>
        {outside.isLoading && <Spinner label="loading outside collaborators" />}
        {outside.isError && (
          <InlineError title="Failed to load outside collaborators" detail={String(outside.error)} />
        )}
        {outside.data &&
          (outside.data.length === 0 ? (
            <EmptyRow>No outside collaborators.</EmptyRow>
          ) : (
            outside.data.map((u: GithubAccount, i) => (
              <PersonRow key={u.id} last={i === outside.data.length - 1}>
                <span style={{ fontWeight: 500 }}>@{u.login}</span>
                <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>{u.type}</span>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={removeOutsideMut.isPending}
                  onClick={async () => {
                    if (await confirmAction(`Remove ${u.login} as an outside collaborator?`)) {
                      removeOutsideMut.mutate(u.login);
                    }
                  }}
                >
                  remove
                </Button>
              </PersonRow>
            ))
          ))}
      </Box>

      <Box header={<span style={{ fontWeight: 600 }}>Blocked users</span>}>
        <div className="flex gap-2" style={{ padding: "0.75rem 1rem", borderBottom: "1px solid var(--color-border)" }}>
          <input
            aria-label="Username to block"
            placeholder="username"
            value={blockUsername}
            onChange={(e) => setBlockUsername(e.target.value)}
            className="w-full"
          />
          <Button
            variant="danger"
            size="sm"
            disabled={blockMut.isPending || !blockUsername.trim()}
            onClick={() => {
              setError(null);
              blockMut.mutate();
            }}
          >
            Block
          </Button>
        </div>
        {blocks.isLoading && <Spinner label="loading blocked users" />}
        {blocks.isError && <InlineError title="Failed to load blocked users" detail={String(blocks.error)} />}
        {blocks.data &&
          (blocks.data.length === 0 ? (
            <EmptyRow>No blocked users.</EmptyRow>
          ) : (
            blocks.data.map((u: GithubAccount, i) => (
              <PersonRow key={u.id} last={i === blocks.data.length - 1}>
                <span style={{ fontWeight: 500 }}>@{u.login}</span>
                <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>{u.type}</span>
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={unblockMut.isPending}
                  onClick={() => unblockMut.mutate(u.login)}
                >
                  unblock
                </Button>
              </PersonRow>
            ))
          ))}
      </Box>
      <div style={{ marginTop: "1rem" }}>
        <InteractionLimitsCard
          path={`/api/v3/orgs/${org}/interaction-limits`}
          queryKey={["org-interaction-limit", org]}
          scopeLabel={`across all public repositories owned by ${org}`}
        />
      </div>
    </div>
  );
}

function PersonRow({ children, last }: { children: React.ReactNode; last: boolean }) {
  return (
    <div
      className="flex flex-wrap items-center justify-between gap-3"
      style={{
        padding: "0.6rem 1rem",
        borderBottom: last ? "none" : "1px solid var(--color-border)",
      }}
    >
      {children}
    </div>
  );
}

function EmptyRow({ children }: { children: React.ReactNode }) {
  return (
    <div style={{ padding: "0.75rem 1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
      {children}
    </div>
  );
}

// ─── Organization roles ─────────────────────────────────────────────────

function RolesPanel({ org }: { org: string }) {
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["org-roles", org],
    queryFn: () => fetchOrgRoles(org),
  });

  if (isLoading) return <Spinner label="loading organization roles" />;
  if (isError || !data) {
    return <InlineError title="Failed to load organization roles" detail={String(error)} />;
  }

  return (
    <div className="flex flex-col gap-3">
      <div style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
        {data.totalCount} predefined role{data.totalCount === 1 ? "" : "s"}. Expand a role to see and
        manage its team and user assignments.
      </div>
      {data.items.map((role) => (
        <RoleCard key={role.id} org={org} role={role} />
      ))}
    </div>
  );
}

function RoleCard({ org, role }: { org: string; role: GithubOrgRole }) {
  const [open, setOpen] = useState(false);
  return (
    <Box>
      <button
        type="button"
        className="flex w-full items-center gap-2 text-left"
        onClick={() => setOpen((v) => !v)}
        style={{ padding: "0.7rem 1rem", background: "transparent", border: "none", color: "var(--color-fg)" }}
      >
        {open ? <ChevronDownIcon size={14} /> : <ChevronRightIcon size={14} />}
        <span style={{ fontWeight: 600, fontSize: "0.9rem" }}>{role.name}</span>
        <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>
          base role: {role.base_role}
          {role.permissions.length > 0 && ` · ${role.permissions.join(", ")}`}
        </span>
      </button>
      {open && (
        <div style={{ borderTop: "1px solid var(--color-border)", padding: "0.75rem 1rem" }}>
          <div style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)", marginBottom: "0.75rem" }}>
            {role.description}
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <RoleAssignments org={org} roleId={role.id} kind="teams" />
            <RoleAssignments org={org} roleId={role.id} kind="users" />
          </div>
        </div>
      )}
    </Box>
  );
}

function RoleAssignments({ org, roleId, kind }: { org: string; roleId: number; kind: "teams" | "users" }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");

  const key = kind === "teams" ? ["org-role-teams", org, roleId] : ["org-role-users", org, roleId];
  const query = useQuery({
    queryKey: key,
    queryFn: async (): Promise<{ key: string; label: string; assignment: string }[]> => {
      if (kind === "teams") {
        const teams = await fetchOrgRoleTeams(org, roleId);
        return teams.map((t) => ({ key: t.slug, label: `@${t.slug}`, assignment: t.assignment }));
      }
      const users = await fetchOrgRoleUsers(org, roleId);
      return users.map((u) => ({ key: u.login, label: `@${u.login}`, assignment: u.assignment }));
    },
  });

  const assignMut = useMutation({
    mutationFn: () =>
      kind === "teams"
        ? assignOrgRoleToTeam(org, name.trim(), roleId)
        : assignOrgRoleToUser(org, name.trim(), roleId),
    onSuccess: () => {
      setError(null);
      setName("");
      qc.invalidateQueries({ queryKey: key });
    },
    onError: (err: Error) => setError(err.message),
  });
  const revokeMut = useMutation({
    mutationFn: (target: string) =>
      kind === "teams"
        ? revokeOrgRoleFromTeam(org, target, roleId)
        : revokeOrgRoleFromUser(org, target, roleId),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: key });
    },
    onError: (err: Error) => setError(err.message),
  });

  if (query.isLoading) return <Spinner label={`loading role ${kind}`} />;
  if (query.isError) {
    return <InlineError title={`Failed to load assigned ${kind}`} detail={String(query.error)} />;
  }

  const entries = query.data;

  return (
    <div>
      <div style={{ fontSize: "0.8rem", fontWeight: 600, marginBottom: "0.4rem" }}>
        {kind === "teams" ? "Teams" : "Users"}
      </div>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {!entries || entries.length === 0 ? (
        <div style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
          No {kind} assigned.
        </div>
      ) : (
        <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
          {entries.map((e) => (
            <li key={e.key} className="flex items-center justify-between gap-2 py-1" style={{ fontSize: "0.85rem" }}>
              <span>
                {e.label}{" "}
                <span style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem" }}>({e.assignment})</span>
              </span>
              <Button size="sm" variant="danger" disabled={revokeMut.isPending} onClick={() => revokeMut.mutate(e.key)}>
                revoke
              </Button>
            </li>
          ))}
        </ul>
      )}
      <div className="mt-2 flex gap-2">
        <input
          aria-label={kind === "teams" ? `Team slug to assign role ${roleId}` : `Username to assign role ${roleId}`}
          placeholder={kind === "teams" ? "team-slug" : "username"}
          value={name}
          onChange={(e) => setName(e.target.value)}
          className="w-full"
        />
        <Button
          size="sm"
          disabled={assignMut.isPending || !name.trim()}
          onClick={() => {
            setError(null);
            assignMut.mutate();
          }}
        >
          Assign
        </Button>
      </div>
    </div>
  );
}

// ─── Custom properties schema ───────────────────────────────────────────

function PropertiesPanel({ org }: { org: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<GithubCustomProperty | null>(null);
  const [creating, setCreating] = useState(false);

  const { data: properties, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["org-custom-properties", org],
    queryFn: () => fetchOrgCustomProperties(org),
  });

  const deleteMut = useMutation({
    mutationFn: (name: string) => deleteOrgCustomProperty(org, name),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["org-custom-properties", org] });
    },
    onError: (err: Error) => setError(err.message),
  });

  if (isLoading) return <Spinner label="loading custom properties" />;
  if (isError || !properties) {
    return <InlineError title="Failed to load custom properties" detail={String(loadErr)} />;
  }

  return (
    <div>
      <div className="mb-3 flex items-center justify-between gap-3">
        <span style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          Typed key/value definitions repositories in this organization can carry.
        </span>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
          New property
        </Button>
      </div>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {properties.length === 0 ? (
        <EmptyBox>No custom properties defined.</EmptyBox>
      ) : (
        <Box>
          {properties.map((p, i) => (
            <div
              key={p.property_name}
              className="flex flex-wrap items-center gap-3"
              style={{
                padding: "0.7rem 1rem",
                borderBottom: i < properties.length - 1 ? "1px solid var(--color-border)" : "none",
              }}
            >
              <span style={{ fontWeight: 600, fontSize: "0.9rem" }}>{p.property_name}</span>
              <span style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }} className="min-w-0 flex-1">
                {p.value_type}
                {p.required && " · required"}
                {p.default_value != null && ` · default: ${formatPropertyValue(p.default_value)}`}
                {p.allowed_values && p.allowed_values.length > 0 && ` · [${p.allowed_values.join(", ")}]`}
                {p.description && ` · ${p.description}`}
              </span>
              <Button size="sm" variant="ghost" onClick={() => setEditing(p)}>
                edit
              </Button>
              <Button
                size="sm"
                variant="danger"
                disabled={deleteMut.isPending}
                onClick={async () => {
                  if (await confirmAction(`Delete property ${p.property_name}?`)) deleteMut.mutate(p.property_name);
                }}
              >
                delete
              </Button>
            </div>
          ))}
        </Box>
      )}
      {(creating || editing) && (
        <PropertyDialog
          org={org}
          property={editing ?? undefined}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
        />
      )}
      <RepoValuesPanel org={org} properties={properties} />
    </div>
  );
}

/** Format a custom property value (string or multi_select array). */
function formatPropertyValue(value: unknown): string {
  return Array.isArray(value) ? value.join(", ") : String(value);
}

function PropertyDialog({
  org,
  property,
  onClose,
}: {
  org: string;
  property?: GithubCustomProperty | undefined;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [name, setName] = useState(property?.property_name ?? "");
  const [valueType, setValueType] = useState<GithubCustomPropertyValueType>(
    property?.value_type ?? "string",
  );
  const [description, setDescription] = useState(property?.description ?? "");
  const [allowedValues, setAllowedValues] = useState(property?.allowed_values?.join(", ") ?? "");
  const [required, setRequired] = useState(property?.required ?? false);
  const [defaultValue, setDefaultValue] = useState(
    property?.default_value != null ? formatPropertyValue(property.default_value) : "",
  );
  const [error, setError] = useState<string | null>(null);

  const isSelect = valueType === "single_select" || valueType === "multi_select";
  const parsedAllowed = allowedValues
    .split(",")
    .map((v) => v.trim())
    .filter(Boolean);

  const mutation = useMutation({
    mutationFn: () =>
      upsertOrgCustomProperties(org, [
        {
          property_name: name.trim(),
          value_type: valueType,
          required,
          default_value: defaultValue.trim() === "" ? undefined : defaultValue.trim(),
          description: description || undefined,
          allowed_values: isSelect ? parsedAllowed : undefined,
        },
      ]),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["org-custom-properties", org] });
      onClose();
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <Modal title={property ? `Edit ${property.property_name}` : "New custom property"} onClose={onClose}>
      <FormLabel id="prop-name">Property name</FormLabel>
      <input
        id="prop-name"
        value={name}
        onChange={(e) => setName(e.target.value)}
        disabled={!!property}
        className="mb-3 w-full"
      />
      <FormLabel id="prop-type">Value type</FormLabel>
      <select
        id="prop-type"
        value={valueType}
        onChange={(e) => setValueType(e.target.value as GithubCustomPropertyValueType)}
        className="mb-3 w-full"
      >
        <option value="string">string</option>
        <option value="single_select">single_select</option>
        <option value="multi_select">multi_select</option>
        <option value="true_false">true_false</option>
        <option value="url">url</option>
      </select>
      {isSelect && (
        <>
          <FormLabel id="prop-allowed">Allowed values (comma-separated)</FormLabel>
          <input
            id="prop-allowed"
            value={allowedValues}
            onChange={(e) => setAllowedValues(e.target.value)}
            className="mb-3 w-full"
          />
        </>
      )}
      <FormLabel id="prop-desc">Description (optional)</FormLabel>
      <input
        id="prop-desc"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        className="mb-3 w-full"
      />
      <label className="mb-3 flex items-center gap-2" style={{ fontSize: "0.85rem" }}>
        <input
          type="checkbox"
          checked={required}
          onChange={(e) => setRequired(e.target.checked)}
        />
        Required (repositories without an explicit value get the default)
      </label>
      <FormLabel id="prop-default">
        {required ? "Default value" : "Default value (optional)"}
      </FormLabel>
      {valueType === "true_false" ? (
        <select
          id="prop-default"
          value={defaultValue}
          onChange={(e) => setDefaultValue(e.target.value)}
          className="mb-4 w-full"
        >
          <option value="">no default</option>
          <option value="true">true</option>
          <option value="false">false</option>
        </select>
      ) : valueType === "single_select" ? (
        <select
          id="prop-default"
          value={defaultValue}
          onChange={(e) => setDefaultValue(e.target.value)}
          className="mb-4 w-full"
        >
          <option value="">no default</option>
          {parsedAllowed.map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </select>
      ) : (
        <input
          id="prop-default"
          value={defaultValue}
          onChange={(e) => setDefaultValue(e.target.value)}
          className="mb-4 w-full"
        />
      )}
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <DialogActions>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={mutation.isPending}>
          Cancel
        </Button>
        <Button
          variant="primary"
          size="sm"
          disabled={!name.trim() || (required && !defaultValue.trim()) || mutation.isPending}
          onClick={() => {
            setError(null);
            mutation.mutate();
          }}
        >
          {mutation.isPending ? "Saving…" : "Save property"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

function RepoValuesPanel({ org, properties }: { org: string; properties: GithubCustomProperty[] }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [queryInput, setQueryInput] = useState("");
  const [repoQuery, setRepoQuery] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [propName, setPropName] = useState(properties[0]?.property_name ?? "");
  const [valueInput, setValueInput] = useState("");

  const valuesQuery = useQuery({
    queryKey: ["org-property-values", org, repoQuery],
    queryFn: () => fetchOrgRepoCustomPropertyValues(org, repoQuery || undefined),
  });

  const selectedProp = properties.find((p) => p.property_name === propName);

  const setMut = useMutation({
    mutationFn: () => {
      // An empty input unsets the property (the PATCH's null-value contract).
      let value: unknown;
      if (valueInput.trim() === "") {
        value = null;
      } else if (selectedProp?.value_type === "multi_select") {
        value = valueInput
          .split(",")
          .map((v) => v.trim())
          .filter(Boolean);
      } else {
        value = valueInput.trim();
      }
      return setOrgRepoCustomPropertyValues(org, selected, [{ property_name: propName, value }]);
    },
    onSuccess: () => {
      setError(null);
      setSelected([]);
      qc.invalidateQueries({ queryKey: ["org-property-values", org] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const toggleRepo = (name: string) =>
    setSelected((cur) => (cur.includes(name) ? cur.filter((n) => n !== name) : [...cur, name]));

  const rowSummary = (row: GithubOrgRepoCustomPropertyValues) =>
    row.properties.map((p) => `${p.property_name}=${formatPropertyValue(p.value)}`).join(", ");

  return (
    <div className="mt-4">
      <Box header={<span style={{ fontWeight: 600 }}>Repository values</span>}>
        <div style={{ padding: "0.75rem 1rem" }}>
          {error && <ErrorBanner>{error}</ErrorBanner>}
          <div className="mb-3 flex gap-2">
            <input
              aria-label="Repository search query"
              placeholder="filter repositories (repo:owner/name for an exact match)"
              value={queryInput}
              onChange={(e) => setQueryInput(e.target.value)}
              className="w-full"
            />
            <Button size="sm" onClick={() => setRepoQuery(queryInput.trim())}>
              Search
            </Button>
          </div>
          {valuesQuery.isLoading && <Spinner label="loading repository property values" />}
          {valuesQuery.isError && (
            <InlineError
              title="Failed to load repository property values"
              detail={String(valuesQuery.error)}
            />
          )}
          {valuesQuery.data &&
            (valuesQuery.data.length === 0 ? (
              <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
                No repositories matched.
              </div>
            ) : (
              <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
                {valuesQuery.data.map((row) => (
                  <li
                    key={row.repository_id}
                    className="flex flex-wrap items-center gap-2"
                    style={{
                      padding: "0.45rem 0",
                      borderBottom: "1px solid var(--color-border)",
                      fontSize: "0.88rem",
                    }}
                  >
                    <label className="flex min-w-0 flex-1 items-center gap-2">
                      <input
                        type="checkbox"
                        aria-label={`Select ${row.repository_full_name}`}
                        checked={selected.includes(row.repository_name)}
                        onChange={() => toggleRepo(row.repository_name)}
                      />
                      <span style={{ fontWeight: 500 }}>{row.repository_full_name}</span>
                      <span style={{ color: "var(--color-fg-muted)", fontSize: "0.8rem" }}>
                        {row.properties.length === 0 ? "no values" : rowSummary(row)}
                      </span>
                    </label>
                  </li>
                ))}
              </ul>
            ))}
          {properties.length === 0 ? (
            <div className="mt-3" style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
              Define a custom property above to set values on repositories.
            </div>
          ) : (
            <div className="mt-3 flex flex-wrap items-center gap-2">
              <select
                aria-label="Property to set"
                value={propName}
                onChange={(e) => {
                  setPropName(e.target.value);
                  setValueInput("");
                }}
              >
                {properties.map((p) => (
                  <option key={p.property_name} value={p.property_name}>
                    {p.property_name}
                  </option>
                ))}
              </select>
              {selectedProp?.value_type === "single_select" ? (
                <select
                  aria-label="Property value"
                  value={valueInput}
                  onChange={(e) => setValueInput(e.target.value)}
                >
                  <option value="">unset</option>
                  {(selectedProp.allowed_values ?? []).map((v) => (
                    <option key={v} value={v}>
                      {v}
                    </option>
                  ))}
                </select>
              ) : selectedProp?.value_type === "true_false" ? (
                <select
                  aria-label="Property value"
                  value={valueInput}
                  onChange={(e) => setValueInput(e.target.value)}
                >
                  <option value="">unset</option>
                  <option value="true">true</option>
                  <option value="false">false</option>
                </select>
              ) : (
                <input
                  aria-label="Property value"
                  placeholder={
                    selectedProp?.value_type === "multi_select"
                      ? "values, comma-separated (empty unsets)"
                      : "value (empty unsets)"
                  }
                  value={valueInput}
                  onChange={(e) => setValueInput(e.target.value)}
                  className="min-w-0 flex-1"
                />
              )}
              <Button
                size="sm"
                variant="primary"
                disabled={
                  setMut.isPending || !propName || selected.length === 0 || selected.length > 30
                }
                onClick={() => {
                  setError(null);
                  setMut.mutate();
                }}
              >
                {valueInput.trim() === "" ? "Unset on selected" : "Set on selected"}
              </Button>
              <span style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem" }}>
                {selected.length} selected (max 30)
              </span>
            </div>
          )}
        </div>
      </Box>
    </div>
  );
}

// ─── Issue types ────────────────────────────────────────────────────────

const ISSUE_TYPE_COLORS = ["gray", "blue", "green", "yellow", "orange", "red", "pink", "purple"];

function IssueTypesPanel({ org }: { org: string }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [color, setColor] = useState("gray");
  const [description, setDescription] = useState("");

  const { data: issueTypes, isLoading, isError, error: loadErr } = useQuery({
    queryKey: ["org-issue-types", org],
    queryFn: () => fetchOrgIssueTypes(org),
  });

  const invalidate = () => {
    setError(null);
    qc.invalidateQueries({ queryKey: ["org-issue-types", org] });
  };
  const createMut = useMutation({
    mutationFn: () =>
      createOrgIssueType(org, {
        name: name.trim(),
        is_enabled: true,
        color,
        description: description || undefined,
      }),
    onSuccess: () => {
      invalidate();
      setName("");
      setDescription("");
    },
    onError: (err: Error) => setError(err.message),
  });
  const toggleMut = useMutation({
    mutationFn: (it: GithubIssueType) =>
      updateOrgIssueType(org, it.id, {
        name: it.name,
        is_enabled: !it.is_enabled,
        color: it.color ?? undefined,
        description: it.description ?? undefined,
      }),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const deleteMut = useMutation({
    mutationFn: (id: number) => deleteOrgIssueType(org, id),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });

  if (isLoading) return <Spinner label="loading issue types" />;
  if (isError || !issueTypes) {
    return <InlineError title="Failed to load issue types" detail={String(loadErr)} />;
  }

  return (
    <div className="flex flex-col gap-4">
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>New issue type</span>}>
        <div className="flex flex-wrap gap-2" style={{ padding: "0.75rem 1rem" }}>
          <input
            aria-label="Issue type name"
            placeholder="Name (e.g. Bug)"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          <select aria-label="Issue type color" value={color} onChange={(e) => setColor(e.target.value)}>
            {ISSUE_TYPE_COLORS.map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </select>
          <input
            aria-label="Issue type description"
            placeholder="Description (optional)"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            className="min-w-0 flex-1"
          />
          <Button
            variant="primary"
            size="sm"
            disabled={createMut.isPending || !name.trim()}
            onClick={() => {
              setError(null);
              createMut.mutate();
            }}
          >
            Create
          </Button>
        </div>
      </Box>
      {issueTypes.length === 0 ? (
        <EmptyBox>No issue types defined.</EmptyBox>
      ) : (
        <Box>
          {issueTypes.map((it, i) => (
            <div
              key={it.id}
              className="flex flex-wrap items-center gap-3"
              style={{
                padding: "0.7rem 1rem",
                borderBottom: i < issueTypes.length - 1 ? "1px solid var(--color-border)" : "none",
              }}
            >
              <span style={{ fontWeight: 600, fontSize: "0.9rem" }}>{it.name}</span>
              <span className="min-w-0 flex-1" style={{ color: "var(--color-fg-muted)", fontSize: "0.82rem" }}>
                {it.color ?? "no color"}
                {it.description && ` · ${it.description}`}
                {!it.is_enabled && " · disabled"}
              </span>
              <Button size="sm" variant="ghost" disabled={toggleMut.isPending} onClick={() => toggleMut.mutate(it)}>
                {it.is_enabled ? "disable" : "enable"}
              </Button>
              <Button
                size="sm"
                variant="danger"
                disabled={deleteMut.isPending}
                onClick={async () => {
                  if (await confirmAction(`Delete issue type ${it.name}?`)) deleteMut.mutate(it.id);
                }}
              >
                delete
              </Button>
            </div>
          ))}
        </Box>
      )}
    </div>
  );
}

function EmptyBox({ children }: { children: React.ReactNode }) {
  return (
    <Box>
      <div style={{ padding: "0.9rem 1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
        {children}
      </div>
    </Box>
  );
}
