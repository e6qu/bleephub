import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { confirmAction } from "../components/confirmAction.js";
import { fetchAccountSettings, setTwoFactor, setNotificationSettings, ghFetch, ghSend, type NotificationSettings } from "../api.js";
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
  GithubGPGKey,
  GithubSSHKey,
  GithubSSHSigningKey,
  GithubUserEmail,
} from "../types.js";
import { useTheme } from "@bleephub/ui-core/hooks";
import { PageTitle, Box, Button, ErrorBanner, FormLabel, Blankslate } from "../components/ui.js";
import { SettingsLayout, type SettingsNavSection } from "../components/SettingsLayout.js";
import { InteractionLimitsCard } from "../components/InteractionLimitsCard.js";
import { KeyIcon, LockIcon, GraphIcon } from "../components/octicons.js";

type AccountTab = "profile" | "appearance" | "notifications" | "tokens" | "ssh-keys" | "gpg-keys" | "signing-keys" | "emails" | "blocked" | "interaction-limits" | "authentication" | "codespaces" | "billing";

const ACCOUNT_NAV: SettingsNavSection<AccountTab>[] = [
  {
    items: [
      { key: "profile", label: "Public profile" },
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
    ],
  },
  {
    title: "Code, planning, and automation",
    items: [{ key: "codespaces", label: "Codespaces" }],
  },
  { title: "Billing", items: [{ key: "billing", label: "Billing and plans" }] },
  { title: "Moderation", items: [{ key: "blocked", label: "Blocked users" }, { key: "interaction-limits", label: "Interaction limits" }] },
];

export function AccountPage() {
  const [tab, setTab] = useState<AccountTab>("profile");
  return (
    <div>
      <PageTitle title="Account" meta="Your public profile, appearance, keys, emails, and blocked users" />
      <SettingsLayout sections={ACCOUNT_NAV} active={tab} onSelect={setTab}>
        {tab === "profile" && <ProfileSettingsTab />}
        {tab === "appearance" && <AppearanceTab />}
        {tab === "notifications" && <NotificationsSettingsTab />}
        {tab === "authentication" && <AuthenticationTab />}
        {tab === "ssh-keys" && <SSHKeysTab />}
        {tab === "tokens" && <FineGrainedTokensTab />}
        {tab === "gpg-keys" && <GPGKeysTab />}
        {tab === "signing-keys" && <SigningKeysTab />}
        {tab === "emails" && <EmailsTab />}
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

function FineGrainedTokensTab() {
  const client = useQueryClient();
  const query = useQuery({ queryKey: ["fine-grained-pats"], queryFn: fetchFineGrainedPATDashboard });
  const [name, setName] = useState("");
  const [owner, setOwner] = useState("");
  const [selection, setSelection] = useState<"all" | "subset" | "none">("all");
  const [repositoryIDs, setRepositoryIDs] = useState<number[]>([]);
  const [expires, setExpires] = useState("");
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
      <Button size="sm" onClick={() => navigator.clipboard.writeText(credential)} style={{ marginTop: ".65rem" }}>Copy token</Button>
    </div>}
    {error && <ErrorBanner>{String(error)}</ErrorBanner>}
    <Box header={<span style={{ fontWeight: 650 }}>Generate new token</span>}>
      <div className="grid gap-4" style={{ padding: "1rem", gridTemplateColumns: "repeat(auto-fit, minmax(240px, 1fr))" }}>
        <div><FormLabel id="pat-name">Token name</FormLabel><input id="pat-name" className="w-full" maxLength={40} value={name} onChange={(e) => setName(e.target.value)} placeholder="Deployment automation" /></div>
        <div><FormLabel id="pat-owner">Resource owner</FormLabel><select id="pat-owner" className="w-full" value={selectedOwner} onChange={(e) => { setOwner(e.target.value); setRepositoryIDs([]); }}>{data.resource_owners.map((item) => <option key={item.login} value={item.login}>{item.login} · {item.type}</option>)}</select></div>
        <div><FormLabel id="pat-expiry">Expiration</FormLabel><input id="pat-expiry" type="date" className="w-full" min={new Date().toISOString().slice(0, 10)} value={expires} onChange={(e) => setExpires(e.target.value)} /></div>
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
          <div style={truncatedMono}>{k.key}</div>
          <div style={{ color: "var(--color-fg-muted)", fontSize: "0.72rem" }}>
            {k.verified ? "verified" : "unverified"} · added{" "}
            {new Date(k.created_at).toLocaleDateString()}
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
            · added {new Date(k.created_at).toLocaleDateString()}
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
          <div style={truncatedMono}>{k.key}</div>
          <div style={{ color: "var(--color-fg-muted)", fontSize: "0.72rem" }}>
            added {new Date(k.created_at).toLocaleDateString()}
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
    // Sealed-box encrypt the value in the browser against the user
    // Codespaces public key; only ciphertext + key_id leave the client.
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
                      updated {new Date(s.updated_at).toLocaleDateString()}
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

// Billing amounts are decimal US dollars (grossAmount = quantity * pricePerUnit),
// not integer cents; format them as currency.
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
  const options: { value: "light" | "dark"; label: string; hint: string }[] = [
    { value: "light", label: "Light", hint: "Default light theme" },
    { value: "dark", label: "Dark", hint: "Dark theme for low-light" },
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

function AuthenticationTab() {
  const client = useQueryClient();
  const query = useQuery({ queryKey: ["account-settings"], queryFn: () => fetchAccountSettings() });
  const [error, setError] = useState<string | null>(null);
  const mutation = useMutation({
    mutationFn: (enabled: boolean) => setTwoFactor(enabled),
    onSuccess: () => { setError(null); void client.invalidateQueries({ queryKey: ["account-settings"] }); },
    onError: (e: Error) => setError(e.message),
  });
  if (query.isLoading) return <Spinner label="loading authentication settings" />;
  if (query.isError) return <InlineError title="Failed to load authentication settings" detail={String(query.error)} />;
  const on = query.data?.two_factor_enabled ?? false;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>Two-factor authentication</span>}>
        <div style={{ padding: "1rem", display: "flex", alignItems: "center", justifyContent: "space-between", gap: "1rem" }}>
          <div>
            <p style={{ fontSize: "0.9rem", margin: 0 }}>
              Two-factor authentication is <strong>{on ? "enabled" : "not enabled"}</strong> for your account.
            </p>
            <p style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", margin: "0.25rem 0 0" }}>
              Add an extra layer of security to your account by requiring a second authentication step at sign-in.
            </p>
          </div>
          <Button variant={on ? "secondary" : "primary"} size="sm" disabled={mutation.isPending}
            onClick={() => mutation.mutate(!on)}>
            {mutation.isPending ? "Saving…" : on ? "Disable" : "Enable two-factor authentication"}
          </Button>
        </div>
      </Box>
    </div>
  );
}

const NOTIFICATION_TOGGLES: { key: keyof NotificationSettings; label: string; hint: string }[] = [
  { key: "participating", label: "Participating and @mentions", hint: "Notify me about threads I'm participating in or @mentioned in." },
  { key: "watching", label: "Watching", hint: "Notify me about activity on repositories I watch." },
  { key: "email", label: "Email", hint: "Deliver notifications to my email." },
  { key: "web", label: "Web and mobile", hint: "Deliver notifications in the web UI." },
];

function NotificationsSettingsTab() {
  const client = useQueryClient();
  const query = useQuery({ queryKey: ["account-settings"], queryFn: () => fetchAccountSettings() });
  const [error, setError] = useState<string | null>(null);
  const mutation = useMutation({
    mutationFn: (next: NotificationSettings) => setNotificationSettings(next),
    onSuccess: () => { setError(null); void client.invalidateQueries({ queryKey: ["account-settings"] }); },
    onError: (e: Error) => setError(e.message),
  });
  if (query.isLoading) return <Spinner label="loading notification settings" />;
  if (query.isError) return <InlineError title="Failed to load notification settings" detail={String(query.error)} />;
  const settings = query.data!.notification_settings;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "1rem" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <Box header={<span style={{ fontWeight: 600 }}>Default notifications</span>}>
        <fieldset style={{ padding: "1rem", display: "flex", flexDirection: "column", gap: "0.7rem", border: "none", margin: 0 }}>
          <legend style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", marginBottom: "0.3rem" }}>
            Choose how you receive notifications.
          </legend>
          {NOTIFICATION_TOGGLES.map((t) => (
            <label key={t.key} className="flex items-start gap-2" style={{ fontSize: "0.9rem", cursor: "pointer" }}>
              <input type="checkbox" checked={settings[t.key]} disabled={mutation.isPending}
                onChange={(e) => mutation.mutate({ ...settings, [t.key]: e.target.checked })} />
              <span>
                <span style={{ fontWeight: 600 }}>{t.label}</span>
                <span style={{ display: "block", fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>{t.hint}</span>
              </span>
            </label>
          ))}
        </fieldset>
      </Box>
    </div>
  );
}
