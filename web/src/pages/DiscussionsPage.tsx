import { useState, type CSSProperties, type ReactNode } from "react";
import Markdown from "../components/Markdown";
import { useParams, Link, useNavigate } from "react-router";
import { useMutation, useQuery, useQueryClient, useInfiniteQuery } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import {
  fetchDiscussionCategories,
  fetchDiscussionsPage,
  fetchDiscussionDetail,
  fetchRepoDetail,
  createDiscussion,
  addDiscussionComment,
  markDiscussionCommentAsAnswer,
  unmarkDiscussionCommentAsAnswer,
  deleteDiscussion,
  updateDiscussion,
  lockDiscussion,
  unlockDiscussion,
  deleteDiscussionComment,
  updateDiscussionComment,
  addReaction,
  removeReaction,
} from "../api.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
import type { GithubDiscussion, GithubDiscussionComment, GithubReactionGroup } from "../types.js";
import { RepoHeader } from "../components/PageHeader.js";
import {
  Button,
  Box,
  Blankslate,
  Modal,
  FormLabel,
  ErrorBanner,
  DialogActions,
} from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import { DiscussionIcon } from "../components/octicons.js";

export function DiscussionsPage() {
  const { owner = "", repo = "", number } = useParams<{
    owner: string;
    repo: string;
    number?: string;
  }>();

  if (number) {
    return (
      <DiscussionDetail owner={owner} repo={repo} number={parseInt(number, 10)} />
    );
  }
  return <DiscussionList owner={owner} repo={repo} />;
}

function DiscussionList({ owner, repo }: { owner: string; repo: string }) {
  const counts = useOpenCounts(owner, repo);
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [selectedCategory, setSelectedCategory] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [newTitle, setNewTitle] = useState("");
  const [newBody, setNewBody] = useState("");
  const [newCategory, setNewCategory] = useState<string>("");
  const [createError, setCreateError] = useState<string | null>(null);

  const { data: repoDetail } = useQuery({
    queryKey: ["repo-detail", owner, repo],
    queryFn: () => fetchRepoDetail(owner, repo),
  });

  const { data: categories = [] } = useQuery({
    queryKey: ["discussion-categories", owner, repo],
    queryFn: ({ signal }) => fetchDiscussionCategories(owner, repo, signal),
    enabled: !!owner && !!repo,
  });

  const discussions = useInfiniteQuery({
    queryKey: ["discussions", owner, repo, selectedCategory],
    queryFn: ({ pageParam, signal }) => fetchDiscussionsPage(owner, repo, selectedCategory, pageParam as string | null, signal),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => (last.pageInfo.hasNextPage ? last.pageInfo.endCursor : undefined),
    enabled: !!owner && !!repo,
  });

  const allDiscussions = discussions.data?.pages.flatMap((p) => p.nodes) ?? [];

  const createMutation = useMutation({
    mutationFn: () =>
      createDiscussion(repoDetail!.node_id, newCategory || categories[0]?.id || "", newTitle, newBody),
    onSuccess: (discussion: GithubDiscussion) => {
      qc.invalidateQueries({ queryKey: ["discussions", owner, repo] });
      setCreating(false);
      setNewTitle("");
      setNewBody("");
      setCreateError(null);
      navigate(`/ui/repos/${owner}/${repo}/discussions/${discussion.number}`);
    },
    onError: (err: Error) => setCreateError(err.message),
  });

  if (discussions.isLoading) return <Spinner label="loading discussions" />;
  if (discussions.isError) {
    return (
      <div>
        <RepoHeader owner={owner} repo={repo} active="discussions" {...counts} />
        <InlineError title="Failed to load discussions" detail={String(discussions.error)} />
      </div>
    );
  }

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="discussions" {...counts} />

      <div className="mb-4 flex items-center justify-between gap-3">
        <div className="flex flex-wrap items-center gap-1">
          <CategoryPill
            label="All"
            active={selectedCategory === null}
            onClick={() => setSelectedCategory(null)}
          />
          {categories.map((cat) => (
            <CategoryPill
              key={cat.id}
              label={`${cat.emoji} ${cat.name}`}
              active={selectedCategory === cat.id}
              onClick={() => setSelectedCategory(cat.id)}
            />
          ))}
        </div>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
          New discussion
        </Button>
      </div>

      {creating && (
        <Modal title="New discussion" onClose={() => setCreating(false)}>
          <FormLabel id="discussion-title">Title</FormLabel>
          <input
            id="discussion-title"
            autoFocus
            value={newTitle}
            onChange={(e) => setNewTitle(e.target.value)}
            placeholder="Discussion title"
            className="mb-3 w-full"
          />
          <FormLabel id="discussion-category">Category</FormLabel>
          <select
            id="discussion-category"
            value={newCategory}
            onChange={(e) => setNewCategory(e.target.value)}
            className="mb-3 w-full"
          >
            {categories.map((cat) => (
              <option key={cat.id} value={cat.id}>
                {cat.emoji} {cat.name}
              </option>
            ))}
          </select>
          <FormLabel id="discussion-body">Body (optional)</FormLabel>
          <textarea
            id="discussion-body"
            value={newBody}
            onChange={(e) => setNewBody(e.target.value)}
            rows={5}
            placeholder="Start a discussion…"
            className="mb-4 w-full"
            style={{ resize: "vertical" }}
          />
          {createError && <ErrorBanner>{createError}</ErrorBanner>}
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setCreating(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="sm"
              disabled={!newTitle.trim() || createMutation.isPending}
              onClick={() => {
                setCreateError(null);
                createMutation.mutate();
              }}
            >
              {createMutation.isPending ? "Creating…" : "Create discussion"}
            </Button>
          </DialogActions>
        </Modal>
      )}

      {allDiscussions.length === 0 ? (
        <Blankslate icon={<DiscussionIcon size={26} />} title="No discussions yet" />
      ) : (
        <>
          <Box>
            {allDiscussions.map((d, i) => (
              <Link
                key={d.id}
                to={`/ui/repos/${owner}/${repo}/discussions/${d.number}`}
                className="flex items-start gap-2.5"
                style={{
                  padding: "0.7rem 1rem",
                  borderBottom: i < allDiscussions.length - 1 ? "1px solid var(--color-border)" : "none",
                  textDecoration: "none",
                }}
              >
                <span style={{ marginTop: "0.1rem", color: "var(--color-fg-muted)" }}>
                  <DiscussionIcon />
                </span>
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span style={{ fontSize: "0.92rem", fontWeight: 600, color: "var(--color-fg)" }}>
                      {d.title}
                    </span>
                    <span
                      style={{
                        fontSize: "0.75rem",
                        color: "var(--color-fg-muted)",
                        background: "var(--color-bg-subtle)",
                        padding: "0.1rem 0.4rem",
                        borderRadius: "var(--radius-md)",
                      }}
                    >
                      {d.category.emoji} {d.category.name}
                    </span>
                  </div>
                  <div className="mt-1" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
                    #{d.number} opened by {d.author?.login ?? "unknown"} ·{" "}
                    {new Date(d.createdAt).toLocaleDateString()}
                    {d.comments.totalCount > 0 && ` · ${d.comments.totalCount} comments`}
                  </div>
                </div>
              </Link>
            ))}
          </Box>
          {discussions.hasNextPage && (
            <div className="mt-3 flex justify-center">
              <Button
                variant="ghost"
                size="sm"
                disabled={discussions.isFetchingNextPage}
                onClick={() => discussions.fetchNextPage()}
              >
                {discussions.isFetchingNextPage ? "Loading…" : "Load more"}
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function CategoryPill({
  label,
  active,
  onClick,
}: {
  label: string;
  active: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      style={{
        padding: "0.25rem 0.6rem",
        borderRadius: "var(--radius-md)",
        fontSize: "0.82rem",
        fontWeight: active ? 600 : 500,
        color: active ? "var(--color-fg)" : "var(--color-fg-muted)",
        background: active ? "color-mix(in srgb, var(--color-fg-muted) 12%, transparent)" : "transparent",
        border: "1px solid var(--color-border)",
      }}
    >
      {label}
    </button>
  );
}

function DiscussionDetail({
  owner,
  repo,
  number,
}: {
  owner: string;
  repo: string;
  number: number;
}) {
  const counts = useOpenCounts(owner, repo);
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [replyTo, setReplyTo] = useState<{ id: string; login?: string | undefined } | null>(null);
  const [commentBody, setCommentBody] = useState("");
  const [commentError, setCommentError] = useState<string | null>(null);
  const [editingComment, setEditingComment] = useState<string | null>(null);
  const [editBody, setEditBody] = useState("");
  const [editingDiscussion, setEditingDiscussion] = useState(false);
  const [editTitle, setEditTitle] = useState("");
  const [editDiscBody, setEditDiscBody] = useState("");
  const [editCategory, setEditCategory] = useState("");

  const { data: discussion, isLoading, isError, error } = useQuery({
    queryKey: ["discussion", owner, repo, number],
    queryFn: ({ signal }) => fetchDiscussionDetail(owner, repo, number, signal),
  });
  const { data: categories = [] } = useQuery({
    queryKey: ["discussion-categories", owner, repo],
    queryFn: ({ signal }) => fetchDiscussionCategories(owner, repo, signal),
  });

  const addCommentMutation = useMutation({
    mutationFn: () => addDiscussionComment(discussion!.id, commentBody, replyTo?.id ?? undefined),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["discussion", owner, repo, number] });
      qc.invalidateQueries({ queryKey: ["discussions", owner, repo] });
      setCommentBody("");
      setReplyTo(null);
      setCommentError(null);
    },
    onError: (err: Error) => setCommentError(err.message),
  });

  const markAnswerMutation = useMutation({
    mutationFn: ({ commentId, mark }: { commentId: string; mark: boolean }) =>
      mark ? markDiscussionCommentAsAnswer(commentId) : unmarkDiscussionCommentAsAnswer(commentId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["discussion", owner, repo, number] }),
  });

  const deleteCommentMutation = useMutation({
    mutationFn: (commentId: string) => deleteDiscussionComment(commentId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["discussion", owner, repo, number] });
      qc.invalidateQueries({ queryKey: ["discussions", owner, repo] });
    },
  });

  const editCommentMutation = useMutation({
    mutationFn: ({ commentId, body }: { commentId: string; body: string }) =>
      updateDiscussionComment(commentId, body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["discussion", owner, repo, number] });
      setEditingComment(null);
      setEditBody("");
    },
  });

  const deleteDiscussionMutation = useMutation({
    mutationFn: () => deleteDiscussion(discussion!.id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["discussions", owner, repo] });
      navigate(`/ui/repos/${owner}/${repo}/discussions`);
    },
  });

  const lockMutation = useMutation({
    mutationFn: () =>
      discussion!.locked ? unlockDiscussion(discussion!.id) : lockDiscussion(discussion!.id, "RESOLVED"),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["discussion", owner, repo, number] }),
  });

  const editDiscussionMutation = useMutation({
    mutationFn: () =>
      updateDiscussion(discussion!.id, {
        title: editTitle.trim(),
        body: editDiscBody,
        ...(editCategory ? { categoryId: editCategory } : {}),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["discussion", owner, repo, number] });
      qc.invalidateQueries({ queryKey: ["discussions", owner, repo] });
      setEditingDiscussion(false);
    },
  });

  if (isError) {
    return (
      <div>
        <RepoHeader owner={owner} repo={repo} active="discussions" {...counts} />
        <InlineError title={`Failed to load discussion #${number}`} detail={String(error)} />
      </div>
    );
  }
  if (isLoading || !discussion) return <Spinner label={`loading discussion #${number}`} />;

  const comments = discussion.comments.nodes;
  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="discussions" {...counts} />

      <MutationError
        of={[lockMutation, markAnswerMutation, deleteCommentMutation, editCommentMutation, deleteDiscussionMutation]}
      />

      <div className="mb-1 flex flex-wrap items-baseline gap-2">
        <h1 style={{ fontSize: "1.4rem", fontWeight: 600, color: "var(--color-fg)" }}>
          {discussion.title}{" "}
          <span style={{ color: "var(--color-fg-muted)", fontWeight: 400 }}>#{discussion.number}</span>
        </h1>
      </div>
      <div className="mb-4 flex flex-wrap items-center gap-3">
        <span
          style={{
            fontSize: "0.75rem",
            color: "var(--color-fg-muted)",
            background: "var(--color-bg-subtle)",
            padding: "0.15rem 0.5rem",
            borderRadius: "var(--radius-md)",
          }}
        >
          {discussion.category.emoji} {discussion.category.name}
        </span>
        <span style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          {discussion.author?.login ?? "unknown"} started this on{" "}
          {new Date(discussion.createdAt).toLocaleDateString()} · {comments.length} comment
          {comments.length === 1 ? "" : "s"}
        </span>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => {
            setEditTitle(discussion.title);
            setEditDiscBody(discussion.bodyText ?? discussion.body ?? "");
            setEditCategory(discussion.category.id);
            setEditingDiscussion(true);
          }}
        >
          Edit
        </Button>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => lockMutation.mutate()}
          disabled={lockMutation.isPending}
        >
          {discussion.locked ? "Unlock conversation" : "Lock conversation"}
        </Button>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => deleteDiscussionMutation.mutate()}
          disabled={deleteDiscussionMutation.isPending}
        >
          Delete
        </Button>
      </div>

      {editingDiscussion ? (
        <div
          className="mb-4"
          style={{ border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)", padding: "0.85rem 1rem" }}
        >
          <FormLabel id="edit-discussion-title">Title</FormLabel>
          <input
            id="edit-discussion-title"
            value={editTitle}
            onChange={(e) => setEditTitle(e.target.value)}
            className="mb-3 w-full"
          />
          <FormLabel id="edit-discussion-category">Category</FormLabel>
          <select
            id="edit-discussion-category"
            value={editCategory}
            onChange={(e) => setEditCategory(e.target.value)}
            className="mb-3 w-full"
          >
            {categories.map((cat) => (
              <option key={cat.id} value={cat.id}>
                {cat.emoji} {cat.name}
              </option>
            ))}
          </select>
          <FormLabel id="edit-discussion-body">Body</FormLabel>
          <textarea
            id="edit-discussion-body"
            value={editDiscBody}
            onChange={(e) => setEditDiscBody(e.target.value)}
            rows={5}
            className="mb-2 w-full"
            style={{ resize: "vertical" }}
          />
          <MutationError of={editDiscussionMutation} />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setEditingDiscussion(false)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="sm"
              disabled={!editTitle.trim() || editDiscussionMutation.isPending}
              onClick={() => editDiscussionMutation.mutate()}
            >
              {editDiscussionMutation.isPending ? "Saving…" : "Save"}
            </Button>
          </DialogActions>
        </div>
      ) : (
        <DiscussionPost body={discussion.body} bodyText={discussion.bodyText} />
      )}
      <DiscussionReactionBar
        subjectId={discussion.id}
        groups={discussion.reactionGroups ?? []}
        invalidateKey={["discussion", owner, repo, number]}
      />

      <div className="mt-6">
        <h2 style={{ fontSize: "1rem", fontWeight: 600, marginBottom: "1rem" }}>Comments</h2>
        {comments.map((comment) => (
          <DiscussionCommentCard
            key={comment.id}
            comment={comment}
            isAnswer={comment.isAnswer}
            canMarkAnswer={discussion.category.isAnswerable}
            onMarkAnswer={() => markAnswerMutation.mutate({ commentId: comment.id, mark: !comment.isAnswer })}
            onReply={() => {
              setReplyTo({ id: comment.id, login: comment.author?.login });
              setCommentBody("");
            }}
            onDelete={() => deleteCommentMutation.mutate(comment.id)}
            onEdit={() => {
              setEditingComment(comment.id);
              setEditBody(comment.body);
            }}
            reactionBar={
              <DiscussionReactionBar
                subjectId={comment.id}
                groups={comment.reactionGroups ?? []}
                invalidateKey={["discussion", owner, repo, number]}
              />
            }
          />
        ))}
        {comments.length === 0 && (
          <div style={{ padding: "0.5rem 0", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
            No comments yet.
          </div>
        )}
      </div>

      {editingComment && (
        <Modal title="Edit comment" onClose={() => setEditingComment(null)}>
          <textarea
            aria-label="Comment"
            value={editBody}
            onChange={(e) => setEditBody(e.target.value)}
            rows={5}
            className="mb-3 w-full"
            style={{ resize: "vertical" }}
          />
          <DialogActions>
            <Button variant="ghost" size="sm" onClick={() => setEditingComment(null)}>
              Cancel
            </Button>
            <Button
              variant="primary"
              size="sm"
              disabled={!editBody.trim() || editCommentMutation.isPending}
              onClick={() => editCommentMutation.mutate({ commentId: editingComment, body: editBody })}
            >
              {editCommentMutation.isPending ? "Saving…" : "Save"}
            </Button>
          </DialogActions>
        </Modal>
      )}

      <div className="mt-4" style={{ border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)", padding: "1rem" }}>
        <div className="mb-2" style={{ fontSize: "0.85rem", fontWeight: 600, color: "var(--color-fg)" }}>
          {replyTo ? `Replying to ${replyTo.login ?? "comment"}` : "Add a comment"}
        </div>
        <textarea
          aria-label="Comment"
          value={commentBody}
          onChange={(e) => setCommentBody(e.target.value)}
          rows={4}
          placeholder={replyTo ? "Write a reply…" : "Join the discussion…"}
          className="mb-3 w-full"
          style={{ resize: "vertical" }}
        />
        {commentError && <ErrorBanner>{commentError}</ErrorBanner>}
        <div className="flex items-center gap-2">
          <Button
            variant="primary"
            size="sm"
            disabled={!commentBody.trim() || addCommentMutation.isPending}
            onClick={() => {
              setCommentError(null);
              addCommentMutation.mutate();
            }}
          >
            {addCommentMutation.isPending ? "Posting…" : replyTo ? "Post reply" : "Comment"}
          </Button>
          {replyTo && (
            <Button variant="ghost" size="sm" onClick={() => setReplyTo(null)}>
              Cancel reply
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}

const REACTION_EMOJI: Record<string, string> = {
  THUMBS_UP: "👍", THUMBS_DOWN: "👎", LAUGH: "😄", HOORAY: "🎉",
  CONFUSED: "😕", HEART: "❤️", ROCKET: "🚀", EYES: "👀",
};

function DiscussionReactionBar({ subjectId, groups, invalidateKey }: {
  subjectId: string;
  groups: GithubReactionGroup[];
  invalidateKey: (string | number)[];
}) {
  const qc = useQueryClient();
  const [picking, setPicking] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const toggle = useMutation({
    mutationFn: ({ content, has }: { content: string; has: boolean }) =>
      has ? removeReaction(subjectId, content) : addReaction(subjectId, content),
    onSuccess: () => { setError(null); setPicking(false); void qc.invalidateQueries({ queryKey: invalidateKey }); },
    onError: (e: Error) => setError(e.message),
  });
  const shown = groups.filter((g) => g.users.totalCount > 0);
  const pill = (active: boolean): CSSProperties => ({
    display: "inline-flex", alignItems: "center", gap: "0.25rem", minHeight: "1.625rem",
    padding: "0.1rem 0.5rem", fontSize: "0.8rem", cursor: "pointer",
    border: `1px solid ${active ? "var(--color-accent)" : "var(--color-border)"}`,
    borderRadius: "999px", color: "var(--color-fg)",
    background: active ? "var(--color-accent-soft)" : "var(--color-bg-subtle)",
  });
  return (
    <div className="flex items-center gap-1.5" style={{ marginTop: "0.5rem", flexWrap: "wrap" }}>
      {shown.map((g) => (
        <button key={g.content} type="button" disabled={toggle.isPending} style={pill(g.viewerHasReacted)}
          aria-pressed={g.viewerHasReacted} aria-label={`${g.viewerHasReacted ? "Remove" : "Add"} ${g.content} reaction`}
          onClick={() => toggle.mutate({ content: g.content, has: g.viewerHasReacted })}>
          <span aria-hidden>{REACTION_EMOJI[g.content] ?? "•"}</span>
          <span className="tabular-nums">{g.users.totalCount}</span>
        </button>
      ))}
      <div style={{ position: "relative" }}>
        <button type="button" aria-label="Add reaction" aria-expanded={picking} onClick={() => setPicking((v) => !v)}
          style={{ minHeight: "1.625rem", minWidth: "1.75rem", padding: "0.1rem 0.4rem", border: "1px solid var(--color-border)", borderRadius: "999px", background: "var(--color-bg-subtle)", color: "var(--color-fg-muted)", cursor: "pointer", fontSize: "0.9rem" }}>
          +
        </button>
        {picking && (
          <div role="menu" aria-label="Pick a reaction" style={{ position: "absolute", zIndex: 20, top: "calc(100% + 4px)", left: 0, display: "flex", gap: "0.15rem", padding: "0.3rem", background: "var(--color-surface-raised)", border: "1px solid var(--color-border)", borderRadius: "var(--radius-md)", boxShadow: "var(--shadow-floating)" }}>
            {Object.entries(REACTION_EMOJI).map(([content, emoji]) => {
              const g = groups.find((x) => x.content === content);
              return (
                <button key={content} type="button" role="menuitem" aria-label={content} disabled={toggle.isPending}
                  onClick={() => toggle.mutate({ content, has: g?.viewerHasReacted ?? false })}
                  style={{ minHeight: "1.75rem", minWidth: "1.75rem", border: "none", background: g?.viewerHasReacted ? "var(--color-accent-soft)" : "transparent", borderRadius: "var(--radius-sm)", cursor: "pointer", fontSize: "1rem" }}>
                  {emoji}
                </button>
              );
            })}
          </div>
        )}
      </div>
      {error && <span style={{ color: "var(--color-status-error)", fontSize: "0.75rem" }}>{error}</span>}
    </div>
  );
}

function DiscussionPost({ body, bodyText }: { body: string; bodyText: string }) {
  if (!bodyText.trim()) {
    return (
      <div style={{ color: "var(--color-fg-muted)", fontStyle: "italic", padding: "0.5rem 0" }}>
        No description provided.
      </div>
    );
  }
  return (
    <div className="markdown-body" style={{ fontSize: "0.92rem", lineHeight: 1.6 }}>
      <Markdown>{body}</Markdown>
    </div>
  );
}

function DiscussionCommentCard({
  comment,
  isAnswer,
  canMarkAnswer,
  onMarkAnswer,
  onReply,
  onDelete,
  onEdit,
  reactionBar,
}: {
  comment: GithubDiscussionComment;
  isAnswer: boolean;
  canMarkAnswer: boolean;
  onMarkAnswer: () => void;
  onReply: () => void;
  onDelete: () => void;
  onEdit: () => void;
  reactionBar?: ReactNode;
}) {
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
          background: isAnswer ? "var(--color-accent-subtle, var(--color-bg-subtle))" : "var(--color-bg-subtle)",
          borderBottom: "1px solid var(--color-border)",
          fontSize: "0.82rem",
          color: "var(--color-fg-muted)",
        }}
      >
        <span style={{ color: "var(--color-fg)", fontWeight: 600 }}>{comment.author?.login ?? "unknown"}</span>
        <span>commented {new Date(comment.createdAt).toLocaleString()}</span>
        {isAnswer && (
          <span
            style={{
              marginLeft: "auto",
              padding: "0.05rem 0.45rem",
              borderRadius: "2rem",
              fontSize: "0.7rem",
              background: "var(--color-accent)",
              color: "var(--color-accent-fg)",
            }}
          >
            Answer
          </span>
        )}
      </div>
      <div
        className="markdown-body"
        style={{
          padding: "0.85rem 1rem",
          fontSize: "0.9rem",
          lineHeight: 1.6,
          color: "var(--color-fg)",
        }}
      >
        <Markdown>{comment.body}</Markdown>
      </div>
      {reactionBar && <div style={{ padding: "0 1rem" }}>{reactionBar}</div>}
      <div className="flex items-center gap-2 px-3 pb-2">
        <button
          type="button"
          onClick={onReply}
          style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)", background: "transparent", border: "none", cursor: "pointer" }}
        >
          Reply
        </button>
        <button
          type="button"
          onClick={onEdit}
          style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)", background: "transparent", border: "none", cursor: "pointer" }}
        >
          Edit
        </button>
        <button
          type="button"
          onClick={onDelete}
          style={{ fontSize: "0.78rem", color: "var(--color-danger-fg)", background: "transparent", border: "none", cursor: "pointer" }}
        >
          Delete
        </button>
        {canMarkAnswer && (
          <button
            type="button"
            onClick={onMarkAnswer}
            style={{ fontSize: "0.78rem", color: "var(--color-accent)", background: "transparent", border: "none", cursor: "pointer" }}
          >
            {isAnswer ? "Unmark answer" : "Mark as answer"}
          </button>
        )}
      </div>
      {comment.replies.nodes.length > 0 && (
        <div style={{ padding: "0 1rem 1rem 2rem", borderTop: "1px solid var(--color-border)" }}>
          {comment.replies.nodes.map((reply) => (
            <div key={reply.id} style={{ padding: "0.75rem 0", borderBottom: "1px solid var(--color-border)" }}>
              <div style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)", marginBottom: "0.3rem" }}>
                <strong style={{ color: "var(--color-fg)" }}>{reply.author?.login ?? "unknown"}</strong> ·{" "}
                {new Date(reply.createdAt).toLocaleString()}
              </div>
              <div className="markdown-body" style={{ fontSize: "0.85rem", lineHeight: 1.5 }}>
                <Markdown>{reply.body}</Markdown>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
