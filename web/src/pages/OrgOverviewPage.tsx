import { useState } from "react";
import { useParams, Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { fetchOrgProfile, fetchOrgReposPage, fetchOrgMembers, ghFetch, ghSend } from "../api.js";
import { decodeContentsBase64 } from "../utils/contents.js";
import { fetchViewerOrgRole } from "../utils/uiFetch.js";
import type { BleephubRepo } from "../types.js";
import { OrgHeader } from "../components/PageHeader.js";
import { Avatar } from "../components/Avatar.js";
import { Box, SectionLabel, Blankslate, Button, Modal, DialogActions } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { RepoStatsLine, LocationIcon, MailIcon } from "../components/RepoCardMeta.js";
import { RelativeTime } from "../components/RelativeTime.js";
import Markdown from "../components/Markdown.js";
import { RepoIcon, PeopleIcon, GlobeIcon, LockIcon, BookIcon } from "../components/octicons.js";

export function OrgOverviewPage() {
  const { org = "" } = useParams<{ org: string }>();

  const profile = useQuery({
    queryKey: ["org-profile", org],
    queryFn: () => fetchOrgProfile(org),
  });
  const repos = useQuery({
    queryKey: ["org-overview-repos", org],
    queryFn: () => fetchOrgReposPage(org, { sort: "updated" }),
  });
  const members = useQuery({
    queryKey: ["org-overview-members", org],
    queryFn: () => fetchOrgMembers(org),
  });
  // The org profile README is the README of the {org}/.github repo. The
  // /ui-data wrapper answers 200 with readme: null when absent (the common
  // case) — probing the readme endpoint directly would log a console 404.
  const readme = useQuery({
    queryKey: ["org-profile-readme", org],
    queryFn: async () => {
      const out = await ghFetch<{ readme: { content: string } | null }>(
        `/ui-data/users/${encodeURIComponent(org)}/profile-readme`,
      );
      return out.readme ? decodeContentsBase64(out.readme.content) : null;
    },
    retry: false,
  });
  const pinnedQ = useQuery({
    queryKey: ["org-pinned", org],
    queryFn: () => ghFetch<BleephubRepo[]>(`/ui-data/orgs/${encodeURIComponent(org)}/pinned`),
    retry: false,
  });
  const roleQ = useQuery({
    queryKey: ["viewer-org-role", org],
    queryFn: () => fetchViewerOrgRole(org),
    retry: false,
  });
  const isOrgAdmin = roleQ.data === "admin";

  if (profile.isLoading) return <Spinner label="loading organization" />;
  if (profile.isError || !profile.data) {
    return <InlineError title="Failed to load organization" detail={String(profile.error)} />;
  }
  const p = profile.data;
  const previewRepos = repos.data?.items.slice(0, 6) ?? [];
  const pinned = Array.isArray(pinnedQ.data) ? pinnedQ.data : [];
  const hasPins = pinned.length > 0;

  return (
    <div>
      <OrgHeader org={org} active="overview" />

      <div className="grid gap-6 md:grid-cols-[260px_1fr]">
        <aside className="flex flex-col gap-3">
          <div className="flex items-center gap-3">
            <Avatar login={p.login} src={p.avatar_url} size={64} square />
            <div className="min-w-0">
              <div style={{ fontSize: "1.25rem", fontWeight: 600, lineHeight: 1.2 }}>
                {p.name || p.login}
              </div>
              <div style={{ color: "var(--color-fg-muted)", fontSize: "0.9rem" }}>{p.login}</div>
            </div>
          </div>
          {p.description && (
            <p style={{ fontSize: "0.9rem", color: "var(--color-fg)" }}>{p.description}</p>
          )}
          <ul className="flex flex-col gap-1.5" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)", listStyle: "none", margin: 0, padding: 0 }}>
            <MetaRow icon={<PeopleIcon size={15} />}>
              <Link to={`/ui/orgs/${org}/people`} style={{ color: "var(--color-accent)", textDecoration: "none" }}>
                {members.data ? `${members.data.length} member${members.data.length === 1 ? "" : "s"}` : "Members"}
              </Link>
            </MetaRow>
            <MetaRow icon={<RepoIcon size={15} />}>
              <Link to={`/ui/orgs/${org}/repos`} style={{ color: "var(--color-accent)", textDecoration: "none" }}>
                {p.public_repos} repositor{p.public_repos === 1 ? "y" : "ies"}
              </Link>
            </MetaRow>
            {p.location && <MetaRow icon={<LocationIcon size={15} />}>{p.location}</MetaRow>}
            {p.blog && (
              <MetaRow icon={<GlobeIcon size={15} />}>
                <a href={p.blog} style={{ color: "var(--color-accent)", textDecoration: "none" }} rel="noreferrer">
                  {p.blog}
                </a>
              </MetaRow>
            )}
            {p.email && <MetaRow icon={<MailIcon size={15} />}>{p.email}</MetaRow>}
          </ul>
        </aside>

        <div className="flex flex-col gap-5">
          {readme.data && (
            <Box header={<span className="inline-flex items-center gap-2"><BookIcon size={14} />{org}/.github · README.md</span>}>
              <div style={{ padding: "1rem 1.25rem" }} className="markdown-body">
                <Markdown>{readme.data}</Markdown>
              </div>
            </Box>
          )}

          {(hasPins || isOrgAdmin) && (
            <OrgPinnedSection org={org} pinned={pinned} isOrgAdmin={isOrgAdmin} />
          )}

          <section>
            <SectionLabel>
              <span className="inline-flex items-center gap-2">
                {hasPins ? "Recently updated" : "Repositories"}
                <Link
                  to={`/ui/orgs/${org}/repos`}
                  style={{ fontSize: "0.82rem", fontWeight: 500, color: "var(--color-accent)", textDecoration: "none" }}
                >
                  View all
                </Link>
              </span>
            </SectionLabel>
            {repos.isLoading && <Spinner label="loading repositories" />}
            {repos.isError && <InlineError title="Failed to load repositories" detail={String(repos.error)} />}
            {repos.data &&
              (previewRepos.length === 0 ? (
                <Blankslate icon={<RepoIcon size={28} />} title="No repositories yet">
                  This organization has no repositories.
                </Blankslate>
              ) : (
                <div className="grid gap-3 sm:grid-cols-2">
                  {previewRepos.map((repo) => (
                    <RepoPreviewCard key={repo.id} repo={repo} />
                  ))}
                </div>
              ))}
          </section>
        </div>
      </div>
    </div>
  );
}

function MetaRow({ icon, children }: { icon?: React.ReactNode; children: React.ReactNode }) {
  return (
    <li className="flex items-center gap-2">
      {icon && <span style={{ color: "var(--color-fg-subtle)" }}>{icon}</span>}
      <span>{children}</span>
    </li>
  );
}

/**
 * Org pinned repositories, from GET /ui-data/orgs/{org}/pinned. Org owners
 * (viewer role "admin") get an "Edit pins" dialog; PUT is owner-only and
 * takes at most 6 org-owned repos.
 */
function OrgPinnedSection({
  org,
  pinned,
  isOrgAdmin,
}: {
  org: string;
  pinned: BleephubRepo[];
  isOrgAdmin: boolean;
}) {
  const [editing, setEditing] = useState(false);
  return (
    <section>
      <div className="mb-2 flex items-center justify-between">
        <SectionLabel>Pinned repositories</SectionLabel>
        {isOrgAdmin && (
          <Button size="sm" onClick={() => setEditing(true)}>
            Edit pins
          </Button>
        )}
      </div>
      {pinned.length === 0 ? (
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          No pinned repositories yet. Use “Edit pins”.
        </p>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2">
          {pinned.map((repo) => (
            <RepoPreviewCard key={repo.id} repo={repo} />
          ))}
        </div>
      )}
      {editing && (
        <OrgPinnedEditor org={org} current={pinned.map((r) => r.full_name)} onClose={() => setEditing(false)} />
      )}
    </section>
  );
}

function OrgPinnedEditor({
  org,
  current,
  onClose,
}: {
  org: string;
  current: string[];
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [selected, setSelected] = useState<string[]>(current);
  const reposQ = useQuery({
    queryKey: ["org-repos-for-pins", org],
    queryFn: () => fetchOrgReposPage(org, { sort: "updated" }),
  });
  const saveMut = useMutation({
    mutationFn: () => ghSend("PUT", `/ui-data/orgs/${encodeURIComponent(org)}/pinned`, { repos: selected }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["org-pinned", org] });
      onClose();
    },
  });
  const toggle = (fullName: string) =>
    setSelected((s) =>
      s.includes(fullName) ? s.filter((x) => x !== fullName) : s.length >= 6 ? s : [...s, fullName],
    );
  const repos = reposQ.data?.items ?? [];

  return (
    <Modal title="Edit pinned repositories" onClose={onClose}>
      <div style={{ fontSize: "0.8rem", color: "var(--color-fg-muted)", marginBottom: "0.5rem" }}>
        Select up to 6 repositories ({selected.length}/6)
      </div>
      {reposQ.isLoading ? (
        <Spinner label="loading repositories" />
      ) : (
        <ul style={{ listStyle: "none", margin: 0, padding: 0, maxHeight: "16rem", overflowY: "auto" }}>
          {repos.map((r) => {
            const checked = selected.includes(r.full_name);
            return (
              <li key={r.id}>
                <label className="flex items-center gap-2" style={{ padding: "0.25rem 0", fontSize: "0.85rem" }}>
                  <input
                    type="checkbox"
                    checked={checked}
                    disabled={!checked && selected.length >= 6}
                    onChange={() => toggle(r.full_name)}
                  />
                  {r.name}
                </label>
              </li>
            );
          })}
        </ul>
      )}
      <MutationError of={saveMut} />
      <DialogActions>
        <Button variant="ghost" onClick={onClose} disabled={saveMut.isPending}>
          Cancel
        </Button>
        <Button variant="primary" size="sm" disabled={saveMut.isPending} onClick={() => saveMut.mutate()}>
          {saveMut.isPending ? "Saving…" : "Save pins"}
        </Button>
      </DialogActions>
    </Modal>
  );
}

function RepoPreviewCard({ repo }: { repo: BleephubRepo }) {
  const [owner, name] = repo.full_name.split("/");
  return (
    <Box style={{ padding: "0.85rem 1rem" }}>
      <div className="flex items-center gap-2">
        <Link
          to={`/ui/repos/${owner}/${name}`}
          style={{ color: "var(--color-accent)", fontWeight: 600, fontSize: "0.95rem", textDecoration: "none" }}
        >
          {repo.name}
        </Link>
        {repo.private && <LockIcon size={13} style={{ color: "var(--color-fg-muted)" }} />}
      </div>
      {repo.description && (
        <p className="mt-1" style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
          {repo.description}
        </p>
      )}
      <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1">
        <RepoStatsLine repo={repo} showUpdated={false} />
        <span style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
          Updated <RelativeTime iso={repo.updated_at} />
        </span>
      </div>
    </Box>
  );
}
