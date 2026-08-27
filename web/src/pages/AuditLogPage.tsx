import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { DataTable, InlineError, Spinner } from "@bleephub/ui-core/components";
import { createColumnHelper } from "@bleephub/ui-core/components";
import { buildAuditLogPhrase, fetchAuditLog, fetchAuditLogOrgs, ghFetch } from "../api.js";
import type { BleephubAuditEvent } from "../types.js";
import { Button, FormLabel, PageTitle } from "../components/ui.js";
import { RelativeTime } from "../components/RelativeTime.js";

const col = createColumnHelper<BleephubAuditEvent>();

const AUDIT_CSV_COLUMNS = [
  "id",
  "created_at",
  "actor_login",
  "action",
  "entity_type",
  "entity_id",
  "details",
] as const;

/** Serialize audit events to CSV (object fields like `details` become JSON). */
export function auditEventsToCsv(events: BleephubAuditEvent[]): string {
  const esc = (value: unknown) => {
    const s = value !== null && typeof value === "object" ? JSON.stringify(value) : String(value ?? "");
    return `"${s.replace(/"/g, '""')}"`;
  };
  const header = AUDIT_CSV_COLUMNS.join(",");
  const rows = events.map((e) =>
    AUDIT_CSV_COLUMNS.map((c) => esc((e as unknown as Record<string, unknown>)[c])).join(","),
  );
  return [header, ...rows].join("\n");
}

export interface ParsedAuditQuery {
  /** Free-text terms plus qualifier values, joined for the server's phrase matcher. */
  actor?: string;
  action?: string;
  text: string;
  /** ISO dates (YYYY-MM-DD), inclusive. */
  from?: string;
  to?: string;
}

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

// Parse GitHub-style audit qualifiers (action:/actor:/created:, with created:
// ranges and >=/<= forms). The server's phrase matcher is plain AND-of-substring
// with no qualifiers, so values fold into the phrase and created: ranges filter
// client-side.
export function parseAuditQuery(input: string): ParsedAuditQuery {
  const out: ParsedAuditQuery = { text: "" };
  const rest: string[] = [];
  for (const token of input.trim().split(/\s+/).filter(Boolean)) {
    const m = /^(action|actor|created):(.+)$/.exec(token);
    if (!m) {
      rest.push(token);
      continue;
    }
    const key = m[1] as "action" | "actor" | "created";
    const value = m[2]!;
    if (key === "action") out.action = value;
    else if (key === "actor") out.actor = value;
    else {
      const range = /^(\d{4}-\d{2}-\d{2})\.\.(\d{4}-\d{2}-\d{2})$/.exec(value);
      if (range) {
        out.from = range[1]!;
        out.to = range[2]!;
      } else if (value.startsWith(">=") && DATE_RE.test(value.slice(2))) {
        out.from = value.slice(2);
      } else if (value.startsWith("<=") && DATE_RE.test(value.slice(2))) {
        out.to = value.slice(2);
      } else if (DATE_RE.test(value)) {
        out.from = value;
        out.to = value;
      }
      // Ignore anything else after created: instead of searching it.
    }
  }
  out.text = rest.join(" ");
  return out;
}

/** Inclusive created_at date-range filter (dates as YYYY-MM-DD). */
export function filterByCreated(
  events: BleephubAuditEvent[],
  from?: string,
  to?: string,
): BleephubAuditEvent[] {
  if (!from && !to) return events;
  const fromTs = from ? Date.parse(`${from}T00:00:00Z`) : -Infinity;
  const toTs = to ? Date.parse(`${to}T23:59:59.999Z`) : Infinity;
  return events.filter((e) => {
    const ts = Date.parse(e.created_at);
    return !Number.isNaN(ts) && ts >= fromTs && ts <= toTs;
  });
}

function downloadTextFile(filename: string, mime: string, content: string): void {
  const url = URL.createObjectURL(new Blob([content], { type: mime }));
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

// Export walks at most this many 100-row pages; the cap is shown by the buttons.
const EXPORT_PAGE_CAP = 10;
const EXPORT_ROW_CAP = EXPORT_PAGE_CAP * 100;

interface RawAuditEntry {
  _document_id: number | string;
  "@timestamp": string;
  action: string;
  actor: string;
  org?: string;
  data?: Record<string, unknown>;
}

function rawToEvent(entry: RawAuditEntry, org: string): BleephubAuditEvent {
  return {
    id: Number(entry._document_id),
    actor_login: entry.actor,
    action: entry.action,
    entity_type: String(entry.data?.["entity_type"] ?? entry.data?.["target_type"] ?? entry.org ?? org),
    entity_id: String(entry.data?.["entity_id"] ?? entry.data?.["target_id"] ?? entry.data?.["repo"] ?? entry.org ?? org),
    details: entry.data ?? {},
    created_at: entry["@timestamp"],
  };
}

// Walk the audit-log pages for the current server filters, up to the cap, so
// exports cover all matching rows.
async function fetchAllAuditEvents(
  org: string,
  phrase: string | undefined,
  order: "asc" | "desc",
): Promise<BleephubAuditEvent[]> {
  const events: BleephubAuditEvent[] = [];
  for (let page = 1; page <= EXPORT_PAGE_CAP; page++) {
    const params = new URLSearchParams();
    params.set("include", "all");
    params.set("per_page", "100");
    params.set("page", String(page));
    params.set("order", order);
    if (phrase) params.set("phrase", phrase);
    const entries = await ghFetch<RawAuditEntry[]>(
      `/api/v3/orgs/${encodeURIComponent(org)}/audit-log?${params.toString()}`,
    );
    events.push(...entries.map((e) => rawToEvent(e, org)));
    if (entries.length < 100) break;
  }
  return events;
}

export function AuditLogPage() {
  const [org, setOrg] = useState("");
  const [actor, setActor] = useState("");
  const [action, setAction] = useState("");
  const [phrase, setPhrase] = useState("");
  const [fromDate, setFromDate] = useState("");
  const [toDate, setToDate] = useState("");
  const [order, setOrder] = useState<"desc" | "asc">("desc");
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<string | null>(null);
  const [appliedFilters, setAppliedFilters] = useState({
    org: "",
    actor: "",
    action: "",
    phrase: "",
    from: "",
    to: "",
    order: "desc" as "desc" | "asc",
  });

  const { data: orgs, isLoading: orgsLoading, isError: orgsError } = useQuery({
    queryKey: ["audit-log-orgs"],
    queryFn: ({ signal }) => fetchAuditLogOrgs(signal),
  });

  const effectiveOrg = appliedFilters.org || orgs?.[0]?.login || "";
  const effectivePhrase = buildAuditLogPhrase({
    actor: appliedFilters.actor || undefined,
    action: appliedFilters.action || undefined,
    text: appliedFilters.phrase || undefined,
  });

  const { data, isLoading, isError } = useQuery({
    queryKey: ["audit-log", appliedFilters],
    queryFn: () =>
      fetchAuditLog({
        org: effectiveOrg,
        phrase: effectivePhrase || undefined,
        order: appliedFilters.order,
      }),
    enabled: Boolean(effectiveOrg),
  });

  const apply = () => {
    // Phrase qualifiers merge with, and win over, the dedicated inputs.
    const parsed = parseAuditQuery(phrase);
    setAppliedFilters({
      org: org.trim() || effectiveOrg,
      actor: parsed.actor ?? actor.trim(),
      action: parsed.action ?? action.trim(),
      phrase: parsed.text,
      from: parsed.from ?? fromDate,
      to: parsed.to ?? toDate,
      order,
    });
  };

  const runExport = async (format: "csv" | "json") => {
    setExporting(true);
    setExportError(null);
    try {
      const all = await fetchAllAuditEvents(effectiveOrg, effectivePhrase || undefined, appliedFilters.order);
      const filtered = filterByCreated(all, appliedFilters.from || undefined, appliedFilters.to || undefined);
      if (format === "csv") {
        downloadTextFile(`audit-log-${effectiveOrg || "org"}.csv`, "text/csv", auditEventsToCsv(filtered));
      } else {
        downloadTextFile(
          `audit-log-${effectiveOrg || "org"}.json`,
          "application/json",
          JSON.stringify(filtered, null, 2),
        );
      }
    } catch (err) {
      setExportError(err instanceof Error ? err.message : String(err));
    } finally {
      setExporting(false);
    }
  };

  if (orgsError || isError) return <InlineError title="Failed to load audit log" />;

  const columns = [
    col.accessor("id", {
      header: "ID",
      cell: (info) => (
        <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
          {info.getValue()}
        </span>
      ),
    }),
    col.accessor("created_at", {
      header: "Time",
      cell: (info) => <RelativeTime iso={info.getValue()} />,
    }),
    col.accessor("actor_login", {
      header: "Actor",
      cell: (info) => <span style={{ fontWeight: 500, color: "var(--color-fg)" }}>{info.getValue()}</span>,
    }),
    col.accessor("action", {
      header: "Action",
      cell: (info) => <span style={{ color: "var(--color-fg-muted)" }}>{info.getValue()}</span>,
    }),
    col.accessor("entity_type", {
      header: "Entity type",
      cell: (info) => <span style={{ color: "var(--color-fg-muted)" }}>{info.getValue()}</span>,
    }),
    col.accessor("entity_id", {
      header: "Entity ID",
      cell: (info) => (
        <span className="tabular-nums" style={{ color: "var(--color-fg-muted)" }}>
          {String(info.getValue())}
        </span>
      ),
    }),
    col.accessor("details", {
      header: "Details",
      cell: (info) => {
        const details = info.getValue();
        return (
          <pre
            // Focusable so the scroll region is keyboard-scrollable
            // (axe scrollable-region-focusable).
            tabIndex={0}
            aria-label="Event details"
            style={{
              margin: 0,
              fontSize: "0.75rem",
              color: "var(--color-fg-muted)",
              maxWidth: "24rem",
              overflow: "auto",
            }}
          >
            {JSON.stringify(details, null, 2)}
          </pre>
        );
      },
    }),
  ];

  const events = filterByCreated(
    data ?? [],
    appliedFilters.from || undefined,
    appliedFilters.to || undefined,
  );
  return (
    <div>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <PageTitle title="Audit log" meta="GitHub Enterprise Server organization audit events." />
        <div className="flex flex-wrap items-center gap-2">
          <span style={{ fontSize: "0.72rem", color: "var(--color-fg-muted)" }}>
            Exports walk all pages of the current filters, up to {EXPORT_ROW_CAP.toLocaleString()} events.
          </span>
          <Button
            size="sm"
            disabled={!effectiveOrg || exporting}
            onClick={() => void runExport("csv")}
          >
            {exporting ? "Exporting…" : "Export CSV"}
          </Button>
          <Button
            size="sm"
            disabled={!effectiveOrg || exporting}
            onClick={() => void runExport("json")}
          >
            {exporting ? "Exporting…" : "Export JSON"}
          </Button>
        </div>
      </div>
      {exportError && (
        <div className="mb-3" style={{ color: "var(--color-danger-fg)", fontSize: "0.85rem" }}>
          Export failed: {exportError}
        </div>
      )}

      <div
        className="mb-5 grid gap-3"
        style={{ gridTemplateColumns: "repeat(auto-fit, minmax(160px, 1fr))" }}
      >
        <div>
          <FormLabel id="filter-org">Organization</FormLabel>
          <select id="filter-org" value={org || effectiveOrg} onChange={(e) => setOrg(e.target.value)}>
            {(orgs ?? []).map((o) => (
              <option key={o.login} value={o.login}>
                {o.login}
              </option>
            ))}
          </select>
        </div>
        <div>
          <FormLabel id="filter-actor">Actor</FormLabel>
          <input
            id="filter-actor"
            type="text"
            value={actor}
            onChange={(e) => setActor(e.target.value)}
            placeholder="username"
          />
        </div>
        <div>
          <FormLabel id="filter-action">Action</FormLabel>
          <input
            id="filter-action"
            type="text"
            value={action}
            onChange={(e) => setAction(e.target.value)}
            placeholder="create_user"
          />
        </div>
        <div>
          <FormLabel id="filter-phrase">Search phrase</FormLabel>
          <input
            id="filter-phrase"
            type="text"
            value={phrase}
            onChange={(e) => setPhrase(e.target.value)}
            placeholder="action:repo.create actor:admin created:2026-01-01..2026-02-01"
          />
        </div>
        <div>
          <FormLabel id="filter-from">From date</FormLabel>
          <input
            id="filter-from"
            type="date"
            value={fromDate}
            onChange={(e) => setFromDate(e.target.value)}
          />
        </div>
        <div>
          <FormLabel id="filter-to">To date</FormLabel>
          <input
            id="filter-to"
            type="date"
            value={toDate}
            onChange={(e) => setToDate(e.target.value)}
          />
        </div>
        <div>
          <FormLabel id="filter-order">Order</FormLabel>
          <select id="filter-order" value={order} onChange={(e) => setOrder(e.target.value as "desc" | "asc")}>
            <option value="desc">Newest first</option>
            <option value="asc">Oldest first</option>
          </select>
        </div>
        <div className="flex items-end">
          <Button onClick={apply} variant="secondary" size="sm">
            Apply filters
          </Button>
        </div>
        <p
          className="col-span-full"
          style={{ margin: 0, fontSize: "0.72rem", color: "var(--color-fg-muted)", gridColumn: "1 / -1" }}
        >
          The search phrase supports <code>action:</code>, <code>actor:</code>, and{" "}
          <code>created:YYYY-MM-DD..YYYY-MM-DD</code> (or <code>created:&gt;=YYYY-MM-DD</code> /{" "}
          <code>created:&lt;=YYYY-MM-DD</code>) qualifiers. Date filters are applied in the browser —
          the server&apos;s phrase search has no date support.
        </p>
      </div>

      {!effectiveOrg && !orgsLoading ? (
        <InlineError title="No organization audit log is available" detail="No organizations exist on this instance yet. Create an organization to record and view its audit events." />
      ) : orgsLoading || isLoading || !data ? (
        <Spinner label="loading audit log" />
      ) : (
        <DataTable
          data={events}
          columns={columns}
          filterPlaceholder="Filter events…"
          emptyMessage="No audit events match the filters."
        />
      )}
    </div>
  );
}
