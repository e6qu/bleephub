import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { fetchAuthenticatedUserOrgs } from "../api.js";
import type { BleephubOrg } from "../types.js";
import { Avatar } from "../components/Avatar.js";
import { Box, Blankslate, Button, PageTitle } from "../components/ui.js";
import { OrganizationIcon, PlusIcon } from "../components/octicons.js";

/**
 * Your organizations — the authenticated user's org memberships, mirroring
 * GitHub's github.com/settings/organizations. Reached from the avatar menu's
 * "Your organizations" entry. Backed by GET /api/v3/user/orgs.
 */
export function MyOrganizationsPage() {
  const orgs = useQuery({
    queryKey: ["my-organizations"],
    queryFn: ({ signal }) => fetchAuthenticatedUserOrgs(signal),
  });

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-4">
      <PageTitle
        icon={<OrganizationIcon size={22} />}
        title="Your organizations"
        actions={
          <Link to="/ui/operations/orgs?new=1" style={{ textDecoration: "none" }}>
            <Button variant="primary" size="sm">
              <PlusIcon size={14} /> New organization
            </Button>
          </Link>
        }
      />

      {orgs.isLoading && <Spinner label="loading organizations" />}
      {orgs.isError && (
        <InlineError title="Failed to load organizations" detail={String(orgs.error)} />
      )}
      {orgs.data &&
        (orgs.data.length === 0 ? (
          <Blankslate icon={<OrganizationIcon size={28} />} title="No organizations yet">
            Organizations you belong to appear here. Create one to collaborate across teams and
            repositories.
          </Blankslate>
        ) : (
          <Box>
            <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
              {orgs.data.map((org, i) => (
                <OrgRow key={org.id} org={org} last={i === orgs.data.length - 1} />
              ))}
            </ul>
          </Box>
        ))}
    </div>
  );
}

function OrgRow({ org, last }: { org: BleephubOrg; last: boolean }) {
  return (
    <li
      style={{
        borderBottom: last ? "none" : "1px solid var(--color-border)",
      }}
    >
      <Link
        to={`/ui/orgs/${org.login}`}
        className="flex items-center gap-3"
        style={{
          padding: "0.7rem 1rem",
          color: "var(--color-fg)",
          textDecoration: "none",
        }}
      >
        <Avatar login={org.login} src={org.avatar_url} size={32} square />
        <span className="min-w-0 flex-1">
          <span className="truncate" style={{ display: "block", fontWeight: 600, fontSize: "0.9rem" }}>
            {org.login}
          </span>
          {(org.name || org.description) && (
            <span
              className="truncate"
              style={{ display: "block", fontSize: "0.8rem", color: "var(--color-fg-muted)" }}
            >
              {org.description || org.name}
            </span>
          )}
        </span>
      </Link>
    </li>
  );
}
