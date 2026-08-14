import { useState, type ReactNode } from "react";
import { useMutation, useQueryClient, type QueryKey } from "@tanstack/react-query";
import Markdown from "./Markdown";
import { updateIssueComment, deleteIssueComment } from "../api.js";
import type { GithubComment } from "../types.js";
import { Button, DialogActions, FormLabel } from "./ui.js";
import { MutationError } from "./MutationError.js";
import { confirmAction } from "./confirmAction.js";

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
export function EditableCommentList({
  owner,
  repo,
  comments,
  invalidateKeys,
}: {
  owner: string;
  repo: string;
  comments: GithubComment[];
  invalidateKeys: QueryKey[];
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
      {comments.map((c) =>
        editingId === c.id ? (
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
          <CommentCard
            key={c.id}
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
        ),
      )}
      <MutationError of={deleteMut} />
    </>
  );
}
