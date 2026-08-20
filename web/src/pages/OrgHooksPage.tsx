import { useState } from "react";
import { useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchOrgHooksPage,
  updateOrgHook,
  deleteOrgHook,
  pingOrgHook,
  ghPostJSON,
  ghSend,
} from "../api.js";
import type { GithubOrgWebhook } from "../types.js";
import { OrgHeader } from "../components/PageHeader.js";
import { PageTitle, Box, Blankslate, Button, ButtonLink, ErrorBanner, Modal, DialogActions } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { confirmAction } from "../components/confirmAction.js";
import { WebhookForm, ORG_WEBHOOK_EVENT_CATALOG, type WebhookFormValues } from "../components/WebhookForm.js";

// Page-local hook writers. The entry-resident createOrgHook/updateOrgHook
// wrappers have no `secret` member in their config types, so the org form
// posts/patches through these lazy-page fetchers instead of widening api.ts.
// On PATCH, a blank/absent config.secret keeps the stored secret
// (internal/server/gh_org_hooks_rest.go mirrors the repo-hook handler).
const createOrgHookFull = (org: string, values: WebhookFormValues) =>
  ghPostJSON<GithubOrgWebhook>(`/api/v3/orgs/${encodeURIComponent(org)}/hooks`, {
    name: "web",
    active: values.active,
    events: values.events,
    config: {
      url: values.url,
      content_type: values.contentType,
      insecure_ssl: values.insecureSsl,
      ...(values.secret ? { secret: values.secret } : {}),
    },
  });
const patchOrgHookFull = (org: string, id: number, values: WebhookFormValues) =>
  ghSend("PATCH", `/api/v3/orgs/${encodeURIComponent(org)}/hooks/${id}`, {
    active: values.active,
    events: values.events,
    config: {
      url: values.url,
      content_type: values.contentType,
      insecure_ssl: values.insecureSsl,
      ...(values.secret ? { secret: values.secret } : {}),
    },
  });

/** Organization webhooks: list, create, edit, ping, enable/disable, and delete. */
export function OrgHooksPage() {
  const { org = "" } = useParams<{ org: string }>();
  const qc = useQueryClient();
  const [extra, setExtra] = useState<GithubOrgWebhook[]>([]);
  const [nextUrl, setNextUrl] = useState<string | null>(null);
  const [pageError, setPageError] = useState<string | null>(null);

  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<GithubOrgWebhook | null>(null);

  const firstPage = useQuery({
    queryKey: ["org-hooks", org],
    queryFn: () => fetchOrgHooksPage(org),
    enabled: !!org,
  });

  const refresh = () => {
    setExtra([]);
    setNextUrl(null);
    qc.invalidateQueries({ queryKey: ["org-hooks", org] });
  };
  const createMut = useMutation({
    mutationFn: (values: WebhookFormValues) => createOrgHookFull(org, values),
    onSuccess: () => {
      refresh();
      setCreating(false);
    },
  });
  const editMut = useMutation({
    mutationFn: ({ id, values }: { id: number; values: WebhookFormValues }) =>
      patchOrgHookFull(org, id, values),
    onSuccess: () => {
      refresh();
      setEditing(null);
    },
  });
  const toggleMut = useMutation({
    mutationFn: (h: GithubOrgWebhook) => updateOrgHook(org, h.id, { active: !h.active }),
    onSuccess: refresh,
  });
  const pingMut = useMutation({
    mutationFn: (id: number) => pingOrgHook(org, id),
  });
  const deleteMut = useMutation({
    mutationFn: (id: number) => deleteOrgHook(org, id),
    onSuccess: refresh,
  });

  if (firstPage.isLoading) return <Spinner label={`loading ${org} webhooks`} />;
  if (firstPage.isError)
    return (
      <div>
        <OrgHeader org={org} active="hooks" />
        <InlineError title="Failed to load organization webhooks" detail={String(firstPage.error)} />
      </div>
    );

  const hooks = [...(firstPage.data?.items ?? []), ...extra];
  const followUrl = nextUrl ?? firstPage.data?.nextUrl ?? null;

  const loadMore = async () => {
    if (!followUrl) return;
    try {
      const page = await fetchOrgHooksPage(org, followUrl);
      setExtra((prev) => [...prev, ...page.items]);
      setNextUrl(page.nextUrl);
      setPageError(null);
    } catch (err) {
      setPageError(String(err));
    }
  };

  return (
    <div>
      <OrgHeader org={org} active="hooks" />
      <div className="flex items-center justify-between">
        <PageTitle title="Webhooks" />
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
          New webhook
        </Button>
      </div>
      {pageError && <ErrorBanner>{pageError}</ErrorBanner>}
      <MutationError of={[toggleMut, pingMut, deleteMut]} />

      {creating && (
        <Modal title="Add organization webhook" onClose={() => setCreating(false)}>
          <WebhookForm
            eventCatalog={ORG_WEBHOOK_EVENT_CATALOG}
            submitLabel="Add webhook"
            pendingLabel="Creating…"
            pending={createMut.isPending}
            onSubmit={(values) => createMut.mutate(values)}
          />
          <MutationError of={createMut} />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setCreating(false)}>
              Cancel
            </Button>
          </DialogActions>
        </Modal>
      )}

      {editing && (
        <Modal title={`Edit webhook #${editing.id}`} onClose={() => setEditing(null)}>
          <WebhookForm
            eventCatalog={ORG_WEBHOOK_EVENT_CATALOG}
            initial={{
              url: editing.config.url,
              contentType: editing.config.content_type,
              insecureSsl: editing.config.insecure_ssl === "1" ? "1" : "0",
              events: editing.events,
              active: editing.active,
            }}
            editingWithSecret
            submitLabel="Update webhook"
            pendingLabel="Updating…"
            pending={editMut.isPending}
            onSubmit={(values) => editMut.mutate({ id: editing.id, values })}
          />
          <MutationError of={editMut} />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setEditing(null)}>
              Cancel
            </Button>
          </DialogActions>
        </Modal>
      )}

      {hooks.length === 0 ? (
        <Blankslate title="No organization webhooks">
          Create one with the “New webhook” button.
        </Blankslate>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: "0.75rem" }}>
          <Box>
            {hooks.map((h, i) => (
              <div
                key={h.id}
                className="flex items-center gap-3"
                style={{
                  padding: "0.7rem 1rem",
                  borderBottom: i < hooks.length - 1 ? "1px solid var(--color-border)" : "none",
                }}
              >
                <span
                  aria-hidden
                  style={{
                    width: 8,
                    height: 8,
                    borderRadius: "999px",
                    background: h.active ? "var(--gh-open)" : "var(--color-fg-subtle)",
                    flexShrink: 0,
                  }}
                />
                <div className="min-w-0 flex-1">
                  <div style={{ fontSize: "0.88rem", fontWeight: 500, color: "var(--color-fg)" }}>
                    {h.name}{" "}
                    <span style={{ color: "var(--color-fg-subtle)", fontWeight: 400 }}>#{h.id}</span>
                  </div>
                  <div
                    className="font-mono"
                    style={{ fontSize: "0.74rem", color: "var(--color-fg-muted)" }}
                  >
                    {h.config.url || "no url"} · events: {h.events.join(", ") || "none"}
                  </div>
                </div>
                <Button
                  size="sm"
                  aria-label={`Edit webhook ${h.id}`}
                  disabled={editMut.isPending}
                  onClick={() => setEditing(h)}
                >
                  Edit
                </Button>
                <Button
                  size="sm"
                  aria-label={`Ping webhook ${h.id}`}
                  disabled={pingMut.isPending}
                  onClick={() => pingMut.mutate(h.id)}
                >
                  Ping
                </Button>
                <Button
                  size="sm"
                  disabled={toggleMut.isPending}
                  onClick={() => toggleMut.mutate(h)}
                >
                  {h.active ? "Disable" : "Enable"}
                </Button>
                <Button
                  size="sm"
                  aria-label={`Delete webhook ${h.id}`}
                  disabled={deleteMut.isPending}
                  onClick={async () => {
                    if (
                      await confirmAction(`Delete webhook #${h.id}?`, {
                        title: "Delete webhook",
                        confirmLabel: "Delete",
                      })
                    ) {
                      deleteMut.mutate(h.id);
                    }
                  }}
                >
                  Delete
                </Button>
                <ButtonLink to={`/ui/orgs/${org}/hooks/${h.id}/deliveries`} variant="secondary" size="sm">
                  Deliveries
                </ButtonLink>
              </div>
            ))}
          </Box>
          {followUrl && (
            <div className="flex justify-center">
              <Button variant="secondary" size="sm" onClick={() => void loadMore()}>
                Load more
              </Button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
