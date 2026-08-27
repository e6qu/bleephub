import { useMemo, useState } from "react";
import { useParams, Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import {
  ghFetch,
  ghSend,
  fetchPublicOrgMembers,
  fetchOutsideCollaborators,
  removeOutsideCollaborator,
  setOrgMembership,
  removeOrgMember,
  publicizeOrgMembership,
  concealOrgMembership,
  fetchCurrentUser,
} from "../api.js";
import { fetchViewerOrgRole } from "../utils/uiFetch.js";
import { useSignedIn } from "../session.js";
import type { GithubAccount } from "../types.js";
import { OrgHeader } from "../components/PageHeader.js";
import { Avatar } from "../components/Avatar.js";
import { Box, SectionLabel, Blankslate, Button, FormLabel, Tabs } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { confirmAction } from "../components/confirmAction.js";
import { PeopleIcon } from "../components/octicons.js";

type PeopleTab = "members" | "outside";

export function OrgPeoplePage() {
  const { org = "" } = useParams<{ org: string }>();
  const [tab, setTab] = useState<PeopleTab>("members");
  // Signed out: public members list only (viewer-role read 401s, collaborators are member-only).
  const signedIn = useSignedIn();

  // The viewer's org role gates write controls: only owners see the invite box
  // and per-member actions (GET /api/v3/user/memberships/orgs/{org}).
  const roleQ = useQuery({
    queryKey: ["viewer-org-role", org],
    queryFn: () => fetchViewerOrgRole(org),
    retry: false,
    enabled: signedIn,
  });
  const isOrgAdmin = roleQ.data === "admin";

  return (
    <div>
      <OrgHeader org={org} active="people" />
      <Tabs<PeopleTab>
        items={
          signedIn
            ? [
                { key: "members", label: "Members" },
                { key: "outside", label: "Outside collaborators" },
              ]
            : [{ key: "members", label: "Members" }]
        }
        active={tab}
        onChange={setTab}
      />
      {tab === "members" ? (
        <OrgMembersPanel org={org} isOrgAdmin={isOrgAdmin} />
      ) : (
        <OutsideCollaboratorsPanel org={org} isOrgAdmin={isOrgAdmin} />
      )}
    </div>
  );
}

function OrgMembersPanel({ org, isOrgAdmin }: { org: string; isOrgAdmin: boolean }) {
  const qc = useQueryClient();
  const [filter, setFilter] = useState("");
  const [inviteLogin, setInviteLogin] = useState("");
  const [inviteRole, setInviteRole] = useState<"member" | "admin">("member");

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["org-members", org],
    // per_page=100 so >30-member orgs aren't truncated (server defaults to 30).
    queryFn: () => ghFetch<GithubAccount[]>(`/api/v3/orgs/${encodeURIComponent(org)}/members?per_page=100`),
  });
  const signedIn = useSignedIn();
  const viewerQ = useQuery({
    queryKey: ["current-user"],
    queryFn: ({ signal }) => fetchCurrentUser(signal),
    enabled: signedIn,
  });
  const publicQ = useQuery({
    queryKey: ["org-public-members", org],
    queryFn: () => fetchPublicOrgMembers(org),
  });
  // ?role=admin narrows the list to org owners, for the owner badges.
  const ownersQ = useQuery({
    queryKey: ["org-owner-members", org],
    queryFn: () => ghFetch<GithubAccount[]>(`/api/v3/orgs/${encodeURIComponent(org)}/members?role=admin&per_page=100`),
    retry: false,
  });
  const ownerLogins = useMemo(
    () => new Set((Array.isArray(ownersQ.data) ? ownersQ.data : []).map((m) => m.login)),
    [ownersQ.data],
  );
  const publicLogins = useMemo(
    () => new Set((publicQ.data ?? []).map((m) => m.login)),
    [publicQ.data],
  );

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["org-members", org] });
    qc.invalidateQueries({ queryKey: ["org-public-members", org] });
    qc.invalidateQueries({ queryKey: ["org-owner-members", org] });
    qc.invalidateQueries({ queryKey: ["org-outside-collaborators", org] });
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
  // Convert an org member into an outside collaborator. Inline ghSend to keep
  // this off api.ts (entry bundle budget).
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
      <SectionLabel>
        People{data ? ` · ${data.length}` : ""}
      </SectionLabel>

      {isOrgAdmin && (
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
      )}
      {!isOrgAdmin && <MutationError of={[visibilityMut]} />}

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
                  orgRole={ownerLogins.has(m.login) ? "Owner" : "Member"}
                  canAdmin={isOrgAdmin}
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
  orgRole,
  canAdmin,
  onToggleVisibility,
  onSetRole,
  onRemove,
  onConvert,
  busy,
}: {
  member: GithubAccount;
  isSelf: boolean;
  isPublic: boolean;
  orgRole: "Owner" | "Member";
  canAdmin: boolean;
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
            {orgRole}
            {member.site_admin ? " · Site admin" : ""}
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
      {canAdmin && (
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
      )}
    </Box>
  );
}

function OutsideCollaboratorsPanel({ org, isOrgAdmin }: { org: string; isOrgAdmin: boolean }) {
  const qc = useQueryClient();
  const [filter, setFilter] = useState("");
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["org-outside-collaborators", org],
    queryFn: () => fetchOutsideCollaborators(org),
  });
  const removeMut = useMutation({
    mutationFn: (login: string) => removeOutsideCollaborator(org, login),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["org-outside-collaborators", org] }),
  });

  const filtered = useMemo(() => {
    if (!data) return [];
    const q = filter.trim().toLowerCase();
    if (!q) return data;
    return data.filter((m) => m.login.toLowerCase().includes(q));
  }, [data, filter]);

  return (
    <div>
      <SectionLabel>Outside collaborators{data ? ` · ${data.length}` : ""}</SectionLabel>
      <MutationError of={[removeMut]} />
      {isLoading && <Spinner label="loading outside collaborators" />}
      {isError && <InlineError title="Failed to load outside collaborators" detail={String(error)} />}
      {data && (
        <>
          <input
            type="search"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Find a collaborator…"
            aria-label="Find a collaborator"
            className="mb-4 w-full"
            style={{ maxWidth: "20rem", fontSize: "0.85rem" }}
          />
          {data.length === 0 ? (
            <Blankslate icon={<PeopleIcon size={28} />} title="No outside collaborators">
              This organization has no outside collaborators.
            </Blankslate>
          ) : filtered.length === 0 ? (
            <Blankslate icon={<PeopleIcon size={28} />} title="No matches">
              No collaborator matches “{filter}”.
            </Blankslate>
          ) : (
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
              {filtered.map((m) => (
                <Box key={m.id} style={{ padding: "0.85rem 1rem" }}>
                  <div className="flex items-center gap-3">
                    <Avatar login={m.login} src={m.avatar_url} size={44} />
                    <div className="min-w-0 flex-1">
                      <Link
                        to={`/ui/${m.login}`}
                        style={{ color: "var(--color-fg)", fontWeight: 600, fontSize: "0.92rem", textDecoration: "none" }}
                      >
                        {m.login}
                      </Link>
                      <div style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>Outside collaborator</div>
                    </div>
                  </div>
                  {isOrgAdmin && (
                    <div className="mt-2">
                      <Button
                        size="sm"
                        aria-label={`Remove ${m.login} from outside collaborators`}
                        disabled={removeMut.isPending}
                        onClick={async () => {
                          if (
                            await confirmAction(`Remove ${m.login} as an outside collaborator of ${org}?`, {
                              title: "Remove outside collaborator",
                              confirmLabel: "Remove",
                            })
                          ) {
                            removeMut.mutate(m.login);
                          }
                        }}
                      >
                        Remove
                      </Button>
                    </div>
                  )}
                </Box>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  );
}
