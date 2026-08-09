import { useState, type ReactNode } from "react";
import { useMutation, useQueryClient, type QueryKey } from "@tanstack/react-query";
import { createIssueComment } from "../api.js";
import { Button, FormLabel } from "./ui.js";
import { MutationError } from "./MutationError.js";

/**
 * Comment composer for an issue or pull request. GitHub models a PR's
 * conversation on the shared issue-comments endpoint, so the same box serves
 * both surfaces. On success it invalidates `invalidateKeys` (the comment list,
 * and usually the issue/PR detail so its comment count refreshes) and clears
 * the field. `extraActions` renders to the left of the Comment button — used to
 * place the "Close/Reopen" control alongside the box, as github.com does.
 */
export function CommentComposer({
  owner,
  repo,
  number,
  invalidateKeys,
  extraActions,
}: {
  owner: string;
  repo: string;
  number: number;
  invalidateKeys: QueryKey[];
  extraActions?: ReactNode;
}) {
  const qc = useQueryClient();
  const [body, setBody] = useState("");
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
      <FormLabel id="new-comment">Add a comment</FormLabel>
      <textarea
        id="new-comment"
        value={body}
        onChange={(e) => setBody(e.target.value)}
        rows={4}
        placeholder="Leave a comment"
        className="mb-2 w-full"
        style={{ resize: "vertical" }}
      />
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
