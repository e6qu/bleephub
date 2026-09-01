import { useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { confirmAction } from "../components/confirmAction.js";
import { ghFetch, ghPostJSON, ghSend } from "../api.js";
import { sealSecret } from "../utils/sealedBox.js";
import {
  addUserEmails,
  blockUser,
  createUserGPGKey,
  createUserSSHKey,
  createUserSSHSigningKey,
  deleteUserEmails,
  deleteUserGPGKey,
  deleteUserSSHKey,
  deleteUserSSHSigningKey,
  createFineGrainedPAT,
  deleteFineGrainedPAT,
  fetchFineGrainedPATDashboard,
  fetchBlockedUsers,
  fetchUserEmails,
  fetchUserGPGKeys,
  fetchUserSSHKeys,
  fetchUserSSHSigningKeys,
  setUserEmailVisibility,
  reviewFineGrainedPATRequest,
  unblockUser,
  fetchAuthenticatedUser,
  fetchUserProfile,
  updateAuthenticatedUser,
  fetchSocialAccounts,
  addSocialAccounts,
  deleteSocialAccounts,
} from "../api.js";
import type {
  GithubBlockedUser,
  GithubClassicAuthorization,
  GithubGPGKey,
  GithubSSHKey,
  GithubSSHSigningKey,
  GithubUserEmail,
} from "../types.js";
import { useTheme, type Theme } from "@bleephub/ui-core/hooks";
import { PageTitle, Box, Button, ErrorBanner, FormLabel, Blankslate, Modal, DialogActions } from "../components/ui.js";
import { SettingsLayout, type SettingsNavSection } from "../components/SettingsLayout.js";
import { InteractionLimitsCard } from "../components/InteractionLimitsCard.js";
import { AuthorizedApplications } from "../components/AuthorizedApplications.js";
import { RelativeTime } from "../components/RelativeTime.js";
import { KeyIcon, LockIcon, GraphIcon } from "../components/octicons.js";

type AccountTab = "profile" | "account" | "appearance" | "notifications" | "tokens" | "ssh-keys" | "gpg-keys" | "signing-keys" | "emails" | "blocked" | "interaction-limits" | "authentication" | "applications" | "codespaces" | "billing";

/** Sidebar keys: every tab plus two pure-navigation entries. */
type AccountNavKey = AccountTab | "organizations" | "repositories";

// CopyTokenButton copies a one-time token to the clipboard, surfacing success
// and failure. navigator.clipboard.writeText rejects on a denied permission,
// an insecure context, or missing focus; leaving that promise unhandled becomes
// a console error (which fails the e2e console guard) and gives no feedback.
function CopyTokenButton({ value, label }: { value: string; label: string }) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setState("copied");
      window.setTimeout(() => setState("idle"), 1500);
    } catch {
      setState("failed");
    }
  };
  return (
    <Button size="sm" onClick={copy} style={{ marginTop: ".65rem" }}>
      {state === "copied" ? "Copied" : state === "failed" ? "Copy failed — select and copy manually" : label}
    </Button>
  );
}

const ACCOUNT_TABS: readonly AccountTab[] = [
  "profile", "account", "appearance", "notifications", "tokens", "ssh-keys", "gpg-keys",
  "signing-keys", "emails", "blocked", "interaction-limits", "authentication",
  "applications", "codespaces", "billing",
];

const ACCOUNT_NAV: SettingsNavSection<AccountNavKey>[] = [
  {
    items: [
      { key: "profile", label: "Public profile" },
      { key: "account", label: "Account" },
      { key: "appearance", label: "Appearance" },
      { key: "notifications", label: "Notifications" },
      { key: "emails", label: "Emails" },
    ],
  },
  {
    title: "Access",
    items: [
      { key: "authentication", label: "Password and authentication" },
      { key: "tokens", label: "Personal access tokens" },
      { key: "ssh-keys", label: "SSH keys" },
      { key: "gpg-keys", label: "GPG keys" },
      { key: "signing-keys", label: "Signing keys" },
      { key: "organizations", label: "Organizations" },
    ],
  },
  {
    title: "Integrations",
    items: [{ key: "applications", label: "Applications" }],
  },
  {
    title: "Code, planning, and automation",
    items: [
      { key: "repositories", label: "Repositories" },
      { key: "codespaces", label: "Codespaces" },
    ],
  },
  { title: "Billing", items: [{ key: "billing", label: "Billing and plans" }] },
  { title: "Moderation", items: [{ key: "blocked", label: "Blocked users" }, { key: "interaction-limits", label: "Interaction limits" }] },
];

export function AccountPage() {
  const navigate = useNavigate();
  // Active tab lives in ?tab= so back/refresh and deep links work.
  const [searchParams, setSearchParams] = useSearchParams();
  const requested = searchParams.get("tab");
  const tab: AccountTab = ACCOUNT_TABS.includes(requested as AccountTab)
    ? (requested as AccountTab)
    : "profile";
  const onSelect = (key: AccountNavKey) => {
    if (key === "organizations") {
      navigate("/ui/settings/organizations");
      return;
    }
    if (key === "repositories") {
      navigate("/ui/repos");
      return;
    }
    setSearchParams({ tab: key });
  };
  return (
    <div>
      <PageTitle title="Account" meta="Your public profile, appearance, keys, emails, and blocked users" />
      <SettingsLayout sections={ACCOUNT_NAV} active={tab} onSelect={onSelect}>
        {tab === "profile" && <ProfileSettingsTab />}
        {tab === "account" && <AccountAdminTab />}
        {tab === "appearance" && <AppearanceTab />}
        {tab === "notifications" && <NotificationsSettingsTab />}
        {tab === "authentication" && <AuthenticationTab />}
        {tab === "ssh-keys" && <SSHKeysTab />}
        {tab === "tokens" && (
          <div className="flex flex-col gap-6">
            <FineGrainedTokensTab />
            <ClassicTokensSection />
          </div>
        )}
        {tab === "gpg-keys" && <GPGKeysTab />}
        {tab === "signing-keys" && <SigningKeysTab />}
        {tab === "emails" && <EmailsTab />}
        {tab === "applications" && <AuthorizedApplications />}
        {tab === "codespaces" && <CodespacesSecretsTab />}
        {tab === "billing" && <BillingTab />}
        {tab === "blocked" && <BlockedUsersTab />}
        {tab === "interaction-limits" && (
          <InteractionLimitsCard
            path="/api/v3/user/interaction-limits"
            queryKey={["user-interaction-limit"]}
            scopeLabel="across all your public repositories"
          />
        )}
      </SettingsLayout>
    </div>
  );
}

const REPOSITORY_PERMISSIONS = [
  ["contents", "Contents"], ["issues", "Issues"], ["pull_requests", "Pull requests"],
  ["actions", "Actions"], ["checks", "Checks"], ["deployments", "Deployments"],
] as const;

// ─── Token expiration presets (7/30/60/90 days / custom / none, GitHub-style) ──────

type ExpiryPreset = "7" | "30" | "60" | "90" | "custom" | "none";

const EXPIRY_PRESETS: { value: ExpiryPreset; label: string }[] = [
  { value: "7", label: "7 days" },
  { value: "30", label: "30 days" },
  { value: "60", label: "60 days" },
  { value: "90", label: "90 days" },
  { value: "custom", label: "Custom…" },
  { value: "none", label: "No expiration" },
];

/** Today + n days as the YYYY-MM-DD value the date input expects. */
function dateInDays(days: number): string {
  const d = new Date();
  d.setDate(d.getDate() + days);
  return d.toISOString().slice(0, 10);
}

/** Expiration preset dropdown + optional custom date; "none" clears it and warns. */
function ExpirationPicker({
  idPrefix,
  preset,
  onPresetChange,
  expires,
  onExpiresChange,
}: {
  idPrefix: string;
  preset: ExpiryPreset;
  onPresetChange: (p: ExpiryPreset) => void;
  expires: string;
  onExpiresChange: (date: string) => void;
}) {
  return (
    <div>
      <FormLabel id={`${idPrefix}-expiry-preset`}>Expiration</FormLabel>
      <div className="flex flex-wrap items-center gap-2">
        <select
          id={`${idPrefix}-expiry-preset`}
          value={preset}
          onChange={(e) => {
            const next = e.target.value as ExpiryPreset;
            onPresetChange(next);
            if (next === "none") onExpiresChange("");
            else if (next !== "custom") onExpiresChange(dateInDays(Number(next)));
          }}
        >
          {EXPIRY_PRESETS.map((p) => (
            <option key={p.value} value={p.value}>{p.label}</option>
          ))}
        </select>
        {preset === "custom" && (
          <input
            aria-label="Custom expiration date"
            type="date"
            min={new Date().toISOString().slice(0, 10)}
            value={expires}
            onChange={(e) => onExpiresChange(e.target.value)}
          />
        )}
        {preset !== "custom" && preset !== "none" && expires && (
          <span style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
            The token will expire on {new Date(`${expires}T00:00:00`).toLocaleDateString()}
          </span>
        )}
      </div>
      {preset === "none" && (
        <p role="note" style={{ marginTop: "0.35rem", fontSize: "0.8rem", color: "var(--color-status-warn, var(--color-fg-muted))" }}>
          This token will never expire. Setting an expiration date is strongly recommended.
        </p>
      )}
    </div>
  );
}

function FineGrainedTokensTab() {
  const client = useQueryClient();
  const query = useQuery({ queryKey: ["fine-grained-pats"], queryFn: fetchFineGrainedPATDashboard });
  const [name, setName] = useState("");
  const [owner, setOwner] = useState("");
  const [selection, setSelection] = useState<"all" | "subset" | "none">("all");
  const [repositoryIDs, setRepositoryIDs] = useState<number[]>([]);
  const [expiryPreset, setExpiryPreset] = useState<ExpiryPreset>("30");
  const [expires, setExpires] = useState(() => dateInDays(30));
  const [reason, setReason] = useState("");
  const [permissions, setPermissions] = useState<Record<string, string>>({ contents: "read" });
  const [credential, setCredential] = useState<string | null>(null);

  const createMutation = useMutation({
    mutationFn: () => createFineGrainedPAT({
      name, resource_owner: owner || query.data!.resource_owners[0]?.login || "",
      repository_selection: selection, repository_ids: selection === "subset" ? repositoryIDs : [],
      permissions: { repository: permissions, organization: { members: "read" } },
      ...(expires ? { expires_at: new Date(`${expires}T23:59:59Z`).toISOString() } : {}),
      ...(reason.trim() ? { reason: reason.trim() } : {}),
    }),
    onSuccess: (created) => {
      setCredential(created.token); setName(""); setReason(""); setRepositoryIDs([]);
      client.invalidateQueries({ queryKey: ["fine-grained-pats"] });
    },
  });
  const deleteMutation = useMutation({ mutationFn: deleteFineGrainedPAT, onSuccess: () => client.invalidateQueries({ queryKey: ["fine-grained-pats"] }) });
  const reviewMutation = useMutation({
    mutationFn: ({ org, id, action }: { org: string; id: number; action: "approve" | "deny" }) => reviewFineGrainedPATRequest(org, id, action),
    onSuccess: () => client.invalidateQueries({ queryKey: ["fine-grained-pats"] }),
  });

  if (query.isLoading) return <Spinner label="loading personal access tokens" />;
  if (query.isError) return <InlineError title="Failed to load personal access tokens" detail={String(query.error)} />;
  const data = query.data!;
  const selectedOwner = owner || data.resource_owners[0]?.login || "";
  const repositories = data.repositories[selectedOwner] ?? [];
  const error = createMutation.error || deleteMutation.error || reviewMutation.error;

  return <div className="flex flex-col gap-4">
    <div style={{ padding: "1.15rem", border: "1px solid color-mix(in srgb, var(--color-brand-purple) 48%, var(--color-border))", borderRadius: 10, background: "linear-gradient(120deg, color-mix(in srgb, var(--color-brand-purple) 18%, var(--color-bg)), color-mix(in srgb, var(--color-brand-cyan) 14%, var(--color-bg-subtle)), color-mix(in srgb, var(--color-brand-pink) 12%, var(--color-bg)))", boxShadow: "var(--shadow-floating)" }}>
      <h2 style={{ fontSize: "1.15rem", fontWeight: 700 }}>Fine-grained personal access tokens</h2>
      <p style={{ color: "var(--color-fg-muted)", marginTop: ".25rem" }}>Limit every credential to one resource owner, selected repositories, explicit permissions, and an expiration date.</p>
    </div>
    {credential && <div role="alert" style={{ padding: "1rem", borderRadius: 8, border: "1px solid var(--color-status-ok)", background: "color-mix(in srgb, var(--color-status-ok) 13%, var(--color-bg))" }}>
      <b>Your new token</b><p style={{ color: "var(--color-fg-muted)", margin: ".25rem 0 .65rem" }}>Copy it now. For your security, it will not be shown again.</p>
      <code style={{ display: "block", overflowWrap: "anywhere", padding: ".7rem", borderRadius: 6, background: "var(--color-bg-subtle)", border: "1px solid var(--color-border)" }}>{credential}</code>
      <CopyTokenButton value={credential} label="Copy token" />
    </div>}
    {error && <ErrorBanner>{String(error)}</ErrorBanner>}
    <Box header={<span style={{ fontWeight: 650 }}>Generate new token</span>}>
      <div className="grid gap-4" style={{ padding: "1rem", gridTemplateColumns: "repeat(auto-fit, minmax(240px, 1fr))" }}>
        <div><FormLabel id="pat-name">Token name</FormLabel><input id="pat-name" className="w-full" maxLength={40} value={name} onChange={(e) => setName(e.target.value)} placeholder="Deployment automation" /></div>
        <div><FormLabel id="pat-owner">Resource owner</FormLabel><select id="pat-owner" className="w-full" value={selectedOwner} onChange={(e) => { setOwner(e.target.value); setRepositoryIDs([]); }}>{data.resource_owners.map((item) => <option key={item.login} value={item.login}>{item.login} · {item.type}</option>)}</select></div>
        <ExpirationPicker idPrefix="pat" preset={expiryPreset} onPresetChange={setExpiryPreset} expires={expires} onExpiresChange={setExpires} />
        <div><FormLabel id="pat-reason">Reason for organization access</FormLabel><input id="pat-reason" className="w-full" value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Used by the release workflow" /></div>
      </div>
      <div style={{ padding: "0 1rem 1rem" }}><FormLabel id="pat-access">Repository access</FormLabel><div id="pat-access" className="flex flex-wrap gap-3">{([['all','All repositories'],['subset','Only selected repositories'],['none','No repositories']] as const).map(([value,label]) => <label key={value} className="flex items-center gap-2"><input type="radio" name="pat-access" checked={selection === value} onChange={() => setSelection(value)} />{label}</label>)}</div></div>
      {selection === "subset" && <div style={{ padding: "0 1rem 1rem" }}><FormLabel id="pat-repositories">Selected repositories</FormLabel><div id="pat-repositories" className="grid gap-2" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(210px, 1fr))" }}>{repositories.map((repo) => <label key={repo.id} className="flex items-center gap-2"><input type="checkbox" checked={repositoryIDs.includes(repo.id)} onChange={(e) => setRepositoryIDs(e.target.checked ? [...repositoryIDs, repo.id] : repositoryIDs.filter((id) => id !== repo.id))} />{repo.name}{repo.private ? " · Private" : ""}</label>)}</div></div>}
      <div style={{ padding: "0 1rem 1rem" }}><FormLabel id="pat-permissions">Repository permissions</FormLabel><div id="pat-permissions" className="grid gap-2" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(210px, 1fr))" }}>{REPOSITORY_PERMISSIONS.map(([key, label]) => <label key={key} className="flex items-center justify-between gap-3"><span>{label}</span><select aria-label={`${label} permission`} value={permissions[key] ?? "none"} onChange={(e) => { const next = { ...permissions }; if (e.target.value === "none") delete next[key]; else next[key] = e.target.value; setPermissions(next); }}><option value="none">No access</option><option value="read">Read</option><option value="write">Read and write</option></select></label>)}</div></div>
      <div className="flex justify-end" style={{ padding: "0 1rem 1rem" }}><Button variant="primary" disabled={!name.trim() || (selection === "subset" && repositoryIDs.length === 0) || createMutation.isPending} onClick={() => createMutation.mutate()}>Generate token</Button></div>
    </Box>
    {data.pending_requests.length > 0 && (
      <Box header={<span style={{ fontWeight: 650 }}>Organization approval requests</span>}>
        <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
          {data.pending_requests.map((request) => (
            <li
              key={`${request.organization}-${request.id}`}
              className="flex flex-wrap items-center justify-between gap-3"
              style={{ padding: ".9rem 1rem", borderBottom: "1px solid var(--color-border)" }}
            >
              <div>
                <b>{request.token_name}</b>
                <div style={{ color: "var(--color-fg-muted)", fontSize: ".82rem" }}>{request.owner.login} requests {request.organization} · {request.repository_selection} repositories{request.reason ? ` · ${request.reason}` : ""}</div>
              </div>
              <div className="flex gap-2">
                <Button size="sm" onClick={() => reviewMutation.mutate({ org: request.organization, id: request.id, action: "deny" })}>Deny</Button>
                <Button size="sm" variant="primary" onClick={() => reviewMutation.mutate({ org: request.organization, id: request.id, action: "approve" })}>Approve</Button>
              </div>
            </li>
          ))}
        </ul>
      </Box>
    )}
    <Box header={<span style={{ fontWeight: 650 }}>Your fine-grained tokens</span>}>
      {data.tokens.length === 0 ? (
        <div style={{ padding: "1rem", color: "var(--color-fg-muted)" }}>You have not generated any fine-grained tokens.</div>
      ) : (
        <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
          {data.tokens.map((token) => (
            <li
              key={token.id}
              className="flex flex-wrap items-center justify-between gap-3"
              style={{ padding: ".9rem 1rem", borderBottom: "1px solid var(--color-border)" }}
            >
              <div>
                <div className="flex items-center gap-2">
                  <b>{token.name}</b>
                  <span
                    style={{
                      padding: ".12rem .45rem",
                      borderRadius: 999,
                      fontSize: ".72rem",
                      fontWeight: 650,
                      color: token.status === "active" ? "var(--color-status-ok)" : "var(--color-status-warn)",
                      background: token.status === "active" ? "var(--color-status-ok-soft)" : "var(--color-status-warn-soft)",
                    }}
                  >
                    {token.status}
                  </span>
                </div>
                <div style={{ color: "var(--color-fg-muted)", fontSize: ".82rem" }}>{token.resource_owner} · {token.repository_selection} repositories · {token.expires_at ? `expires ${new Date(token.expires_at).toLocaleDateString()}` : "no expiration"}</div>
              </div>
              <Button
                size="sm"
                variant="danger"
                disabled={deleteMutation.isPending}
                onClick={async () => {
                  if (await confirmAction(`Delete ${token.name}?`)) deleteMutation.mutate(token.id);
                }}
              >
                Delete
              </Button>
            </li>
          ))}
        </ul>
      )}
    </Box>
  </div>;
}

// ─── Tokens (classic) — the legacy /authorizations API ─────────────────────────────

/** Classic OAuth scopes the server's classicScopeGrants map evaluates. */
const CLASSIC_SCOPES: { name: string; hint: string }[] = [
  { name: "repo", hint: "Full control of private repositories" },
  { name: "public_repo", hint: "Access public repositories" },
  { name: "workflow", hint: "Update GitHub Action workflows" },
  { name: "admin:org", hint: "Full control of orgs and teams" },
  { name: "write:org", hint: "Read and write org and team membership" },
  { name: "read:org", hint: "Read org and team membership" },
  { name: "admin:org_hook", hint: "Full control of organization hooks" },
  { name: "admin:repo_hook", hint: "Full control of repository hooks" },
  { name: "delete_repo", hint: "Delete repositories" },
  { name: "write:discussion", hint: "Read and write team discussions" },
  { name: "read:discussion", hint: "Read team discussions" },
  { name: "project", hint: "Full control of projects" },
  { name: "read:project", hint: "Read access of projects" },
  { name: "security_events", hint: "Read and write security events" },
  { name: "codespace", hint: "Full control of codespaces" },
  { name: "repo_deployment", hint: "Access deployment status" },
  { name: "user", hint: "Update all user data" },
  { name: "read:user", hint: "Read all user profile data" },
];

const fetchClassicAuthorizations = () =>
  ghFetch<GithubClassicAuthorization[]>("/api/v3/authorizations");

function ClassicTokensSection() {
  const client = useQueryClient();
  const query = useQuery({ queryKey: ["classic-pats"], queryFn: fetchClassicAuthorizations });
  const [note, setNote] = useState("");
  const [scopes, setScopes] = useState<string[]>([]);
  const [expiryPreset, setExpiryPreset] = useState<ExpiryPreset>("30");
  const [expires, setExpires] = useState(() => dateInDays(30));
  const [credential, setCredential] = useState<string | null>(null);

  const createMutation = useMutation({
    // /api/v3/authorizations has no expiry field, so create via the /ui-data
    // endpoint, which accepts expires_at (RFC 3339, or null).
    mutationFn: () =>
      ghPostJSON<GithubClassicAuthorization>("/ui-data/user/tokens/classic", {
        scopes,
        note: note.trim(),
        expires_at: expires ? new Date(`${expires}T23:59:59Z`).toISOString() : null,
      }),
    onSuccess: (created) => {
      setCredential(created.token);
      setNote("");
      setScopes([]);
      client.invalidateQueries({ queryKey: ["classic-pats"] });
    },
  });
  const deleteMutation = useMutation({
    mutationFn: (id: number) => ghSend("DELETE", `/api/v3/authorizations/${id}`),
    onSuccess: () => client.invalidateQueries({ queryKey: ["classic-pats"] }),
  });

  if (query.isLoading) return <Spinner label="loading classic tokens" />;
  if (query.isError) return <InlineError title="Failed to load classic tokens" detail={String(query.error)} />;
  const tokens = query.data ?? [];
  const error = createMutation.error || deleteMutation.error;

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h2 style={{ fontSize: "1.05rem", fontWeight: 700 }}>Tokens (classic)</h2>
        <p style={{ color: "var(--color-fg-muted)", marginTop: ".25rem", fontSize: "0.85rem" }}>
          Classic personal access tokens grant broad, scope-based access. Prefer a fine-grained
          token when you can.
        </p>
      </div>
      {credential && (
        <div role="alert" style={{ padding: "1rem", borderRadius: 8, border: "1px solid var(--color-status-ok)", background: "color-mix(in srgb, var(--color-status-ok) 13%, var(--color-bg))" }}>
          <b>Your new classic token</b>
          <p style={{ color: "var(--color-fg-muted)", margin: ".25rem 0 .65rem" }}>Copy it now. For your security, it will not be shown again.</p>
          <code style={{ display: "block", overflowWrap: "anywhere", padding: ".7rem", borderRadius: 6, background: "var(--color-bg-subtle)", border: "1px solid var(--color-border)" }}>{credential}</code>
          <CopyTokenButton value={credential} label="Copy classic token" />
        </div>
      )}
      {error && <ErrorBanner>{String(error)}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 650 }}>New classic token</span>}>
        <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
          <div>
            <FormLabel id="classic-pat-note">Note</FormLabel>
            <input id="classic-pat-note" className="w-full" value={note} onChange={(e) => setNote(e.target.value)} placeholder="What's this token for?" />
          </div>
          <ExpirationPicker idPrefix="classic-pat" preset={expiryPreset} onPresetChange={setExpiryPreset} expires={expires} onExpiresChange={setExpires} />
          <div>
            <FormLabel id="classic-pat-scopes">Select scopes</FormLabel>
            <div id="classic-pat-scopes" className="grid gap-2" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(230px, 1fr))" }}>
              {CLASSIC_SCOPES.map((s) => (
                <label key={s.name} className="flex items-start gap-2" style={{ fontSize: "0.85rem" }}>
                  <input
                    type="checkbox"
                    checked={scopes.includes(s.name)}
                    onChange={(e) => setScopes(e.target.checked ? [...scopes, s.name] : scopes.filter((v) => v !== s.name))}
                  />
                  <span>
                    <span className="font-mono" style={{ fontWeight: 600 }}>{s.name}</span>
                    <span style={{ display: "block", fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>{s.hint}</span>
                  </span>
                </label>
              ))}
            </div>
          </div>
          <div className="flex justify-end">
            <Button variant="primary" disabled={!note.trim() || createMutation.isPending} onClick={() => createMutation.mutate()}>
              Generate classic token
            </Button>
          </div>
        </div>
      </Box>
      <Box header={<span style={{ fontWeight: 650 }}>Your classic tokens</span>}>
        {tokens.length === 0 ? (
          <div style={{ padding: "1rem", color: "var(--color-fg-muted)" }}>You have not generated any classic tokens.</div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {tokens.map((t) => (
              <li
                key={t.id}
                className="flex flex-wrap items-center justify-between gap-3"
                style={{ padding: ".9rem 1rem", borderBottom: "1px solid var(--color-border)" }}
              >
                <div>
                  <b>{t.note || t.app.name || `Authorization #${t.id}`}</b>
                  <div style={{ color: "var(--color-fg-muted)", fontSize: ".82rem" }}>
                    {t.scopes.length > 0 ? t.scopes.join(", ") : "no scopes"}
                    {t.token_last_eight ? <> · <span className="font-mono">…{t.token_last_eight}</span></> : null}
                    {" · created "}<RelativeTime iso={t.created_at} />
                    {t.expires_at ? ` · expires ${new Date(t.expires_at).toLocaleDateString()}` : " · no expiration"}
                  </div>
                </div>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={deleteMutation.isPending}
                  onClick={async () => {
                    if (await confirmAction(`Delete ${t.note || `authorization #${t.id}`}?`)) deleteMutation.mutate(t.id);
                  }}
                >
                  Delete
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Box>
    </div>
  );
}

/** Shared add-key form + key list for the three key kinds. */
function KeyManager<T extends { id: number }>({
  kind,
  queryKey,
  list,
  create,
  remove,
  titleOptional,
  renderKey,
}: {
  kind: string;
  queryKey: string;
  list: () => Promise<T[]>;
  create: (title: string, key: string) => Promise<T>;
  remove: (id: number) => Promise<void>;
  titleOptional?: boolean;
  renderKey: (k: T) => React.ReactNode;
}) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [title, setTitle] = useState("");
  const [key, setKey] = useState("");

  const query = useQuery({ queryKey: [queryKey], queryFn: list });

  const addMut = useMutation({
    mutationFn: () => create(title.trim(), key.trim()),
    onSuccess: () => {
      setError(null);
      setTitle("");
      setKey("");
      queryClient.invalidateQueries({ queryKey: [queryKey] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const deleteMut = useMutation({
    mutationFn: (id: number) => remove(id),
    onSuccess: () => {
      setError(null);
      queryClient.invalidateQueries({ queryKey: [queryKey] });
    },
    onError: (err: Error) => setError(err.message),
  });

  if (query.isLoading) return <Spinner label={`loading ${kind}s`} />;
  if (query.isError)
    return <InlineError title={`Failed to load ${kind}s`} detail={String(query.error)} />;

  const keys = query.data ?? [];

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>Add {kind}</span>}>
        <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
          <FormLabel id={`${queryKey}-title`}>Title{titleOptional ? " (optional)" : ""}</FormLabel>
          <input
            id={`${queryKey}-title`}
            type="text"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            className="w-full"
          />
          <FormLabel id={`${queryKey}-key`}>Key</FormLabel>
          <textarea
            id={`${queryKey}-key`}
            value={key}
            onChange={(e) => setKey(e.target.value)}
            rows={4}
            className="w-full"
            style={{ fontFamily: "var(--font-mono)", fontSize: "0.8rem" }}
          />
          <div className="flex justify-end">
            <Button
              variant="primary"
              onClick={() => {
                setError(null);
                addMut.mutate();
              }}
              disabled={addMut.isPending || !key.trim() || (!titleOptional && !title.trim())}
            >
              Add {kind}
            </Button>
          </div>
        </div>
      </Box>
      <Box header={<span style={{ fontWeight: 600 }}>{kind.charAt(0).toUpperCase() + kind.slice(1)}s</span>}>
        {keys.length === 0 ? (
          <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
            No {kind}s.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {keys.map((k) => (
              <li
                key={k.id}
                className="flex items-center justify-between gap-4"
                style={{ padding: "0.6rem 1rem", borderBottom: "1px solid var(--color-border)" }}
              >
                <div className="flex min-w-0 items-center gap-2">
                  <KeyIcon size={16} style={{ color: "var(--color-fg-muted)", flexShrink: 0 }} />
                  <div style={{ minWidth: 0 }}>{renderKey(k)}</div>
                </div>
                <Button
                  size="sm"
                  variant="danger"
                  onClick={async () => {
                    if (await confirmAction(`Delete this ${kind}?`)) deleteMut.mutate(k.id);
                  }}
                  disabled={deleteMut.isPending}
                >
                  delete
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Box>
    </div>
  );
}

const truncatedMono: React.CSSProperties = {
  color: "var(--color-fg-muted)",
  fontSize: "0.78rem",
  fontFamily: "var(--font-mono)",
  overflow: "hidden",
  textOverflow: "ellipsis",
  whiteSpace: "nowrap",
};

/**
 * SHA256 fingerprint of an OpenSSH public key ("SHA256:<unpadded base64>", the
 * form ssh-keygen and github.com print). Null while computing or when unparsable.
 */
async function sshKeyFingerprint(keyText: string): Promise<string | null> {
  try {
    const parts = keyText.trim().split(/\s+/);
    const blob = parts.length > 1 ? parts[1]! : parts[0]!;
    const bin = atob(blob);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
    let digestBin = "";
    for (const b of digest) digestBin += String.fromCharCode(b);
    return `SHA256:${btoa(digestBin).replace(/=+$/, "")}`;
  } catch {
    return null;
  }
}

/** Key type + SHA256 fingerprint in place of the raw key material. */
function SSHKeyFingerprint({ keyText }: { keyText: string }) {
  const [fingerprint, setFingerprint] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    void sshKeyFingerprint(keyText).then((fp) => {
      if (alive) setFingerprint(fp);
    });
    return () => {
      alive = false;
    };
  }, [keyText]);
  const keyType = keyText.trim().split(/\s+/)[0] ?? "";
  return <div style={truncatedMono}>{fingerprint ? `${keyType} ${fingerprint}` : keyType}</div>;
}

function SSHKeysTab() {
  return (
    <KeyManager<GithubSSHKey>
      kind="SSH key"
      queryKey="user-ssh-keys"
      list={fetchUserSSHKeys}
      create={createUserSSHKey}
      remove={deleteUserSSHKey}
      titleOptional
      renderKey={(k) => (
        <>
          <div style={{ fontWeight: 500 }}>{k.title || `Key #${k.id}`}</div>
          <SSHKeyFingerprint keyText={k.key} />
          <div style={{ color: "var(--color-fg-muted)", fontSize: "0.72rem" }}>
            {k.verified ? "verified" : "unverified"} · added <RelativeTime iso={k.created_at} />
          </div>
        </>
      )}
    />
  );
}

function GPGKeysTab() {
  return (
    <KeyManager<GithubGPGKey>
      kind="GPG key"
      queryKey="user-gpg-keys"
      list={fetchUserGPGKeys}
      create={(name, armored) => createUserGPGKey(armored, name || undefined)}
      remove={deleteUserGPGKey}
      titleOptional
      renderKey={(k) => (
        <>
          <div style={{ fontWeight: 500 }}>{k.name || k.key_id || `Key #${k.id}`}</div>
          <div style={truncatedMono}>{k.public_key}</div>
          <div style={{ color: "var(--color-fg-muted)", fontSize: "0.72rem" }}>
            {[
              k.can_sign && "sign",
              k.can_encrypt_commits && "encrypt",
              k.can_certify && "certify",
            ]
              .filter(Boolean)
              .join(" · ")}{" "}
            · added <RelativeTime iso={k.created_at} />
          </div>
        </>
      )}
    />
  );
}

function SigningKeysTab() {
  return (
    <KeyManager<GithubSSHSigningKey>
      kind="SSH signing key"
      queryKey="user-ssh-signing-keys"
      list={fetchUserSSHSigningKeys}
      create={createUserSSHSigningKey}
      remove={deleteUserSSHSigningKey}
      titleOptional
      renderKey={(k) => (
        <>
          <div style={{ fontWeight: 500 }}>{k.title || `Key #${k.id}`}</div>
          <SSHKeyFingerprint keyText={k.key} />
          <div style={{ color: "var(--color-fg-muted)", fontSize: "0.72rem" }}>
            added <RelativeTime iso={k.created_at} />
          </div>
        </>
      )}
    />
  );
}

function EmailsTab() {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [newEmail, setNewEmail] = useState("");

  const query = useQuery({ queryKey: ["user-emails"], queryFn: fetchUserEmails });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["user-emails"] });
  const addMut = useMutation({
    mutationFn: () => addUserEmails([newEmail.trim()]),
    onSuccess: () => {
      setError(null);
      setNewEmail("");
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });
  const deleteMut = useMutation({
    mutationFn: (email: string) => deleteUserEmails([email]),
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });
  const visibilityMut = useMutation({
    mutationFn: (visibility: "public" | "private") => setUserEmailVisibility(visibility),
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });
  // Re-pointing primary is web-only on github.com too; via /ui-data.
  const primaryMut = useMutation({
    mutationFn: (email: string) => ghSend("PUT", "/ui-data/user/emails/primary", { email }),
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });

  if (query.isLoading) return <Spinner label="loading emails" />;
  if (query.isError)
    return <InlineError title="Failed to load emails" detail={String(query.error)} />;

  const emails = query.data ?? [];
  const primary = emails.find((e) => e.primary);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>Add email address</span>}>
        <div style={{ padding: "1rem", display: "flex", gap: "0.75rem", alignItems: "center" }}>
          <input
            type="email"
            aria-label="New email address"
            value={newEmail}
            onChange={(e) => setNewEmail(e.target.value)}
            placeholder="you@example.com"
            className="flex-1"
            style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem" }}
          />
          <Button
            variant="primary"
            onClick={() => {
              setError(null);
              addMut.mutate();
            }}
            disabled={addMut.isPending || !newEmail.trim()}
          >
            Add
          </Button>
        </div>
      </Box>
      <Box header={<span style={{ fontWeight: 600 }}>Email addresses</span>}>
        {emails.length === 0 ? (
          <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
            No email addresses.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {emails.map((e: GithubUserEmail) => (
              <li
                key={e.email}
                className="flex items-center justify-between gap-4"
                style={{ padding: "0.6rem 1rem", borderBottom: "1px solid var(--color-border)" }}
              >
                <div>
                  <span style={{ fontWeight: 500 }}>{e.email}</span>
                  <span style={{ marginLeft: "0.5rem", fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
                    {[
                      e.primary && "primary",
                      e.verified ? "verified" : "unverified",
                      e.visibility ? `visibility: ${e.visibility}` : "visibility unset",
                    ]
                      .filter(Boolean)
                      .join(" · ")}
                  </span>
                </div>
                {!e.primary && (
                  <div className="flex items-center gap-2">
                    {e.verified && (
                      <Button
                        size="sm"
                        variant="secondary"
                        aria-label={`Set ${e.email} as primary`}
                        onClick={() => primaryMut.mutate(e.email)}
                        disabled={primaryMut.isPending}
                      >
                        Set as primary
                      </Button>
                    )}
                    <Button
                      size="sm"
                      variant="danger"
                      onClick={async () => {
                        if (await confirmAction(`Remove ${e.email}?`)) deleteMut.mutate(e.email);
                      }}
                      disabled={deleteMut.isPending}
                    >
                      remove
                    </Button>
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </Box>
      {primary && (
        <Box header={<span style={{ fontWeight: 600 }}>Primary email visibility</span>}>
          <div style={{ padding: "1rem", display: "flex", alignItems: "center", gap: "1rem" }}>
            <span style={{ fontSize: "0.85rem" }}>
              {primary.email} is {primary.visibility ?? "unset"}
            </span>
            <Button
              size="sm"
              variant="secondary"
              onClick={() =>
                visibilityMut.mutate(primary.visibility === "public" ? "private" : "public")
              }
              disabled={visibilityMut.isPending}
            >
              Make {primary.visibility === "public" ? "private" : "public"}
            </Button>
          </div>
        </Box>
      )}
    </div>
  );
}

function BlockedUsersTab() {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [username, setUsername] = useState("");

  const query = useQuery({ queryKey: ["user-blocks"], queryFn: fetchBlockedUsers });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["user-blocks"] });
  const blockMut = useMutation({
    mutationFn: () => blockUser(username.trim()),
    onSuccess: () => {
      setError(null);
      setUsername("");
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });
  const unblockMut = useMutation({
    mutationFn: (login: string) => unblockUser(login),
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });

  if (query.isLoading) return <Spinner label="loading blocked users" />;
  if (query.isError)
    return <InlineError title="Failed to load blocked users" detail={String(query.error)} />;

  const blocked = query.data ?? [];

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>Block a user</span>}>
        <div style={{ padding: "1rem", display: "flex", gap: "0.75rem", alignItems: "center" }}>
          <input
            type="text"
            aria-label="Username to block"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            placeholder="username"
            className="flex-1"
            style={{ fontSize: "0.9rem", padding: "0.4rem 0.5rem" }}
          />
          <Button
            variant="danger"
            onClick={() => {
              setError(null);
              blockMut.mutate();
            }}
            disabled={blockMut.isPending || !username.trim()}
          >
            Block
          </Button>
        </div>
      </Box>
      <Box header={<span style={{ fontWeight: 600 }}>Blocked users</span>}>
        {blocked.length === 0 ? (
          <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
            No blocked users.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {blocked.map((b: GithubBlockedUser) => (
              <li
                key={b.login}
                className="flex items-center justify-between gap-4"
                style={{ padding: "0.6rem 1rem", borderBottom: "1px solid var(--color-border)" }}
              >
                <span style={{ fontWeight: 500 }}>{b.login}</span>
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => unblockMut.mutate(b.login)}
                  disabled={unblockMut.isPending}
                >
                  unblock
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Box>
    </div>
  );
}

// ─── Codespaces secrets (sealed-box encrypted user secrets for Codespaces) ─────────

interface CodespaceSecret {
  name: string;
  created_at: string;
  updated_at: string;
  visibility?: string;
}

const enc = encodeURIComponent;

const fetchCodespaceSecrets = () =>
  ghFetch<{ total_count: number; secrets: CodespaceSecret[] }>(
    "/api/v3/user/codespaces/secrets",
  );

function CodespacesSecretsTab() {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [value, setValue] = useState("");

  const query = useQuery({
    queryKey: ["user-codespaces-secrets"],
    queryFn: fetchCodespaceSecrets,
  });
  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ["user-codespaces-secrets"] });

  const addMut = useMutation({
    // Sealed-box encrypt in the browser; only ciphertext + key_id leave the client.
    mutationFn: async () => {
      const secretName = name.trim();
      if (!secretName) throw new Error("Name is required");
      if (!value) throw new Error("Value is required");
      const pk = await ghFetch<{ key_id: string; key: string }>(
        "/api/v3/user/codespaces/secrets/public-key",
      );
      const encrypted = await sealSecret(value, pk.key);
      await ghSend("PUT", `/api/v3/user/codespaces/secrets/${enc(secretName)}`, {
        encrypted_value: encrypted,
        key_id: pk.key_id,
      });
    },
    onSuccess: () => {
      setError(null);
      setName("");
      setValue("");
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });

  const deleteMut = useMutation({
    mutationFn: (secretName: string) =>
      ghSend("DELETE", `/api/v3/user/codespaces/secrets/${enc(secretName)}`),
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });

  if (query.isLoading) return <Spinner label="loading Codespaces secrets" />;
  if (query.isError)
    return <InlineError title="Failed to load Codespaces secrets" detail={String(query.error)} />;

  const secrets = query.data?.secrets ?? [];

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>New secret</span>}>
        <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
          <FormLabel id="codespaces-secret-name">Name</FormLabel>
          <input
            id="codespaces-secret-name"
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="YOUR_SECRET_NAME"
            className="w-full font-mono"
          />
          <FormLabel id="codespaces-secret-value">Value</FormLabel>
          <textarea
            id="codespaces-secret-value"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            rows={4}
            className="w-full font-mono"
            style={{ resize: "vertical" }}
          />
          <div className="flex justify-end">
            <Button
              variant="primary"
              onClick={() => {
                setError(null);
                addMut.mutate();
              }}
              disabled={addMut.isPending || !name.trim() || !value}
            >
              {addMut.isPending ? "Encrypting…" : "Add secret"}
            </Button>
          </div>
        </div>
      </Box>
      <Box header={<span style={{ fontWeight: 600 }}>Codespaces secrets</span>}>
        {secrets.length === 0 ? (
          <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
            No Codespaces secrets.
          </div>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {secrets.map((s) => (
              <li
                key={s.name}
                className="flex items-center justify-between gap-4"
                style={{ padding: "0.6rem 1rem", borderBottom: "1px solid var(--color-border)" }}
              >
                <div className="flex min-w-0 items-center gap-2">
                  <LockIcon size={16} style={{ color: "var(--color-fg-muted)", flexShrink: 0 }} />
                  <div style={{ minWidth: 0 }}>
                    <div className="font-mono" style={{ fontWeight: 500 }}>{s.name}</div>
                    <div style={{ color: "var(--color-fg-muted)", fontSize: "0.72rem" }}>
                      updated <RelativeTime iso={s.updated_at} />
                    </div>
                  </div>
                </div>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={deleteMut.isPending}
                  onClick={async () => {
                    if (await confirmAction(`Delete secret ${s.name}?`)) deleteMut.mutate(s.name);
                  }}
                >
                  delete
                </Button>
              </li>
            ))}
          </ul>
        )}
      </Box>
    </div>
  );
}

// ─── Billing and plans (read-only usage reports, github.com/settings/billing) ──────

interface BillingSummaryItem {
  product: string;
  sku: string;
  unitType: string;
  pricePerUnit: number;
  grossQuantity: number;
  grossAmount: number;
  discountAmount: number;
  netQuantity: number;
  netAmount: number;
}
interface BillingSummaryReport {
  timePeriod: { year: number; month?: number; day?: number };
  user: string;
  usageItems: BillingSummaryItem[];
}
interface BillingModelReport {
  timePeriod: { year: number; month?: number; day?: number };
  user: string;
  usageItems: Record<string, unknown>[];
}

// Billing amounts are decimal USD, not integer cents.
const usd = (n: number, maxFrac = 2) =>
  new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: Math.max(2, maxFrac),
  }).format(n);

function billingPeriodLabel(p: { year: number; month?: number; day?: number }): string {
  if (!p?.year) return "";
  const month = p.month ? String(p.month).padStart(2, "0") : null;
  const day = p.day ? String(p.day).padStart(2, "0") : null;
  return [p.year, month, day].filter(Boolean).join("-");
}

function BillingTab() {
  const viewer = useQuery({ queryKey: ["viewer"], queryFn: fetchAuthenticatedUser });
  const login = viewer.data?.login;

  const summary = useQuery({
    queryKey: ["billing-usage-summary", login],
    queryFn: () =>
      ghFetch<BillingSummaryReport>(`/api/v3/users/${enc(login as string)}/settings/billing/usage/summary`),
    enabled: !!login,
  });
  const aiCredit = useQuery({
    queryKey: ["billing-ai-credit", login],
    queryFn: () =>
      ghFetch<BillingModelReport>(`/api/v3/users/${enc(login as string)}/settings/billing/ai_credit/usage`),
    enabled: !!login,
  });
  const premium = useQuery({
    queryKey: ["billing-premium-request", login],
    queryFn: () =>
      ghFetch<BillingModelReport>(`/api/v3/users/${enc(login as string)}/settings/billing/premium_request/usage`),
    enabled: !!login,
  });

  if (viewer.isError)
    return <InlineError title="Failed to load your account" detail={String(viewer.error)} />;
  if (viewer.isLoading || summary.isLoading) return <Spinner label="loading billing usage" />;
  if (summary.isError)
    return <InlineError title="Failed to load billing usage" detail={String(summary.error)} />;

  const report = summary.data!;
  const items = report.usageItems ?? [];
  const netTotal = items.reduce((sum, it) => sum + (it.netAmount ?? 0), 0);
  const grossTotal = items.reduce((sum, it) => sum + (it.grossAmount ?? 0), 0);
  const discountTotal = items.reduce((sum, it) => sum + (it.discountAmount ?? 0), 0);
  const period = billingPeriodLabel(report.timePeriod);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      <div
        style={{
          padding: "1.15rem",
          border: "1px solid var(--color-border)",
          borderRadius: 10,
          background: "var(--color-bg-subtle)",
        }}
      >
        <h2 style={{ fontSize: "1.15rem", fontWeight: 700 }}>Billing and plans</h2>
        <p style={{ color: "var(--color-fg-muted)", marginTop: ".25rem" }}>
          A read-only summary of your metered usage{period ? ` for ${period}` : ""}.
        </p>
      </div>

      <Box header={<span style={{ fontWeight: 650 }}>Usage this period</span>}>
        {items.length === 0 ? (
          <div style={{ padding: "1rem" }}>
            <Blankslate icon={<GraphIcon size={26} />} title="No usage to report">
              You have no metered usage for this period.
            </Blankslate>
          </div>
        ) : (
          <div style={{ overflowX: "auto" }}>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: "0.85rem" }}>
              <thead>
                <tr style={{ textAlign: "left", color: "var(--color-fg-muted)" }}>
                  <th style={{ padding: "0.55rem 1rem", fontWeight: 600 }}>Product</th>
                  <th style={{ padding: "0.55rem 1rem", fontWeight: 600 }}>SKU</th>
                  <th style={{ padding: "0.55rem 1rem", fontWeight: 600, textAlign: "right" }}>Quantity</th>
                  <th style={{ padding: "0.55rem 1rem", fontWeight: 600, textAlign: "right" }}>Unit price</th>
                  <th style={{ padding: "0.55rem 1rem", fontWeight: 600, textAlign: "right" }}>Amount</th>
                </tr>
              </thead>
              <tbody>
                {items.map((it) => (
                  <tr key={`${it.product}/${it.sku}`} style={{ borderTop: "1px solid var(--color-border)" }}>
                    <td style={{ padding: "0.55rem 1rem", fontWeight: 500 }}>{it.product}</td>
                    <td style={{ padding: "0.55rem 1rem", color: "var(--color-fg-muted)" }}>{it.sku}</td>
                    <td style={{ padding: "0.55rem 1rem", textAlign: "right" }}>
                      {it.netQuantity}
                      {it.unitType ? <span style={{ color: "var(--color-fg-muted)" }}> {it.unitType}</span> : null}
                    </td>
                    <td style={{ padding: "0.55rem 1rem", textAlign: "right" }}>{usd(it.pricePerUnit, 4)}</td>
                    <td style={{ padding: "0.55rem 1rem", textAlign: "right", fontWeight: 500 }}>{usd(it.netAmount)}</td>
                  </tr>
                ))}
              </tbody>
              <tfoot>
                <tr style={{ borderTop: "2px solid var(--color-border)" }}>
                  <td colSpan={4} style={{ padding: "0.55rem 1rem", textAlign: "right", color: "var(--color-fg-muted)" }}>
                    Gross {usd(grossTotal)}
                    {discountTotal ? ` · Discount ${usd(-discountTotal)}` : ""} · Total
                  </td>
                  <td style={{ padding: "0.55rem 1rem", textAlign: "right", fontWeight: 700 }}>{usd(netTotal)}</td>
                </tr>
              </tfoot>
            </table>
          </div>
        )}
      </Box>

      <BillingModelPanel title="AI credit usage" query={aiCredit} emptyLabel="No AI credit usage" />
      <BillingModelPanel title="Premium request usage" query={premium} emptyLabel="No premium request usage" />
    </div>
  );
}

function BillingModelPanel({
  title,
  query,
  emptyLabel,
}: {
  title: string;
  query: { isLoading: boolean; isError: boolean; error: unknown; data: BillingModelReport | undefined };
  emptyLabel: string;
}) {
  return (
    <Box header={<span style={{ fontWeight: 650 }}>{title}</span>}>
      <div style={{ padding: "1rem" }}>
        {query.isLoading ? (
          <Spinner label={`loading ${title.toLowerCase()}`} />
        ) : query.isError ? (
          <InlineError title={`Failed to load ${title.toLowerCase()}`} detail={String(query.error)} />
        ) : (query.data?.usageItems?.length ?? 0) === 0 ? (
          <Blankslate icon={<GraphIcon size={24} />} title={emptyLabel}>
            No usage for this period.
          </Blankslate>
        ) : (
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {query.data!.usageItems.map((it, i) => (
              <li key={i} style={{ padding: "0.4rem 0", fontSize: "0.85rem", fontFamily: "var(--font-mono)" }}>
                {JSON.stringify(it)}
              </li>
            ))}
          </ul>
        )}
      </div>
    </Box>
  );
}

// ─── Public profile (edit name/bio/company/location/blog/twitter via PATCH /user) ──

function ProfileSettingsTab() {
  const qc = useQueryClient();
  const viewer = useQuery({ queryKey: ["viewer"], queryFn: fetchAuthenticatedUser });
  const login = viewer.data?.login;
  const profile = useQuery({
    queryKey: ["user-profile", login],
    queryFn: () => fetchUserProfile(login as string),
    enabled: !!login,
  });

  const [form, setForm] = useState<{ name: string; bio: string; company: string; location: string; blog: string; twitter_username: string } | null>(null);
  const current = form ?? {
    name: profile.data?.name ?? "",
    bio: profile.data?.bio ?? "",
    company: profile.data?.company ?? "",
    location: profile.data?.location ?? "",
    blog: profile.data?.blog ?? "",
    twitter_username: profile.data?.twitter_username ?? "",
  };
  const set = (k: keyof typeof current, v: string) => setForm({ ...current, [k]: v });

  const saveMut = useMutation({
    mutationFn: () => updateAuthenticatedUser(current),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["user-profile", login] });
      qc.invalidateQueries({ queryKey: ["current-user"] });
    },
  });

  if (viewer.isError)
    return <InlineError title="Failed to load your account" detail={String(viewer.error)} />;
  if (profile.isError)
    return <InlineError title="Failed to load profile" detail={String(profile.error)} />;

  const field = (key: keyof typeof current, label: string, placeholder = "") => (
    <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
      <FormLabel id={`profile-${key}`}>{label}</FormLabel>
      <input id={`profile-${key}`} type="text" value={current[key]} placeholder={placeholder} onChange={(e) => set(key, e.target.value)} className="w-full" />
    </div>
  );

  return (
    <Box header={<span style={{ fontWeight: 600 }}>Public profile</span>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        {saveMut.error && <ErrorBanner>{String(saveMut.error)}</ErrorBanner>}
        {field("name", "Name", "Your name")}
        <div style={{ display: "flex", flexDirection: "column", gap: "0.25rem" }}>
          <FormLabel id="profile-bio">Bio</FormLabel>
          <textarea id="profile-bio" value={current.bio} rows={3} onChange={(e) => set("bio", e.target.value)} className="w-full" />
        </div>
        {field("company", "Company")}
        {field("location", "Location")}
        {field("blog", "Website", "https://example.com")}
        {field("twitter_username", "Twitter username", "without @")}
        <div className="flex items-center justify-end gap-3">
          {saveMut.isSuccess && <span style={{ fontSize: "0.82rem", color: "var(--gh-open)" }}>Profile updated.</span>}
          <Button variant="primary" disabled={saveMut.isPending} onClick={() => saveMut.mutate()}>
            {saveMut.isPending ? "Saving…" : "Update profile"}
          </Button>
        </div>
        <SocialAccountsEditor />
      </div>
    </Box>
  );
}

// ─── Social accounts (add/remove profile links via /user/social_accounts) ──────────

function SocialAccountsEditor() {
  const qc = useQueryClient();
  const [newUrl, setNewUrl] = useState("");
  const query = useQuery({ queryKey: ["social-accounts"], queryFn: fetchSocialAccounts });
  const invalidate = () => qc.invalidateQueries({ queryKey: ["social-accounts"] });
  const addMut = useMutation({
    mutationFn: () => addSocialAccounts([newUrl.trim()]),
    onSuccess: () => {
      setNewUrl("");
      invalidate();
    },
  });
  const removeMut = useMutation({
    mutationFn: (url: string) => deleteSocialAccounts([url]),
    onSuccess: invalidate,
  });

  return (
    <div style={{ borderTop: "1px solid var(--color-border)", paddingTop: "0.85rem", display: "flex", flexDirection: "column", gap: "0.5rem" }}>
      <span style={{ fontWeight: 600, fontSize: "0.9rem" }}>Social accounts</span>
      {(addMut.error || removeMut.error) && <ErrorBanner>{String(addMut.error ?? removeMut.error)}</ErrorBanner>}
      {query.data && query.data.length === 0 && (
        <span style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>No social accounts linked.</span>
      )}
      {query.data?.map((account) => (
        <div key={account.url} className="flex items-center justify-between gap-2">
          <a href={account.url} style={{ fontSize: "0.85rem", color: "var(--color-accent-fg)" }}>{account.url}</a>
          <Button
            size="sm"
            variant="ghost"
            aria-label={`Remove ${account.url}`}
            disabled={removeMut.isPending}
            onClick={() => removeMut.mutate(account.url)}
          >
            Remove
          </Button>
        </div>
      ))}
      <form
        className="flex items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (newUrl.trim()) addMut.mutate();
        }}
      >
        <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: "0.25rem" }}>
          <FormLabel id="social-account-url">Add a social link</FormLabel>
          <input
            id="social-account-url"
            type="url"
            value={newUrl}
            placeholder="https://example.com/you"
            onChange={(e) => setNewUrl(e.target.value)}
            className="w-full"
          />
        </div>
        <Button type="submit" variant="secondary" size="sm" disabled={!newUrl.trim() || addMut.isPending}>
          {addMut.isPending ? "Adding…" : "Add"}
        </Button>
      </form>
    </div>
  );
}

// ─── Appearance (theme, in its github.com-correct Settings location) ──────────────

function AppearanceTab() {
  const { theme, setTheme } = useTheme("light");
  const options: { value: Theme; label: string; hint: string }[] = [
    { value: "light", label: "Light", hint: "Default light theme" },
    { value: "dark", label: "Dark", hint: "Dark theme for low-light" },
    { value: "system", label: "Sync with system", hint: "Follows your OS appearance setting" },
  ];
  return (
    <Box header={<span style={{ fontWeight: 600 }}>Theme</span>}>
      <fieldset style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.6rem", border: "none", margin: 0 }}>
        <legend style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", marginBottom: "0.4rem" }}>
          Choose how bleephub looks to you.
        </legend>
        {options.map((o) => (
          <label key={o.value} className="flex items-center gap-2" style={{ fontSize: "0.9rem", cursor: "pointer" }}>
            <input
              type="radio"
              name="theme"
              value={o.value}
              checked={theme === o.value}
              onChange={() => setTheme(o.value)}
            />
            <span style={{ fontWeight: 600 }}>{o.label}</span>
            <span style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>· {o.hint}</span>
          </label>
        ))}
      </fieldset>
    </Box>
  );
}

/** Real headings (not bold text) so screen readers get a navigable outline. */
const sectionHeading: React.CSSProperties = { fontWeight: 600, fontSize: "inherit", margin: 0 };

// ─── Password and authentication ───────────────────────────────────────────
// Fetch wrappers defined here, not in the shared API module, which is at its bundle budget.

interface AccountAuthentication {
  kind: "local" | "external";
  providers?: string[];
  password_set: boolean;
}

interface TwoFactorStatus {
  enabled: boolean;
  pending_enrollment: boolean;
  enrolled_at?: string;
  recovery_codes_total: number;
  recovery_codes_remaining: number;
  recovery_codes_generated_at?: string;
}

interface AuthenticationSettings {
  authentication: AccountAuthentication;
  two_factor: TwoFactorStatus;
}

interface TwoFactorEnrollment {
  secret: string;
  otpauth_uri: string;
  issuer: string;
  account: string;
  digits: number;
  period: number;
  qr?: { size: number; modules: string[] };
}

interface BrowserSession {
  handle: string;
  created_at: string | null;
  expires_at: string;
  user_agent: string;
  ip: string;
  provider: string;
  current: boolean;
}

const fetchAuthenticationSettings = (signal?: AbortSignal) =>
  ghFetch<AuthenticationSettings>("/ui-data/user/authentication", signal);
const fetchBrowserSessions = (signal?: AbortSignal) =>
  ghFetch<{ sessions: BrowserSession[] }>("/ui-data/user/sessions", signal);
const beginTwoFactorEnrollment = () =>
  ghPostJSON<TwoFactorEnrollment>("/ui-data/user/two-factor/enrollment", {});
const cancelTwoFactorEnrollment = () =>
  ghSend("DELETE", "/ui-data/user/two-factor/enrollment");
const confirmTwoFactorEnrollment = (code: string) =>
  ghPostJSON<AuthenticationSettings & { recovery_codes: string[] }>(
    "/ui-data/user/two-factor/enrollment/confirm",
    { code },
  );
const disableTwoFactor = (code: string) =>
  ghPostJSON<AuthenticationSettings>("/ui-data/user/two-factor/disable", { code });
const regenerateRecoveryCodes = (code: string) =>
  ghPostJSON<{ two_factor: TwoFactorStatus; recovery_codes: string[] }>(
    "/ui-data/user/two-factor/recovery-codes",
    { code },
  );
const changePassword = (currentPassword: string, newPassword: string) =>
  ghSend("PUT", "/ui-data/user/password", {
    current_password: currentPassword,
    new_password: newPassword,
  });
const revokeBrowserSession = (handle: string) =>
  ghSend("DELETE", `/ui-data/user/sessions/${enc(handle)}`);

/** Pull the server's {"message":…} out of the API layer's method/status/body envelope. */
function describeAccountError(error: unknown): string {
  const text = error instanceof Error ? error.message : String(error);
  const brace = text.indexOf("{");
  if (brace >= 0) {
    try {
      const parsed = JSON.parse(text.slice(brace)) as { message?: string };
      if (parsed.message) return parsed.message;
    } catch {
      // Not JSON; fall through to raw text.
    }
  }
  return text;
}

/**
 * QR code as a single SVG path (one subpath per dark module — lighter than
 * thousands of <rect>). Colours are fixed black-on-white in both themes with the
 * standard 4-module quiet zone: scanners expect dark-on-light, so theming it breaks scanning.
 */
function ProvisioningQRCode({ modules, label }: { modules: string[]; label: string }) {
  const size = modules.length;
  const quiet = 4;
  const path = modules
    .flatMap((row, y) =>
      row.split("").flatMap((module, x) => (module === "1" ? [`M${x + quiet} ${y + quiet}h1v1h-1z`] : [])),
    )
    .join("");
  return (
    <svg
      role="img"
      aria-label={label}
      viewBox={`0 0 ${size + quiet * 2} ${size + quiet * 2}`}
      width="180"
      height="180"
      style={{ display: "block", borderRadius: "6px", border: "1px solid var(--color-border)" }}
      shapeRendering="crispEdges"
    >
      <rect width="100%" height="100%" fill="#ffffff" />
      <path d={path} fill="#000000" />
    </svg>
  );
}

/** The one-and-only showing of a freshly issued recovery-code set. */
function RecoveryCodesPanel({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const asText = codes.join("\n");
  const download = () => {
    const blob = new Blob([`${asText}\n`], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = "bleephub-recovery-codes.txt";
    link.click();
    URL.revokeObjectURL(url);
  };
  return (
    <Box header={<h2 style={sectionHeading}>Save your recovery codes</h2>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        <p style={{ fontSize: "0.9rem", margin: 0 }}>
          Keep these somewhere safe. Each code can be used once, in place of your authenticator
          app, if you lose access to it. <strong>They are shown only now</strong> — you can
          generate a new set later, but you cannot see these again.
        </p>
        <ul
          style={{
            margin: 0,
            padding: "0.75rem",
            listStyle: "none",
            display: "grid",
            gridTemplateColumns: "repeat(auto-fill, minmax(9rem, 1fr))",
            gap: "0.35rem",
            fontFamily: "var(--font-mono, ui-monospace, monospace)",
            fontSize: "0.9rem",
            background: "var(--color-bg-subtle)",
            borderRadius: "6px",
          }}
        >
          {codes.map((code) => (
            <li key={code}>{code}</li>
          ))}
        </ul>
        <div className="flex flex-wrap gap-2">
          <Button size="sm" onClick={download}>
            Download codes
          </Button>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => void navigator.clipboard?.writeText(asText)}
          >
            Copy codes
          </Button>
          <Button size="sm" variant="primary" onClick={onDone}>
            I have saved my codes
          </Button>
        </div>
      </div>
    </Box>
  );
}

function AuthenticationTab() {
  const client = useQueryClient();
  const settings = useQuery({
    queryKey: ["account-authentication"],
    queryFn: ({ signal }) => fetchAuthenticationSettings(signal),
  });
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);

  if (settings.isLoading) return <Spinner label="loading authentication settings" />;
  if (settings.isError)
    return (
      <InlineError
        title="Failed to load authentication settings"
        detail={describeAccountError(settings.error)}
      />
    );

  const data = settings.data!;
  const refresh = () => void client.invalidateQueries({ queryKey: ["account-authentication"] });
  const external = data.authentication.kind === "external";

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {external ? (
        <IdentityProviderNotice providers={data.authentication.providers ?? []} />
      ) : (
        <>
          <PasswordCard passwordSet={data.authentication.password_set} onChanged={refresh} />
          {recoveryCodes ? (
            <RecoveryCodesPanel codes={recoveryCodes} onDone={() => setRecoveryCodes(null)} />
          ) : (
            <TwoFactorCard
              status={data.two_factor}
              onChanged={refresh}
              onNewRecoveryCodes={(codes) => {
                setRecoveryCodes(codes);
                refresh();
              }}
            />
          )}
        </>
      )}
      <BrowserSessionsCard />
    </div>
  );
}

/** Federated account: no local password/2FA to manage, so show who holds them. */
function IdentityProviderNotice({ providers }: { providers: string[] }) {
  return (
    <Box header={<h2 style={sectionHeading}>Password and authentication</h2>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.5rem" }}>
        <p style={{ fontSize: "0.9rem", margin: 0 }}>
          This account signs in through an identity provider, so your password, passkeys and
          two-factor authentication are managed there — not here.
        </p>
        {providers.length > 0 && (
          <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: 0 }}>
            Signed in via{" "}
            {providers.map((provider, index) => (
              <span key={provider}>
                {index > 0 && ", "}
                <code>{provider}</code>
              </span>
            ))}
            . Change your password or manage two-factor authentication in your provider&apos;s
            account settings.
          </p>
        )}
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: 0 }}>
          Your active sessions on this instance are listed below, and you can end any of them
          from here.
        </p>
      </div>
    </Box>
  );
}

function PasswordCard({ passwordSet, onChanged }: { passwordSet: boolean; onChanged: () => void }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [done, setDone] = useState(false);
  const mutation = useMutation({
    mutationFn: () => changePassword(current, next),
    onSuccess: () => {
      setCurrent("");
      setNext("");
      setConfirm("");
      setDone(true);
      onChanged();
    },
  });
  const mismatch = confirm !== "" && confirm !== next;
  const title = passwordSet ? "Change password" : "Set a password";
  return (
    <Box header={<h2 style={sectionHeading}>{title}</h2>}>
      <form
        style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem", maxWidth: "26rem" }}
        onSubmit={(event) => {
          event.preventDefault();
          setDone(false);
          mutation.mutate();
        }}
      >
        {mutation.error && <ErrorBanner>{describeAccountError(mutation.error)}</ErrorBanner>}
        {done && (
          <p style={{ fontSize: "0.85rem", margin: 0, color: "var(--color-fg-muted)" }} role="status">
            Password updated. Every other signed-in session was ended.
          </p>
        )}
        {passwordSet && (
          <div>
            <FormLabel id="password-current">Old password</FormLabel>
            <input
              id="password-current"
              type="password"
              className="w-full"
              autoComplete="current-password"
              value={current}
              onChange={(event) => setCurrent(event.target.value)}
            />
          </div>
        )}
        <div>
          <FormLabel id="password-new">New password</FormLabel>
          <input
            id="password-new"
            type="password"
            className="w-full"
            autoComplete="new-password"
            value={next}
            onChange={(event) => setNext(event.target.value)}
            aria-describedby="password-rule"
          />
          <p id="password-rule" style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", margin: "0.25rem 0 0" }}>
            Make sure it is at least 15 characters, or at least 8 characters including a number
            and a lowercase letter.
          </p>
        </div>
        <div>
          <FormLabel id="password-confirm">Confirm new password</FormLabel>
          <input
            id="password-confirm"
            type="password"
            className="w-full"
            autoComplete="new-password"
            value={confirm}
            onChange={(event) => setConfirm(event.target.value)}
            aria-invalid={mismatch}
          />
          {mismatch && (
            <p style={{ fontSize: "0.8rem", color: "var(--color-status-error, var(--color-fg))", margin: "0.25rem 0 0" }}>
              The two passwords do not match.
            </p>
          )}
        </div>
        <div>
          <Button
            type="submit"
            variant="primary"
            disabled={!next || mismatch || mutation.isPending || (passwordSet && !current)}
          >
            {mutation.isPending ? "Saving…" : title}
          </Button>
        </div>
      </form>
    </Box>
  );
}

function TwoFactorCard({
  status,
  onChanged,
  onNewRecoveryCodes,
}: {
  status: TwoFactorStatus;
  onChanged: () => void;
  onNewRecoveryCodes: (codes: string[]) => void;
}) {
  const [enrollment, setEnrollment] = useState<TwoFactorEnrollment | null>(null);

  const begin = useMutation({
    mutationFn: beginTwoFactorEnrollment,
    onSuccess: (data) => {
      setEnrollment(data);
      onChanged();
    },
  });
  const cancel = useMutation({
    mutationFn: cancelTwoFactorEnrollment,
    onSuccess: () => {
      setEnrollment(null);
      onChanged();
    },
  });

  if (enrollment) {
    return (
      <TwoFactorEnrollmentCard
        enrollment={enrollment}
        onCancel={() => cancel.mutate()}
        cancelling={cancel.isPending}
        onEnrolled={(codes) => {
          setEnrollment(null);
          onNewRecoveryCodes(codes);
        }}
      />
    );
  }

  if (!status.enabled) {
    return (
      <Box header={<h2 style={sectionHeading}>Two-factor authentication</h2>}>
        <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.6rem" }}>
          {begin.error && <ErrorBanner>{describeAccountError(begin.error)}</ErrorBanner>}
          <p style={{ fontSize: "0.9rem", margin: 0 }}>
            Two-factor authentication is <strong>not enabled</strong> for your account.
          </p>
          <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: 0 }}>
            Adding a time-based one-time password (TOTP) app means signing in needs a code from
            your phone as well as your password. You will be asked to enter a code before it is
            switched on, and you will get single-use recovery codes in case you lose the app.
          </p>
          <div>
            <Button variant="primary" disabled={begin.isPending} onClick={() => begin.mutate()}>
              {begin.isPending ? "Preparing…" : "Enable two-factor authentication"}
            </Button>
          </div>
        </div>
      </Box>
    );
  }

  return (
    <TwoFactorEnabledCard
      status={status}
      onChanged={onChanged}
      onNewRecoveryCodes={onNewRecoveryCodes}
    />
  );
}

function TwoFactorEnrollmentCard({
  enrollment,
  onCancel,
  cancelling,
  onEnrolled,
}: {
  enrollment: TwoFactorEnrollment;
  onCancel: () => void;
  cancelling: boolean;
  onEnrolled: (codes: string[]) => void;
}) {
  const [code, setCode] = useState("");
  const confirm = useMutation({
    mutationFn: () => confirmTwoFactorEnrollment(code),
    onSuccess: (data) => onEnrolled(data.recovery_codes),
  });
  return (
    <Box header={<h2 style={sectionHeading}>Set up your authenticator app</h2>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "1rem" }}>
        <p style={{ fontSize: "0.9rem", margin: 0 }}>
          Scan this code with your authenticator app, then enter the six-digit code it shows.
          Two-factor authentication is switched on only once that code checks out.
        </p>
        <div className="flex flex-wrap items-start gap-4">
          {enrollment.qr ? (
            <ProvisioningQRCode
              modules={enrollment.qr.modules}
              label={`QR code enrolling ${enrollment.account} in two-factor authentication`}
            />
          ) : (
            <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: 0 }}>
              The QR code could not be rendered — use the setup key below instead.
            </p>
          )}
          <div style={{ flex: 1, minWidth: "14rem", display: "flex", flexDirection: "column", gap: "0.5rem" }}>
            <div>
              <span style={{ fontSize: "0.8rem", fontWeight: 600 }}>Setup key</span>
              <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", margin: "0.15rem 0" }}>
                If you cannot scan, enter this into your app by hand.
              </p>
              <code style={{ wordBreak: "break-all", fontSize: "0.85rem" }}>{enrollment.secret}</code>
            </div>
            <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", margin: 0 }}>
              {enrollment.digits} digits · refreshes every {enrollment.period} seconds ·{" "}
              {enrollment.issuer}
            </p>
          </div>
        </div>
        <form
          className="flex flex-wrap items-end gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            confirm.mutate();
          }}
        >
          <div style={{ minWidth: "10rem" }}>
            <FormLabel id="two-factor-code">Verification code</FormLabel>
            <input
              id="two-factor-code"
              type="text"
              inputMode="numeric"
              autoComplete="one-time-code"
              maxLength={7}
              className="w-full"
              value={code}
              onChange={(event) => setCode(event.target.value)}
            />
          </div>
          <Button type="submit" variant="primary" disabled={code.trim().length < 6 || confirm.isPending}>
            {confirm.isPending ? "Verifying…" : "Verify and enable"}
          </Button>
          <Button type="button" variant="ghost" disabled={cancelling} onClick={onCancel}>
            Cancel
          </Button>
        </form>
        {confirm.error && <ErrorBanner>{describeAccountError(confirm.error)}</ErrorBanner>}
      </div>
    </Box>
  );
}

function TwoFactorEnabledCard({
  status,
  onChanged,
  onNewRecoveryCodes,
}: {
  status: TwoFactorStatus;
  onChanged: () => void;
  onNewRecoveryCodes: (codes: string[]) => void;
}) {
  const [action, setAction] = useState<"disable" | "regenerate" | null>(null);
  const [code, setCode] = useState("");
  const disable = useMutation({
    mutationFn: () => disableTwoFactor(code),
    onSuccess: () => {
      setAction(null);
      setCode("");
      onChanged();
    },
  });
  const regenerate = useMutation({
    mutationFn: () => regenerateRecoveryCodes(code),
    onSuccess: (data) => {
      setAction(null);
      setCode("");
      onNewRecoveryCodes(data.recovery_codes);
    },
  });
  const pending = disable.isPending || regenerate.isPending;
  const error = disable.error ?? regenerate.error;
  const lowOnCodes = status.recovery_codes_remaining <= 3;

  return (
    <Box header={<h2 style={sectionHeading}>Two-factor authentication</h2>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        <p style={{ fontSize: "0.9rem", margin: 0 }}>
          Two-factor authentication is <strong>enabled</strong>
          {status.enrolled_at && (
            <>
              {" "}
              since <RelativeTime iso={status.enrolled_at} />
            </>
          )}
          .
        </p>
        <p
          style={{
            fontSize: "0.85rem",
            margin: 0,
            color: lowOnCodes ? "var(--color-status-error, var(--color-fg))" : "var(--color-fg-muted)",
          }}
        >
          {status.recovery_codes_remaining} of {status.recovery_codes_total} recovery codes remain
          {status.recovery_codes_generated_at && (
            <>
              {" "}
              (generated <RelativeTime iso={status.recovery_codes_generated_at} />)
            </>
          )}
          {lowOnCodes && " — generate a new set before you run out."}
        </p>
        {error && <ErrorBanner>{describeAccountError(error)}</ErrorBanner>}
        {action === null ? (
          <div className="flex flex-wrap gap-2">
            <Button size="sm" onClick={() => setAction("regenerate")}>
              Generate new recovery codes
            </Button>
            <Button size="sm" variant="danger" onClick={() => setAction("disable")}>
              Disable two-factor authentication
            </Button>
          </div>
        ) : (
          <form
            className="flex flex-wrap items-end gap-2"
            onSubmit={(event) => {
              event.preventDefault();
              if (action === "disable") disable.mutate();
              else regenerate.mutate();
            }}
          >
            <div style={{ minWidth: "12rem" }}>
              <FormLabel id="two-factor-confirm-code">
                {action === "disable"
                  ? "Enter a code to turn two-factor authentication off"
                  : "Enter a code to replace your recovery codes"}
              </FormLabel>
              <input
                id="two-factor-confirm-code"
                type="text"
                inputMode="numeric"
                autoComplete="one-time-code"
                className="w-full"
                value={code}
                onChange={(event) => setCode(event.target.value)}
                aria-describedby="two-factor-confirm-hint"
              />
              <p
                id="two-factor-confirm-hint"
                style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", margin: "0.25rem 0 0" }}
              >
                A code from your authenticator app, or one of your recovery codes.
              </p>
            </div>
            <Button
              type="submit"
              variant={action === "disable" ? "danger" : "primary"}
              disabled={!code.trim() || pending}
            >
              {pending ? "Checking…" : action === "disable" ? "Disable" : "Generate"}
            </Button>
            <Button
              type="button"
              variant="ghost"
              disabled={pending}
              onClick={() => {
                setAction(null);
                setCode("");
              }}
            >
              Cancel
            </Button>
          </form>
        )}
      </div>
    </Box>
  );
}

function BrowserSessionsCard() {
  const client = useQueryClient();
  const query = useQuery({
    queryKey: ["account-sessions"],
    queryFn: ({ signal }) => fetchBrowserSessions(signal),
  });
  const revoke = useMutation({
    mutationFn: (handle: string) => revokeBrowserSession(handle),
    onSuccess: () => void client.invalidateQueries({ queryKey: ["account-sessions"] }),
  });
  return (
    <Box header={<h2 style={sectionHeading}>Sessions</h2>}>
      <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.75rem" }}>
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: 0 }}>
          Browsers currently signed in to your account. Ending a session signs that browser out
          immediately.
        </p>
        {revoke.error && <ErrorBanner>{describeAccountError(revoke.error)}</ErrorBanner>}
        {query.isLoading && <Spinner label="loading sessions" />}
        {query.isError && (
          <InlineError title="Failed to load sessions" detail={describeAccountError(query.error)} />
        )}
        {query.data && query.data.sessions.length === 0 && (
          <Blankslate title="No active sessions">
            You are signed in with a token rather than a browser session.
          </Blankslate>
        )}
        {query.data && query.data.sessions.length > 0 && (
          <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: "0.5rem" }}>
            {query.data.sessions.map((session) => (
              <li
                key={session.handle}
                className="flex flex-wrap items-center justify-between gap-2"
                style={{ borderTop: "1px solid var(--color-border)", paddingTop: "0.5rem" }}
              >
                <div>
                  <p style={{ fontSize: "0.9rem", margin: 0, fontWeight: 600 }}>
                    {session.user_agent || "Unknown browser"}
                    {session.current && (
                      <span style={{ fontWeight: 400, color: "var(--color-fg-muted)" }}> · this session</span>
                    )}
                  </p>
                  <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", margin: 0 }}>
                    {session.ip || "unknown address"}
                    {session.provider && ` · via ${session.provider}`}
                    {session.created_at && (
                      <>
                        {" · signed in "}
                        <RelativeTime iso={session.created_at} />
                      </>
                    )}
                  </p>
                </div>
                <Button
                  size="sm"
                  variant={session.current ? "secondary" : "danger"}
                  disabled={revoke.isPending}
                  onClick={() => {
                    void confirmAction(
                      session.current
                        ? "This signs you out of this browser."
                        : `This signs out ${session.user_agent || "that browser"}.`,
                      { title: "End this session?", confirmLabel: "End session" },
                    ).then((confirmed) => {
                      if (confirmed) revoke.mutate(session.handle);
                    });
                  }}
                >
                  {session.current ? "Sign out" : "End session"}
                </Button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </Box>
  );
}

// ─── Account (username rename + account deletion — admin-mediated, GHES-style) ─────

function AccountAdminTab() {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const viewer = useQuery({ queryKey: ["viewer"], queryFn: fetchAuthenticatedUser });
  const [nextLogin, setNextLogin] = useState("");
  const [showDelete, setShowDelete] = useState(false);

  const login = viewer.data?.login ?? "";
  const renameMut = useMutation({
    mutationFn: (next: string) =>
      ghSend("PATCH", `/api/v3/admin/users/${enc(login)}`, { login: next }),
    onSuccess: (_ignored, next) => {
      // Viewer identity changed; every login-derived cache read is stale.
      void qc.invalidateQueries({ queryKey: ["viewer"] });
      void qc.invalidateQueries({ queryKey: ["current-user"] });
      navigate(`/ui/${enc(next)}`);
    },
  });

  if (viewer.isLoading) return <Spinner label="loading your account" />;
  if (viewer.isError)
    return <InlineError title="Failed to load your account" detail={String(viewer.error)} />;

  if (!viewer.data?.site_admin) {
    return (
      <Box header={<span style={{ fontWeight: 600 }}>Account</span>}>
        <div style={{ padding: "1rem" }}>
          <p style={{ fontSize: "0.9rem", margin: 0 }}>
            Changing your username or deleting your account is managed by your site
            administrator on this deployment.
          </p>
          <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", margin: "0.25rem 0 0" }}>
            Contact your administrator to rename or remove the <b>{login}</b> account.
          </p>
        </div>
      </Box>
    );
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {(renameMut.error) && <ErrorBanner>{String(renameMut.error)}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>Change username</span>}>
        <div style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.6rem" }}>
          <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", margin: 0 }}>
            Renames the <b>{login}</b> account. Old profile links will stop working.
          </p>
          <div className="flex flex-wrap items-end gap-2">
            <div style={{ flex: 1, minWidth: "14rem" }}>
              <FormLabel id="account-new-login">New username</FormLabel>
              <input
                id="account-new-login"
                type="text"
                className="w-full"
                value={nextLogin}
                onChange={(e) => setNextLogin(e.target.value)}
                placeholder={login}
              />
            </div>
            <Button
              variant="primary"
              disabled={!nextLogin.trim() || nextLogin.trim() === login || renameMut.isPending}
              onClick={() => renameMut.mutate(nextLogin.trim())}
            >
              {renameMut.isPending ? "Renaming…" : "Change username"}
            </Button>
          </div>
        </div>
      </Box>
      <Box header={<span style={{ fontWeight: 600, color: "var(--color-status-error, var(--color-fg))" }}>Delete account</span>}>
        <div style={{ padding: "1rem", display: "flex", alignItems: "center", justifyContent: "space-between", gap: "1rem", flexWrap: "wrap" }}>
          <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", margin: 0 }}>
            Permanently deletes the <b>{login}</b> account and signs you out. This cannot be undone.
          </p>
          <Button variant="danger" onClick={() => setShowDelete(true)}>
            Delete your account
          </Button>
        </div>
      </Box>
      {showDelete && <DeleteAccountDialog login={login} onClose={() => setShowDelete(false)} />}
    </div>
  );
}

function DeleteAccountDialog({ login, onClose }: { login: string; onClose: () => void }) {
  const [confirmText, setConfirmText] = useState("");
  const deleteMut = useMutation({
    mutationFn: () => ghSend("DELETE", `/api/v3/admin/users/${enc(login)}`),
    onSuccess: () => {
      // Account and session are gone: leave via sign-out so cookies/state clear.
      const form = document.createElement("form");
      form.method = "post";
      form.action = "/auth/logout";
      document.body.appendChild(form);
      form.submit();
    },
  });
  return (
    <Modal title="Delete account" onClose={onClose}>
      <p style={{ fontSize: "0.9rem", marginTop: 0 }}>
        This permanently deletes <b>{login}</b> and everything it owns, then signs you out.
      </p>
      <FormLabel id="delete-account-confirm">
        To confirm, type &quot;{login}&quot; below
      </FormLabel>
      <input
        id="delete-account-confirm"
        type="text"
        className="mb-4 w-full"
        value={confirmText}
        onChange={(e) => setConfirmText(e.target.value)}
        autoComplete="off"
      />
      {deleteMut.error && <ErrorBanner>{String(deleteMut.error)}</ErrorBanner>}
      <DialogActions>
        <Button variant="ghost" onClick={onClose} disabled={deleteMut.isPending}>
          Cancel
        </Button>
        <Button
          variant="danger"
          disabled={confirmText !== login || deleteMut.isPending}
          onClick={() => deleteMut.mutate()}
        >
          {deleteMut.isPending ? "Deleting…" : "Delete this account"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

// ─── Notifications ─────────────────────────────────────────────────────────
// Web channels actually gate the inbox, so a toggle has a visible effect.

interface NotificationChannels {
  email: boolean;
  web: boolean;
}

interface NotificationPreferences {
  participating: NotificationChannels;
  watching: NotificationChannels;
  automatically_watch_repositories: boolean;
  automatically_watch_teams: boolean;
  events: Record<string, NotificationChannels>;
  include_own_updates: boolean;
  actions_failed_workflows_only: boolean;
  dependabot_weekly_digest: boolean;
}

const fetchNotificationPreferences = (signal?: AbortSignal) =>
  ghFetch<NotificationPreferences>("/ui-data/user/notification-settings", signal);
const saveNotificationPreferences = (preferences: NotificationPreferences) =>
  ghSend("PUT", "/ui-data/user/notification-settings", preferences);

/** The subscription classes, in github.com's order. */
const SUBSCRIPTION_CLASSES: {
  key: "participating" | "watching";
  label: string;
  hint: string;
}[] = [
  {
    key: "participating",
    label: "Participating and @mentions",
    hint: "Issues, pull requests and discussions you are taking part in, and anything that @mentions you.",
  },
  {
    key: "watching",
    label: "Watching",
    hint: "Everything else from the repositories, teams and conversations you watch.",
  },
];

/** The per-event-type rows, keyed by the server's event constants. */
const NOTIFICATION_EVENTS: { key: string; label: string; hint: string }[] = [
  { key: "issue", label: "Issues", hint: "Opened, closed, commented on, or reassigned." },
  { key: "pull_request", label: "Pull requests", hint: "Reviews, review comments, merges and pushes." },
  { key: "release", label: "Releases", hint: "New releases in repositories you watch." },
  { key: "discussion", label: "Discussions", hint: "New discussions, answers and replies." },
  { key: "commit", label: "Commits", hint: "Comments left directly on a commit." },
  { key: "actions", label: "Actions", hint: "Workflow runs in your repositories." },
  { key: "dependabot", label: "Dependabot alerts", hint: "New and reintroduced vulnerability alerts." },
];

const DEFAULT_CHANNELS: NotificationChannels = { email: true, web: true };

function ChannelChecks({
  name,
  channels,
  onChange,
}: {
  name: string;
  channels: NotificationChannels;
  onChange: (next: NotificationChannels) => void;
}) {
  return (
    <div className="flex items-center gap-4">
      {(["email", "web"] as const).map((channel) => (
        <label key={channel} className="flex items-center gap-1" style={{ fontSize: "0.85rem", cursor: "pointer" }}>
          <input
            type="checkbox"
            checked={channels[channel]}
            aria-label={`${channel === "email" ? "Email" : "Web"} notifications for ${name}`}
            onChange={(event) => onChange({ ...channels, [channel]: event.target.checked })}
          />
          <span>{channel === "email" ? "Email" : "Web"}</span>
        </label>
      ))}
    </div>
  );
}

function NotificationsSettingsTab() {
  const client = useQueryClient();
  const query = useQuery({
    queryKey: ["notification-preferences"],
    queryFn: ({ signal }) => fetchNotificationPreferences(signal),
  });
  // Save-on-toggle: the checkbox moves optimistically (waiting on the server
  // reads as broken). Re-seeded from every fetch, so a rejected save snaps back.
  const [draft, setDraft] = useState<NotificationPreferences | null>(null);
  useEffect(() => {
    if (query.data) setDraft(query.data);
  }, [query.data]);
  const mutation = useMutation({
    mutationFn: (next: NotificationPreferences) => saveNotificationPreferences(next),
    onSettled: () => void client.invalidateQueries({ queryKey: ["notification-preferences"] }),
  });

  if (query.isLoading) return <Spinner label="loading notification settings" />;
  if (query.isError)
    return (
      <InlineError
        title="Failed to load notification settings"
        detail={describeAccountError(query.error)}
      />
    );

  const preferences = draft ?? query.data!;
  const save = (patch: Partial<NotificationPreferences>) => {
    const next = { ...preferences, ...patch };
    setDraft(next);
    mutation.mutate(next);
  };
  const saveEvent = (key: string, channels: NotificationChannels) =>
    save({ events: { ...preferences.events, [key]: channels } });

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {mutation.error && <ErrorBanner>{describeAccountError(mutation.error)}</ErrorBanner>}

      <Box header={<h2 style={sectionHeading}>Subscriptions</h2>}>
        <fieldset style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.9rem", border: "none", margin: 0 }}>
          <legend style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", marginBottom: "0.3rem" }}>
            Choose where notifications are delivered for each kind of subscription.
          </legend>
          {SUBSCRIPTION_CLASSES.map((subscription) => (
            <div key={subscription.key} className="flex flex-wrap items-start justify-between gap-3">
              <div style={{ maxWidth: "34rem" }}>
                <span style={{ fontSize: "0.9rem", fontWeight: 600 }}>{subscription.label}</span>
                <span style={{ display: "block", fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
                  {subscription.hint}
                </span>
              </div>
              <ChannelChecks
                name={subscription.label}
                channels={preferences[subscription.key]}
                onChange={(next) => save({ [subscription.key]: next } as Partial<NotificationPreferences>)}
              />
            </div>
          ))}
        </fieldset>
      </Box>

      <Box header={<h2 style={sectionHeading}>Notification types</h2>}>
        <fieldset style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.9rem", border: "none", margin: 0 }}>
          <legend style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", marginBottom: "0.3rem" }}>
            Choose delivery separately for each kind of activity.
          </legend>
          {NOTIFICATION_EVENTS.map((event) => (
            <div key={event.key} className="flex flex-wrap items-start justify-between gap-3">
              <div style={{ maxWidth: "34rem" }}>
                <span style={{ fontSize: "0.9rem", fontWeight: 600 }}>{event.label}</span>
                <span style={{ display: "block", fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
                  {event.hint}
                </span>
              </div>
              <ChannelChecks
                name={event.label}
                channels={preferences.events[event.key] ?? DEFAULT_CHANNELS}
                onChange={(next) => saveEvent(event.key, next)}
              />
            </div>
          ))}
        </fieldset>
      </Box>

      <Box header={<h2 style={sectionHeading}>Automatic watching</h2>}>
        <fieldset style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.7rem", border: "none", margin: 0 }}>
          <legend style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", marginBottom: "0.3rem" }}>
            Decide what you start watching without asking.
          </legend>
          {[
            {
              key: "automatically_watch_repositories" as const,
              label: "Automatically watch repositories",
              hint: "Watch a repository when you gain push access to it.",
            },
            {
              key: "automatically_watch_teams" as const,
              label: "Automatically watch teams",
              hint: "Watch a team when you join it.",
            },
            {
              key: "include_own_updates" as const,
              label: "Include your own updates",
              hint: "Receive notifications about your own activity as well as other people's.",
            },
          ].map((option) => (
            <label key={option.key} className="flex items-start gap-2" style={{ fontSize: "0.9rem", cursor: "pointer" }}>
              <input
                type="checkbox"
                checked={preferences[option.key]}
                onChange={(event) => save({ [option.key]: event.target.checked } as Partial<NotificationPreferences>)}
              />
              <span>
                <span style={{ fontWeight: 600 }}>{option.label}</span>
                <span style={{ display: "block", fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
                  {option.hint}
                </span>
              </span>
            </label>
          ))}
        </fieldset>
      </Box>

      <Box header={<h2 style={sectionHeading}>Digests</h2>}>
        <fieldset style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.7rem", border: "none", margin: 0 }}>
          <legend style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", marginBottom: "0.3rem" }}>
            Reduce the volume of the noisiest sources.
          </legend>
          {[
            {
              key: "actions_failed_workflows_only" as const,
              label: "Only notify for failed workflows",
              hint: "Skip notifications for workflow runs that succeed.",
            },
            {
              key: "dependabot_weekly_digest" as const,
              label: "Weekly Dependabot digest",
              hint: "Roll new alerts into one weekly email instead of one per alert.",
            },
          ].map((option) => (
            <label key={option.key} className="flex items-start gap-2" style={{ fontSize: "0.9rem", cursor: "pointer" }}>
              <input
                type="checkbox"
                checked={preferences[option.key]}
                onChange={(event) => save({ [option.key]: event.target.checked } as Partial<NotificationPreferences>)}
              />
              <span>
                <span style={{ fontWeight: 600 }}>{option.label}</span>
                <span style={{ display: "block", fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
                  {option.hint}
                </span>
              </span>
            </label>
          ))}
        </fieldset>
      </Box>
    </div>
  );
}
