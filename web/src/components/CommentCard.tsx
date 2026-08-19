import { useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient, type QueryKey } from "@tanstack/react-query";
import Markdown from "./Markdown";
import {
  updateIssueComment,
  deleteIssueComment,
  fetchIssueCommentReactions,
  addIssueCommentReaction,
  removeIssueCommentReaction,
  ghGraphQL,
} from "../api.js";
import type { GithubTimelineItem } from "../types.js";
import { Button, DialogActions } from "./ui.js";
import { MutationError } from "./MutationError.js";
import { confirmAction } from "./confirmAction.js";
import { ReactionBar } from "./ReactionBar.js";
import { TimelineEventRow } from "./TimelineEventRow.js";
import { Avatar } from "./Avatar.js";
import { RelativeTime } from "./RelativeTime.js";
import { MarkdownComposer } from "./MarkdownComposer.js";

export interface CommentCardProps {
  login?: string | undefined;
  body?: string | undefined;
  date: string;
  isOp?: boolean | undefined;
  /** Author avatar image; falls back to a monogram tile when absent. */
  avatarUrl?: string | null | undefined;
  /** Rendered at the right of the header — e.g. Edit/Delete controls. */
  headerActions?: ReactNode | undefined;
  /** When set, task-list checkboxes become interactive (index + new state). */
  onToggleTask?: ((index: number, checked: boolean) => void) | undefined;
}

export function CommentCard({ login, body, date, isOp = false, avatarUrl, headerActions, onToggleTask }: CommentCardProps) {
  return (
    <div
      style={{
        border: "1px solid var(--color-border)",
        borderRadius: "var(--radius-md)",
        marginBottom: "1rem",
        overflow: "hidden",
      }}
    >
      <div
        className="flex items-center gap-2"
        style={{
          padding: "0.5rem 0.85rem",
          background: "var(--color-bg-subtle)",
          borderBottom: "1px solid var(--color-border)",
          fontSize: "0.82rem",
          color: "var(--color-fg-muted)",
        }}
      >
        <Avatar login={login ?? "?"} src={avatarUrl} size={20} />
        <span style={{ color: "var(--color-fg)", fontWeight: 600 }}>{login}</span>
        <span>
          commented <RelativeTime iso={date} />
        </span>
        <span className="flex items-center gap-2" style={{ marginLeft: "auto" }}>
          {isOp && (
            <span
              style={{
                padding: "0.05rem 0.45rem",
                border: "1px solid var(--color-border)",
                borderRadius: "2rem",
                fontSize: "0.7rem",
                color: "var(--color-fg-muted)",
              }}
            >
              Author
            </span>
          )}
          {headerActions}
        </span>
      </div>
      <div
        className={body ? "markdown-body" : undefined}
        style={{
          padding: "0.85rem 1rem",
          fontSize: "0.9rem",
          lineHeight: 1.6,
          color: "var(--color-fg)",
          wordBreak: "break-word",
        }}
      >
        {body ? (
          <Markdown onToggleTask={onToggleTask}>{body}</Markdown>
        ) : (
          <span style={{ color: "var(--color-fg-muted)" }}>No description provided.</span>
        )}
      </div>
    </div>
  );
}

/**
 * A comment timeline whose entries can be edited and deleted in place, matching
 * github.com's per-comment actions. Edits/deletes go through the shared
 * issue-comments endpoints (issues and PRs alike) and invalidate `invalidateKeys`
 * — normally the comment list plus the issue/PR detail so its count refreshes.
 */
// Renders an issue/PR conversation timeline: comment events become editable
// comments (edit / delete / reactions), and every other event (labeled, assigned,
// closed, renamed, referenced, milestoned …) becomes a shared TimelineEventRow —
// interleaved in the order github.com shows them.
// github.com's "Hide" classifiers → GraphQL ReportedContentClassifiers /
// the stored MinimizedReason enum.
const HIDE_CLASSIFIERS = ["OFF_TOPIC", "OUTDATED", "RESOLVED", "DUPLICATE", "SPAM", "ABUSE"] as const;
type HideClassifier = (typeof HIDE_CLASSIFIERS)[number];

const hideReasonLabel = (reason?: string | null): string =>
  ({ OFF_TOPIC: "off-topic", OUTDATED: "outdated", RESOLVED: "resolved", DUPLICATE: "duplicate", SPAM: "spam", ABUSE: "abuse" }[String(reason)] ?? "hidden");

function minimizeComment(subjectId: string, classifier: HideClassifier): Promise<unknown> {
  return ghGraphQL(
    `mutation($input: MinimizeCommentInput!) { minimizeComment(input: $input) { minimizedComment { isMinimized } } }`,
    { input: { subjectId, classifier } },
  );
}
function unminimizeComment(subjectId: string): Promise<unknown> {
  return ghGraphQL(
    `mutation($input: UnminimizeCommentInput!) { unminimizeComment(input: $input) { unminimizedComment { isMinimized } } }`,
    { input: { subjectId } },
  );
}

interface MinimizationState {
  isMinimized: boolean;
  minimizedReason: string | null;
}
async function fetchCommentMinimization(
  owner: string,
  repo: string,
  number: number,
): Promise<Map<number, MinimizationState>> {
  const data = await ghGraphQL<{
    repository: { issue: { comments: { nodes: { databaseId: number; isMinimized: boolean; minimizedReason: string | null }[] } } | null } | null;
  }>(
    `query($owner: String!, $repo: String!, $n: Int!) {
      repository(owner: $owner, name: $repo) {
        issue(number: $n) { comments(first: 100) { nodes { databaseId isMinimized minimizedReason } } }
      }
    }`,
    { owner, repo, n: number },
  );
  const map = new Map<number, MinimizationState>();
  for (const c of data.repository?.issue?.comments.nodes ?? []) {
    map.set(c.databaseId, { isMinimized: c.isMinimized, minimizedReason: c.minimizedReason });
  }
  return map;
}

export function EditableCommentList({
  owner,
  repo,
  number,
  items,
  invalidateKeys,
  viewerLogin,
}: {
  owner: string;
  repo: string;
  number?: number;
  items: GithubTimelineItem[];
  invalidateKeys: QueryKey[];
  viewerLogin?: string | null | undefined;
}) {
  const qc = useQueryClient();
  const [editingId, setEditingId] = useState<number | null>(null);
  const [draft, setDraft] = useState("");
  const invalidate = () => {
    for (const key of invalidateKeys) qc.invalidateQueries({ queryKey: key });
  };
  const editMut = useMutation({
    mutationFn: (v: { id: number; body: string }) => updateIssueComment(owner, repo, v.id, v.body),
    onSuccess: () => {
      invalidate();
      setEditingId(null);
    },
  });
  const deleteMut = useMutation({
    mutationFn: (id: number) => deleteIssueComment(owner, repo, id),
    onSuccess: invalidate,
  });

  const minimizedQuery = useQuery({
    queryKey: ["issue-comment-minimization", owner, repo, number],
    queryFn: () => fetchCommentMinimization(owner, repo, number as number),
    enabled: typeof number === "number",
  });
  const minimized = minimizedQuery.data ?? new Map<number, MinimizationState>();
  const refetchMinimized = () => {
    invalidate();
    minimizedQuery.refetch();
  };
  const minimizeMut = useMutation({
    mutationFn: (v: { subjectId: string; classifier: HideClassifier }) => minimizeComment(v.subjectId, v.classifier),
    onSuccess: refetchMinimized,
  });
  const unminimizeMut = useMutation({
    mutationFn: (subjectId: string) => unminimizeComment(subjectId),
    onSuccess: refetchMinimized,
  });
  // Comment id currently showing its hide-reason picker, and the chosen reason.
  const [hidingId, setHidingId] = useState<number | null>(null);
  const [hideReason, setHideReason] = useState<HideClassifier>("OFF_TOPIC");
  // Minimized comments the viewer chose to expand ("Show").
  const [shown, setShown] = useState<Set<number>>(new Set());

  return (
    <>
      {items.map((item, index) => {
        // Non-comment events render as a shared timeline row.
        if (item.event !== "commented" || typeof item.id !== "number") {
          return <TimelineEventRow key={`${item.event}-${item.id ?? index}`} item={item} />;
        }
        const c = { id: item.id, node_id: item.node_id, body: item.body, created_at: item.created_at ?? "", user: item.user };
        const min = minimized.get(c.id);
        // A minimized comment renders collapsed until the viewer clicks Show.
        if (min?.isMinimized && !shown.has(c.id)) {
          return (
            <div
              key={c.id}
              className="mb-4 flex items-center justify-between gap-2"
              style={{
                border: "1px solid var(--color-border)",
                borderRadius: "var(--radius-md)",
                padding: "0.55rem 0.9rem",
                color: "var(--color-fg-muted)",
                fontSize: "0.85rem",
              }}
            >
              <span>
                {c.user?.login ?? "someone"}'s comment was hidden — {hideReasonLabel(min.minimizedReason)}
              </span>
              <span className="flex gap-2">
                <Button size="sm" variant="ghost" onClick={() => setShown((s) => new Set(s).add(c.id))}>
                  Show
                </Button>
                {c.node_id && (
                  <Button
                    size="sm"
                    variant="ghost"
                    aria-label="Unhide comment"
                    disabled={unminimizeMut.isPending}
                    onClick={() => unminimizeMut.mutate(c.node_id as string)}
                  >
                    Unhide
                  </Button>
                )}
              </span>
            </div>
          );
        }
        return editingId === c.id ? (
          <div
            key={c.id}
            className="mb-4"
            style={{
              border: "1px solid var(--color-border)",
              borderRadius: "var(--radius-md)",
              padding: "0.85rem 1rem",
            }}
          >
            <div className="mb-2">
              <MarkdownComposer
                id={`edit-comment-${c.id}`}
                label="Edit comment"
                value={draft}
                onChange={setDraft}
                rows={4}
                disabled={editMut.isPending}
              />
            </div>
            <MutationError of={editMut} />
            <DialogActions>
              <Button variant="ghost" size="sm" onClick={() => setEditingId(null)}>
                Cancel
              </Button>
              <Button
                variant="primary"
                size="sm"
                disabled={!draft.trim() || editMut.isPending}
                onClick={() => editMut.mutate({ id: c.id, body: draft })}
              >
                {editMut.isPending ? "Saving…" : "Save"}
              </Button>
            </DialogActions>
          </div>
        ) : (
          <div key={c.id}>
          <CommentCard
            login={c.user?.login}
            avatarUrl={c.user?.avatar_url}
            body={c.body}
            date={c.created_at}
            headerActions={
              <>
                <Button
                  size="sm"
                  aria-label="Edit comment"
                  onClick={() => {
                    setEditingId(c.id);
                    setDraft(c.body ?? "");
                  }}
                >
                  Edit
                </Button>
                <Button
                  size="sm"
                  aria-label="Delete comment"
                  onClick={async () => {
                    if (
                      await confirmAction("Delete this comment?", {
                        title: "Delete comment",
                        confirmLabel: "Delete",
                      })
                    ) {
                      deleteMut.mutate(c.id);
                    }
                  }}
                >
                  Delete
                </Button>
                {c.node_id && typeof number === "number" && (
                  hidingId === c.id ? (
                    <>
                      <select
                        aria-label="Reason for hiding"
                        value={hideReason}
                        onChange={(e) => setHideReason(e.target.value as HideClassifier)}
                      >
                        {HIDE_CLASSIFIERS.map((r) => (
                          <option key={r} value={r}>
                            {hideReasonLabel(r)}
                          </option>
                        ))}
                      </select>
                      <Button
                        size="sm"
                        variant="danger"
                        disabled={minimizeMut.isPending}
                        onClick={() =>
                          minimizeMut.mutate(
                            { subjectId: c.node_id as string, classifier: hideReason },
                            { onSuccess: () => setHidingId(null) },
                          )
                        }
                      >
                        Hide
                      </Button>
                      <Button size="sm" variant="ghost" onClick={() => setHidingId(null)}>
                        Cancel
                      </Button>
                    </>
                  ) : (
                    <Button
                      size="sm"
                      aria-label="Hide comment"
                      onClick={() => {
                        setHideReason("OFF_TOPIC");
                        setHidingId(c.id);
                      }}
                    >
                      Hide
                    </Button>
                  )
                )}
              </>
            }
          />
          <ReactionBar
            queryKey={["issue-comment-reactions", owner, repo, c.id]}
            fetchList={() => fetchIssueCommentReactions(owner, repo, c.id)}
            add={(content) => addIssueCommentReaction(owner, repo, c.id, content)}
            remove={(reactionId) => removeIssueCommentReaction(owner, repo, c.id, reactionId)}
            viewerLogin={viewerLogin ?? null}
          />
          </div>
        );
      })}
      <MutationError of={deleteMut} />
      <MutationError of={minimizeMut} />
      <MutationError of={unminimizeMut} />
    </>
  );
}
