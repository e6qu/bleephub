import { useState } from "react";
import { useParams, Link } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { ghFetch } from "../api.js";
import type { GithubOrgTeam } from "../types.js";
import { OrgHeader } from "../components/PageHeader.js";
import { PageTitle, Tabs } from "../components/ui.js";
import { TeamMembersPanel, TeamReposPanel, TeamChildrenPanel } from "./TeamsPage.js";

const fetchOrgTeam = (org: string, slug: string) =>
  ghFetch<GithubOrgTeam>(`/api/v3/orgs/${encodeURIComponent(org)}/teams/${encodeURIComponent(slug)}`);

// Org-context team detail: the Members / Repositories / Child-teams surface
// github.com opens when you click a team on the org Teams tab. Reuses the same
// panels as the operations Teams page against the org REST endpoints.
export function OrgTeamDetailPage() {
  const { org = "", slug = "" } = useParams<{ org: string; slug: string }>();
  const [tab, setTab] = useState<"members" | "repos" | "children">("members");

  const team = useQuery({
    queryKey: ["org-team", org, slug],
    queryFn: () => fetchOrgTeam(org, slug),
  });

  return (
    <div>
      <OrgHeader org={org} active="teams" />
      <PageTitle
        title={team.data ? team.data.name : slug}
        meta={
          <Link to={`/ui/orgs/${org}/teams`} style={{ color: "var(--color-accent)", textDecoration: "none" }}>
            ← All teams
          </Link>
        }
      />
      {team.isLoading && <Spinner label="loading team" />}
      {team.isError && <InlineError title="Failed to load team" detail={String(team.error)} />}
      {team.data && (
        <>
          <p className="mb-4" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
            {team.data.description || "No description."} · @{team.data.slug} · {team.data.privacy}
          </p>
          <Tabs
            items={[
              { key: "members" as const, label: "Members" },
              { key: "repos" as const, label: "Repositories" },
              { key: "children" as const, label: "Child teams" },
            ]}
            active={tab}
            onChange={setTab}
          />
          <div className="mt-4">
            {tab === "members" && <TeamMembersPanel org={org} slug={slug} />}
            {tab === "repos" && <TeamReposPanel org={org} slug={slug} />}
            {tab === "children" && <TeamChildrenPanel org={org} slug={slug} />}
          </div>
        </>
      )}
    </div>
  );
}
