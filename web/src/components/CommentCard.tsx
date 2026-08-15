import { useState, type ReactNode } from "react";
import { useMutation, useQueryClient, type QueryKey } from "@tanstack/react-query";
import Markdown from "./Markdown";
import {
  updateIssueComment,
  deleteIssueComment,
  fetchIssueCommentReactions,
  addIssueCommentReaction,
  removeIssueCommentReaction,
} from "../api.js";
import type { GithubTimelineItem } from "../types.js";
import { Button, DialogActions, FormLabel } from "./ui.js";
import { MutationError } from "./MutationError.js";
import { confirmAction } from "./confirmAction.js";
import { ReactionBar } from "./ReactionBar.js";
import { TimelineEventRow } from "./TimelineEventRow.js";

export interface CommentCardProps {
  login?: string | undefined;
  body?: string | undefined;
  date: string;
  isOp?: boolean | undefined;
  /** Rendered at the right of the header — e.g. Edit/Delete controls. */
  headerActions?: ReactNode | undefined;
  /** When set, task-list checkboxes become interactive (index + new state). */
  onToggleTask?: ((index: number, checked: boolean) => void) | undefined;
}

export function CommentCard({ login, body, date, isOp = false, headerActions, onToggleTask }: CommentCardProps) {
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
        <span style={{ color: "var(--color-fg)", fontWeight: 600 }}>{login}</span>
        <span>commented {new Date(date).toLocaleString()}</span>
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
export function EditableCommentList({
  owner,
  repo,
  items,
  invalidateKeys,
  viewerLogin,
}: {
  owner: string;
  repo: string;
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

  return (
    <>
      {items.map((item, index) => {
        // Non-comment events render as a shared timeline row.
        if (item.event !== "commented" || typeof item.id !== "number") {
          return <TimelineEventRow key={`${item.event}-${item.id ?? index}`} item={item} />;
        }
        const c = { id: item.id, body: item.body, created_at: item.created_at ?? "", user: item.user };
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
            <FormLabel id={`edit-comment-${c.id}`}>Edit comment</FormLabel>
            <textarea
              id={`edit-comment-${c.id}`}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              rows={4}
              className="mb-2 w-full"
              style={{ resize: "vertical" }}
            />
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
    </>
  );
}
