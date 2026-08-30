import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  createPRReviewComment,
  fetchPRFiles,
  fetchPRReviews,
  fetchPRReviewComments,
  fetchPRReviewThreads,
  fetchAuthenticatedUser,
  ghFetch,
  ghPostJSON,
  ghSend,
} from "../api.js";
import type { PRReviewCommentDraft } from "../api.js";
import type { GithubPRFile, GithubPRReview, GithubPRReviewComment } from "../types.js";
import { Box, Blankslate, Button, FormLabel } from "./ui.js";
import { FileIcon, ChevronDownIcon, ChevronRightIcon } from "./octicons.js";
import { MarkdownComposer } from "./MarkdownComposer.js";
import { languageFromPath, highlightLines } from "./CodeHighlight.js";
import { useDismiss } from "../hooks/useDismiss.js";
import { useSignedIn } from "../session.js";
import {
  groupReviewThreads,
  ReviewThreadCard,
  ReviewCommentBody,
  type ReviewThreadGroup,
} from "./PRReviewThread.js";

/** Color a unified-diff line by its leading marker. */
export function diffLineStyle(line: string): { bg: string; fg: string } {
  if (line.startsWith("@@")) {
    return { bg: "color-mix(in srgb, var(--color-accent) 10%, transparent)", fg: "var(--color-accent)" };
  }
  if (line.startsWith("+")) {
    return { bg: "color-mix(in srgb, var(--gh-open) 14%, transparent)", fg: "var(--color-fg)" };
  }
  if (line.startsWith("-")) {
    return {
      bg: "color-mix(in srgb, var(--color-status-error) 14%, transparent)",
      fg: "var(--color-fg)",
    };
  }
  return { bg: "transparent", fg: "var(--color-fg)" };
}

export interface ParsedDiffLine {
  text: string;
  oldLine: number | null;
  newLine: number | null;
  commentLine: number | null;
  side: "LEFT" | "RIGHT" | null;
}

/** Convert a unified patch into the old/new line coordinates GitHub's review API expects. */
export function parseDiffLines(patch: string): ParsedDiffLine[] {
  let oldLine = 0;
  let newLine = 0;
  return patch.split("\n").map((text) => {
    const header = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(text);
    if (header) {
      oldLine = Number(header[1]);
      newLine = Number(header[2]);
      return { text, oldLine: null, newLine: null, commentLine: null, side: null };
    }
    if (text.startsWith("\\ No newline")) {
      return { text, oldLine: null, newLine: null, commentLine: null, side: null };
    }
    // A trailing-newline patch yields one empty sentinel; real empty source lines still carry the diff marker.
    if (text === "") {
      return { text, oldLine: null, newLine: null, commentLine: null, side: null };
    }
    if (text.startsWith("-") && !text.startsWith("---")) {
      const current = oldLine++;
      return { text, oldLine: current, newLine: null, commentLine: current, side: "LEFT" };
    }
    if (text.startsWith("+") && !text.startsWith("+++")) {
      const current = newLine++;
      return { text, oldLine: null, newLine: current, commentLine: current, side: "RIGHT" };
    }
    if (oldLine > 0 || newLine > 0) {
      const currentOld = oldLine++;
      const currentNew = newLine++;
      return {
        text,
        oldLine: currentOld,
        newLine: currentNew,
        commentLine: currentNew,
        side: "RIGHT",
      };
    }
    return { text, oldLine: null, newLine: null, commentLine: null, side: null };
  });
}

// ─── Pending (draft) review ──────────────────────────────────────────────
// PENDING-review lifecycle: POST /reviews with no `event` creates the pending
// review (with an optional comments batch), /reviews/{id}/comments reads
// drafts, PUT /reviews/{id} updates the summary, POST .../events submits,
// DELETE /reviews/{id} discards. No add-to-pending call exists, so growing the
// draft set recreates the pending review.

function repoPath(owner: string, repo: string): string {
  return `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`;
}

function reviewsPath(owner: string, repo: string, number: number): string {
  return `${repoPath(owner, repo)}/pulls/${number}/reviews`;
}

function asArray<T>(value: unknown): T[] {
  return Array.isArray(value) ? (value as T[]) : [];
}

/**
 * The viewer's server-side PENDING review on this PR plus its draft comments.
 * Shared with PullsPage for the Files-tab "Pending" badge.
 */
export function usePendingReview(owner: string, repo: string, number: number) {
  // The /api/v3/user read 401s anonymously.
  const signedIn = useSignedIn();
  const viewerQ = useQuery({ queryKey: ["viewer"], queryFn: fetchAuthenticatedUser, enabled: signedIn });
  const viewerLogin = typeof viewerQ.data?.login === "string" ? viewerQ.data.login : null;
  const reviewsQ = useQuery({
    queryKey: ["pr-reviews", owner, repo, number],
    queryFn: () => fetchPRReviews(owner, repo, number),
    enabled: signedIn,
  });
  const reviews = asArray<GithubPRReview>(reviewsQ.data);
  const review =
    reviews.find((r) => r.state === "PENDING" && r.user?.login != null && r.user.login === viewerLogin) ??
    null;
  const commentsQ = useQuery({
    queryKey: ["pr-pending-review-comments", owner, repo, number, review?.id ?? 0],
    queryFn: () =>
      ghFetch<GithubPRReviewComment[]>(`${reviewsPath(owner, repo, number)}/${review?.id}/comments`),
    enabled: review != null,
  });
  return {
    viewerLogin,
    review,
    comments: review ? asArray<GithubPRReviewComment>(commentsQ.data) : [],
  };
}

function toDraft(c: GithubPRReviewComment): PRReviewCommentDraft {
  return {
    path: c.path,
    body: c.body,
    line: c.line ?? 1,
    side: c.side === "LEFT" ? "LEFT" : "RIGHT",
    ...(c.start_line != null ? { start_line: c.start_line } : {}),
  };
}

// ─── Per-file diff ───────────────────────────────────────────────────────

interface CommentTarget {
  line: number;
  side: "LEFT" | "RIGHT";
  source: string;
}

/** Does a review comment anchor to this diff row? */
function anchorsToLine(
  comment: { side: string; line: number | null },
  line: ParsedDiffLine,
): boolean {
  if (comment.line == null) return false;
  return comment.side === "LEFT" ? line.oldLine === comment.line : line.newLine === comment.line;
}

/**
 * Syntax-highlight a patch's content, one safe-HTML entry per line (null for
 * header/meta lines). Renders plain until highlight.js loads; unknown
 * languages stay plain.
 */
function useHighlightedPatch(filename: string, patch: string | undefined): (string | null)[] | null {
  const [lines, setLines] = useState<(string | null)[] | null>(null);
  useEffect(() => {
    setLines(null);
    if (!patch) return undefined;
    const lang = languageFromPath(filename);
    if (!lang) return undefined;
    let cancelled = false;
    const parsed = parseDiffLines(patch);
    const isContent = (l: ParsedDiffLine) => l.oldLine !== null || l.newLine !== null;
    const code = parsed.map((l) => (isContent(l) ? l.text.slice(1) : "")).join("\n");
    highlightLines(code, lang)
      .then((html) => {
        if (cancelled) return;
        setLines(parsed.map((l, i) => (isContent(l) ? (html[i] ?? null) : null)));
      })
      .catch(() => {
        /* keep the plain-text rendering */
      });
    return () => {
      cancelled = true;
    };
  }, [filename, patch]);
  return lines;
}

function PendingDraftCard({
  draft,
  onRemove,
  removing,
}: {
  draft: GithubPRReviewComment;
  onRemove: () => void;
  removing: boolean;
}) {
  return (
    <div className="mb-3">
      <Box
        header={
          <span className="flex items-center gap-2" style={{ fontSize: "0.82rem" }}>
            <span
              style={{
                border: "1px solid var(--color-status-warn)",
                color: "var(--color-status-warn)",
                borderRadius: "2rem",
                padding: "0.05rem 0.5rem",
                fontSize: "0.72rem",
                fontWeight: 600,
              }}
            >
              Pending
            </span>
            <span className="font-mono min-w-0 flex-1 truncate" style={{ color: "var(--color-fg)" }}>
              {draft.path}
              {draft.line != null && `:${draft.line}`}
            </span>
            <Button
              size="sm"
              variant="ghost"
              disabled={removing}
              aria-label={`Remove pending comment on ${draft.path} line ${draft.line}`}
              onClick={onRemove}
            >
              {removing ? "…" : "Remove"}
            </Button>
          </span>
        }
      >
        <div style={{ padding: "0.6rem 1rem", fontSize: "0.86rem" }}>
          <ReviewCommentBody comment={draft} />
        </div>
      </Box>
    </div>
  );
}

function FileDiff({
  owner,
  repo,
  number,
  file,
  onComment,
  threads,
  drafts,
  threadInfoByCommentId,
  viewerLogin,
  viewed,
  onToggleViewed,
  collapsed,
  onToggleCollapsed,
  onRemoveDraft,
  removingDraft,
}: {
  owner: string;
  repo: string;
  number: number;
  file: GithubPRFile;
  onComment: (file: GithubPRFile, target: CommentTarget) => void;
  threads: ReviewThreadGroup[];
  drafts: GithubPRReviewComment[];
  threadInfoByCommentId: Map<number, { id: string; isResolved: boolean }>;
  viewerLogin: string | null;
  viewed: boolean;
  onToggleViewed: () => void;
  collapsed: boolean;
  onToggleCollapsed: () => void;
  onRemoveDraft: (draft: GithubPRReviewComment) => void;
  removingDraft: boolean;
}) {
  const highlighted = useHighlightedPatch(file.filename, file.patch);
  // Inline review comments need a session; anonymous diffs render read-only.
  const signedIn = useSignedIn();
  const parsed = useMemo(() => (file.patch ? parseDiffLines(file.patch) : []), [file.patch]);
  const matchedIds = new Set<number>();
  const attachmentsFor = (line: ParsedDiffLine) => {
    const rowThreads = threads.filter((g) => anchorsToLine(g.root, line));
    const rowDrafts = drafts.filter((d) => anchorsToLine(d, line));
    for (const g of rowThreads) matchedIds.add(g.root.id);
    for (const d of rowDrafts) matchedIds.add(d.id);
    return { rowThreads, rowDrafts };
  };

  const renderThread = (g: ReviewThreadGroup) => (
    <ReviewThreadCard
      key={`thread-${g.root.id}`}
      owner={owner}
      repo={repo}
      number={number}
      group={g}
      threadInfo={threadInfoByCommentId.get(g.root.id) ?? null}
      viewerLogin={viewerLogin}
      hideDiffHunk
    />
  );
  const renderDraft = (d: GithubPRReviewComment) => (
    <PendingDraftCard
      key={`draft-${d.id}`}
      draft={d}
      onRemove={() => onRemoveDraft(d)}
      removing={removingDraft}
    />
  );

  const body = file.patch ? (
    <div style={{ overflowX: "auto" }}>
      {parsed.map((line, i) => {
        const s = diffLineStyle(line.text);
        const { rowThreads, rowDrafts } = attachmentsFor(line);
        const marker = line.oldLine !== null || line.newLine !== null ? line.text.charAt(0) : null;
        const html = highlighted?.[i] ?? null;
        return (
          <div key={i}>
            <div
              className="group flex font-mono"
              style={{
                margin: 0,
                fontSize: "0.76rem",
                lineHeight: 1.6,
                background: s.bg,
                color: s.fg,
              }}
            >
              <span
                aria-hidden="true"
                className="select-none text-right tabular-nums"
                style={{
                  width: "3rem",
                  flexShrink: 0,
                  paddingRight: "0.55rem",
                  color: "var(--color-fg-subtle)",
                  borderRight: "1px solid var(--color-border-muted)",
                }}
              >
                {line.oldLine ?? ""}
              </span>
              <span
                aria-hidden="true"
                className="select-none text-right tabular-nums"
                style={{ width: "3rem", flexShrink: 0, paddingRight: "0.55rem", color: "var(--color-fg-subtle)" }}
              >
                {line.newLine ?? ""}
              </span>
              <span style={{ width: "2rem", flexShrink: 0, textAlign: "center" }}>
                {signedIn && line.commentLine && line.side ? (
                  <button
                    type="button"
                    aria-label={`Comment on ${file.filename} line ${line.commentLine}`}
                    title={`Comment on line ${line.commentLine}`}
                    onClick={() =>
                      onComment(file, {
                        line: line.commentLine!,
                        side: line.side!,
                        source: line.text.slice(1),
                      })
                    }
                    style={{
                      border: 0,
                      borderRadius: "var(--radius-sm)",
                      background: "var(--color-accent)",
                      color: "#fff",
                      width: "1.5rem",
                      height: "1.5rem",
                      lineHeight: 1,
                    }}
                  >
                    +
                  </button>
                ) : null}
              </span>
              {html !== null && marker !== null ? (
                <pre style={{ margin: 0, paddingRight: "1rem", whiteSpace: "pre", flex: 1 }}>
                  {marker}
                  <span className="code-highlight" dangerouslySetInnerHTML={{ __html: html }} />
                </pre>
              ) : (
                <pre style={{ margin: 0, paddingRight: "1rem", whiteSpace: "pre", flex: 1 }}>
                  {line.text || " "}
                </pre>
              )}
            </div>
            {(rowThreads.length > 0 || rowDrafts.length > 0) && (
              <div style={{ padding: "0.5rem 0.75rem", borderTop: "1px solid var(--color-border-muted)" }}>
                {rowThreads.map(renderThread)}
                {rowDrafts.map(renderDraft)}
              </div>
            )}
          </div>
        );
      })}
    </div>
  ) : (
    <div style={{ padding: "0.6rem 1rem", fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
      {file.status === "removed"
        ? "File removed."
        : file.status === "added"
          ? "New file."
          : "Binary file or no textual diff."}
    </div>
  );

  // Threads/drafts whose anchor line is absent from the diff (outdated position, file-level, binary) still need a home.
  const unmatchedThreads = threads.filter((g) => !matchedIds.has(g.root.id));
  const unmatchedDrafts = drafts.filter((d) => !matchedIds.has(d.id));

  return (
    <div className="mb-3" style={viewed ? { opacity: 0.65 } : undefined}>
      <Box
        header={
          <span className="flex min-w-0 flex-1 items-center gap-2">
            <button
              type="button"
              aria-label={`Toggle diff for ${file.filename}`}
              aria-expanded={!collapsed}
              onClick={onToggleCollapsed}
              className="inline-flex items-center justify-center"
              style={{
                border: "none",
                background: "transparent",
                color: "var(--color-fg-muted)",
                cursor: "pointer",
                width: "1.625rem",
                height: "1.625rem",
                padding: 0,
              }}
            >
              {collapsed ? <ChevronRightIcon size={14} /> : <ChevronDownIcon size={14} />}
            </button>
            <FileIcon size={14} style={{ color: "var(--color-fg-muted)", flexShrink: 0 }} />
            <span className="font-mono min-w-0 flex-1 truncate" style={{ color: "var(--color-fg)" }}>
              {file.previous_filename && file.previous_filename !== file.filename
                ? `${file.previous_filename} → ${file.filename}`
                : file.filename}
            </span>
            <span className="tabular-nums" style={{ color: "var(--gh-open)", fontSize: "0.76rem" }}>
              +{file.additions}
            </span>
            <span
              className="tabular-nums"
              style={{ color: "var(--color-status-error)", fontSize: "0.76rem" }}
            >
              −{file.deletions}
            </span>
            <label
              className="inline-flex items-center gap-1.5"
              style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)", whiteSpace: "nowrap" }}
            >
              <input
                type="checkbox"
                checked={viewed}
                onChange={onToggleViewed}
                aria-label={`Viewed ${file.filename}`}
              />
              Viewed
            </label>
          </span>
        }
      >
        {!collapsed && (
          <>
            {body}
            {(unmatchedThreads.length > 0 || unmatchedDrafts.length > 0) && (
              <div style={{ padding: "0.5rem 0.75rem", borderTop: "1px solid var(--color-border-muted)" }}>
                {unmatchedThreads.map(renderThread)}
                {unmatchedDrafts.map(renderDraft)}
              </div>
            )}
          </>
        )}
      </Box>
    </div>
  );
}

// ─── Files-changed view ──────────────────────────────────────────────────

/** GitHub's "Files changed" tab — the PR's changed files rendered as diffs. */
export function PRFilesView({
  owner,
  repo,
  number,
  headSha,
}: {
  owner: string;
  repo: string;
  number: number;
  headSha: string;
}) {
  const qc = useQueryClient();
  const [target, setTarget] = useState<{ file: GithubPRFile; line: number; side: "LEFT" | "RIGHT"; source: string } | null>(null);
  const [body, setBody] = useState("");
  // A start line strictly above the selected line spans start_line..line.
  const [startLine, setStartLine] = useState("");
  const rangeStart =
    target && /^\d+$/.test(startLine.trim()) && Number(startLine) >= 1 && Number(startLine) < target.line
      ? Number(startLine)
      : undefined;

  // "Hide whitespace changes": the /ui-data variant recomputes patches
  // ignoring whitespace-only changes. Keep the flag INSIDE the ["pr-files",
  // owner, repo, number] prefix so invalidations reach both variants.
  const [hideWhitespace, setHideWhitespace] = useState(false);
  const q = useQuery({
    queryKey: ["pr-files", owner, repo, number, hideWhitespace],
    queryFn: () =>
      hideWhitespace
        ? ghFetch<GithubPRFile[]>(
            `/ui-data/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/pulls/${number}/files?ignore_whitespace=1`,
          )
        : fetchPRFiles(owner, repo, number),
    // Keep the prior list rendered while the other variant loads so the
    // toolbar doesn't blink out mid-toggle.
    placeholderData: (prev: GithubPRFile[] | undefined) => prev,
  });
  // Reviews and thread overlays 401 anonymously; review comments are public.
  // Signed out, the diff renders comments without reviews or overlays.
  const signedIn = useSignedIn();
  const reviewsQ = useQuery({
    queryKey: ["pr-reviews", owner, repo, number],
    queryFn: () => fetchPRReviews(owner, repo, number),
    enabled: signedIn,
  });
  const commentsQ = useQuery({
    queryKey: ["pr-review-comments", owner, repo, number],
    queryFn: () => fetchPRReviewComments(owner, repo, number),
  });
  const threadsQ = useQuery({
    queryKey: ["pr-review-threads", owner, repo, number],
    queryFn: () => fetchPRReviewThreads(owner, repo, number),
    retry: false,
    enabled: signedIn,
  });
  const pending = usePendingReview(owner, repo, number);

  // Per-file "Viewed" state; sessionStorage stands in for GitHub's per-viewer
  // server-side state.
  const viewedKey = `bleephub:pr-viewed:${owner}/${repo}#${number}`;
  const [viewedFiles, setViewedFiles] = useState<Set<string>>(() => {
    try {
      return new Set(JSON.parse(sessionStorage.getItem(viewedKey) ?? "[]") as string[]);
    } catch {
      return new Set();
    }
  });
  const [collapsedFiles, setCollapsedFiles] = useState<Set<string>>(new Set());
  const toggleViewed = (filename: string) => {
    setViewedFiles((prev) => {
      const next = new Set(prev);
      const nowViewed = !next.has(filename);
      if (nowViewed) next.add(filename);
      else next.delete(filename);
      try {
        sessionStorage.setItem(viewedKey, JSON.stringify([...next]));
      } catch {
        /* storage unavailable */
      }
      // Marking viewed collapses the file; unviewing re-expands it.
      setCollapsedFiles((prevCollapsed) => {
        const nextCollapsed = new Set(prevCollapsed);
        if (nowViewed) nextCollapsed.add(filename);
        else nextCollapsed.delete(filename);
        return nextCollapsed;
      });
      return next;
    });
  };
  const toggleCollapsed = (filename: string) =>
    setCollapsedFiles((prev) => {
      const next = new Set(prev);
      if (next.has(filename)) next.delete(filename);
      else next.add(filename);
      return next;
    });

  // The summary body mirrors to sessionStorage so half-typed text survives
  // reloads; the draft comments themselves live in the pending review.
  const bodyKey = `bleephub:pr-review-body:${owner}/${repo}#${number}`;
  const [finishOpen, setFinishOpen] = useState(false);
  const finishRef = useDismiss<HTMLDivElement>(finishOpen, () => setFinishOpen(false));
  const [reviewBody, setReviewBodyState] = useState(() => {
    try {
      return sessionStorage.getItem(bodyKey) ?? "";
    } catch {
      return "";
    }
  });
  const setReviewBody = (next: string) => {
    setReviewBodyState(next);
    try {
      if (next) sessionStorage.setItem(bodyKey, next);
      else sessionStorage.removeItem(bodyKey);
    } catch {
      /* storage unavailable */
    }
  };
  // Adopt the pending review's summary once, when nothing was typed locally.
  useEffect(() => {
    if (pending.review?.body && reviewBody === "") {
      setReviewBodyState(pending.review.body);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pending.review?.id]);
  const [reviewEvent, setReviewEvent] = useState<"COMMENT" | "APPROVE" | "REQUEST_CHANGES">("COMMENT");

  const invalidateReview = () => {
    qc.invalidateQueries({ queryKey: ["pr-review-comments", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["pr-review-threads", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["pr-timeline", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["pr-reviews", owner, repo, number] });
  };
  const invalidatePending = () => {
    qc.invalidateQueries({ queryKey: ["pr-reviews", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["pr-pending-review-comments", owner, repo, number] });
    qc.invalidateQueries({ queryKey: ["pr-review-comments", owner, repo, number] });
  };

  const closeComposer = () => {
    setTarget(null);
    setBody("");
    setStartLine("");
  };

  const commentMutation = useMutation({
    mutationFn: () => {
      if (!target) throw new Error("Select a line first");
      return createPRReviewComment(owner, repo, number, {
        body: body.trim(),
        commit_id: headSha,
        path: target.file.filename,
        line: target.line,
        side: target.side,
        ...(rangeStart !== undefined ? { start_line: rangeStart } : {}),
      });
    },
    onSuccess: () => {
      closeComposer();
      invalidateReview();
    },
  });

  // Add the composed comment to the pending review. With no add-to-pending
  // call, recreate the review with the grown batch, then delete the superseded
  // copy (old draft comments first, then the old review shell).
  const addDraftMutation = useMutation({
    mutationFn: async (draft: PRReviewCommentDraft) => {
      const prior = pending.comments.map(toDraft);
      const oldReview = pending.review;
      const oldComments = pending.comments;
      await ghPostJSON<GithubPRReview>(reviewsPath(owner, repo, number), {
        body: oldReview?.body ?? "",
        comments: [...prior, draft],
      });
      for (const c of oldComments) {
        await ghSend("DELETE", `${repoPath(owner, repo)}/pulls/comments/${c.id}`);
      }
      if (oldReview) {
        await ghSend("DELETE", `${reviewsPath(owner, repo, number)}/${oldReview.id}`);
      }
    },
    onSuccess: () => {
      closeComposer();
      invalidatePending();
    },
  });

  const removeDraftMutation = useMutation({
    mutationFn: async (comment: GithubPRReviewComment) => {
      await ghSend("DELETE", `${repoPath(owner, repo)}/pulls/comments/${comment.id}`);
      if (pending.review && pending.comments.length <= 1) {
        await ghSend("DELETE", `${reviewsPath(owner, repo, number)}/${pending.review.id}`);
      }
    },
    onSuccess: invalidatePending,
  });

  const finishMutation = useMutation({
    mutationFn: async (event: "COMMENT" | "APPROVE" | "REQUEST_CHANGES") => {
      const summary = reviewBody.trim();
      if (!pending.review) {
        // No drafts: create + submit in one call.
        await ghPostJSON<GithubPRReview>(reviewsPath(owner, repo, number), { body: summary, event });
        return;
      }
      if (summary !== (pending.review.body ?? "")) {
        await ghSend("PUT", `${reviewsPath(owner, repo, number)}/${pending.review.id}`, { body: summary });
      }
      await ghPostJSON<GithubPRReview>(
        `${reviewsPath(owner, repo, number)}/${pending.review.id}/events`,
        { event },
      );
    },
    onSuccess: () => {
      setReviewBody("");
      setFinishOpen(false);
      invalidateReview();
      invalidatePending();
    },
  });

  const discardMutation = useMutation({
    mutationFn: async () => {
      if (!pending.review) return;
      for (const c of pending.comments) {
        await ghSend("DELETE", `${repoPath(owner, repo)}/pulls/comments/${c.id}`);
      }
      await ghSend("DELETE", `${reviewsPath(owner, repo, number)}/${pending.review.id}`);
    },
    onSuccess: () => {
      setReviewBody("");
      setFinishOpen(false);
      invalidatePending();
    },
  });

  if (q.isLoading) return <Spinner label="loading changed files" />;
  if (q.isError) return <InlineError title="Failed to load changed files" detail={String(q.error)} />;
  const files = q.data ?? [];
  // While hiding whitespace, an empty list means all changes were
  // whitespace-only — keep the toolbar so the checkbox can be untoggled.
  if (files.length === 0 && !hideWhitespace) {
    return <Blankslate icon={<FileIcon size={26} />} title="No file changes" />;
  }

  // Comments attached to a PENDING review are private drafts, not public
  // conversation — exclude them here; the viewer's drafts render through the pending-review UI instead.
  const pendingReviewIds = new Set(
    asArray<GithubPRReview>(reviewsQ.data)
      .filter((r) => r.state === "PENDING")
      .map((r) => r.id),
  );
  const publicComments = asArray<GithubPRReviewComment>(commentsQ.data).filter(
    (c) => !pendingReviewIds.has(c.pull_request_review_id),
  );
  const threadGroups = groupReviewThreads(publicComments);
  const groupsByFile = new Map<string, ReviewThreadGroup[]>();
  for (const g of threadGroups) {
    const list = groupsByFile.get(g.root.path) ?? [];
    list.push(g);
    groupsByFile.set(g.root.path, list);
  }
  const draftsByFile = new Map<string, GithubPRReviewComment[]>();
  for (const d of pending.comments) {
    const list = draftsByFile.get(d.path) ?? [];
    list.push(d);
    draftsByFile.set(d.path, list);
  }
  const threadInfoByCommentId = new Map<number, { id: string; isResolved: boolean }>();
  for (const t of threadsQ.data ?? []) {
    for (const c of t.comments) {
      threadInfoByCommentId.set(c.databaseId, { id: t.id, isResolved: t.isResolved });
    }
  }

  const pendingCount = pending.comments.length;
  const totalAdd = files.reduce((n, f) => n + f.additions, 0);
  const totalDel = files.reduce((n, f) => n + f.deletions, 0);
  const submitDisabled =
    finishMutation.isPending ||
    (!pending.review && reviewEvent !== "APPROVE" && !reviewBody.trim());

  return (
    <div>
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <div className="min-w-0 flex-1" style={{ fontSize: "0.83rem", color: "var(--color-fg-muted)" }}>
          Showing {files.length} changed file{files.length === 1 ? "" : "s"} with{" "}
          <span style={{ color: "var(--gh-open)" }}>{totalAdd} additions</span> and{" "}
          <span style={{ color: "var(--color-status-error)" }}>{totalDel} deletions</span>.
        </div>
        <label
          className="inline-flex items-center gap-1.5"
          style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)", whiteSpace: "nowrap" }}
        >
          <input
            type="checkbox"
            checked={hideWhitespace}
            onChange={(event) => setHideWhitespace(event.target.checked)}
          />
          Hide whitespace changes
        </label>
        <div ref={finishRef} style={{ position: "relative" }}>
          {/* Reviews need a session; hide the control signed out. */}
          {signedIn && (
            <Button
              variant="primary"
              size="sm"
              aria-expanded={finishOpen}
              aria-haspopup="dialog"
              onClick={() => setFinishOpen((open) => !open)}
            >
              {pendingCount > 0 ? `Finish your review (${pendingCount})` : "Review changes"}
            </Button>
          )}
          {finishOpen && (
            <div
              role="dialog"
              aria-label="Finish your review"
              style={{
                position: "absolute",
                right: 0,
                top: "calc(100% + 0.35rem)",
                zIndex: 30,
                width: "min(24rem, 88vw)",
                background: "var(--color-surface)",
                border: "1px solid var(--color-border)",
                borderRadius: "var(--radius-md)",
                boxShadow: "var(--shadow-md, 0 8px 24px rgba(0,0,0,0.2))",
                padding: "0.85rem",
              }}
            >
              <div className="mb-2" style={{ fontWeight: 600, fontSize: "0.9rem", color: "var(--color-fg)" }}>
                Finish your review
                {pendingCount > 0 && (
                  <span style={{ marginLeft: "0.4rem", fontWeight: 400, fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                    {pendingCount} pending comment{pendingCount === 1 ? "" : "s"}
                  </span>
                )}
              </div>
              <MarkdownComposer
                value={reviewBody}
                onChange={setReviewBody}
                rows={3}
                label="Review summary"
                placeholder="Leave a summary comment (optional)…"
              />
              <fieldset className="mt-2" style={{ border: "none", margin: 0, padding: 0 }}>
                <legend
                  style={{
                    position: "absolute",
                    width: 1,
                    height: 1,
                    margin: -1,
                    padding: 0,
                    overflow: "hidden",
                    clip: "rect(0 0 0 0)",
                    whiteSpace: "nowrap",
                    border: 0,
                  }}
                >
                  Review action
                </legend>
                {(
                  [
                    { value: "COMMENT", label: "Comment", hint: "Submit general feedback without explicit approval." },
                    { value: "APPROVE", label: "Approve", hint: "Submit feedback and approve merging these changes." },
                    { value: "REQUEST_CHANGES", label: "Request changes", hint: "Submit feedback that must be addressed before merging." },
                  ] as const
                ).map((opt) => (
                  <label
                    key={opt.value}
                    className="flex items-start gap-2"
                    style={{ padding: "0.25rem 0", fontSize: "0.84rem", color: "var(--color-fg)" }}
                  >
                    <input
                      type="radio"
                      name="review-event"
                      value={opt.value}
                      checked={reviewEvent === opt.value}
                      onChange={() => setReviewEvent(opt.value)}
                      style={{ marginTop: "0.2rem" }}
                    />
                    <span>
                      {opt.label}
                      <span className="block" style={{ fontSize: "0.75rem", color: "var(--color-fg-muted)" }}>
                        {opt.hint}
                      </span>
                    </span>
                  </label>
                ))}
              </fieldset>
              {finishMutation.isError && (
                <InlineError inline title="Could not submit review" detail={String(finishMutation.error)} />
              )}
              {discardMutation.isError && (
                <InlineError inline title="Could not discard review" detail={String(discardMutation.error)} />
              )}
              <div className="mt-2 flex flex-wrap justify-end gap-2">
                {pending.review && (
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={discardMutation.isPending}
                    onClick={() => discardMutation.mutate()}
                  >
                    {discardMutation.isPending ? "Discarding…" : "Discard review"}
                  </Button>
                )}
                <Button
                  size="sm"
                  variant="primary"
                  disabled={submitDisabled}
                  onClick={() => finishMutation.mutate(reviewEvent)}
                >
                  {finishMutation.isPending ? "Submitting…" : "Submit review"}
                </Button>
              </div>
            </div>
          )}
        </div>
      </div>
      {commentsQ.isError && (
        <InlineError inline title="Failed to load review comments" detail={String(commentsQ.error)} />
      )}
      {target && (
        <Box
          header={
            <span>
              Comment on <span className="font-mono">{target.file.filename}</span>, line {target.line}
            </span>
          }
          className="mb-3"
        >
          <div className="space-y-2" style={{ padding: "0.75rem" }}>
            <div className="flex items-center gap-2">
              <FormLabel id="inline-review-start-line">Start line (optional)</FormLabel>
              <input
                id="inline-review-start-line"
                type="number"
                min={1}
                max={target.line - 1}
                value={startLine}
                onChange={(event) => setStartLine(event.target.value)}
                style={{ width: "6rem" }}
                placeholder={`< ${target.line}`}
              />
              <span style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                {rangeStart !== undefined
                  ? `Comment spans lines ${rangeStart}–${target.line}.`
                  : "Leave blank for a single-line comment."}
              </span>
            </div>
            <MarkdownComposer
              value={body}
              onChange={setBody}
              rows={5}
              label="Review comment"
              placeholder="Leave a comment on this line…"
            />
            {commentMutation.isError && (
              <InlineError inline title="Could not add review comment" detail={String(commentMutation.error)} />
            )}
            {addDraftMutation.isError && (
              <InlineError inline title="Could not add to review" detail={String(addDraftMutation.error)} />
            )}
            <div className="flex flex-wrap justify-end gap-2">
              {target.side === "RIGHT" && (
                <Button
                  type="button"
                  size="sm"
                  onClick={() => setBody(`\`\`\`suggestion\n${target.source}\n\`\`\``)}
                >
                  Add suggestion
                </Button>
              )}
              <Button
                type="button"
                size="sm"
                onClick={() => {
                  closeComposer();
                  commentMutation.reset();
                  addDraftMutation.reset();
                }}
              >
                Cancel
              </Button>
              <Button
                type="button"
                size="sm"
                disabled={!body.trim() || addDraftMutation.isPending}
                onClick={() =>
                  addDraftMutation.mutate({
                    path: target.file.filename,
                    body: body.trim(),
                    line: target.line,
                    side: target.side,
                    ...(rangeStart !== undefined ? { start_line: rangeStart } : {}),
                  })
                }
              >
                {addDraftMutation.isPending
                  ? "Adding…"
                  : pending.review
                    ? "Add review comment"
                    : "Start a review"}
              </Button>
              <Button
                type="button"
                size="sm"
                variant="primary"
                disabled={!body.trim() || commentMutation.isPending}
                onClick={() => commentMutation.mutate()}
              >
                {commentMutation.isPending ? "Adding…" : "Add single comment"}
              </Button>
            </div>
          </div>
        </Box>
      )}
      {removeDraftMutation.isError && (
        <InlineError inline title="Could not remove pending comment" detail={String(removeDraftMutation.error)} />
      )}
      {files.length === 0 && (
        <Blankslate icon={<FileIcon size={26} />} title="No changes once whitespace is ignored" />
      )}
      {files.map((f) => (
        <FileDiff
          key={f.sha + f.filename}
          owner={owner}
          repo={repo}
          number={number}
          file={f}
          threads={groupsByFile.get(f.filename) ?? []}
          drafts={draftsByFile.get(f.filename) ?? []}
          threadInfoByCommentId={threadInfoByCommentId}
          viewerLogin={pending.viewerLogin}
          viewed={viewedFiles.has(f.filename)}
          onToggleViewed={() => toggleViewed(f.filename)}
          collapsed={collapsedFiles.has(f.filename)}
          onToggleCollapsed={() => toggleCollapsed(f.filename)}
          onRemoveDraft={(d) => removeDraftMutation.mutate(d)}
          removingDraft={removeDraftMutation.isPending}
          onComment={(file, next) => {
            setTarget({ file, ...next });
            setBody("");
            commentMutation.reset();
            addDraftMutation.reset();
          }}
        />
      ))}
    </div>
  );
}
