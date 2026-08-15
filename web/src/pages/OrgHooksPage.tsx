import { useState } from "react";
import { Link, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchOrgHooksPage,
  createOrgHook,
  updateOrgHook,
  deleteOrgHook,
  pingOrgHook,
} from "../api.js";
import type { GithubOrgWebhook } from "../types.js";
import { OrgHeader } from "../components/PageHeader.js";
import { PageTitle, Box, Blankslate, Button, ErrorBanner, Modal, DialogActions, FormLabel } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { confirmAction } from "../components/confirmAction.js";

/** Organization webhooks: list, create, ping, enable/disable, and delete. */
export function OrgHooksPage() {
  const { org = "" } = useParams<{ org: string }>();
  const qc = useQueryClient();
  const [extra, setExtra] = useState<GithubOrgWebhook[]>([]);
  const [nextUrl, setNextUrl] = useState<string | null>(null);
  const [pageError, setPageError] = useState<string | null>(null);

  const [creating, setCreating] = useState(false);
  const [url, setUrl] = useState("");
  const [contentType, setContentType] = useState("json");
  const [events, setEvents] = useState("push");
  const [active, setActive] = useState(true);

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
    mutationFn: () =>
      createOrgHook(org, {
        url,
        contentType,
        events: events.split(",").map((e) => e.trim()).filter(Boolean),
        active,
      }),
    onSuccess: () => {
      refresh();
      setCreating(false);
      setUrl("");
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
          <FormLabel id="hook-url">Payload URL</FormLabel>
          <input
            id="hook-url"
            autoFocus
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://example.com/webhook"
            className="mb-3 w-full"
          />
          <FormLabel id="hook-content-type">Content type</FormLabel>
          <select
            id="hook-content-type"
            value={contentType}
            onChange={(e) => setContentType(e.target.value)}
            className="mb-3 w-full"
          >
            <option value="json">application/json</option>
            <option value="form">application/x-www-form-urlencoded</option>
          </select>
          <FormLabel id="hook-events">Events (comma-separated)</FormLabel>
          <input
            id="hook-events"
            value={events}
            onChange={(e) => setEvents(e.target.value)}
            placeholder="push, pull_request"
            className="mb-3 w-full"
          />
          <label className="mb-3 flex items-center gap-2" style={{ fontSize: "0.85rem" }}>
            <input type="checkbox" checked={active} onChange={(e) => setActive(e.target.checked)} />
            Active
          </label>
          <MutationError of={createMut} />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setCreating(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="sm"
              disabled={!url.trim() || createMut.isPending}
              onClick={() => createMut.mutate()}
            >
              {createMut.isPending ? "Creating…" : "Add webhook"}
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
                <Link to={`/ui/orgs/${org}/hooks/${h.id}/deliveries`}>
                  <Button variant="secondary" size="sm">
                    Deliveries
                  </Button>
                </Link>
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
