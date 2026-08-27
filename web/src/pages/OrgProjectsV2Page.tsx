import { useState } from "react";
import { Link, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchOrgProjectsV2,
  fetchOrgProjectV2,
  fetchOrgProjectV2Items,
  fetchOrgProjectV2Fields,
  addOrgProjectV2Item,
  createOrgProjectV2Draft,
  deleteOrgProjectV2Item,
  setOrgProjectV2ItemField,
  ghGraphQL,
} from "../api.js";
import { OrgHeader } from "../components/PageHeader.js";
import { PageTitle, Box, Blankslate, Button, ErrorBanner } from "../components/ui.js";
import { confirmAction } from "../components/confirmAction.js";
import type { GithubProjectV2Field, GithubProjectV2Item } from "../types.js";

/** The option id a single-select field currently holds on this item, or null. */
function itemOptionId(item: GithubProjectV2Item, fieldId: number): string | null {
  const fv = item.fields?.find((f) => f.id === fieldId);
  if (fv && fv.value && typeof fv.value === "object" && "id" in (fv.value as object)) {
    return String((fv.value as { id: string }).id);
  }
  return null;
}

/** Display an item's value for one field. */
function itemFieldValue(item: GithubProjectV2Item, field: GithubProjectV2Field): string {
  const fv = item.fields?.find((f) => f.id === field.id);
  if (!fv || fv.value == null) return "";
  const v = fv.value;
  if (field.data_type === "single_select" && typeof v === "object" && "id" in (v as object)) {
    const optId = String((v as { id: string }).id);
    return field.options?.find((o) => o.id === optId)?.name.raw ?? "";
  }
  if (typeof v === "object") {
    const o = v as Record<string, unknown>;
    return String(o.name ?? o.title ?? o.date ?? o.raw ?? JSON.stringify(v));
  }
  return String(v);
}

export function OrgProjectsV2Page() {
  const { org = "", number } = useParams<{ org: string; number?: string }>();
  return (
    <div>
      <OrgHeader org={org} active="projects" />
      {number ? <ProjectV2Detail org={org} number={Number(number)} /> : <ProjectsV2List org={org} />}
    </div>
  );
}

function ProjectsV2List({ org }: { org: string }) {
  const q = useQuery({
    queryKey: ["projects-v2", org],
    queryFn: () => fetchOrgProjectsV2(org),
    enabled: !!org,
  });
  if (q.isLoading) return <Spinner label="loading projects" />;
  if (q.isError) return <InlineError title="Failed to load projects" detail={String(q.error)} />;
  const projects = q.data ?? [];
  if (projects.length === 0) return <Blankslate title="No projects" />;
  return (
    <Box>
      {projects.map((p, i) => (
        <Link
          key={p.id}
          to={`/ui/orgs/${org}/projects/${p.number}`}
          className="flex items-center gap-2"
          style={{
            padding: "0.7rem 1rem",
            borderBottom: i === projects.length - 1 ? "none" : "1px solid var(--color-border)",
            textDecoration: "none",
            color: "inherit",
          }}
        >
          <span style={{ fontWeight: 600 }}>{p.title}</span>
          <span style={{ color: "var(--color-fg-muted)", fontSize: "0.8rem" }}>#{p.number}</span>
        </Link>
      ))}
    </Box>
  );
}

function ProjectV2Detail({ org, number }: { org: string; number: number }) {
  const qc = useQueryClient();
  const projectQ = useQuery({
    queryKey: ["project-v2", org, number],
    queryFn: () => fetchOrgProjectV2(org, number),
  });
  const itemsQ = useQuery({
    queryKey: ["project-v2-items", org, number],
    queryFn: () => fetchOrgProjectV2Items(org, number),
  });
  const fieldsQ = useQuery({
    queryKey: ["project-v2-fields", org, number],
    queryFn: () => fetchOrgProjectV2Fields(org, number),
  });
  const moveMut = useMutation({
    mutationFn: ({ itemId, fieldId, value }: { itemId: number; fieldId: number; value: string | number }) =>
      setOrgProjectV2ItemField(org, number, itemId, fieldId, value),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["project-v2-items", org, number] }),
  });
  const [type, setType] = useState<"Issue" | "PullRequest">("Issue");
  const [contentRepo, setContentRepo] = useState("");
  const [contentNumber, setContentNumber] = useState("");
  const invalidate = () => void qc.invalidateQueries({ queryKey: ["project-v2-items", org, number] });
  const addMut = useMutation({
    mutationFn: () => {
      const [owner = "", repo = ""] = contentRepo.trim().split("/");
      return addOrgProjectV2Item(org, number, { type, owner, repo, number: Number(contentNumber.trim()) });
    },
    onSuccess: () => {
      invalidate();
      setContentRepo("");
      setContentNumber("");
    },
  });
  const deleteMut = useMutation({
    mutationFn: (itemId: number) => deleteOrgProjectV2Item(org, number, itemId),
    onSuccess: invalidate,
  });
  const [view, setView] = useState<"table" | "board">("table");
  const [filterText, setFilterText] = useState("");
  const [groupFieldId, setGroupFieldId] = useState<number | "none" | "">("");
  const [draftTitle, setDraftTitle] = useState("");
  const draftMut = useMutation({
    mutationFn: () => createOrgProjectV2Draft(org, number, { title: draftTitle.trim() }),
    onSuccess: () => {
      invalidate();
      setDraftTitle("");
    },
  });

  if (projectQ.isLoading) return <Spinner label="loading project" />;
  if (projectQ.isError || !projectQ.data) {
    return <InlineError title="Failed to load project" detail={String(projectQ.error)} />;
  }
  const allItems = itemsQ.data ?? [];
  const needle = filterText.trim().toLowerCase();
  const items = needle
    ? allItems.filter((it) => (it.content?.title ?? "").toLowerCase().includes(needle))
    : allItems;
  // Board grouping field: the user's choice, defaulting to the first with
  // options; "none" renders ungrouped.
  const singleSelectFields = (fieldsQ.data ?? []).filter(
    (f) => f.data_type === "single_select" && (f.options?.length ?? 0) > 0,
  );
  const groupField =
    groupFieldId === "none"
      ? undefined
      : groupFieldId === ""
        ? singleSelectFields[0]
        : singleSelectFields.find((f) => f.id === groupFieldId);
  const removeItem = async (itemId: number) => {
    if (await confirmAction("Remove this item from the project?")) deleteMut.mutate(itemId);
  };
  const validRepo = /^[^/]+\/[^/]+$/.test(contentRepo.trim());
  const validNumber = /^\d+$/.test(contentNumber.trim());
  return (
    <>
      <div className="mb-3">
        <Link to={`/ui/orgs/${org}/projects`} style={{ color: "var(--color-accent)", textDecoration: "none" }}>
          ← Projects
        </Link>
      </div>
      <PageTitle title={projectQ.data.title} meta={`#${projectQ.data.number}`} />
      <ProjectOverviewPanel org={org} number={number} />
      <Box className="mt-3" header={<span style={{ fontWeight: 600 }}>Add an item</span>}>
        <div style={{ padding: "1rem", display: "flex", flexWrap: "wrap", gap: "0.5rem", alignItems: "flex-end" }}>
          {addMut.error && <ErrorBanner>{String(addMut.error)}</ErrorBanner>}
          <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
            Type
            <select aria-label="item type" value={type} onChange={(e) => setType(e.target.value as "Issue" | "PullRequest")}>
              <option value="Issue">Issue</option>
              <option value="PullRequest">Pull request</option>
            </select>
          </label>
          <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
            Repository
            <input aria-label="item repo" value={contentRepo} onChange={(e) => setContentRepo(e.target.value)} placeholder="owner/repo" />
          </label>
          <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
            Number
            <input aria-label="item number" value={contentNumber} onChange={(e) => setContentNumber(e.target.value)} placeholder="42" />
          </label>
          <Button
            variant="primary"
            disabled={addMut.isPending || !validRepo || !validNumber}
            onClick={() => addMut.mutate()}
          >
            Add item
          </Button>
        </div>
        <div style={{ padding: "0 1rem 1rem", display: "flex", gap: "0.5rem", alignItems: "flex-end" }}>
          {draftMut.error && <ErrorBanner>{String(draftMut.error)}</ErrorBanner>}
          <label className="flex min-w-0 flex-1 flex-col gap-1" style={{ fontSize: "0.78rem" }}>
            New draft
            <input
              aria-label="draft title"
              value={draftTitle}
              onChange={(e) => setDraftTitle(e.target.value)}
              placeholder="Draft title"
              className="w-full"
            />
          </label>
          <Button
            variant="secondary"
            disabled={draftMut.isPending || !draftTitle.trim()}
            onClick={() => draftMut.mutate()}
          >
            Add draft
          </Button>
        </div>
      </Box>
      {deleteMut.error && <ErrorBanner>{String(deleteMut.error)}</ErrorBanner>}
      {moveMut.error && <ErrorBanner>{String(moveMut.error)}</ErrorBanner>}
      <div className="mt-3 flex flex-wrap items-end gap-3">
        <div role="group" aria-label="View" className="flex gap-1">
          {(["table", "board"] as const).map((v) => (
            <Button
              key={v}
              size="sm"
              variant={view === v ? "primary" : "secondary"}
              aria-pressed={view === v}
              onClick={() => setView(v)}
            >
              {v === "table" ? "Table" : "Board"}
            </Button>
          ))}
        </div>
        <label className="flex flex-col gap-1" style={{ fontSize: "0.72rem", color: "var(--color-fg-muted)" }}>
          Filter items
          <input
            aria-label="Filter items by title"
            value={filterText}
            onChange={(e) => setFilterText(e.target.value)}
            placeholder="Filter by title…"
            style={{ fontSize: "0.8rem", padding: "0.2rem 0.4rem" }}
          />
        </label>
        {singleSelectFields.length > 0 && (
          <label className="flex flex-col gap-1" style={{ fontSize: "0.72rem", color: "var(--color-fg-muted)" }}>
            Group by
            <select
              aria-label="Group items by field"
              value={groupFieldId === "" ? String(singleSelectFields[0]?.id ?? "none") : String(groupFieldId)}
              onChange={(e) => setGroupFieldId(e.target.value === "none" ? "none" : Number(e.target.value))}
              style={{ fontSize: "0.8rem", padding: "0.2rem 0.4rem" }}
            >
              {singleSelectFields.map((f) => (
                <option key={f.id} value={String(f.id)}>
                  {f.name}
                </option>
              ))}
              <option value="none">No grouping</option>
            </select>
          </label>
        )}
      </div>
      {itemsQ.isLoading ? (
        <Spinner label="loading items" />
      ) : items.length === 0 ? (
        <Blankslate title="No items" />
      ) : view === "table" ? (
        <ProjectTableView
          items={items}
          fields={fieldsQ.data ?? []}
          busy={moveMut.isPending || deleteMut.isPending}
          onMove={(itemId, fieldId, value) => moveMut.mutate({ itemId, fieldId, value })}
          onRemove={(itemId) => void removeItem(itemId)}
        />
      ) : groupField ? (
        <div className="mt-3 flex gap-3" style={{ overflowX: "auto" }}>
          {[...(groupField.options ?? []).map((o) => ({ id: o.id, label: o.name.raw })), { id: null, label: `No ${groupField.name}` }].map(
            (col) => {
              const colItems = items.filter((it) => itemOptionId(it, groupField.id) === col.id);
              return (
                <div key={col.id ?? "none"} style={{ minWidth: "15rem", flex: "0 0 15rem" }}>
                  <Box header={<span style={{ fontWeight: 600 }}>{col.label} ({colItems.length})</span>}>
                    <div style={{ padding: "0.5rem", display: "flex", flexDirection: "column", gap: "0.5rem" }}>
                      {colItems.map((it) => (
                        <ItemCard
                          key={it.id}
                          item={it}
                          groupField={groupField}
                          busy={moveMut.isPending || deleteMut.isPending}
                          onMove={(optionId) => moveMut.mutate({ itemId: it.id, fieldId: groupField.id, value: optionId })}
                          onRemove={() => void removeItem(it.id)}
                        />
                      ))}
                      {colItems.length === 0 && (
                        <div style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem", padding: "0.25rem" }}>Empty</div>
                      )}
                    </div>
                  </Box>
                </div>
              );
            },
          )}
        </div>
      ) : (
        <Box className="mt-3">
          {items.map((it) => (
            <div key={it.id} style={{ padding: "0.5rem 0.8rem", borderBottom: "1px solid var(--color-border)" }}>
              <ItemCard
                item={it}
                groupField={undefined}
                busy={deleteMut.isPending}
                onMove={() => {}}
                onRemove={() => void removeItem(it.id)}
              />
            </div>
          ))}
        </Box>
      )}
    </>
  );
}

// ─── Project overview, status updates and workflows ──────────────────────────
//
// Via GraphQL: the REST Projects v2 surface covers only items, fields and
// views, so description/status-updates/workflows have no REST route. Declared
// page-local to keep the entry chunk from growing.

interface ProjectOverview {
  id: string;
  shortDescription: string | null;
  readme: string | null;
  public: boolean;
  closed: boolean;
  template: boolean;
  viewerCanUpdate: boolean;
  statusUpdates: { nodes: ProjectStatusUpdate[] };
  workflows: { nodes: ProjectWorkflow[] };
}

interface ProjectStatusUpdate {
  id: string;
  body: string | null;
  status: string | null;
  startDate: string | null;
  targetDate: string | null;
  createdAt: string;
  creator: { login: string } | null;
}

interface ProjectWorkflow {
  id: string;
  name: string;
  number: number;
  enabled: boolean;
}

const PROJECT_OVERVIEW_QUERY = `query($org:String!,$number:Int!){
  organization(login:$org){
    projectV2(number:$number){
      id shortDescription readme public closed template viewerCanUpdate
      statusUpdates(first:20){ nodes{ id body status startDate targetDate createdAt creator{ login } } }
      workflows(first:20){ nodes{ id name number enabled } }
    }
  }
}`;

const UPDATE_PROJECT_MUTATION = `mutation($id:ID!,$shortDescription:String,$readme:String){
  updateProjectV2(input:{projectId:$id,shortDescription:$shortDescription,readme:$readme}){
    projectV2{ id }
  }
}`;

const CREATE_STATUS_UPDATE_MUTATION = `mutation($id:ID!,$body:String,$status:ProjectV2StatusUpdateStatus){
  createProjectV2StatusUpdate(input:{projectId:$id,body:$body,status:$status}){ statusUpdate{ id } }
}`;

const DELETE_STATUS_UPDATE_MUTATION = `mutation($id:ID!){
  deleteProjectV2StatusUpdate(input:{statusUpdateId:$id}){ deletedStatusUpdateId }
}`;

const STATUS_CHOICES = ["INACTIVE", "ON_TRACK", "AT_RISK", "OFF_TRACK", "COMPLETE"] as const;

/** SCREAMING_SNAKE status to display label. */
function statusLabel(status: string): string {
  return status.charAt(0) + status.slice(1).toLowerCase().replace(/_/g, " ");
}

function fetchProjectOverview(org: string, number: number) {
  return ghGraphQL<{ organization: { projectV2: ProjectOverview | null } | null }>(
    PROJECT_OVERVIEW_QUERY,
    { org, number },
  ).then((d) => d.organization?.projectV2 ?? null);
}

function ProjectOverviewPanel({ org, number }: { org: string; number: number }) {
  const qc = useQueryClient();
  const key = ["project-v2-overview", org, number];
  const q = useQuery({ queryKey: key, queryFn: () => fetchProjectOverview(org, number) });
  const [editing, setEditing] = useState(false);
  const [description, setDescription] = useState("");
  const [readme, setReadme] = useState("");
  const [statusBody, setStatusBody] = useState("");
  const [status, setStatus] = useState<string>("ON_TRACK");
  const invalidate = () => void qc.invalidateQueries({ queryKey: key });

  const saveMut = useMutation({
    mutationFn: () =>
      ghGraphQL(UPDATE_PROJECT_MUTATION, {
        id: q.data?.id,
        shortDescription: description,
        readme,
      }),
    onSuccess: () => {
      invalidate();
      setEditing(false);
    },
  });
  const statusMut = useMutation({
    mutationFn: () =>
      ghGraphQL(CREATE_STATUS_UPDATE_MUTATION, { id: q.data?.id, body: statusBody.trim(), status }),
    onSuccess: () => {
      invalidate();
      setStatusBody("");
    },
  });
  const deleteStatusMut = useMutation({
    mutationFn: (id: string) => ghGraphQL(DELETE_STATUS_UPDATE_MUTATION, { id }),
    onSuccess: invalidate,
  });

  if (q.isLoading) return <Spinner label="loading project details" />;
  if (q.isError) return <InlineError title="Failed to load project details" detail={String(q.error)} />;
  const project = q.data;
  if (!project) return null;

  const beginEditing = () => {
    setDescription(project.shortDescription ?? "");
    setReadme(project.readme ?? "");
    setEditing(true);
  };

  return (
    <>
      <Box
        className="mt-3"
        header={
          <div className="flex items-center justify-between gap-2">
            <span style={{ fontWeight: 600 }}>About</span>
            {project.viewerCanUpdate && !editing && (
              <Button size="sm" variant="secondary" onClick={beginEditing}>
                Edit details
              </Button>
            )}
          </div>
        }
      >
        <div style={{ padding: "0.8rem 1rem", display: "flex", flexDirection: "column", gap: "0.6rem" }}>
          {saveMut.error && <ErrorBanner>{String(saveMut.error)}</ErrorBanner>}
          {editing ? (
            <>
              <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
                Short description
                <input
                  aria-label="project short description"
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                  className="w-full"
                />
              </label>
              <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
                README
                <textarea
                  aria-label="project readme"
                  value={readme}
                  onChange={(e) => setReadme(e.target.value)}
                  rows={4}
                  className="w-full"
                />
              </label>
              <div className="flex gap-2">
                <Button variant="primary" disabled={saveMut.isPending} onClick={() => saveMut.mutate()}>
                  Save
                </Button>
                <Button variant="secondary" onClick={() => setEditing(false)}>
                  Cancel
                </Button>
              </div>
            </>
          ) : (
            <>
              <p style={{ margin: 0 }}>
                {project.shortDescription ?? (
                  <span style={{ color: "var(--color-fg-muted)" }}>No description</span>
                )}
              </p>
              {project.readme && (
                <pre
                  style={{
                    margin: 0,
                    whiteSpace: "pre-wrap",
                    fontFamily: "inherit",
                    color: "var(--color-fg-muted)",
                    fontSize: "0.85rem",
                  }}
                >
                  {project.readme}
                </pre>
              )}
              <div className="flex flex-wrap gap-2" style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
                <span>{project.public ? "Public" : "Private"}</span>
                <span>{project.closed ? "Closed" : "Open"}</span>
                {project.template && <span>Template</span>}
              </div>
            </>
          )}
        </div>
      </Box>

      <Box className="mt-3" header={<span style={{ fontWeight: 600 }}>Status updates</span>}>
        <div style={{ padding: "0.8rem 1rem", display: "flex", flexDirection: "column", gap: "0.6rem" }}>
          {statusMut.error && <ErrorBanner>{String(statusMut.error)}</ErrorBanner>}
          {deleteStatusMut.error && <ErrorBanner>{String(deleteStatusMut.error)}</ErrorBanner>}
          {project.viewerCanUpdate && (
            <div className="flex flex-wrap items-end gap-2">
              <label className="flex min-w-0 flex-1 flex-col gap-1" style={{ fontSize: "0.78rem" }}>
                New status update
                <input
                  aria-label="status update body"
                  value={statusBody}
                  onChange={(e) => setStatusBody(e.target.value)}
                  placeholder="How is it going?"
                  className="w-full"
                />
              </label>
              <label className="flex flex-col gap-1" style={{ fontSize: "0.78rem" }}>
                Status
                <select aria-label="status update status" value={status} onChange={(e) => setStatus(e.target.value)}>
                  {STATUS_CHOICES.map((choice) => (
                    <option key={choice} value={choice}>
                      {statusLabel(choice)}
                    </option>
                  ))}
                </select>
              </label>
              <Button
                variant="secondary"
                disabled={statusMut.isPending || !statusBody.trim()}
                onClick={() => statusMut.mutate()}
              >
                Post update
              </Button>
            </div>
          )}
          {project.statusUpdates.nodes.length === 0 ? (
            <p style={{ margin: 0, color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>No status updates yet.</p>
          ) : (
            <ul style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: "0.5rem" }}>
              {project.statusUpdates.nodes.map((update) => (
                <li
                  key={update.id}
                  className="flex items-start justify-between gap-2"
                  style={{ borderTop: "1px solid var(--color-border)", paddingTop: "0.5rem" }}
                >
                  <div className="min-w-0">
                    <div style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
                      {update.status ? statusLabel(update.status) : "No status"}
                      {update.creator ? ` · ${update.creator.login}` : ""}
                    </div>
                    <div>{update.body}</div>
                  </div>
                  {project.viewerCanUpdate && (
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={deleteStatusMut.isPending}
                      onClick={() => deleteStatusMut.mutate(update.id)}
                    >
                      Delete
                    </Button>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      </Box>

      {project.workflows.nodes.length > 0 && (
        <Box className="mt-3" header={<span style={{ fontWeight: 600 }}>Workflows</span>}>
          <ul style={{ listStyle: "none", margin: 0, padding: "0.5rem 1rem" }}>
            {project.workflows.nodes.map((workflow) => (
              <li key={workflow.id} className="flex items-center justify-between gap-2" style={{ padding: "0.25rem 0" }}>
                <span>{workflow.name}</span>
                <span style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
                  {workflow.enabled ? "Enabled" : "Disabled"}
                </span>
              </li>
            ))}
          </ul>
        </Box>
      )}
    </>
  );
}

// Editable cell: single-select is a dropdown, text/number/date an input
// committed on blur, anything else read-only.
function ProjectFieldCell({
  item,
  field,
  busy,
  onSet,
}: {
  item: GithubProjectV2Item;
  field: GithubProjectV2Field;
  busy: boolean;
  onSet: (value: string | number) => void;
}) {
  const label = `${field.name} for item ${item.id}`;
  const cellStyle = { padding: "0.5rem 0.8rem" };
  if (field.data_type === "single_select") {
    return (
      <td style={cellStyle}>
        <select
          aria-label={label}
          value={itemOptionId(item, field.id) ?? ""}
          onChange={(e) => onSet(e.target.value)}
          disabled={busy}
          style={{ fontSize: "0.78rem" }}
        >
          <option value="" disabled>
            Set {field.name}…
          </option>
          {(field.options ?? []).map((o) => (
            <option key={o.id} value={o.id}>
              {o.name.raw}
            </option>
          ))}
        </select>
      </td>
    );
  }
  if (field.data_type === "text" || field.data_type === "number" || field.data_type === "date") {
    const raw = itemFieldValue(item, field);
    const initial = field.data_type === "date" ? raw.slice(0, 10) : raw;
    return (
      <td style={cellStyle}>
        <input
          aria-label={label}
          type={field.data_type === "text" ? "text" : field.data_type}
          defaultValue={initial}
          disabled={busy}
          style={{ fontSize: "0.78rem", maxWidth: "9rem" }}
          onBlur={(e) => {
            const v = e.target.value;
            if (v === initial) return;
            onSet(field.data_type === "number" && v !== "" ? Number(v) : v);
          }}
        />
      </td>
    );
  }
  return <td style={{ ...cellStyle, color: "var(--color-fg-muted)" }}>{itemFieldValue(item, field) || "—"}</td>;
}

// Spreadsheet table: one row per item, one column per field, editable inline.
function ProjectTableView({
  items,
  fields,
  busy,
  onMove,
  onRemove,
}: {
  items: GithubProjectV2Item[];
  fields: GithubProjectV2Field[];
  busy: boolean;
  onMove: (itemId: number, fieldId: number, value: string | number) => void;
  onRemove: (itemId: number) => void;
}) {
  // Title is its own column; drop any field named "Title".
  const columns = fields.filter((f) => f.name.toLowerCase() !== "title");
  return (
    <Box className="mt-3">
      <div style={{ overflowX: "auto" }}>
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: "0.85rem" }}>
          <caption className="sr-only">Project items</caption>
          <thead>
            <tr style={{ borderBottom: "1px solid var(--color-border)" }}>
              <th scope="col" style={{ textAlign: "left", padding: "0.5rem 0.8rem" }}>
                Title
              </th>
              {columns.map((f) => (
                <th key={f.id} scope="col" style={{ textAlign: "left", padding: "0.5rem 0.8rem" }}>
                  {f.name}
                </th>
              ))}
              <th scope="col" style={{ textAlign: "right", padding: "0.5rem 0.8rem" }}>
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {items.map((it) => (
              <tr key={it.id} style={{ borderBottom: "1px solid var(--color-border)" }}>
                <th scope="row" style={{ textAlign: "left", padding: "0.5rem 0.8rem", fontWeight: 400 }}>
                  <span style={{ fontSize: "0.7rem", color: "var(--color-fg-muted)" }}>{it.content_type}</span>{" "}
                  {it.content?.html_url ? (
                    <Link
                      to={it.content.html_url}
                      style={{
                        display: "inline-block",
                        color: "var(--color-accent)",
                        textDecoration: "none",
                        lineHeight: "1.625rem",
                      }}
                    >
                      {it.content?.title ?? "(untitled)"}
                    </Link>
                  ) : (
                    (it.content?.title ?? "(untitled)")
                  )}
                  {it.content?.number != null && (
                    <span style={{ color: "var(--color-fg-muted)" }}> #{it.content.number}</span>
                  )}
                </th>
                {columns.map((f) => (
                  <ProjectFieldCell
                    key={f.id}
                    item={it}
                    field={f}
                    busy={busy}
                    onSet={(value) => onMove(it.id, f.id, value)}
                  />
                ))}
                <td style={{ padding: "0.5rem 0.8rem", textAlign: "right" }}>
                  <Button
                    size="sm"
                    variant="danger"
                    aria-label={`Remove item ${it.id}`}
                    disabled={busy}
                    onClick={() => onRemove(it.id)}
                  >
                    Remove
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Box>
  );
}

function ItemCard({
  item,
  groupField,
  busy,
  onMove,
  onRemove,
}: {
  item: GithubProjectV2Item;
  groupField: GithubProjectV2Field | undefined;
  busy: boolean;
  onMove: (optionId: string) => void;
  onRemove: () => void;
}) {
  const current = groupField ? itemOptionId(item, groupField.id) : null;
  return (
    <div style={{ border: "1px solid var(--color-border)", borderRadius: "var(--radius-sm)", padding: "0.5rem" }}>
      <div className="flex items-start gap-2">
        <span className="min-w-0 flex-1" style={{ fontSize: "0.85rem" }}>
          <span style={{ fontSize: "0.7rem", color: "var(--color-fg-muted)" }}>{item.content_type}</span>{" "}
          {item.content?.title ?? "(untitled)"}
          {item.content?.number != null && (
            <span style={{ color: "var(--color-fg-muted)" }}> #{item.content.number}</span>
          )}
        </span>
        <Button size="sm" variant="danger" aria-label={`Remove item ${item.id}`} disabled={busy} onClick={onRemove}>
          Remove
        </Button>
      </div>
      {groupField && (
        <select
          aria-label={`Move item ${item.id}`}
          value={current ?? ""}
          onChange={(e) => onMove(e.target.value)}
          disabled={busy}
          className="mt-1 w-full"
          style={{ fontSize: "0.78rem" }}
        >
          <option value="" disabled>
            Set {groupField.name}…
          </option>
          {(groupField.options ?? []).map((o) => (
            <option key={o.id} value={o.id}>
              {o.name.raw}
            </option>
          ))}
        </select>
      )}
    </div>
  );
}
