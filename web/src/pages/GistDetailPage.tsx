import { useState } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { confirmAction } from "../components/confirmAction.js";
import {
  createGistComment,
  updateGistComment,
  deleteGist,
  deleteGistComment,
  fetchCurrentUser,
  fetchGist,
  fetchGistCommits,
  fetchGistComments,
  fetchGistForks,
  forkGist,
  isGistStarred,
  starGist,
  unstarGist,
  updateGist,
} from "../api.js";
import type { BleephubGist, BleephubGistFile, GithubGistCommit } from "../types.js";
import {
  Box,
  Button,
  ButtonLink,
  DialogActions,
  ErrorBanner,
  FormLabel,
  Modal,
  PageTitle,
  StateLabel,
  Tabs,
} from "../components/ui.js";
import { GistIcon, StarIcon, BranchIcon } from "../components/octicons.js";
import { RepoNotFound } from "../components/RepoNotFound.js";
import { isNotFoundError } from "../components/notFound.js";
import { Avatar } from "../components/Avatar.js";
import { RelativeTime } from "../components/RelativeTime.js";
import { CodeHighlight } from "../components/CodeHighlight.js";
import { SignInPrompt } from "../components/SignInPrompt.js";
import { loginPath, useSignedIn } from "../session.js";
import { useComposerDraft, clearComposerDraft } from "../hooks/useComposerDraft.js";
import Markdown from "../components/Markdown.js";

/**
 * The gist permalink page (/ui/gists/{id}) — GitHub's gist.github.com/{id}
 * equivalent: every file rendered (Markdown files as markdown, everything
 * else syntax-highlighted) with per-file raw links, plus star/fork actions
 * and History/Forks/Comments tabs.
 */
export function GistDetailPage() {
  const { id = "" } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<BleephubGist | null>(null);
  const [tab, setTab] = useState<"files" | "commits" | "forks" | "comments">("files");
  const [actionError, setActionError] = useState<string | null>(null);

  // The viewer and star-state reads 401 for an anonymous visitor; the gist
  // itself is a public read.
  const signedIn = useSignedIn();
  const location = useLocation();
  const viewer = useQuery({
    queryKey: ["current-user"],
    queryFn: ({ signal }) => fetchCurrentUser(signal),
    enabled: signedIn,
  });
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["gists", id],
    queryFn: () => fetchGist(id),
  });

  const { data: starred, isLoading: starLoading } = useQuery({
    queryKey: ["gists", id, "starred"],
    queryFn: () => isGistStarred(id),
    enabled: signedIn,
  });

  const starMut = useMutation({
    mutationFn: () => (starred ? unstarGist(id) : starGist(id)),
    onSuccess: () => {
      setActionError(null);
      queryClient.invalidateQueries({ queryKey: ["gists", id, "starred"] });
      queryClient.invalidateQueries({ queryKey: ["gists", "starred"] });
    },
    onError: (err: Error) => setActionError(err.message),
  });

  const forkMut = useMutation({
    mutationFn: () => forkGist(id),
    onSuccess: () => {
      setActionError(null);
      queryClient.invalidateQueries({ queryKey: ["gists"] });
    },
    onError: (err: Error) => setActionError(err.message),
  });

  const deleteMut = useMutation({
    mutationFn: () => deleteGist(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["gists"] });
      navigate("/ui/gists");
    },
    onError: (err: Error) => setActionError(err.message),
  });

  // Unknown gist id → github.com's full-page 404 (also cloaks secret gists);
  // other failures keep the banner.
  if (isError && isNotFoundError(error)) return <RepoNotFound />;
  if (isError) return <InlineError title="Failed to load gist" />;
  if (isLoading || !data) return <Spinner label="loading gist" />;

  const files = Object.entries(data.files);
  const isOwn = !!viewer.data?.login && viewer.data.login === data.owner?.login;

  return (
    <div>
      <PageTitle
        icon={<GistIcon size={20} />}
        title={data.description || `Gist ${data.id}`}
        meta={
          <span className="inline-flex flex-wrap items-center gap-2">
            {data.owner?.login && (
              <Link
                to={`/ui/${data.owner.login}`}
                className="inline-flex items-center gap-1.5"
                style={{ color: "var(--color-fg-muted)", textDecoration: "none" }}
              >
                <Avatar login={data.owner.login} src={data.owner.avatar_url} size={20} />
                {data.owner.login}
              </Link>
            )}
            <span>
              Created <RelativeTime iso={data.created_at} />
            </span>
            <span>
              · Updated <RelativeTime iso={data.updated_at} />
            </span>
          </span>
        }
        actions={
          <div className="flex flex-wrap items-center gap-2">
            {signedIn ? (
              <>
                <Button
                  size="sm"
                  variant={starred ? "primary" : "secondary"}
                  onClick={() => starMut.mutate()}
                  disabled={starLoading || starMut.isPending}
                >
                  <StarIcon size={14} /> {starred ? "Unstar" : "Star"}
                </Button>
                <Button size="sm" variant="secondary" onClick={() => forkMut.mutate()} disabled={forkMut.isPending}>
                  <BranchIcon size={14} /> Fork
                </Button>
              </>
            ) : (
              // Signed out, Star/Fork link to sign-in (github.com prompts on click).
              <>
                <ButtonLink size="sm" variant="secondary" to={loginPath(location)}>
                  <StarIcon size={14} /> Star
                </ButtonLink>
                <ButtonLink size="sm" variant="secondary" to={loginPath(location)}>
                  <BranchIcon size={14} /> Fork
                </ButtonLink>
              </>
            )}
            {isOwn && (
              <>
                <Button size="sm" variant="ghost" onClick={() => setEditing(data)}>
                  Edit
                </Button>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={deleteMut.isPending}
                  onClick={async () => {
                    if (await confirmAction(`Delete gist ${data.id}?`, { title: "Delete gist", confirmLabel: "Delete" })) {
                      deleteMut.mutate();
                    }
                  }}
                >
                  Delete
                </Button>
              </>
            )}
          </div>
        }
      />

      {actionError && <ErrorBanner>{actionError}</ErrorBanner>}

      <div className="mb-4 flex flex-wrap items-center gap-2">
        {data.public ? <StateLabel state="open">public</StateLabel> : <StateLabel state="closed">secret</StateLabel>}
        <span style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>
          {files.length} file{files.length === 1 ? "" : "s"}
        </span>
      </div>

      <Tabs<"files" | "commits" | "forks" | "comments">
        items={[
          { key: "files", label: "Files" },
          { key: "commits", label: "History" },
          { key: "forks", label: "Forks" },
          { key: "comments", label: "Comments" },
        ]}
        active={tab}
        onChange={setTab}
      />

      {tab === "files" && (
        <div>
          {files.map(([filename, file]) => (
            <GistFileBox key={filename} filename={filename} file={file} />
          ))}
        </div>
      )}

      {tab === "commits" && <GistCommits id={id} />}
      {tab === "forks" && <GistForks id={id} />}
      {tab === "comments" && <GistComments id={id} />}

      {editing && (
        <EditGistDialog
          gist={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            queryClient.invalidateQueries({ queryKey: ["gists", id] });
            queryClient.invalidateQueries({ queryKey: ["gists"] });
            setEditing(null);
          }}
        />
      )}
    </div>
  );
}

function GistFileBox({ filename, file }: { filename: string; file: BleephubGistFile }) {
  const isMarkdown = /\.(md|markdown)$/i.test(filename);
  return (
    <Box
      className="mb-4"
      header={
        <span className="flex items-center justify-between gap-2">
          <span>{filename}</span>
          {file.raw_url && (
            <a
              href={file.raw_url}
              rel="noreferrer"
              style={{
                fontSize: "0.78rem",
                fontWeight: 500,
                color: "var(--color-accent)",
                textDecoration: "none",
                display: "inline-block",
                lineHeight: "1.625rem",
              }}
            >
              Raw
            </a>
          )}
        </span>
      }
    >
      {file.content != null ? (
        isMarkdown ? (
          <div style={{ padding: "1rem 1.25rem" }} className="markdown-body">
            <Markdown>{file.content}</Markdown>
          </div>
        ) : (
          <CodeHighlight code={file.content} path={filename} />
        )
      ) : (
        <div style={{ padding: "1rem", color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>
          Content unavailable
        </div>
      )}
    </Box>
  );
}

function GistCommits({ id }: { id: string }) {
  const { data, isLoading, isError } = useQuery({
    queryKey: ["gists", id, "commits"],
    queryFn: () => fetchGistCommits(id),
  });

  if (isError) return <InlineError title="Failed to load history" />;
  if (isLoading || !data) return <Spinner label="loading history" />;

  if (data.length === 0) {
    return <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>No history available.</div>;
  }

  return (
    <div className="space-y-2">
      {data.map((commit) => (
        <CommitRow key={commit.version} commit={commit} />
      ))}
    </div>
  );
}

function CommitRow({ commit }: { commit: GithubGistCommit }) {
  const additions = commit.change_status?.additions ?? 0;
  const deletions = commit.change_status?.deletions ?? 0;
  return (
    <div className="flex flex-col gap-1 rounded border p-3" style={{ borderColor: "var(--color-border)" }}>
      <div className="flex items-center justify-between">
        <span className="font-mono text-sm" style={{ color: "var(--color-fg)" }}>
          {commit.version.slice(0, 7)}
        </span>
        <span style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
          <RelativeTime iso={commit.committed_at} />
        </span>
      </div>
      <div style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>{commit.user?.login ?? "unknown"}</div>
      {(additions > 0 || deletions > 0) && (
        <div className="flex gap-3" style={{ fontSize: "0.78rem" }}>
          <span style={{ color: "var(--gh-open-solid)" }}>+{additions}</span>
          <span style={{ color: "var(--color-status-error)" }}>-{deletions}</span>
        </div>
      )}
    </div>
  );
}

function GistForks({ id }: { id: string }) {
  const { data, isLoading, isError } = useQuery({
    queryKey: ["gists", id, "forks"],
    queryFn: () => fetchGistForks(id),
  });

  if (isError) return <InlineError title="Failed to load forks" />;
  if (isLoading || !data) return <Spinner label="loading forks" />;

  if (data.length === 0) {
    return <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>No forks yet.</div>;
  }

  return (
    <div className="space-y-2">
      {data.map((fork) => (
        <Box key={fork.id} header={fork.owner?.login ?? "unknown"} className="p-3">
          <div className="flex items-center justify-between">
            <Link
              to={`/ui/gists/${fork.id}`}
              className="font-mono text-sm"
              style={{ color: "var(--color-accent)", textDecoration: "none", display: "inline-block", lineHeight: "1.625rem" }}
            >
              {fork.id}
            </Link>
            <span style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
              <RelativeTime iso={fork.created_at} />
            </span>
          </div>
          <div style={{ fontSize: "0.82rem" }}>{fork.description || "(no description)"}</div>
        </Box>
      ))}
    </div>
  );
}

function GistComments({ id }: { id: string }) {
  const queryClient = useQueryClient();
  const [body, setBody] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [editingId, setEditingId] = useState<number | null>(null);
  const [editBody, setEditBody] = useState("");
  // Draft durability for the gist comment box (github.com restores it).
  const draftKey = `gist-comment:${id}`;
  useComposerDraft(draftKey, body, setBody);

  // Anonymous visitors read comments but cannot author them.
  const signedIn = useSignedIn();
  const viewer = useQuery({
    queryKey: ["current-user"],
    queryFn: ({ signal }) => fetchCurrentUser(signal),
    enabled: signedIn,
  });
  const { data, isLoading, isError } = useQuery({
    queryKey: ["gists", id, "comments"],
    queryFn: () => fetchGistComments(id),
  });

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["gists", id, "comments"] });

  const createMut = useMutation({
    mutationFn: () => createGistComment(id, body.trim()),
    onSuccess: () => {
      setBody("");
      clearComposerDraft(draftKey);
      setError(null);
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });

  const deleteMut = useMutation({
    mutationFn: (commentId: number) => deleteGistComment(id, commentId),
    onSuccess: () => {
      setError(null);
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });

  const editMut = useMutation({
    mutationFn: (commentId: number) => updateGistComment(id, commentId, editBody.trim()),
    onSuccess: () => {
      setError(null);
      setEditingId(null);
      setEditBody("");
      invalidate();
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <div className="flex flex-col gap-3">
      {error && <ErrorBanner>{error}</ErrorBanner>}
      {isError && <InlineError title="Failed to load comments" />}
      {isLoading && <Spinner label="loading comments" />}
      {data && data.length === 0 && (
        <div style={{ color: "var(--color-fg-muted)", fontSize: "0.85rem" }}>No comments yet.</div>
      )}
      {data?.map((comment) => (
        <Box key={comment.id} className="p-3">
          <div className="mb-1 flex items-center justify-between gap-2">
            <span className="flex items-center gap-2" style={{ fontSize: "0.85rem", fontWeight: 600 }}>
              <Avatar login={comment.user?.login ?? "ghost"} src={comment.user?.avatar_url} size={20} />
              {comment.user?.login ?? "ghost"}
              <span style={{ fontWeight: 400, color: "var(--color-fg-muted)", fontSize: "0.78rem" }}>
                commented <RelativeTime iso={comment.created_at} />
              </span>
            </span>
            {viewer.data?.login && comment.user?.login === viewer.data.login && (
              <span className="flex gap-1">
                <Button
                  size="sm"
                  variant="ghost"
                  aria-label={`Edit comment ${comment.id}`}
                  onClick={() => {
                    setEditingId(comment.id);
                    setEditBody(comment.body);
                  }}
                >
                  Edit
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  aria-label={`Delete comment ${comment.id}`}
                  disabled={deleteMut.isPending}
                  onClick={async () => {
                    if (await confirmAction("Delete this comment?", { title: "Delete comment", confirmLabel: "Delete" })) {
                      deleteMut.mutate(comment.id);
                    }
                  }}
                >
                  Delete
                </Button>
              </span>
            )}
          </div>
          {editingId === comment.id ? (
            <form
              className="flex flex-col gap-2"
              onSubmit={(event) => {
                event.preventDefault();
                if (editBody.trim()) editMut.mutate(comment.id);
              }}
            >
              <FormLabel id={`gist-comment-edit-${comment.id}`}>Edit comment</FormLabel>
              <textarea
                id={`gist-comment-edit-${comment.id}`}
                value={editBody}
                onChange={(event) => setEditBody(event.target.value)}
                rows={3}
                style={{
                  width: "100%",
                  padding: "0.5rem 0.65rem",
                  fontSize: "0.88rem",
                  borderRadius: "var(--radius-md)",
                  border: "1px solid var(--color-border)",
                  background: "var(--color-surface)",
                  color: "var(--color-fg)",
                }}
              />
              <div className="flex justify-end gap-2">
                <Button type="button" variant="ghost" size="sm" onClick={() => setEditingId(null)}>
                  Cancel
                </Button>
                <Button type="submit" variant="primary" size="sm" disabled={!editBody.trim() || editMut.isPending}>
                  {editMut.isPending ? "Saving…" : "Save"}
                </Button>
              </div>
            </form>
          ) : (
            <div style={{ fontSize: "0.88rem", whiteSpace: "pre-wrap" }}>{comment.body}</div>
          )}
        </Box>
      ))}

      {!signedIn && <SignInPrompt action="comment" />}
      {signedIn && <form
        onSubmit={(event) => {
          event.preventDefault();
          if (body.trim()) createMut.mutate();
        }}
        className="flex flex-col gap-2"
      >
        <FormLabel id="gist-comment-body">Add a comment</FormLabel>
        <textarea
          id="gist-comment-body"
          value={body}
          onChange={(event) => setBody(event.target.value)}
          rows={3}
          placeholder="Leave a comment"
          style={{
            width: "100%",
            padding: "0.5rem 0.65rem",
            fontSize: "0.88rem",
            borderRadius: "var(--radius-md)",
            border: "1px solid var(--color-border)",
            background: "var(--color-surface)",
            color: "var(--color-fg)",
          }}
        />
        <div className="flex justify-end">
          <Button type="submit" variant="primary" size="sm" disabled={!body.trim() || createMut.isPending}>
            {createMut.isPending ? "Commenting…" : "Comment"}
          </Button>
        </div>
      </form>}
    </div>
  );
}

function EditGistDialog({
  gist,
  onClose,
  onSaved,
}: {
  gist: BleephubGist;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [description, setDescription] = useState(gist.description);
  const [nextFileId, setNextFileId] = useState(Object.keys(gist.files).length);
  const [files, setFiles] = useState<
    { id: number; filename: string; content?: string | undefined; original: string }[]
  >(() =>
    Object.entries(gist.files).map(([name, file], i) => ({
      id: i,
      filename: name,
      content: file.content,
      original: name,
    })),
  );
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () => {
      const fileMap: Record<string, BleephubGistFile | null> = {};
      files.forEach((f) => {
        if (f.filename.trim()) {
          fileMap[f.filename.trim()] = { content: f.content };
        }
      });
      Object.keys(gist.files).forEach((name) => {
        if (!files.some((f) => f.filename.trim() === name)) {
          fileMap[name] = null;
        }
      });
      return updateGist(gist.id, { description, files: fileMap });
    },
    onSuccess: onSaved,
    onError: (err: Error) => setError(err.message),
  });

  const updateFile = (idx: number, patch: Partial<{ filename: string; content?: string }>) => {
    setFiles((cur) => cur.map((f, i) => (i === idx ? { ...f, ...patch } : f)));
  };

  return (
    <Modal title="Edit gist" onClose={onClose}>
      <FormLabel id="gist-edit-desc">Description</FormLabel>
      <input
        id="gist-edit-desc"
        type="text"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        className="mb-4 w-full"
      />

      <FormLabel>Files</FormLabel>
      {files.map((file, idx) => (
        <div key={file.id} className="mb-3 rounded border p-3" style={{ borderColor: "var(--color-border)" }}>
          <input
            type="text"
            aria-label="File name"
            value={file.filename}
            onChange={(e) => updateFile(idx, { filename: e.target.value })}
            placeholder="filename.ext"
            className="mb-2 w-full"
          />
          <textarea
            aria-label="File content"
            value={file.content || ""}
            onChange={(e) => updateFile(idx, { content: e.target.value })}
            rows={4}
            placeholder="file content"
            className="w-full"
            style={{ resize: "vertical" }}
          />
          <div className="mt-2 flex justify-end">
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setFiles((cur) => cur.filter((_, i) => i !== idx))}
              disabled={files.length === 1}
            >
              remove file
            </Button>
          </div>
        </div>
      ))}

      <div className="mb-4">
        <Button
          size="sm"
          variant="secondary"
          onClick={() => {
            setFiles((cur) => [...cur, { id: nextFileId, filename: "", content: "", original: "" }]);
            setNextFileId((n) => n + 1);
          }}
        >
          Add file
        </Button>
      </div>

      {error && <ErrorBanner>{error}</ErrorBanner>}

      <DialogActions>
        <Button onClick={onClose} disabled={mutation.isPending} variant="ghost">
          Cancel
        </Button>
        <Button
          onClick={() => {
            setError(null);
            mutation.mutate();
          }}
          disabled={mutation.isPending}
          variant="primary"
        >
          {mutation.isPending ? "Saving…" : "Save gist"}
        </Button>
      </DialogActions>
    </Modal>
  );
}
