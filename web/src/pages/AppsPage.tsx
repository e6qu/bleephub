import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { DataTable, InlineError, Spinner } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@tanstack/react-table";
import { useState } from "react";
import { Link } from "react-router";
import {
  addInstallationRepository,
  createApp,
  createOAuthApp,
  deleteApp,
  deleteInstallation,
  deleteOAuthApp,
  fetchApps,
  fetchAppSettings,
  fetchInstallableRepositories,
  fetchInstallationRepositories,
  fetchInstallations,
  fetchMarketplaceAccounts,
  fetchOAuthApps,
  installApp,
  removeInstallationRepository,
  rotateAppClientSecret,
  rotateAppPrivateKey,
  rotateOAuthAppClientSecret,
  suspendInstallation,
  updateAppSettings,
  updateOAuthApp,
} from "../api.js";
import type {
  BleephubApp,
  BleephubInstallation,
  BleephubOAuthApp,
  GithubMarketplaceAccount,
} from "../types.js";
import {
  PageTitle,
  Tabs,
  Button,
  Modal,
  FormLabel,
  ErrorBanner,
  DialogActions,
  CodeBlock,
} from "../components/ui.js";

type Tab = "apps" | "installations" | "oauth-apps";

export function AppsPage() {
  const [tab, setTab] = useState<Tab>("apps");
  const [showCreate, setShowCreate] = useState<"app" | "oauth-app" | null>(null);

  return (
    <div>
      <PageTitle
        title="Apps & installations"
        meta="GitHub Apps, OAuth Apps, and the active installations between them."
        actions={
          tab === "oauth-apps" ? (
            <Button variant="primary" size="sm" onClick={() => setShowCreate("oauth-app")}>
              New OAuth app
            </Button>
          ) : (
            <Button variant="primary" size="sm" onClick={() => setShowCreate("app")}>
              New GitHub app
            </Button>
          )
        }
      />

      <Tabs
        items={[
          { key: "apps", label: "GitHub Apps" },
          { key: "installations", label: "Installations" },
          { key: "oauth-apps", label: "OAuth Apps" },
        ]}
        active={tab}
        onChange={setTab}
      />

      {tab === "apps" && <AppsTab />}
      {tab === "installations" && <InstallationsTab />}
      {tab === "oauth-apps" && <OAuthAppsTab />}

      {showCreate === "app" && <CreateAppDialog onClose={() => setShowCreate(null)} />}
      {showCreate === "oauth-app" && <CreateOAuthAppDialog onClose={() => setShowCreate(null)} />}
    </div>
  );
}

const appsCol = createColumnHelper<BleephubApp>();

function AppsTab() {
  const [settingsSlug, setSettingsSlug] = useState<string | null>(null);
  const [installSlug, setInstallSlug] = useState<string | null>(null);
  const { data, isLoading, isError } = useQuery({
    queryKey: ["apps"],
    queryFn: fetchApps,
    refetchInterval: 5000,
  });
  if (isError) return <InlineError title="Failed to load apps" />;
  if (isLoading || !data) return <Spinner label="loading apps" />;

  const columns = [
    appsCol.accessor("id", {
      header: "Identifier",
      cell: (info) => (
        <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
          {info.getValue()}
        </span>
      ),
    }),
    appsCol.accessor("slug", {
      header: "Slug",
      cell: (info) => (
        <Link
          to={`/ui/apps/${info.getValue()}/marketplace`}
          style={{ color: "var(--color-accent)", fontWeight: 600 }}
        >
          {info.getValue()}
        </Link>
      ),
    }),
    appsCol.accessor("name", {
      header: "Name",
      cell: (info) => <span style={{ color: "var(--color-fg)", fontWeight: 500 }}>{info.getValue()}</span>,
    }),
    appsCol.accessor("description", {
      header: "Description",
      cell: (info) => <span style={{ color: "var(--color-fg-muted)" }}>{info.getValue()}</span>,
    }),
    appsCol.accessor("createdAt", {
      header: "Created",
      cell: (info) => new Date(info.getValue()).toLocaleString(),
    }),
    appsCol.display({
      id: "actions",
      header: "Actions",
      cell: (info) => (
        <div className="flex flex-wrap gap-2">
          <Button size="sm" variant="secondary" onClick={() => setSettingsSlug(info.row.original.slug)}>
            Settings
          </Button>
          <Button size="sm" variant="primary" onClick={() => setInstallSlug(info.row.original.slug)}>
            Install
          </Button>
          <Link
            to={`/ui/apps/${info.row.original.slug}/marketplace`}
            style={{ color: "var(--color-accent)", fontSize: "0.78rem", alignSelf: "center" }}
          >
            Marketplace
          </Link>
        </div>
      ),
    }),
  ];

  return (
    <>
      <DataTable
        data={data}
        columns={columns}
        filterPlaceholder="Filter apps…"
        emptyMessage="No apps yet. Create a GitHub App through the manifest flow."
      />
      {settingsSlug && <AppSettingsDialog slug={settingsSlug} onClose={() => setSettingsSlug(null)} />}
      {installSlug && <InstallAppDialog slug={installSlug} onClose={() => setInstallSlug(null)} />}
    </>
  );
}

const installsCol = createColumnHelper<BleephubInstallation>();

function InstallationsTab() {
  const queryClient = useQueryClient();
  const [mutationError, setMutationError] = useState<string | null>(null);
  const [manageRepositories, setManageRepositories] = useState<BleephubInstallation | null>(null);
  const { data, isLoading, isError } = useQuery({
    queryKey: ["installations"],
    queryFn: fetchInstallations,
    refetchInterval: 5000,
  });

  const suspendMut = useMutation({
    mutationFn: ({ id, suspend }: { id: number; suspend: boolean }) => suspendInstallation(id, suspend),
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["installations"] });
    },
    onError: (err: Error) => setMutationError(err.message),
  });
  const deleteMut = useMutation({
    mutationFn: (id: number) => deleteInstallation(id),
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["installations"] });
    },
    onError: (err: Error) => setMutationError(err.message),
  });

  if (isError) return <InlineError title="Failed to load installations" />;
  if (isLoading || !data) return <Spinner label="loading installations" />;

  const columns = [
    installsCol.accessor("id", {
      header: "Identifier",
      cell: (info) => (
        <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
          {info.getValue()}
        </span>
      ),
    }),
    installsCol.accessor("appSlug", {
      header: "App",
      cell: (info) => <span style={{ color: "var(--color-accent)" }}>{info.getValue()}</span>,
    }),
    installsCol.accessor("targetLogin", {
      header: "Target",
      cell: (info) => <span style={{ color: "var(--color-fg)", fontWeight: 500 }}>{info.getValue()}</span>,
    }),
    installsCol.accessor("targetType", {
      header: "Type",
      cell: (info) => <span style={{ color: "var(--color-fg-muted)" }}>{info.getValue()}</span>,
    }),
    installsCol.accessor("repositorySelection", {
      header: "Repo selection",
      cell: (info) => <span style={{ color: "var(--color-fg-muted)" }}>{info.getValue()}</span>,
    }),
    installsCol.accessor("suspendedAt", {
      header: "Status",
      cell: (info) => {
        const suspended = !!info.getValue();
        return (
          <span
            style={{
              fontSize: "0.78rem",
              fontWeight: 500,
              color: suspended ? "var(--color-status-warn)" : "var(--color-status-ok)",
            }}
          >
            {suspended ? "suspended" : "active"}
          </span>
        );
      },
    }),
    installsCol.display({
      id: "actions",
      header: "Actions",
      cell: (info) => {
        const inst = info.row.original;
        const suspended = !!inst.suspendedAt;
        return (
          <div className="flex gap-2">
            {inst.repositorySelection === "selected" && (
              <Button size="sm" variant="secondary" onClick={() => setManageRepositories(inst)}>
                repositories
              </Button>
            )}
            <Button
              size="sm"
              variant="ghost"
              onClick={() => suspendMut.mutate({ id: inst.id, suspend: !suspended })}
              disabled={suspendMut.isPending}
            >
              {suspended ? "unsuspend" : "suspend"}
            </Button>
            <Button
              size="sm"
              variant="danger"
              onClick={() => {
                if (confirm(`Delete installation #${inst.id}?`)) {
                  deleteMut.mutate(inst.id);
                }
              }}
              disabled={deleteMut.isPending}
            >
              delete
            </Button>
          </div>
        );
      },
    }),
    installsCol.accessor("createdAt", {
      header: "Created",
      cell: (info) => new Date(info.getValue()).toLocaleString(),
    }),
  ];

  return (
    <>
      {mutationError && <ErrorBanner>{mutationError}</ErrorBanner>}
      <DataTable
        data={data}
        columns={columns}
        filterPlaceholder="Filter installations…"
        emptyMessage="No installations."
      />
      {manageRepositories && (
        <InstallationRepositoriesDialog
          installation={manageRepositories}
          onClose={() => setManageRepositories(null)}
        />
      )}
    </>
  );
}

const oauthCol = createColumnHelper<BleephubOAuthApp>();

function OAuthAppsTab() {
  const [settingsClientId, setSettingsClientId] = useState<string | null>(null);
  const { data, isLoading, isError } = useQuery({
    queryKey: ["oauth-apps"],
    queryFn: fetchOAuthApps,
    refetchInterval: 5000,
  });
  if (isError) return <InlineError title="Failed to load oauth apps" />;
  if (isLoading || !data) return <Spinner label="loading oauth apps" />;
  const settingsApp = data.find((item) => item.clientId === settingsClientId);

  const columns = [
    oauthCol.accessor("clientId", {
      header: "Client identifier",
      cell: (info) => (
        <span className="font-mono" style={{ color: "var(--color-accent)" }}>
          {info.getValue()}
        </span>
      ),
    }),
    oauthCol.accessor("name", {
      header: "Name",
      cell: (info) => <span style={{ color: "var(--color-fg)", fontWeight: 500 }}>{info.getValue()}</span>,
    }),
    oauthCol.accessor("description", {
      header: "Description",
      cell: (info) => <span style={{ color: "var(--color-fg-muted)" }}>{info.getValue()}</span>,
    }),
    oauthCol.accessor("callbackUrl", {
      header: "Callback",
      cell: (info) => (
        <span className="font-mono" style={{ color: "var(--color-fg-muted)" }}>
          {info.getValue() || "—"}
        </span>
      ),
    }),
    oauthCol.accessor("createdAt", {
      header: "Created",
      cell: (info) => new Date(info.getValue()).toLocaleString(),
    }),
    oauthCol.display({
      id: "actions",
      header: "Actions",
      cell: (info) => (
        <Button size="sm" variant="secondary" onClick={() => setSettingsClientId(info.row.original.clientId)}>
          Settings
        </Button>
      ),
    }),
  ];

  return (
    <>
      <DataTable
        data={data}
        columns={columns}
        filterPlaceholder="Filter OAuth apps…"
        emptyMessage="No OAuth Apps yet."
      />
      {settingsApp && (
        <OAuthAppSettingsDialog
          app={settingsApp}
          onClose={() => setSettingsClientId(null)}
        />
      )}
    </>
  );
}

const allPermScopes = [
  "metadata",
  "contents",
  "issues",
  "pull_requests",
  "actions",
  "checks",
  "secrets",
  "administration",
  "members",
  "discussions",
  "deployments",
  "organization_administration",
  "security_events",
  "dependabot_secrets",
  "codespaces",
  "reactions",
  "projects",
  "pages",
  "organization_personal_access_token_requests",
  "organization_personal_access_tokens",
];

const allEvents = [
  "push",
  "pull_request",
  "issues",
  "issue_comment",
  "installation",
  "installation_repositories",
  "check_run",
  "check_suite",
  "discussion",
  "discussion_comment",
  "deployment",
  "deployment_status",
  "release",
  "repository",
  "repository_dispatch",
  "security_advisory",
  "workflow_dispatch",
  "workflow_run",
];

function CreateAppDialog({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [url, setURL] = useState("");
  const [callbackURL, setCallbackURL] = useState("");
  const [webhookURL, setWebhookURL] = useState("");
  const [webhookActive, setWebhookActive] = useState(true);
  const [perms, setPerms] = useState<Record<string, string>>({ metadata: "read" });
  const [events, setEvents] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<{
    pem: string;
    client_id?: string;
    client_secret: string;
    webhook_secret: string;
  } | null>(null);

  const mutation = useMutation({
    mutationFn: () =>
      createApp({
        name,
        description,
        url,
        callback_url: callbackURL,
        webhook_url: webhookURL,
        webhook_active: webhookActive,
        permissions: Object.keys(perms).length ? perms : undefined,
        events: events.length ? events : undefined,
      }),
    onSuccess: (resp) => {
      queryClient.invalidateQueries({ queryKey: ["apps"] });
      setCreated({
        pem: resp.pem,
        client_id: resp.clientId,
        client_secret: resp.client_secret,
        webhook_secret: resp.webhook_secret,
      });
    },
    onError: (err: Error) => setError(err.message),
  });

  if (created) {
    return <CreatedAppDialog created={created} onClose={onClose} />;
  }

  return (
    <Modal title="Create GitHub app" onClose={onClose}>
      <FormLabel id="app-name">Name</FormLabel>
      <input
        id="app-name"
        type="text"
        value={name}
        onChange={(e) => setName(e.target.value)}
        className="mb-4 w-full"
      />

      <FormLabel id="app-desc">Description</FormLabel>
      <textarea
        id="app-desc"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        rows={2}
        className="mb-4 w-full"
        style={{ resize: "vertical" }}
      />

      <div className="mb-4 grid gap-3 sm:grid-cols-2">
        <SettingsField label="Homepage URL" value={url} onChange={setURL} />
        <SettingsField label="Callback URL" value={callbackURL} onChange={setCallbackURL} />
        <SettingsField label="Webhook URL" value={webhookURL} onChange={setWebhookURL} />
        <label className="flex items-center gap-2" style={{ alignSelf: "end", minHeight: "2rem" }}>
          <input type="checkbox" checked={webhookActive} onChange={(event) => setWebhookActive(event.target.checked)} />
          Active webhook
        </label>
      </div>

      <FormLabel>Permissions</FormLabel>
      <div className="mb-4 grid grid-cols-2 gap-2 sm:grid-cols-3">
        {allPermScopes.map((scope) => (
          <select
            key={scope}
            aria-label={`${scope} permission`}
            value={scope === "metadata" ? "read" : perms[scope] || ""}
            disabled={scope === "metadata"}
            onChange={(e) => {
              const v = e.target.value;
              setPerms((cur) => {
                const next = { ...cur };
                if (v === "") delete next[scope];
                else next[scope] = v;
                return next;
              });
            }}
            style={{ fontSize: "0.78rem", padding: "0.3rem 0.4rem" }}
          >
            {scope !== "metadata" && <option value="">{scope}: —</option>}
            <option value="read">{scope}: read</option>
            {scope !== "metadata" && <option value="write">{scope}: write</option>}
            {scope !== "metadata" && <option value="admin">{scope}: admin</option>}
          </select>
        ))}
      </div>

      <FormLabel>Events</FormLabel>
      <div className="mb-4 flex flex-wrap gap-2">
        {allEvents.map((ev) => {
          const on = events.includes(ev);
          return (
            <button
              type="button"
              key={ev}
              aria-pressed={on}
              onClick={() => setEvents((cur) => (on ? cur.filter((e) => e !== ev) : [...cur, ev]))}
              style={{
                fontSize: "0.76rem",
                padding: "0.2rem 0.55rem",
                background: on ? "var(--color-accent)" : "var(--color-bg-subtle)",
                color: on ? "var(--color-accent-fg)" : "var(--color-fg-muted)",
                border: "1px solid var(--color-border)",
                borderRadius: "2rem",
                cursor: "pointer",
              }}
            >
              {ev}
            </button>
          );
        })}
      </div>

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
          disabled={mutation.isPending || !name.trim()}
          variant="primary"
        >
          {mutation.isPending ? "Creating…" : "Create app"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

function CreatedAppDialog({
  created,
  onClose,
}: {
  created: { pem: string; client_id?: string; client_secret: string; webhook_secret: string };
  onClose: () => void;
}) {
  return (
    <Modal title="Save these now" onClose={onClose}>
      <p className="mb-4" style={{ fontSize: "0.82rem", color: "var(--color-status-warn)" }}>
        These values will not be shown again. Copy them before closing this dialog.
      </p>

      {created.client_id && (
        <>
          <FormLabel>Client identifier</FormLabel>
          <div className="mb-4">
            <CodeBlock>{created.client_id}</CodeBlock>
          </div>
        </>
      )}

      <FormLabel>Client secret</FormLabel>
      <div className="mb-4">
        <CodeBlock>{created.client_secret}</CodeBlock>
      </div>

      <FormLabel>Webhook secret</FormLabel>
      <div className="mb-4">
        <CodeBlock>{created.webhook_secret}</CodeBlock>
      </div>

      <FormLabel>Privacy Enhanced Mail private key</FormLabel>
      <div className="mb-4">
        <CodeBlock>{created.pem}</CodeBlock>
      </div>

      <DialogActions>
        <Button onClick={onClose} variant="primary">
          I copied them
        </Button>
      </DialogActions>
    </Modal>
  );
}

function CreateOAuthAppDialog({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [url, setURL] = useState("");
  const [callbackURL, setCallbackURL] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<{ client_id: string; client_secret: string } | null>(null);

  const mutation = useMutation({
    mutationFn: () => createOAuthApp({ name, description, url, callback_url: callbackURL }),
    onSuccess: (resp) => {
      queryClient.invalidateQueries({ queryKey: ["oauth-apps"] });
      setCreated({ client_id: resp.clientId, client_secret: resp.client_secret });
    },
    onError: (err: Error) => setError(err.message),
  });

  if (created) {
    return (
      <Modal title="Save your credentials" onClose={onClose}>
        <p className="mb-4" style={{ fontSize: "0.82rem", color: "var(--color-status-warn)" }}>
          The client secret is shown once. Copy it now.
        </p>
        <FormLabel>Client identifier</FormLabel>
        <div className="mb-4">
          <CodeBlock>{created.client_id}</CodeBlock>
        </div>
        <FormLabel>Client secret</FormLabel>
        <div className="mb-4">
          <CodeBlock>{created.client_secret}</CodeBlock>
        </div>
        <DialogActions>
          <Button onClick={onClose} variant="primary">
            I copied it
          </Button>
        </DialogActions>
      </Modal>
    );
  }

  return (
    <Modal title="Create OAuth app" onClose={onClose}>
      <FormLabel id="oa-name">Name</FormLabel>
      <input id="oa-name" type="text" value={name} onChange={(e) => setName(e.target.value)} className="mb-4 w-full" />

      <FormLabel id="oa-desc">Description</FormLabel>
      <input
        id="oa-desc"
        type="text"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        className="mb-4 w-full"
      />

      <FormLabel id="oa-url">Homepage URL</FormLabel>
      <input
        id="oa-url"
        type="text"
        value={url}
        onChange={(e) => setURL(e.target.value)}
        className="mb-4 w-full"
        placeholder="https://example.test"
      />

      <FormLabel id="oa-cb">Callback URL</FormLabel>
      <input
        id="oa-cb"
        type="text"
        value={callbackURL}
        onChange={(e) => setCallbackURL(e.target.value)}
        className="mb-4 w-full"
        placeholder="https://example.test/auth/callback"
      />

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
          disabled={mutation.isPending || !name.trim() || !url.trim() || !callbackURL.trim()}
          variant="primary"
        >
          {mutation.isPending ? "Creating…" : "Create app"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

function AppSettingsDialog({ slug, onClose }: { slug: string; onClose: () => void }) {
  const queryClient = useQueryClient();
  const appQuery = useQuery({
    queryKey: ["app-settings", slug],
    queryFn: () => fetchAppSettings(slug),
  });
  const [draft, setDraft] = useState<BleephubApp | null>(null);
  const [credential, setCredential] = useState<{ title: string; value: string } | null>(null);

  const app = draft ?? appQuery.data ?? null;
  const save = useMutation({
    mutationFn: () =>
      updateAppSettings(slug, {
        name: app!.name,
        description: app!.description,
        url: app!.url,
        callback_url: app!.callbackUrl,
        webhook_url: app!.webhookUrl,
        webhook_active: app!.webhookActive,
        webhook_content_type: app!.webhookContentType,
        permissions: app!.permissions,
        events: app!.events,
      }),
    onSuccess: async (updated) => {
      setDraft(updated);
      await queryClient.invalidateQueries({ queryKey: ["apps"] });
      await queryClient.invalidateQueries({ queryKey: ["app-settings", slug] });
    },
  });
  const rotateSecret = useMutation({
    mutationFn: () => rotateAppClientSecret(slug),
    onSuccess: ({ client_secret }) => setCredential({ title: "New client secret", value: client_secret }),
  });
  const rotateKey = useMutation({
    mutationFn: () => rotateAppPrivateKey(slug),
    onSuccess: ({ pem }) => setCredential({ title: "New private key", value: pem }),
  });
  const remove = useMutation({
    mutationFn: () => deleteApp(slug),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["apps"] });
      await queryClient.invalidateQueries({ queryKey: ["installations"] });
      onClose();
    },
  });

  if (appQuery.isError) {
    return <Modal title={`Settings for ${slug}`} onClose={onClose}><InlineError title="Failed to load app settings" detail={String(appQuery.error)} /></Modal>;
  }
  if (appQuery.isLoading || !app) {
    return <Modal title={`Settings for ${slug}`} onClose={onClose}><Spinner label="loading app settings" /></Modal>;
  }
  const update = (patch: Partial<BleephubApp>) => setDraft({ ...app, ...patch });
  const error = save.error || rotateSecret.error || rotateKey.error || remove.error;

  return (
    <>
      <Modal title={`GitHub App settings · ${slug}`} onClose={onClose}>
        <div className="grid gap-3 sm:grid-cols-2">
          <SettingsField label="Name" value={app.name} onChange={(name) => update({ name })} />
          <SettingsField label="Client identifier" value={app.clientId} readOnly />
          <SettingsField label="Homepage URL" value={app.url} onChange={(url) => update({ url })} />
          <SettingsField label="Callback URL" value={app.callbackUrl} onChange={(callbackUrl) => update({ callbackUrl })} />
          <SettingsField label="Webhook URL" value={app.webhookUrl} onChange={(webhookUrl) => update({ webhookUrl })} />
          <div>
            <FormLabel id="app-webhook-content-type">Webhook content type</FormLabel>
            <select
              id="app-webhook-content-type"
              className="w-full"
              value={app.webhookContentType}
              onChange={(event) => update({ webhookContentType: event.target.value as "json" | "form" })}
            >
              <option value="json">application/json</option>
              <option value="form">application/x-www-form-urlencoded</option>
            </select>
          </div>
        </div>
        <div className="mt-3">
          <FormLabel id="app-settings-description">Description</FormLabel>
          <textarea
            id="app-settings-description"
            className="w-full"
            rows={3}
            value={app.description}
            onChange={(event) => update({ description: event.target.value })}
          />
        </div>
        <label className="mt-3 flex items-center gap-2">
          <input
            type="checkbox"
            checked={app.webhookActive}
            onChange={(event) => update({ webhookActive: event.target.checked })}
          />
          Deliver webhooks
        </label>
        <PermissionEditor
          permissions={app.permissions}
          onChange={(permissions) => update({ permissions })}
        />
        <EventEditor events={app.events} onChange={(events) => update({ events })} />
        {error && <ErrorBanner>{String(error)}</ErrorBanner>}
        <div className="mt-4 flex flex-wrap gap-2">
          <Button size="sm" variant="secondary" onClick={() => rotateSecret.mutate()} disabled={rotateSecret.isPending}>
            Generate client secret
          </Button>
          <Button size="sm" variant="secondary" onClick={() => rotateKey.mutate()} disabled={rotateKey.isPending}>
            Generate private key
          </Button>
          <Button
            size="sm"
            variant="danger"
            onClick={() => {
              if (confirm(`Delete GitHub App ${app.name} and revoke all of its credentials?`)) remove.mutate();
            }}
            disabled={remove.isPending}
          >
            Delete app
          </Button>
        </div>
        <DialogActions>
          <Button variant="ghost" onClick={onClose}>Close</Button>
          <Button
            variant="primary"
            onClick={() => save.mutate()}
            disabled={save.isPending || !app.name.trim() || !app.url.trim()}
          >
            {save.isPending ? "Saving…" : "Save settings"}
          </Button>
        </DialogActions>
      </Modal>
      {credential && (
        <OneTimeCredentialDialog
          title={credential.title}
          value={credential.value}
          onClose={() => setCredential(null)}
        />
      )}
    </>
  );
}

function OAuthAppSettingsDialog({ app, onClose }: { app: BleephubOAuthApp; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState(app);
  const [secret, setSecret] = useState<string | null>(null);
  const save = useMutation({
    mutationFn: () =>
      updateOAuthApp(app.clientId, {
        name: draft.name,
        description: draft.description,
        url: draft.url,
        callback_url: draft.callbackUrl,
      }),
    onSuccess: async (updated) => {
      setDraft(updated);
      await queryClient.invalidateQueries({ queryKey: ["oauth-apps"] });
    },
  });
  const rotate = useMutation({
    mutationFn: () => rotateOAuthAppClientSecret(app.clientId),
    onSuccess: ({ client_secret }) => setSecret(client_secret),
  });
  const remove = useMutation({
    mutationFn: () => deleteOAuthApp(app.clientId),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["oauth-apps"] });
      onClose();
    },
  });
  const error = save.error || rotate.error || remove.error;
  return (
    <>
      <Modal title={`OAuth App settings · ${app.name}`} onClose={onClose}>
        <div className="grid gap-3 sm:grid-cols-2">
          <SettingsField label="Name" value={draft.name} onChange={(name) => setDraft({ ...draft, name })} />
          <SettingsField label="Client identifier" value={draft.clientId} readOnly />
          <SettingsField label="Homepage URL" value={draft.url} onChange={(url) => setDraft({ ...draft, url })} />
          <SettingsField label="Callback URL" value={draft.callbackUrl} onChange={(callbackUrl) => setDraft({ ...draft, callbackUrl })} />
        </div>
        <div className="mt-3">
          <SettingsField label="Description" value={draft.description} onChange={(description) => setDraft({ ...draft, description })} />
        </div>
        {error && <ErrorBanner>{String(error)}</ErrorBanner>}
        <div className="mt-4 flex flex-wrap gap-2">
          <Button size="sm" variant="secondary" onClick={() => rotate.mutate()} disabled={rotate.isPending}>
            Generate client secret
          </Button>
          <Button
            size="sm"
            variant="danger"
            onClick={() => {
              if (confirm(`Delete OAuth App ${draft.name} and revoke all grants?`)) remove.mutate();
            }}
            disabled={remove.isPending}
          >
            Delete app
          </Button>
        </div>
        <DialogActions>
          <Button variant="ghost" onClick={onClose}>Close</Button>
          <Button
            variant="primary"
            onClick={() => save.mutate()}
            disabled={save.isPending || !draft.name.trim() || !draft.url.trim() || !draft.callbackUrl.trim()}
          >
            {save.isPending ? "Saving…" : "Save settings"}
          </Button>
        </DialogActions>
      </Modal>
      {secret && <OneTimeCredentialDialog title="New client secret" value={secret} onClose={() => setSecret(null)} />}
    </>
  );
}

function InstallAppDialog({ slug, onClose }: { slug: string; onClose: () => void }) {
  const queryClient = useQueryClient();
  const accounts = useQuery({ queryKey: ["marketplace", "accounts"], queryFn: fetchMarketplaceAccounts });
  const [target, setTarget] = useState<GithubMarketplaceAccount | null>(null);
  const [selection, setSelection] = useState<"all" | "selected">("all");
  const [repoIds, setRepoIds] = useState<number[]>([]);
  const account = target ?? accounts.data?.[0] ?? null;
  const repos = useQuery({
    queryKey: ["installable-repositories", account?.type, account?.login],
    queryFn: () => fetchInstallableRepositories(account!.login, account!.type),
    enabled: !!account && selection === "selected",
  });
  const mutation = useMutation({
    mutationFn: () =>
      installApp(slug, {
        target_login: account!.login,
        repository_selection: selection,
        repository_ids: repoIds,
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["installations"] });
      onClose();
    },
  });
  return (
    <Modal title={`Install ${slug}`} onClose={onClose}>
      {accounts.isLoading ? <Spinner label="loading installable accounts" /> : accounts.isError ? (
        <InlineError title="Failed to load installable accounts" detail={String(accounts.error)} />
      ) : (
        <>
          <FormLabel id="install-account">Account</FormLabel>
          <select
            id="install-account"
            className="mb-4 w-full"
            value={account ? `${account.type}:${account.id}` : ""}
            onChange={(event) => {
              const selected = accounts.data?.find((item) => `${item.type}:${item.id}` === event.target.value) ?? null;
              setTarget(selected);
              setRepoIds([]);
            }}
          >
            {accounts.data?.map((item) => (
              <option key={`${item.type}:${item.id}`} value={`${item.type}:${item.id}`}>
                {item.login} ({item.type === "Organization" ? "organization" : "personal"})
              </option>
            ))}
          </select>
          <FormLabel id="install-repository-selection">Repository access</FormLabel>
          <select
            id="install-repository-selection"
            className="mb-4 w-full"
            value={selection}
            onChange={(event) => {
              setSelection(event.target.value as "all" | "selected");
              setRepoIds([]);
            }}
          >
            <option value="all">All repositories</option>
            <option value="selected">Only selected repositories</option>
          </select>
          {selection === "selected" && (
            <RepositoryChecklist
              repos={repos.data ?? []}
              selected={repoIds}
              loading={repos.isLoading}
              error={repos.error}
              onChange={setRepoIds}
            />
          )}
          {mutation.error && <ErrorBanner>{String(mutation.error)}</ErrorBanner>}
          <DialogActions>
            <Button variant="ghost" onClick={onClose}>Cancel</Button>
            <Button
              variant="primary"
              onClick={() => mutation.mutate()}
              disabled={!account || mutation.isPending || (selection === "selected" && repoIds.length === 0)}
            >
              {mutation.isPending ? "Installing…" : "Install app"}
            </Button>
          </DialogActions>
        </>
      )}
    </Modal>
  );
}

function InstallationRepositoriesDialog({
  installation,
  onClose,
}: {
  installation: BleephubInstallation;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const available = useQuery({
    queryKey: ["installable-repositories", installation.targetType, installation.targetLogin],
    queryFn: () =>
      fetchInstallableRepositories(
        installation.targetLogin,
        installation.targetType as "User" | "Organization",
      ),
  });
  const installed = useQuery({
    queryKey: ["installation-repositories", installation.id],
    queryFn: () => fetchInstallationRepositories(installation.id),
  });
  const installedIds = new Set(installed.data?.repositories.map((repo) => repo.id) ?? []);
  const mutation = useMutation({
    mutationFn: ({ repoId, add }: { repoId: number; add: boolean }) =>
      add
        ? addInstallationRepository(installation.id, repoId)
        : removeInstallationRepository(installation.id, repoId),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["installation-repositories", installation.id] }),
  });
  return (
    <Modal title={`Repositories for ${installation.appSlug} on ${installation.targetLogin}`} onClose={onClose}>
      {available.isLoading || installed.isLoading ? (
        <Spinner label="loading installation repositories" />
      ) : available.isError || installed.isError ? (
        <InlineError title="Failed to load installation repositories" detail={String(available.error || installed.error)} />
      ) : (
        <div className="grid gap-2">
          {available.data?.map((repo) => {
            const enabled = installedIds.has(repo.id);
            return (
              <label key={repo.id} className="flex items-center justify-between gap-3 rounded border p-2">
                <span>{repo.full_name}</span>
                <input
                  type="checkbox"
                  checked={enabled}
                  disabled={mutation.isPending}
                  onChange={() => mutation.mutate({ repoId: repo.id, add: !enabled })}
                />
              </label>
            );
          })}
        </div>
      )}
      {mutation.error && <ErrorBanner>{String(mutation.error)}</ErrorBanner>}
      <DialogActions><Button variant="primary" onClick={onClose}>Done</Button></DialogActions>
    </Modal>
  );
}

function SettingsField({
  label,
  value,
  onChange,
  readOnly = false,
}: {
  label: string;
  value: string;
  onChange?: (value: string) => void;
  readOnly?: boolean;
}) {
  const id = `settings-${label.toLowerCase().replace(/[^a-z0-9]+/g, "-")}`;
  return (
    <div>
      <FormLabel id={id}>{label}</FormLabel>
      <input
        id={id}
        className="w-full"
        value={value}
        readOnly={readOnly}
        onChange={(event) => onChange?.(event.target.value)}
      />
    </div>
  );
}

function PermissionEditor({
  permissions,
  onChange,
}: {
  permissions: Record<string, string>;
  onChange: (permissions: Record<string, string>) => void;
}) {
  return (
    <div className="mt-4">
      <FormLabel>Permissions</FormLabel>
      <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
        {allPermScopes.map((scope) => (
          <select
            key={scope}
            aria-label={`${scope} permission`}
            value={scope === "metadata" ? "read" : permissions[scope] ?? ""}
            disabled={scope === "metadata"}
            onChange={(event) => {
              const next = { ...permissions };
              if (event.target.value) next[scope] = event.target.value;
              else delete next[scope];
              onChange(next);
            }}
          >
            {scope !== "metadata" && <option value="">{scope}: no access</option>}
            <option value="read">{scope}: read</option>
            {scope !== "metadata" && <option value="write">{scope}: write</option>}
            {scope !== "metadata" && <option value="admin">{scope}: admin</option>}
          </select>
        ))}
      </div>
    </div>
  );
}

function EventEditor({ events, onChange }: { events: string[]; onChange: (events: string[]) => void }) {
  return (
    <fieldset className="mt-4">
      <legend style={{ fontSize: "0.82rem", fontWeight: 600 }}>Webhook events</legend>
      <div className="mt-2 grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
        {allEvents.map((eventName) => (
          <label key={eventName} className="flex items-center gap-2">
            <input
              type="checkbox"
              checked={events.includes(eventName)}
              onChange={(event) =>
                onChange(
                  event.target.checked
                    ? [...events, eventName]
                    : events.filter((value) => value !== eventName),
                )
              }
            />
            {eventName}
          </label>
        ))}
      </div>
    </fieldset>
  );
}

function RepositoryChecklist({
  repos,
  selected,
  loading,
  error,
  onChange,
}: {
  repos: Array<{ id: number; full_name: string }>;
  selected: number[];
  loading: boolean;
  error: Error | null;
  onChange: (ids: number[]) => void;
}) {
  if (loading) return <Spinner label="loading repositories" />;
  if (error) return <InlineError title="Failed to load repositories" detail={String(error)} />;
  if (repos.length === 0) return <p style={{ color: "var(--color-fg-muted)" }}>No repositories are available.</p>;
  return (
    <fieldset className="mb-4 grid max-h-64 gap-2 overflow-auto">
      <legend style={{ fontSize: "0.82rem", fontWeight: 600 }}>Select repositories</legend>
      {repos.map((repo) => (
        <label key={repo.id} className="flex items-center gap-2">
          <input
            type="checkbox"
            checked={selected.includes(repo.id)}
            onChange={(event) =>
              onChange(
                event.target.checked
                  ? [...selected, repo.id]
                  : selected.filter((id) => id !== repo.id),
              )
            }
          />
          {repo.full_name}
        </label>
      ))}
    </fieldset>
  );
}

function OneTimeCredentialDialog({
  title,
  value,
  onClose,
}: {
  title: string;
  value: string;
  onClose: () => void;
}) {
  return (
    <Modal title={title} onClose={onClose}>
      <p className="mb-3" style={{ color: "var(--color-status-warn)" }}>
        This value is shown once. Save it before closing.
      </p>
      <CodeBlock>{value}</CodeBlock>
      <DialogActions><Button variant="primary" onClick={onClose}>I copied it</Button></DialogActions>
    </Modal>
  );
}
