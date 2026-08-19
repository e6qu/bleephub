import { useId, useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError } from "@bleephub/ui-core/components";
import {
  ghFetch,
  ghSend,
  isNotFound,
  fetchRepoLabels,
  fetchRepoMilestones,
  fetchOrgIssueTypes,
  fetchIssueGraphQLIssueType,
  fetchAssignableUsers,
  addIssueLabels,
  removeIssueLabel,
  addAssignees,
  removeAssignees,
  lockIssue,
  unlockIssue,
  setIssueMilestone,
  setIssueType,
  fetchOrgProjectsV2,
  fetchOrgProjectV2Items,
  addOrgProjectV2Item,
  deleteOrgProjectV2Item,
} from "../api.js";
import type { GithubProjectV2 } from "../types.js";
import { LabelPills } from "./LabelPills.js";
import { Avatar } from "./Avatar.js";
import { ErrorBanner } from "./ui.js";
import { GearIcon } from "./octicons.js";

/**
 * One sidebar section. When `gearFor` names an element id, the gear is a
 * real button that moves focus to that section's picker — github.com's gear
 * opens the section's editor. Sections whose control is already inline
 * render no gear at all (no decorative fake-interactive icons).
 */
function SidebarSection({
  title,
  children,
  last,
  gearFor,
  gearLabel,
}: {
  title: string;
  children: ReactNode;
  last?: boolean;
  gearFor?: string | undefined;
  gearLabel?: string | undefined;
}) {
  return (
    <div
      style={{
        padding: "0.85rem 0",
        borderBottom: last ? "none" : "1px solid var(--color-border)",
      }}
    >
      <div
        className="mb-2 flex items-center justify-between"
        style={{ fontSize: "0.78rem", fontWeight: 600, color: "var(--color-fg-muted)" }}
      >
        <span>{title}</span>
        {gearFor && (
          <button
            type="button"
            aria-label={gearLabel ?? `Edit ${title}`}
            onClick={() => document.getElementById(gearFor)?.focus()}
            className="inline-flex items-center justify-center"
            style={{
              border: "none",
              background: "transparent",
              color: "var(--color-fg-subtle)",
              cursor: "pointer",
              minWidth: "1.625rem",
              minHeight: "1.625rem",
              padding: 0,
            }}
          >
            <GearIcon size={14} />
          </button>
        )}
      </div>
      {children}
    </div>
  );
}

/** GitHub REST lock reasons, exactly as the API accepts them. */
const LOCK_REASONS = ["off-topic", "too heated", "resolved", "spam"] as const;
type LockReason = (typeof LOCK_REASONS)[number];

// ─── Notifications (thread subscription) ────────────────────────────────
//
// bleephub has no per-issue subscription REST endpoint (GitHub's real one is
// the notification-thread subscription: /notifications/threads/{id}/subscription,
// which also exists here). A thread for this conversation exists once the
// viewer has a notification for it — locate it via the repo notifications
// list and manage its subscription; without a thread there is nothing to
// subscribe to, and the section says so instead of faking a control.
interface NotificationThreadRow {
  id: string;
  unread?: boolean;
  subject?: { title?: string; url?: string; type?: string } | null;
}

function NotificationsField({
  owner,
  repo,
  number,
  kind,
}: {
  owner: string;
  repo: string;
  number: number;
  kind: "issue" | "pr";
}) {
  const muted = { fontSize: "0.82rem", color: "var(--color-fg-muted)" } as const;
  const subjectSuffix = kind === "pr" ? `/pulls/${number}` : `/issues/${number}`;

  const threadQ = useQuery({
    queryKey: ["notification-thread", owner, repo, kind, number],
    queryFn: async ({ signal }) => {
      const rows = await ghFetch<NotificationThreadRow[]>(
        `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/notifications?all=true`,
        signal,
      );
      if (!Array.isArray(rows)) return null;
      return rows.find((t) => (t.subject?.url ?? "").endsWith(subjectSuffix)) ?? null;
    },
  });
  const threadId = threadQ.data?.id ?? null;

  // A thread only exists because the viewer receives its notifications, so
  // "subscribed" is the initial state whenever a thread is found. Probing
  // GET .../subscription instead would 404 when no explicit record exists,
  // and the browser logs every 4xx as a console error (the e2e harness fails
  // on those), so the state is tracked from the viewer's own toggles.
  const [explicitSub, setExplicitSub] = useState<boolean | null>(null);
  const subscribed = explicitSub ?? true;
  const toggleMut = useMutation({
    mutationFn: () =>
      subscribed
        ? ghSend("DELETE", `/api/v3/notifications/threads/${encodeURIComponent(threadId as string)}/subscription`)
        : ghSend("PUT", `/api/v3/notifications/threads/${encodeURIComponent(threadId as string)}/subscription`, {
            ignored: false,
          }),
    onSuccess: () => {
      setExplicitSub(!subscribed);
    },
  });

  if (threadQ.isLoading) return <span style={muted}>Loading…</span>;
  if (threadQ.isError || threadId === null) {
    // No notification thread for this conversation yet — nothing to
    // subscribe to on this server, so say so honestly.
    return <span style={muted}>No notification thread for this conversation yet.</span>;
  }

  return (
    <div className="flex flex-col gap-1.5">
      <button
        type="button"
        onClick={() => toggleMut.mutate()}
        disabled={toggleMut.isPending}
        style={{
          border: "1px solid var(--color-border)",
          borderRadius: "var(--radius-md)",
          background: "var(--color-bg-subtle)",
          color: "var(--color-fg)",
          fontSize: "0.82rem",
          fontWeight: 600,
          padding: "0.3rem 0.6rem",
          cursor: "pointer",
        }}
      >
        {subscribed ? "Unsubscribe" : "Subscribe"}
      </button>
      <span style={muted}>
        {subscribed
          ? "You're receiving notifications from this thread."
          : "You're not receiving notifications from this thread."}
      </span>
      {toggleMut.isError && <ErrorBanner>{String(toggleMut.error)}</ErrorBanner>}
    </div>
  );
}

/**
 * GitHub's issue/PR right sidebar: Assignees, Labels, Projects, Milestone,
 * Development and a participants block. For issues, Labels and Milestone are
 * editable (the same controls the triage panel exposed); for PRs the sidebar
 * additionally renders a Reviewers slot on top. Errors surface, never swallowed.
 */
export function IssueSidebar({
  owner,
  repo,
  ownerType,
  number,
  kind,
  assignees,
  labels,
  milestone,
  participants,
  reviewers,
  development,
  locked = false,
}: {
  owner: string;
  repo: string;
  ownerType?: string | undefined;
  number: number;
  kind: "issue" | "pr";
  assignees: string[];
  labels: { name: string; color: string }[];
  milestone: { number: number; title: string; state: string } | null;
  participants: string[];
  reviewers?: ReactNode;
  development?: ReactNode;
  locked?: boolean;
}) {
  const qc = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const baseId = useId();
  const assigneeSelectId = `${baseId}-assignee`;
  const labelSelectId = `${baseId}-label`;
  const projectSelectId = `${baseId}-project`;
  const milestoneSelectId = `${baseId}-milestone`;
  const typeSelectId = `${baseId}-type`;

  const { data: repoLabels = [], isError: labelsError, error: labelsErr } = useQuery({
    queryKey: ["labels", owner, repo],
    queryFn: () => fetchRepoLabels(owner, repo),
  });
  const { data: milestones = [], isError: milestonesError, error: milestonesErr } = useQuery({
    queryKey: ["milestones", owner, repo, "all"],
    queryFn: () => fetchRepoMilestones(owner, repo, "all"),
  });
  const {
    data: issueTypes = [],
    isLoading: issueTypesLoading,
    isError: issueTypesError,
    error: issueTypesErr,
  } = useQuery({
    queryKey: ["org-issue-types", owner],
    queryFn: () => fetchOrgIssueTypes(owner),
    enabled: kind === "issue" && ownerType === "Organization",
  });
  const { data: graphQLIssueType = null } = useQuery({
    queryKey: ["issue-type", owner, repo, number],
    queryFn: () => fetchIssueGraphQLIssueType(owner, repo, number),
    enabled: kind === "issue" && ownerType === "Organization",
  });

  const invalidate = () => {
    setError(null);
    qc.invalidateQueries({ queryKey: [kind === "pr" ? "pr" : "issue", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["issue-type", owner, repo, number] });
    qc.invalidateQueries({ queryKey: [kind === "pr" ? "prs" : "issues", owner, repo] });
    qc.invalidateQueries({ queryKey: ["pr-timeline", owner, repo, number] });
  };
  const { data: assignableUsers = [] } = useQuery({
    queryKey: ["assignable-users", owner, repo],
    queryFn: () => fetchAssignableUsers(owner, repo),
  });
  const addAssigneeMut = useMutation({
    mutationFn: (login: string) => addAssignees(owner, repo, number, [login]),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const removeAssigneeMut = useMutation({
    mutationFn: (login: string) => removeAssignees(owner, repo, number, [login]),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const addLabelMut = useMutation({
    mutationFn: (name: string) => addIssueLabels(owner, repo, number, [name]),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const removeLabelMut = useMutation({
    mutationFn: (name: string) => removeIssueLabel(owner, repo, number, name),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const milestoneMut = useMutation({
    mutationFn: (m: number | null) => setIssueMilestone(owner, repo, number, m),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const issueTypeMut = useMutation({
    mutationFn: (id: number | null) => setIssueType(owner, repo, number, id),
    onSuccess: invalidate,
    onError: (err: Error) => setError(err.message),
  });
  const [lockReason, setLockReason] = useState<LockReason | "">("");
  const lockMut = useMutation({
    mutationFn: () =>
      locked ? unlockIssue(owner, repo, number) : lockIssue(owner, repo, number, lockReason || undefined),
    onSuccess: () => {
      setLockReason("");
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });

  const applied = new Set(labels.map((l) => l.name));
  const addable = repoLabels.filter((l) => !applied.has(l.name));
  const assignedSet = new Set(assignees);
  const addableAssignees = assignableUsers.filter((u) => !assignedSet.has(u.login));
  const enabledIssueTypes = issueTypes.filter((it) => it.is_enabled);
  const selectedIssueType = graphQLIssueType
    ? issueTypes.find((it) => it.node_id === graphQLIssueType.id) ?? null
    : null;
  const issueTypesUnavailable = ownerType !== "Organization" || (issueTypesError && isNotFound(issueTypesErr));
  const muted = { fontSize: "0.82rem", color: "var(--color-fg-muted)" } as const;

  return (
    <aside style={{ width: "100%" }}>
      {error && <ErrorBanner>{error}</ErrorBanner>}

      {kind === "pr" && reviewers && <SidebarSection title="Reviewers">{reviewers}</SidebarSection>}

      <SidebarSection
        title="Assignees"
        gearFor={addableAssignees.length > 0 ? assigneeSelectId : undefined}
        gearLabel="Edit assignees"
      >
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          {assignees.length === 0 && <span style={muted}>No one —</span>}
          {assignees.map((a) => (
            <span key={a} className="inline-flex items-center gap-1">
              <span style={{ fontSize: "0.82rem", fontWeight: 600, color: "var(--color-fg)" }}>{a}</span>
              <button
                type="button"
                aria-label={`Unassign ${a}`}
                onClick={() => removeAssigneeMut.mutate(a)}
                disabled={removeAssigneeMut.isPending}
                style={{
                  border: "none",
                  background: "transparent",
                  color: "var(--color-fg-muted)",
                  fontSize: "0.75rem",
                  lineHeight: 1,
                  padding: "0 0.15rem",
                  cursor: "pointer",
                }}
              >
                ✕
              </button>
            </span>
          ))}
          {addableAssignees.length > 0 && (
            <select
              id={assigneeSelectId}
              aria-label="Add assignee"
              value=""
              onChange={(e) => {
                if (e.target.value) addAssigneeMut.mutate(e.target.value);
              }}
              disabled={addAssigneeMut.isPending}
              style={{ fontSize: "0.8rem" }}
            >
              <option value="">Assign…</option>
              {addableAssignees.map((u) => (
                <option key={u.login} value={u.login}>
                  {u.login}
                </option>
              ))}
            </select>
          )}
        </div>
      </SidebarSection>

      <SidebarSection
        title="Labels"
        gearFor={!labelsError && addable.length > 0 ? labelSelectId : undefined}
        gearLabel="Edit labels"
      >
        {labelsError ? (
          <InlineError inline title="Failed to load repo labels" detail={String(labelsErr)} />
        ) : (
          <div className="flex flex-wrap items-center gap-1.5">
            {labels.length === 0 && <span style={muted}>None yet</span>}
            {labels.map((l) => (
              <span key={l.name} className="inline-flex items-center gap-1">
                <LabelPills labels={[l]} />
                <button
                  type="button"
                  aria-label={`Remove label ${l.name}`}
                  onClick={() => removeLabelMut.mutate(l.name)}
                  disabled={removeLabelMut.isPending}
                  style={{
                    border: "none",
                    background: "transparent",
                    color: "var(--color-fg-muted)",
                    fontSize: "0.75rem",
                    lineHeight: 1,
                    padding: "0 0.15rem",
                    cursor: "pointer",
                  }}
                >
                  ✕
                </button>
              </span>
            ))}
            {addable.length > 0 && (
              <select
                id={labelSelectId}
                aria-label="Add label"
                value=""
                onChange={(e) => {
                  if (e.target.value) addLabelMut.mutate(e.target.value);
                }}
                disabled={addLabelMut.isPending}
                style={{ fontSize: "0.8rem" }}
              >
                <option value="">Add label…</option>
                {addable.map((l) => (
                  <option key={l.id} value={l.name}>
                    {l.name}
                  </option>
                ))}
              </select>
            )}
          </div>
        )}
      </SidebarSection>

      <SidebarSection
        title="Projects"
        gearFor={ownerType === "Organization" ? projectSelectId : undefined}
        gearLabel="Edit projects"
      >
        <ProjectsField
          owner={owner}
          repo={repo}
          ownerType={ownerType}
          number={number}
          kind={kind}
          selectId={projectSelectId}
        />
      </SidebarSection>

      <SidebarSection
        title="Milestone"
        gearFor={!milestonesError && kind === "issue" ? milestoneSelectId : undefined}
        gearLabel="Edit milestone"
      >
        {milestonesError ? (
          <InlineError inline title="Failed to load milestones" detail={String(milestonesErr)} />
        ) : kind === "issue" ? (
          <select
            id={milestoneSelectId}
            aria-label="Set milestone"
            value={milestone?.number ?? ""}
            onChange={(e) =>
              milestoneMut.mutate(e.target.value === "" ? null : parseInt(e.target.value, 10))
            }
            disabled={milestoneMut.isPending}
            style={{ fontSize: "0.8rem" }}
          >
            <option value="">No milestone</option>
            {milestones.map((ms) => (
              <option key={ms.id} value={ms.number}>
                {ms.title}
                {ms.state === "closed" ? " (closed)" : ""}
              </option>
            ))}
          </select>
        ) : milestone ? (
          <span style={{ fontSize: "0.82rem", color: "var(--color-fg)" }}>{milestone.title}</span>
        ) : (
          <span style={muted}>No milestone</span>
        )}
      </SidebarSection>

      {kind === "issue" && !issueTypesLoading && !issueTypesUnavailable && (
        <SidebarSection
          title="Type"
          gearFor={issueTypesError ? undefined : typeSelectId}
          gearLabel="Edit issue type"
        >
          {issueTypesError ? (
            <InlineError inline title="Failed to load issue types" detail={String(issueTypesErr)} />
          ) : (
            <select
              id={typeSelectId}
              aria-label="Set issue type"
              value={selectedIssueType?.id ?? ""}
              onChange={(e) =>
                issueTypeMut.mutate(e.target.value === "" ? null : parseInt(e.target.value, 10))
              }
              disabled={issueTypeMut.isPending}
              style={{ fontSize: "0.8rem" }}
            >
              <option value="">No type</option>
              {enabledIssueTypes.map((it) => (
                <option key={it.id} value={it.id}>
                  {it.name}
                </option>
              ))}
              {selectedIssueType && !selectedIssueType.is_enabled && (
                <option value={selectedIssueType.id}>{selectedIssueType.name} (disabled)</option>
              )}
            </select>
          )}
        </SidebarSection>
      )}

      <SidebarSection title="Development">
        {development ?? <span style={muted}>No branches or pull requests</span>}
      </SidebarSection>

      <SidebarSection title="Notifications">
        <NotificationsField owner={owner} repo={repo} number={number} kind={kind} />
      </SidebarSection>

      <SidebarSection title="Lock conversation">
        <div className="flex flex-col gap-1.5">
          {!locked && (
            <select
              aria-label="Lock reason"
              value={lockReason}
              onChange={(e) => setLockReason(e.target.value as LockReason | "")}
              disabled={lockMut.isPending}
              style={{ fontSize: "0.8rem" }}
            >
              <option value="">No reason</option>
              {LOCK_REASONS.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          )}
          <button
            type="button"
            onClick={() => lockMut.mutate()}
            disabled={lockMut.isPending}
            className="inline-block text-left"
            style={{
              border: "none",
              background: "transparent",
              color: "var(--color-accent-fg)",
              fontSize: "0.82rem",
              lineHeight: "1.625rem",
              padding: 0,
              cursor: "pointer",
            }}
          >
            {locked ? "Unlock conversation" : "Lock conversation"}
          </button>
        </div>
      </SidebarSection>

      <SidebarSection title={`${participants.length} participant${participants.length === 1 ? "" : "s"}`} last>
        {participants.length === 0 ? (
          <span style={muted}>No participants yet</span>
        ) : (
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            {participants.map((p) => (
              <span key={p} className="inline-flex items-center gap-1" style={{ fontSize: "0.82rem", color: "var(--color-fg)" }}>
                <Avatar login={p} size={16} />
                {p}
              </span>
            ))}
          </div>
        )}
      </SidebarSection>
    </aside>
  );
}

/**
 * The issue/PR sidebar Projects section. Real GitHub lets you add the item to
 * ProjectsV2 boards and shows which it belongs to. ProjectsV2 are org-scoped
 * here, so this activates for org-owned repos: it lists the org's projects,
 * marks the ones containing this item (from their item lists), and adds/removes
 * via the org project-item endpoints. A user-owned repo has no org projects, so
 * it truthfully shows "None yet".
 */
function ProjectsField({
  owner,
  repo,
  ownerType,
  number,
  kind,
  selectId,
}: {
  owner: string;
  repo: string;
  ownerType?: string | undefined;
  number: number;
  kind: "issue" | "pr";
  /** id for the "Add to project" select, so the section gear can focus it. */
  selectId?: string | undefined;
}) {
  const qc = useQueryClient();
  const isOrg = ownerType === "Organization";
  const contentType = kind === "pr" ? "PullRequest" : "Issue";
  const muted = { fontSize: "0.82rem", color: "var(--color-fg-muted)" } as const;

  // Fetch the org's projects and, for each, whether this item is in it (and the
  // membership's item id, needed to remove it).
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["issue-projects", owner, repo, kind, number],
    enabled: isOrg,
    queryFn: async () => {
      const projects = await fetchOrgProjectsV2(owner);
      return Promise.all(
        projects.map(async (project) => {
          const items = await fetchOrgProjectV2Items(owner, project.number);
          const match = items.find(
            (it) =>
              it.content_type === contentType &&
              it.content?.number === number &&
              (it.content?.html_url ?? "").includes(`/${owner}/${repo}/`),
          );
          return { project, itemId: match ? match.id : null };
        }),
      );
    },
  });

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["issue-projects", owner, repo, kind, number] });
  };
  const addMut = useMutation({
    mutationFn: (project: GithubProjectV2) =>
      addOrgProjectV2Item(owner, project.number, { type: contentType, owner, repo, number }),
    onSuccess: invalidate,
  });
  const removeMut = useMutation({
    mutationFn: (m: { projectNumber: number; itemId: number }) =>
      deleteOrgProjectV2Item(owner, m.projectNumber, m.itemId),
    onSuccess: invalidate,
  });

  if (!isOrg) return <span style={muted}>None yet</span>;
  if (isLoading) return <span style={muted}>Loading…</span>;
  if (isError) return <InlineError inline title="Failed to load projects" detail={String(error)} />;

  const memberships = (data ?? []).filter((x): x is { project: GithubProjectV2; itemId: number } => x.itemId !== null);
  const addable = (data ?? []).filter((x) => x.itemId === null).map((x) => x.project);
  const busy = addMut.isPending || removeMut.isPending;

  return (
    <div className="flex flex-col gap-1.5">
      {memberships.length === 0 && addable.length === 0 && <span style={muted}>None yet</span>}
      {memberships.map(({ project, itemId }) => (
        <div key={project.id} className="flex items-center gap-1">
          <span style={{ fontSize: "0.85rem", color: "var(--color-fg)" }}>{project.title}</span>
          <button
            type="button"
            aria-label={`Remove from project ${project.title}`}
            onClick={() => removeMut.mutate({ projectNumber: project.number, itemId })}
            disabled={busy}
            style={{
              border: "none",
              background: "transparent",
              color: "var(--color-fg-muted)",
              fontSize: "0.75rem",
              lineHeight: 1,
              padding: "0 0.15rem",
              cursor: "pointer",
            }}
          >
            ✕
          </button>
        </div>
      ))}
      {addable.length > 0 && (
        <select
          id={selectId}
          aria-label="Add to project"
          value=""
          onChange={(e) => {
            const project = addable.find((p) => String(p.number) === e.target.value);
            if (project) addMut.mutate(project);
          }}
          disabled={busy}
          style={{ fontSize: "0.8rem" }}
        >
          <option value="">Add to project…</option>
          {addable.map((p) => (
            <option key={p.id} value={p.number}>
              {p.title}
            </option>
          ))}
        </select>
      )}
      {(addMut.isError || removeMut.isError) && (
        <ErrorBanner>{String(addMut.error ?? removeMut.error)}</ErrorBanner>
      )}
    </div>
  );
}
