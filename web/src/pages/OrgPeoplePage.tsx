import { useMemo, useState } from "react";
import { useParams, Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import {
  ghFetch,
  ghSend,
  fetchPublicOrgMembers,
  setOrgMembership,
  removeOrgMember,
  publicizeOrgMembership,
  concealOrgMembership,
  fetchCurrentUser,
} from "../api.js";
import type { GithubAccount } from "../types.js";
import { OrgHeader } from "../components/PageHeader.js";
import { Avatar } from "../components/Avatar.js";
import { Box, SectionLabel, Blankslate, Button, FormLabel } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { confirmAction } from "../components/confirmAction.js";
import { PeopleIcon } from "../components/octicons.js";

export function OrgPeoplePage() {
  const { org = "" } = useParams<{ org: string }>();
  const qc = useQueryClient();
  const [filter, setFilter] = useState("");
  const [inviteLogin, setInviteLogin] = useState("");
  const [inviteRole, setInviteRole] = useState<"member" | "admin">("member");

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["org-members", org],
    // per_page=100 so orgs with >30 members aren't silently truncated to the
    // first page (the server defaults to 30). Inline to avoid touching api.ts.
    queryFn: () => ghFetch<GithubAccount[]>(`/api/v3/orgs/${encodeURIComponent(org)}/members?per_page=100`),
  });
  const viewerQ = useQuery({ queryKey: ["current-user"], queryFn: ({ signal }) => fetchCurrentUser(signal) });
  const publicQ = useQuery({
    queryKey: ["org-public-members", org],
    queryFn: () => fetchPublicOrgMembers(org),
  });
  const publicLogins = useMemo(
    () => new Set((publicQ.data ?? []).map((m) => m.login)),
    [publicQ.data],
  );

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["org-members", org] });
    qc.invalidateQueries({ queryKey: ["org-public-members", org] });
  };
  const visibilityMut = useMutation({
    mutationFn: (v: { login: string; makePublic: boolean }) =>
      v.makePublic ? publicizeOrgMembership(org, v.login) : concealOrgMembership(org, v.login),
    onSuccess: invalidate,
  });
  const inviteMut = useMutation({
    mutationFn: () => setOrgMembership(org, inviteLogin.trim(), inviteRole),
    onSuccess: () => {
      invalidate();
      setInviteLogin("");
    },
  });
  const roleMut = useMutation({
    mutationFn: (v: { login: string; role: "member" | "admin" }) =>
      setOrgMembership(org, v.login, v.role),
    onSuccess: invalidate,
  });
  const removeMut = useMutation({
    mutationFn: (login: string) => removeOrgMember(org, login),
    onSuccess: invalidate,
  });
  // Convert an existing org member into an outside collaborator (github's
  // "Convert to outside collaborator"). Defined inline via ghSend to avoid a new
  // api.ts export (the entry bundle sits at its budget).
  const convertMut = useMutation({
    mutationFn: (login: string) =>
      ghSend("PUT", `/api/v3/orgs/${encodeURIComponent(org)}/outside_collaborators/${encodeURIComponent(login)}`),
    onSuccess: invalidate,
  });

  const filtered = useMemo(() => {
    if (!data) return [];
    const q = filter.trim().toLowerCase();
    if (!q) return data;
    return data.filter((m) => m.login.toLowerCase().includes(q));
  }, [data, filter]);

  return (
    <div>
      <OrgHeader org={org} active="people" />
      <SectionLabel>
        People{data ? ` · ${data.length}` : ""}
      </SectionLabel>

      <Box style={{ padding: "0.85rem 1rem", marginBottom: "1rem" }}>
        <FormLabel id="invite-login">Invite a member</FormLabel>
        <div className="flex flex-wrap items-center gap-2">
          <input
            id="invite-login"
            value={inviteLogin}
            onChange={(e) => setInviteLogin(e.target.value)}
            placeholder="Username"
            style={{ fontSize: "0.85rem", minWidth: "12rem" }}
          />
          <select
            aria-label="Invite role"
            value={inviteRole}
            onChange={(e) => setInviteRole(e.target.value as "member" | "admin")}
            style={{ fontSize: "0.85rem" }}
          >
            <option value="member">Member</option>
            <option value="admin">Owner</option>
          </select>
          <Button
            variant="primary"
            size="sm"
            disabled={!inviteLogin.trim() || inviteMut.isPending}
            onClick={() => inviteMut.mutate()}
          >
            {inviteMut.isPending ? "Inviting…" : "Invite"}
          </Button>
        </div>
        <MutationError of={[inviteMut, roleMut, removeMut, visibilityMut, convertMut]} />
      </Box>

      {isLoading && <Spinner label="loading members" />}
      {isError && <InlineError title="Failed to load members" detail={String(error)} />}
      {data && (
        <>
          <input
            type="search"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Find a member…"
            aria-label="Find a member"
            className="mb-4 w-full"
            style={{ maxWidth: "20rem", fontSize: "0.85rem" }}
          />
          {data.length === 0 ? (
            <Blankslate icon={<PeopleIcon size={28} />} title="No members">
              This organization has no visible members.
            </Blankslate>
          ) : filtered.length === 0 ? (
            <Blankslate icon={<PeopleIcon size={28} />} title="No matches">
              No member matches “{filter}”.
            </Blankslate>
          ) : (
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
              {filtered.map((m) => (
                <MemberCard
                  key={m.id}
                  member={m}
                  isSelf={viewerQ.data?.login === m.login}
                  isPublic={publicLogins.has(m.login)}
                  onToggleVisibility={() =>
                    visibilityMut.mutate({ login: m.login, makePublic: !publicLogins.has(m.login) })
                  }
                  onSetRole={(role) => roleMut.mutate({ login: m.login, role })}
                  onRemove={async () => {
                    if (
                      await confirmAction(`Remove ${m.login} from ${org}?`, {
                        title: "Remove member",
                        confirmLabel: "Remove",
                      })
                    ) {
                      removeMut.mutate(m.login);
                    }
                  }}
                  onConvert={async () => {
                    if (
                      await confirmAction(`Convert ${m.login} to an outside collaborator of ${org}?`, {
                        title: "Convert to outside collaborator",
                        confirmLabel: "Convert",
                      })
                    ) {
                      convertMut.mutate(m.login);
                    }
                  }}
                  busy={roleMut.isPending || removeMut.isPending || visibilityMut.isPending || convertMut.isPending}
                />
              ))}
            </div>
          )}
        </>
      )}
    </div>
  );
}

function MemberCard({
  member,
  isSelf,
  isPublic,
  onToggleVisibility,
  onSetRole,
  onRemove,
  onConvert,
  busy,
}: {
  member: GithubAccount;
  isSelf: boolean;
  isPublic: boolean;
  onToggleVisibility: () => void;
  onSetRole: (role: "member" | "admin") => void;
  onRemove: () => void;
  onConvert: () => void;
  busy: boolean;
}) {
  return (
    <Box style={{ padding: "0.85rem 1rem" }}>
      <div className="flex items-center gap-3">
        <Avatar login={member.login} src={member.avatar_url} size={44} />
        <div className="min-w-0 flex-1">
          <Link
            to={`/ui/${member.login}`}
            style={{ color: "var(--color-fg)", fontWeight: 600, fontSize: "0.92rem", textDecoration: "none" }}
          >
            {member.login}
          </Link>
          <div style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
            {member.site_admin ? "Site admin" : member.type}
            {isPublic ? " · public member" : isSelf ? " · private member" : ""}
          </div>
        </div>
      </div>
      {isSelf && (
        <div className="mt-2">
          <Button
            size="sm"
            variant="ghost"
            aria-label={isPublic ? "Make membership private" : "Make membership public"}
            disabled={busy}
            onClick={onToggleVisibility}
          >
            {isPublic ? "Make private" : "Make public"}
          </Button>
        </div>
      )}
      <div className="mt-2 flex items-center gap-2">
        <select
          aria-label={`Set role for ${member.login}`}
          value=""
          onChange={(e) => {
            if (e.target.value) onSetRole(e.target.value as "member" | "admin");
          }}
          disabled={busy}
          style={{ fontSize: "0.78rem" }}
        >
          <option value="">Change role…</option>
          <option value="member">Member</option>
          <option value="admin">Owner</option>
        </select>
        <Button size="sm" aria-label={`Remove ${member.login}`} disabled={busy} onClick={onRemove}>
          Remove
        </Button>
        {!isSelf && (
          <Button
            size="sm"
            variant="ghost"
            aria-label={`Convert ${member.login} to outside collaborator`}
            disabled={busy}
            onClick={onConvert}
          >
            Convert to outside collaborator
          </Button>
        )}
      </div>
    </Box>
  );
}
