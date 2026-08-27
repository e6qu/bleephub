import { useMemo, useState } from "react";
import { useParams, Link, useNavigate } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { fetchOrgTeams, createTeam } from "../api.js";
import { limitedGhFetch } from "../utils/uiFetch.js";
import type { GithubOrgTeam } from "../types.js";
import { OrgHeader } from "../components/PageHeader.js";
import { Box, SectionLabel, Blankslate, Button, Modal, FormLabel, DialogActions, ErrorBanner } from "../components/ui.js";
import { TeamIcon, LockIcon, ChevronDownIcon, ChevronRightIcon, PeopleIcon, RepoIcon } from "../components/octicons.js";

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
            <TeamTree org={org} teams={filtered} />
          )}
        </>
      )}
    </div>
  );
}

// Nest child teams under their parent; a child whose parent isn't in the list renders as a root.
function TeamTree({ org, teams }: { org: string; teams: GithubOrgTeam[] }) {
  const { roots, children } = useMemo(() => {
    const slugs = new Set(teams.map((t) => t.slug));
    const childMap = new Map<string, GithubOrgTeam[]>();
    const rootList: GithubOrgTeam[] = [];
    for (const t of teams) {
      const parentSlug = t.parent?.slug;
      if (parentSlug && slugs.has(parentSlug)) {
        const bucket = childMap.get(parentSlug) ?? [];
        bucket.push(t);
        childMap.set(parentSlug, bucket);
      } else {
        rootList.push(t);
      }
    }
    return { roots: rootList, children: childMap };
  }, [teams]);

  return (
    <Box>
      <TeamBranch org={org} teams={roots} childMap={children} depth={0} />
    </Box>
  );
}

function TeamBranch({
  org,
  teams,
  childMap,
  depth,
}: {
  org: string;
  teams: GithubOrgTeam[];
  childMap: Map<string, GithubOrgTeam[]>;
  depth: number;
}) {
  return (
    <>
      {teams.map((t) => (
        <TeamNode key={t.id} org={org} team={t} childMap={childMap} depth={depth} />
      ))}
    </>
  );
}

function TeamNode({
  org,
  team,
  childMap,
  depth,
}: {
  org: string;
  team: GithubOrgTeam;
  childMap: Map<string, GithubOrgTeam[]>;
  depth: number;
}) {
  const kids = childMap.get(team.slug) ?? [];
  const [expanded, setExpanded] = useState(true);
  return (
    <>
      <TeamRow
        org={org}
        team={team}
        depth={depth}
        childCount={kids.length}
        expanded={expanded}
        onToggle={() => setExpanded((v) => !v)}
      />
      {expanded && kids.length > 0 && (
        <TeamBranch org={org} teams={kids} childMap={childMap} depth={depth + 1} />
      )}
    </>
  );
}

function TeamRow({
  org,
  team,
  depth,
  childCount,
  expanded,
  onToggle,
}: {
  org: string;
  team: GithubOrgTeam;
  depth: number;
  childCount: number;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <div
      className="flex flex-wrap items-center gap-3"
      style={{
        padding: "0.75rem 1rem",
        paddingLeft: `${1 + depth * 1.5}rem`,
        borderBottom: "1px solid var(--color-border)",
      }}
    >
      {childCount > 0 ? (
        <Button
          size="sm"
          variant="ghost"
          aria-expanded={expanded}
          aria-label={`${expanded ? "Collapse" : "Expand"} child teams of ${team.name}`}
          onClick={onToggle}
        >
          {expanded ? <ChevronDownIcon size={14} /> : <ChevronRightIcon size={14} />}
        </Button>
      ) : (
        <TeamIcon size={16} style={{ color: "var(--color-fg-muted)" }} />
      )}
      <div className="min-w-0 flex-1">
        <Link
          to={`/ui/orgs/${org}/teams/${team.slug}`}
          style={{
            fontWeight: 600,
            fontSize: "0.92rem",
            color: "var(--color-accent)",
            textDecoration: "none",
            display: "inline-block",
            lineHeight: "1.625rem",
          }}
        >
          {team.name}
        </Link>
        <span className="ml-2" style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)" }}>
          @{team.slug}
        </span>
        {team.description && (
          <p className="mt-0.5" style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
            {team.description}
          </p>
        )}
      </div>
      <TeamCounts org={org} slug={team.slug} />
      <span
        className="inline-flex items-center gap-1"
        style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}
      >
        {team.privacy === "secret" && <LockIcon size={12} />}
        {team.privacy}
      </span>
    </div>
  );
}

// The list payload omits counts (team-full only), so hydrate each row from GET
// /orgs/{org}/teams/{slug}, concurrency-capped and cached.
function TeamCounts({ org, slug }: { org: string; slug: string }) {
  const { data } = useQuery({
    queryKey: ["team-counts", org, slug],
    queryFn: () =>
      limitedGhFetch<{ members_count?: number; repos_count?: number }>(
        `/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}`,
      ),
    staleTime: 60_000,
    retry: false,
  });
  if (typeof data?.members_count !== "number" && typeof data?.repos_count !== "number") return null;
  return (
    <span className="inline-flex items-center gap-3" style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
      {typeof data.members_count === "number" && (
        <span className="inline-flex items-center gap-1">
          <PeopleIcon size={12} /> {data.members_count}
        </span>
      )}
      {typeof data.repos_count === "number" && (
        <span className="inline-flex items-center gap-1">
          <RepoIcon size={12} /> {data.repos_count}
        </span>
      )}
    </span>
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
