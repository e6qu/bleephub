import { useState } from "react";
import { Link, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchOrgProjectsV2,
  fetchOrgProjectV2,
  fetchOrgProjectV2Items,
  addOrgProjectV2Item,
  deleteOrgProjectV2Item,
} from "../api.js";
import { OrgHeader } from "../components/Shell.js";
import { PageTitle, Box, Blankslate, Button, ErrorBanner } from "../components/ui.js";
import { confirmAction } from "../components/confirmAction.js";

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

  if (projectQ.isLoading) return <Spinner label="loading project" />;
  if (projectQ.isError || !projectQ.data) {
    return <InlineError title="Failed to load project" detail={String(projectQ.error)} />;
  }
  const items = itemsQ.data ?? [];
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
      <Box header={<span style={{ fontWeight: 600 }}>Add an item</span>}>
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
      </Box>
      {deleteMut.error && <ErrorBanner>{String(deleteMut.error)}</ErrorBanner>}
      {itemsQ.isLoading ? (
        <Spinner label="loading items" />
      ) : items.length === 0 ? (
        <Blankslate title="No items" />
      ) : (
        <Box className="mt-3">
          {items.map((it, i) => (
            <div
              key={it.id}
              className="flex items-center gap-2"
              style={{ padding: "0.6rem 1rem", borderBottom: i === items.length - 1 ? "none" : "1px solid var(--color-border)" }}
            >
              <span style={{ fontSize: "0.72rem", color: "var(--color-fg-muted)" }}>{it.content_type}</span>
              <span className="min-w-0 flex-1 truncate">
                {it.content?.title ?? "(untitled)"}
                {it.content?.number != null && (
                  <span style={{ color: "var(--color-fg-muted)" }}> #{it.content.number}</span>
                )}
              </span>
              <Button
                size="sm"
                variant="danger"
                aria-label={`Remove item ${it.id}`}
                disabled={deleteMut.isPending}
                onClick={async () => {
                  if (await confirmAction("Remove this item from the project?")) deleteMut.mutate(it.id);
                }}
              >
                Remove
              </Button>
            </div>
          ))}
        </Box>
      )}
    </>
  );
}
