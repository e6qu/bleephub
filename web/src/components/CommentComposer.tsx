import { useState, type ReactNode } from "react";
import { useMutation, useQueryClient, type QueryKey } from "@tanstack/react-query";
import { createIssueComment } from "../api.js";
import { Button } from "./ui.js";
import { MutationError } from "./MutationError.js";
import { MarkdownComposer } from "./MarkdownComposer.js";

/**
 * Comment composer for an issue or pull request. GitHub models a PR's
 * conversation on the shared issue-comments endpoint, so the same box serves
 * both surfaces. On success it invalidates `invalidateKeys` (the comment list,
 * and usually the issue/PR detail so its comment count refreshes) and clears
 * the field. `extraActions` renders to the left of the Comment button — used to
 * place the "Close/Reopen" control alongside the box, as github.com does.
 *
 * The draft can optionally be lifted by the caller via `body`/`onBodyChange`
 * (both must be given together) — github.com's "Close with comment" needs the
 * page to read the draft to post it alongside the state change.
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
  /** Controlled draft value; when provided with onBodyChange, the caller owns the draft. */
  body?: string | undefined;
  onBodyChange?: ((body: string) => void) | undefined;
}) {
  const qc = useQueryClient();
  const [ownBody, setOwnBody] = useState("");
  const controlled = controlledBody !== undefined && onBodyChange !== undefined;
  const body = controlled ? controlledBody : ownBody;
  const setBody = controlled ? onBodyChange : setOwnBody;
  const mut = useMutation({
    mutationFn: () => createIssueComment(owner, repo, number, body),
    onSuccess: () => {
      for (const key of invalidateKeys) qc.invalidateQueries({ queryKey: key });
      setBody("");
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
