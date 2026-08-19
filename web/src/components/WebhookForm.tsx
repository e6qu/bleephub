import { useId, useState } from "react";
import { Button, FormLabel } from "./ui.js";

/**
 * The webhook event catalog this server actually delivers. Derived from the
 * distinct eventType arguments passed to emitWebhookEvent in
 * internal/server (plus the org-level "organization" event) — not GitHub's
 * full ~70-event list, so every checkbox here is a real, subscribable event.
 */
export const WEBHOOK_EVENT_CATALOG: string[] = [
  "branch_protection_rule",
  "check_run",
  "check_suite",
  "commit_comment",
  "create",
  "delete",
  "deployment",
  "deployment_status",
  "fork",
  "issue_comment",
  "issues",
  "label",
  "member",
  "milestone",
  "page_build",
  "project",
  "project_card",
  "project_column",
  "public",
  "pull_request",
  "pull_request_review",
  "pull_request_review_comment",
  "push",
  "registry_package",
  "release",
  "repository_dispatch",
  "status",
  "watch",
  "workflow_job",
  "workflow_run",
];

/** Extra events only organization-level hooks receive. */
export const ORG_WEBHOOK_EVENT_CATALOG: string[] = [...WEBHOOK_EVENT_CATALOG, "organization"].sort();

export interface WebhookFormValues {
  url: string;
  contentType: string;
  /** Empty string = not set (create) / keep the current secret (edit). */
  secret: string;
  events: string[];
  active: boolean;
}

type EventMode = "push" | "all" | "select";

function modeFromEvents(events: string[]): EventMode {
  if (events.length === 1 && events[0] === "push") return "push";
  if (events.includes("*")) return "all";
  return events.length === 0 ? "push" : "select";
}

/**
 * Shared add/edit webhook form (repo + org hooks). GitHub-style event
 * selection: a radio trio (just push / everything / individual events) with a
 * checkbox grid of the server's known events.
 */
export function WebhookForm({
  initial,
  eventCatalog = WEBHOOK_EVENT_CATALOG,
  submitLabel,
  pendingLabel,
  pending = false,
  editingWithSecret = false,
  onSubmit,
}: {
  initial?: Partial<WebhookFormValues>;
  eventCatalog?: string[];
  submitLabel: string;
  pendingLabel?: string;
  pending?: boolean;
  /** Edit mode with a stored secret: blank keeps it (the server never echoes it). */
  editingWithSecret?: boolean;
  onSubmit: (values: WebhookFormValues) => void;
}) {
  const uid = useId();
  const initialEvents = initial?.events ?? ["push"];
  const [url, setUrl] = useState(initial?.url ?? "");
  const [contentType, setContentType] = useState(initial?.contentType ?? "json");
  const [secret, setSecret] = useState("");
  const [active, setActive] = useState(initial?.active ?? true);
  const [mode, setMode] = useState<EventMode>(modeFromEvents(initialEvents));
  const [selected, setSelected] = useState<Set<string>>(
    () => new Set(initialEvents.filter((e) => e !== "*")),
  );

  const toggleEvent = (name: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });

  const events =
    mode === "push" ? ["push"] : mode === "all" ? ["*"] : [...selected].sort();

  const submit = () =>
    onSubmit({ url: url.trim(), contentType, secret: secret.trim(), events, active });

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "0.5rem" }}>
      <FormLabel id={`${uid}-hook-url`}>Payload URL</FormLabel>
      <input
        id={`${uid}-hook-url`}
        type="text"
        value={url}
        onChange={(e) => setUrl(e.target.value)}
        placeholder="https://example.com/webhook"
        className="w-full"
      />
      <FormLabel id={`${uid}-hook-content-type`}>Content type</FormLabel>
      <select
        id={`${uid}-hook-content-type`}
        value={contentType}
        onChange={(e) => setContentType(e.target.value)}
        className="w-full"
      >
        <option value="json">application/json</option>
        <option value="form">application/x-www-form-urlencoded</option>
      </select>
      <FormLabel id={`${uid}-hook-secret`}>Secret{editingWithSecret ? "" : " (optional)"}</FormLabel>
      <input
        id={`${uid}-hook-secret`}
        type="password"
        autoComplete="off"
        value={secret}
        onChange={(e) => setSecret(e.target.value)}
        placeholder={editingWithSecret ? "Leave blank to keep the current secret" : "Signing secret"}
        className="w-full"
      />
      <fieldset style={{ border: "none", padding: 0, margin: "0.25rem 0 0" }}>
        <legend style={{ fontSize: "0.85rem", fontWeight: 500, marginBottom: "0.35rem" }}>
          Which events would you like to trigger this webhook?
        </legend>
        <div style={{ display: "flex", flexDirection: "column", gap: "0.3rem" }}>
          <label style={radioRow}>
            <input
              type="radio"
              name={`${uid}-hook-events-mode`}
              checked={mode === "push"}
              onChange={() => setMode("push")}
            />
            Just the push event
          </label>
          <label style={radioRow}>
            <input
              type="radio"
              name={`${uid}-hook-events-mode`}
              checked={mode === "all"}
              onChange={() => setMode("all")}
            />
            Send me everything
          </label>
          <label style={radioRow}>
            <input
              type="radio"
              name={`${uid}-hook-events-mode`}
              checked={mode === "select"}
              onChange={() => setMode("select")}
            />
            Let me select individual events
          </label>
        </div>
        {mode === "select" && (
          <div
            role="group"
            aria-label="Individual events"
            style={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fill, minmax(13rem, 1fr))",
              gap: "0.15rem 0.75rem",
              marginTop: "0.5rem",
              padding: "0.6rem 0.75rem",
              border: "1px solid var(--color-border)",
              borderRadius: "var(--radius-md)",
            }}
          >
            {eventCatalog.map((name) => (
              <label
                key={name}
                style={{
                  display: "flex",
                  alignItems: "center",
                  gap: "0.4rem",
                  fontSize: "0.82rem",
                  fontFamily: "var(--font-mono, monospace)",
                  minHeight: "1.625rem",
                }}
              >
                <input
                  type="checkbox"
                  checked={selected.has(name)}
                  onChange={() => toggleEvent(name)}
                />
                {name}
              </label>
            ))}
          </div>
        )}
      </fieldset>
      <label className="flex items-center gap-2" style={{ fontSize: "0.85rem", minHeight: "1.625rem" }}>
        <input type="checkbox" checked={active} onChange={(e) => setActive(e.target.checked)} />
        Active
      </label>
      <div className="flex justify-end">
        <Button
          variant="primary"
          disabled={pending || !url.trim() || (mode === "select" && selected.size === 0)}
          onClick={submit}
        >
          {pending ? pendingLabel ?? submitLabel : submitLabel}
        </Button>
      </div>
    </div>
  );
}

const radioRow = {
  display: "flex",
  alignItems: "center",
  gap: "0.4rem",
  fontSize: "0.85rem",
  minHeight: "1.625rem",
} as const;
