import { Fragment, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { InlineError } from "@bleephub/ui-core/components";
import {
  replyToPRReviewComment,
  setPRReviewThreadResolved,
  fetchPullReviewCommentReactions,
  addPullReviewCommentReaction,
  removePullReviewCommentReaction,
  ghPostJSON,
  ApiError,
} from "../api.js";
import type { GithubPRReviewComment } from "../types.js";
import { useRepoPermissions } from "../hooks/useRepoPermissions.js";
import { Box, Button } from "./ui.js";
import { Avatar } from "./Avatar.js";
import { RelativeTime } from "./RelativeTime.js";
import { MarkdownComposer } from "./MarkdownComposer.js";
import { clearComposerDraft } from "../hooks/useComposerDraft.js";
import Markdown from "./Markdown.js";
import { ReactionBar } from "./ReactionBar.js";

// Inline review-thread rendering shared by the PR Conversation stream
// (PullsPage) and the Files-changed diff view (PRFilesView).

export interface ReviewThreadGroup {
  root: GithubPRReviewComment;
  comments: GithubPRReviewComment[];
}

/** Group flat review comments into threads by following in_reply_to_id. */
export function groupReviewThreads(comments: GithubPRReviewComment[]): ReviewThreadGroup[] {
  const byId = new Map(comments.map((c) => [c.id, c]));
  const rootOf = (c: GithubPRReviewComment): GithubPRReviewComment => {
    let cur = c;
    const seen = new Set<number>();
    while (cur.in_reply_to_id != null && byId.has(cur.in_reply_to_id) && !seen.has(cur.id)) {
      seen.add(cur.id);
      cur = byId.get(cur.in_reply_to_id) as GithubPRReviewComment;
    }
    return cur;
  };
  const groups = new Map<number, ReviewThreadGroup>();
  const sorted = [...comments].sort(
    (a, b) => a.created_at.localeCompare(b.created_at) || a.id - b.id,
  );
  for (const c of sorted) {
    const root = rootOf(c);
    const g = groups.get(root.id) ?? { root, comments: [] };
    g.comments.push(c);
    groups.set(root.id, g);
  }
  return [...groups.values()];
}

// ─── ```suggestion fences ────────────────────────────────────────────────

interface BodySegment {
  kind: "markdown" | "suggestion";
  content: string;
}

/** Split a comment body into markdown text and ```suggestion fence contents. */
export function splitSuggestionSegments(body: string): BodySegment[] {
  const segments: BodySegment[] = [];
  const re = /```suggestion[^\S\n]*\r?\n([\s\S]*?)```/g;
  let last = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(body)) !== null) {
    if (m.index > last) segments.push({ kind: "markdown", content: body.slice(last, m.index) });
    // Drop the single trailing newline the fence syntax forces.
    segments.push({ kind: "suggestion", content: (m[1] ?? "").replace(/\r?\n$/, "") });
    last = m.index + m[0].length;
  }
  if (last < body.length) segments.push({ kind: "markdown", content: body.slice(last) });
  if (segments.length === 0) segments.push({ kind: "markdown", content: body });
  return segments;
}

/** The last content line of a diff hunk with its marker stripped — the line a
 * suggestion replaces. */
function suggestionTargetLine(diffHunk: string | undefined): string | null {
  if (!diffHunk) return null;
  const lines = diffHunk.split("\n").filter((l) => l !== "" && !l.startsWith("@@") && !l.startsWith("\\"));
  const lastLine = lines[lines.length - 1];
  if (lastLine === undefined) return null;
  return lastLine.slice(1);
}

const suggestionRowStyle = {
  margin: 0,
  padding: "0.15rem 0.75rem",
  fontSize: "0.76rem",
  lineHeight: 1.6,
  whiteSpace: "pre-wrap",
  wordBreak: "break-all",
} as const;

/**
 * A ```suggestion fence rendered as a small diff: the original line struck
 * out in red, the suggested replacement in green. The "Commit suggestion"
 * action renders separately (CommitSuggestionButton) when the caller
 * provides PR coordinates.
 */
function SuggestionBlock({ suggestion, original }: { suggestion: string; original: string | null }) {
  return (
    <div
      className="mb-2 mt-1"
      style={{
        border: "1px solid var(--color-border)",
        borderRadius: "var(--radius-md)",
        overflow: "hidden",
      }}
    >
      <div
        style={{
          padding: "0.3rem 0.75rem",
          fontSize: "0.76rem",
          color: "var(--color-fg-muted)",
          borderBottom: "1px solid var(--color-border)",
          background: "var(--color-bg-subtle)",
        }}
      >
        Suggested change
      </div>
      {original !== null && (
        <del
          className="block font-mono"
          style={{
            ...suggestionRowStyle,
            display: "block",
            textDecoration: "line-through",
            background: "color-mix(in srgb, var(--color-status-error) 14%, transparent)",
            color: "var(--color-fg)",
          }}
        >
          {original || " "}
        </del>
      )}
      {suggestion.split("\n").map((line, i) => (
        <ins
          key={i}
          className="block font-mono"
          style={{
            ...suggestionRowStyle,
            display: "block",
            textDecoration: "none",
            background: "color-mix(in srgb, var(--gh-open) 14%, transparent)",
            color: "var(--color-fg)",
          }}
        >
          {line || " "}
        </ins>
      ))}
    </div>
  );
}

/**
 * "Commit suggestion" — applies the comment's FIRST ```suggestion fence to
 * the PR head branch via the /ui-data apply-suggestion endpoint. The server
 * is the authority on push access and PR state, so those refusals (403/422)
 * surface inline; a 409 marks the suggestion outdated.
 */
function CommitSuggestionButton({
  owner,
  repo,
  number,
  comment,
}: {
  owner: string;
  repo: string;
  number: number;
  comment: GithubPRReviewComment;
}) {
  const qc = useQueryClient();
  // Committing a suggestion pushes to the PR head branch — github.com only
  // offers it to viewers with write access (hidden, not disabled, below).
  const { canPush } = useRepoPermissions(owner, repo);
  const apply = useMutation({
    mutationFn: () =>
      ghPostJSON<{ sha: string }>(
        `/ui-data/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/review-comments/${comment.id}/apply-suggestion`,
        {},
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["pr-files", owner, repo, number] });
      qc.invalidateQueries({ queryKey: ["pr-timeline", owner, repo, number] });
    },
  });
  const errorMessage = apply.error instanceof Error ? apply.error.message : String(apply.error ?? "");
  const outdated = apply.error instanceof ApiError && apply.error.status === 409;
  // LEFT-side and file-level suggestions can never apply — the server would
  // 422; disable up front with the reason.
  const disabledReason =
    comment.side === "LEFT"
      ? "Suggestions on the deleted side can't be applied"
      : comment.line == null
        ? "File-level suggestions can't be applied"
        : null;

  if (!canPush) return null;
  if (outdated) {
    return (
      <div className="mb-2 flex justify-end">
        <Button size="sm" disabled title={errorMessage}>
          Suggestion outdated
        </Button>
      </div>
    );
  }
  if (apply.isSuccess) {
    return (
      <div className="mb-2 flex justify-end">
        <Button size="sm" disabled>
          Suggestion applied
        </Button>
      </div>
    );
  }
  return (
    <div className="mb-2">
      <div className="flex justify-end">
        <Button
          size="sm"
          disabled={disabledReason != null || apply.isPending}
          title={disabledReason ?? "Commit this suggestion to the pull request head branch"}
          onClick={() => apply.mutate()}
        >
          {apply.isPending ? "Committing…" : "Commit suggestion"}
        </Button>
      </div>
      {apply.isError && (
        <InlineError inline title="Could not apply suggestion" detail={errorMessage} />
      )}
    </div>
  );
}

/**
 * A review comment's body with ```suggestion fences rendered as mini-diffs.
 * When PR coordinates are provided (all of owner/repo/number), the first
 * suggestion fence gets a "Commit suggestion" action — the server applies
 * only the first fence. Coordinates are optional so render-only callers
 * (pending drafts, older call sites) stay unchanged.
 */
export function ReviewCommentBody({
  comment,
  owner,
  repo,
  number,
}: {
  comment: GithubPRReviewComment;
  owner?: string;
  repo?: string;
  number?: number;
}) {
  const segments = splitSuggestionSegments(comment.body);
  const original = suggestionTargetLine(comment.diff_hunk);
  const canCommit = owner !== undefined && repo !== undefined && number !== undefined;
  const firstSuggestion = segments.findIndex((seg) => seg.kind === "suggestion");
  return (
    <div style={{ color: "var(--color-fg)" }} className="markdown-body">
      {segments.map((seg, i) =>
        seg.kind === "suggestion" ? (
          <Fragment key={i}>
            <SuggestionBlock suggestion={seg.content} original={original} />
            {canCommit && i === firstSuggestion && (
              <CommitSuggestionButton owner={owner} repo={repo} number={number} comment={comment} />
            )}
          </Fragment>
        ) : seg.content.trim() === "" ? null : (
          <Markdown key={i}>{seg.content}</Markdown>
        ),
      )}
    </div>
  );
}

// ─── Thread card ─────────────────────────────────────────────────────────

export function ReviewThreadCard({
  owner,
  repo,
  number,
  group,
  threadInfo,
  viewerLogin,
  hideDiffHunk = false,
}: {
  owner: string;
  repo: string;
  number: number;
  group: ReviewThreadGroup;
  threadInfo: { id: string; isResolved: boolean } | null;
  viewerLogin: string | null;
  /** In the Files-changed view the thread sits under its own diff row, so the
   * stored hunk context is redundant. */
  hideDiffHunk?: boolean;
}) {
  const qc = useQueryClient();
  // Resolving/unresolving a thread needs write access or authorship of one of
  // the thread's comments (github.com's rule); replying stays for everyone.
  const { canPush } = useRepoPermissions(owner, repo);
  const isThreadParticipant =
    viewerLogin !== null &&
    group.comments.some((c) => c.user?.login != null && c.user.login === viewerLogin);
  const canResolve = canPush || isThreadParticipant;
  const [replyBody, setReplyBody] = useState("");
  // Draft durability per thread (github.com keeps a distinct draft per
  // review-thread reply box); the root comment id is the thread's identity.
  const replyDraftKey = `pr-review-reply:${owner}/${repo}/${number}:${group.root.id}`;
  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["pr-review-comments", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["pr-review-threads", owner, repo, number] });
  };
  const reply = useMutation({
    mutationFn: () => replyToPRReviewComment(owner, repo, number, group.root.id, replyBody.trim()),
    onSuccess: () => {
      setReplyBody("");
      clearComposerDraft(replyDraftKey);
      invalidate();
    },
  });
  const resolve = useMutation({
    mutationFn: (resolved: boolean) => {
      if (!threadInfo) {
        throw new Error("thread resolution state unavailable");
      }
      return setPRReviewThreadResolved(owner, repo, number, threadInfo.id, resolved);
    },
    onSuccess: invalidate,
  });

  const resolved = threadInfo?.isResolved ?? false;

  return (
    <div className="mb-3">
      <Box
        header={
          <span className="flex items-center gap-2" style={{ fontSize: "0.82rem" }}>
            <span className="font-mono min-w-0 flex-1 truncate" style={{ color: "var(--color-fg)" }}>
              {group.root.path}
              {group.root.line != null && `:${group.root.line}`}
            </span>
            {resolved && (
              <span
                style={{
                  border: "1px solid var(--color-border)",
                  borderRadius: "2rem",
                  padding: "0.05rem 0.5rem",
                  fontSize: "0.72rem",
                  color: "var(--color-fg-muted)",
                }}
              >
                Resolved
              </span>
            )}
            {threadInfo && canResolve && (
              <Button
                size="sm"
                variant="ghost"
                disabled={resolve.isPending}
                onClick={() => resolve.mutate(!resolved)}
              >
                {resolve.isPending ? "…" : resolved ? "Unresolve" : "Resolve"}
              </Button>
            )}
          </span>
        }
      >
        {!hideDiffHunk && group.root.diff_hunk && (
          <pre
            className="font-mono"
            style={{
              margin: 0,
              padding: "0.6rem 1rem",
              fontSize: "0.76rem",
              lineHeight: 1.5,
              overflowX: "auto",
              background: "var(--color-bg-subtle)",
              borderBottom: "1px solid var(--color-border)",
              color: "var(--color-fg)",
            }}
          >
            {group.root.diff_hunk}
          </pre>
        )}
        {group.comments.map((c) => (
          <div
            key={c.id}
            style={{
              padding: "0.6rem 1rem",
              borderBottom: "1px solid var(--color-border)",
              fontSize: "0.86rem",
            }}
          >
            <div
              className="flex items-center gap-2"
              style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)", marginBottom: "0.25rem" }}
            >
              <Avatar login={c.user?.login ?? "?"} src={c.user?.avatar_url} size={18} />
              <span style={{ color: "var(--color-fg)", fontWeight: 600 }}>{c.user?.login}</span>{" "}
              <span>
                commented <RelativeTime iso={c.created_at} />
              </span>
            </div>
            <ReviewCommentBody comment={c} owner={owner} repo={repo} number={number} />
            <div className="mt-1">
              <ReactionBar
                queryKey={["pr-review-comment-reactions", owner, repo, c.id]}
                fetchList={() => fetchPullReviewCommentReactions(owner, repo, c.id)}
                add={(content) => addPullReviewCommentReaction(owner, repo, c.id, content)}
                remove={(reactionId) => removePullReviewCommentReaction(owner, repo, c.id, reactionId)}
                viewerLogin={viewerLogin}
              />
            </div>
          </div>
        ))}
        <div style={{ padding: "0.55rem 1rem" }}>
          <MarkdownComposer
            draftKey={replyDraftKey}
            value={replyBody}
            onChange={setReplyBody}
            rows={2}
            placeholder="Reply…"
            label={`reply to thread on ${group.root.path}`}
          />
          <div className="mt-2 flex justify-end">
            <Button
              size="sm"
              disabled={!replyBody.trim() || reply.isPending}
              onClick={() => reply.mutate()}
            >
              {reply.isPending ? "Replying…" : "Reply"}
            </Button>
          </div>
        </div>
      </Box>
      {reply.isError && (
        <InlineError inline title="Failed to reply" detail={String(reply.error)} />
      )}
      {resolve.isError && (
        <InlineError inline title="Failed to update thread resolution" detail={String(resolve.error)} />
      )}
    </div>
  );
}
