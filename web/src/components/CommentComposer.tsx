import { useState, type ReactNode } from "react";
import { useMutation, useQueryClient, type QueryKey } from "@tanstack/react-query";
import { createIssueComment } from "../api.js";
import { Button } from "./ui.js";
import { MutationError } from "./MutationError.js";
import { MarkdownComposer } from "./MarkdownComposer.js";
import { clearComposerDraft } from "../hooks/useComposerDraft.js";

/** Draft-durability key, shared with the close-with-comment path. */
export const issueCommentDraftKey = (owner: string, repo: string, number: number) =>
  `issue-comment:${owner}/${repo}/${number}`;

/**
 * Comment composer for an issue or PR — GitHub serves both from the shared
 * issue-comments endpoint. Pass `body`/`onBodyChange` together to let the
 * caller own the draft (needed for "Close with comment").
 */
export function CommentComposer({
  owner,
  repo,
  number,
  invalidateKeys,
  extraActions,
  body: controlledBody,
  onBodyChange,
}: {
  owner: string;
  repo: string;
  number: number;
  invalidateKeys: QueryKey[];
  extraActions?: ReactNode;
  body?: string | undefined;
  onBodyChange?: ((body: string) => void) | undefined;
}) {
  const qc = useQueryClient();
  const [ownBody, setOwnBody] = useState("");
  const controlled = controlledBody !== undefined && onBodyChange !== undefined;
  const body = controlled ? controlledBody : ownBody;
  const setBody = controlled ? onBodyChange : setOwnBody;
  const draftKey = issueCommentDraftKey(owner, repo, number);
  const mut = useMutation({
    mutationFn: () => createIssueComment(owner, repo, number, body),
    onSuccess: () => {
      for (const key of invalidateKeys) qc.invalidateQueries({ queryKey: key });
      setBody("");
      clearComposerDraft(draftKey);
    },
  });

  return (
    <div
      className="mt-4 border-t pt-4"
      style={{ borderColor: "var(--color-border)" }}
    >
      <div className="mb-2">
        <MarkdownComposer
          id="new-comment"
          label="Add a comment"
          draftKey={draftKey}
          value={body}
          onChange={setBody}
          rows={4}
          placeholder="Leave a comment"
          disabled={mut.isPending}
        />
      </div>
      <MutationError of={mut} />
      <div className="flex items-center justify-end gap-2">
        {extraActions}
        <Button
          variant="primary"
          size="sm"
          disabled={!body.trim() || mut.isPending}
          onClick={() => mut.mutate()}
        >
          {mut.isPending ? "Commenting…" : "Comment"}
        </Button>
      </div>
    </div>
  );
}
