import { useMemo, useState } from "react";
import { useParams, Link, useNavigate } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { fetchOrgTeams, createTeam } from "../api.js";
import type { GithubOrgTeam } from "../types.js";
import { OrgHeader } from "../components/PageHeader.js";
import { Box, SectionLabel, Blankslate, Button, Modal, FormLabel, DialogActions, ErrorBanner } from "../components/ui.js";
import { TeamIcon, LockIcon } from "../components/octicons.js";

export function OrgTeamsPage() {
  const { org = "" } = useParams<{ org: string }>();
  const [filter, setFilter] = useState("");
  const [creating, setCreating] = useState(false);

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["org-teams", org],
    queryFn: () => fetchOrgTeams(org),
  });

  const filtered = useMemo(() => {
    if (!data) return [];
    const q = filter.trim().toLowerCase();
    if (!q) return data;
    return data.filter(
      (t) => t.name.toLowerCase().includes(q) || t.slug.toLowerCase().includes(q),
    );
  }, [data, filter]);

  return (
    <div>
      <OrgHeader org={org} active="teams" />
      <div className="flex items-center justify-between">
        <SectionLabel>Teams{data ? ` · ${data.length}` : ""}</SectionLabel>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>New team</Button>
      </div>
      {creating && <NewTeamModal org={org} onClose={() => setCreating(false)} />}

      {isLoading && <Spinner label="loading teams" />}
      {isError && <InlineError title="Failed to load teams" detail={String(error)} />}
      {data && (
        <>
          <input
            type="search"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Find a team…"
            aria-label="Find a team"
            className="mb-4 w-full"
            style={{ maxWidth: "20rem", fontSize: "0.85rem" }}
          />
          {data.length === 0 ? (
            <Blankslate icon={<TeamIcon size={28} />} title="No teams">
              This organization has no teams yet.
            </Blankslate>
          ) : filtered.length === 0 ? (
            <Blankslate icon={<TeamIcon size={28} />} title="No matches">
              No team matches “{filter}”.
            </Blankslate>
          ) : (
            <Box>
              {filtered.map((t, i) => (
                <TeamRow key={t.id} org={org} team={t} last={i === filtered.length - 1} />
              ))}
            </Box>
          )}
        </>
      )}
    </div>
  );
}

function TeamRow({ org, team, last }: { org: string; team: GithubOrgTeam; last: boolean }) {
  return (
    <Link
      to={`/ui/orgs/${org}/teams/${team.slug}`}
      className="flex flex-wrap items-center gap-3"
      style={{
        padding: "0.75rem 1rem",
        borderBottom: last ? "none" : "1px solid var(--color-border)",
        textDecoration: "none",
        color: "inherit",
      }}
    >
      <TeamIcon size={16} style={{ color: "var(--color-fg-muted)" }} />
      <div className="min-w-0 flex-1">
        <span style={{ fontWeight: 600, fontSize: "0.92rem", color: "var(--color-accent)" }}>{team.name}</span>
        <span className="ml-2" style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
          @{team.slug}
        </span>
        {team.description && (
          <p className="mt-0.5" style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
            {team.description}
          </p>
        )}
      </div>
      <span
        className="inline-flex items-center gap-1"
        style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}
      >
        {team.privacy === "secret" && <LockIcon size={12} />}
        {team.privacy}
      </span>
    </Link>
  );
}

function NewTeamModal({ org, onClose }: { org: string; onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [privacy, setPrivacy] = useState<"closed" | "secret">("closed");
  const [error, setError] = useState<string | null>(null);
  const create = useMutation({
    mutationFn: () => createTeam({ org, name: name.trim(), description: description.trim() || undefined, privacy }),
    onSuccess: (team) => {
      setError(null);
      void qc.invalidateQueries({ queryKey: ["org-teams", org] });
      onClose();
      if (team?.slug) navigate(`/ui/orgs/${org}/teams/${team.slug}`);
    },
    onError: (e: Error) => setError(e.message),
  });
  return (
    <Modal title="Create a team" onClose={onClose}>
      {error && <ErrorBanner>{error}</ErrorBanner>}
      <label><FormLabel id="team-name">Team name</FormLabel>
        <input id="team-name" aria-label="Team name" value={name} onChange={(e) => setName(e.target.value)} className="mb-3 w-full" /></label>
      <label><FormLabel id="team-desc">Description</FormLabel>
        <input id="team-desc" aria-label="Team description" value={description} onChange={(e) => setDescription(e.target.value)} className="mb-3 w-full" /></label>
      <label><FormLabel id="team-privacy">Visibility</FormLabel>
        <select id="team-privacy" aria-label="Team visibility" value={privacy} onChange={(e) => setPrivacy(e.target.value as "closed" | "secret")} className="mb-3 w-full">
          <option value="closed">Visible</option>
          <option value="secret">Secret</option>
        </select></label>
      <DialogActions>
        <Button variant="ghost" onClick={onClose} disabled={create.isPending}>Cancel</Button>
        <Button variant="primary" disabled={!name.trim() || create.isPending} onClick={() => create.mutate()}>
          {create.isPending ? "Creating…" : "Create team"}
        </Button>
      </DialogActions>
    </Modal>
  );
}
