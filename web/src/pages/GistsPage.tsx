import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useLocation, useSearchParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { confirmAction } from "../components/confirmAction.js";
import {
  createGist,
  deleteGist,
  fetchCurrentUser,
  fetchGists,
  fetchPublicGists,
  fetchStarredGists,
  isForbidden,
  isRateLimited,
} from "../api.js";
import type { BleephubGist } from "../types.js";
import {
  Blankslate,
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
import { GistIcon, FileIcon } from "../components/octicons.js";
import { Avatar } from "../components/Avatar.js";
import { RelativeTime } from "../components/RelativeTime.js";
import { CodeHighlight } from "../components/CodeHighlight.js";
import { limitedGhFetch } from "../utils/uiFetch.js";
import { loginPath, useSignedIn } from "../session.js";

type GistScope = "yours" | "public" | "starred";

export function GistsPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  // Anonymous visitors see public gists only: "yours" and "starred" are
  // viewer-scoped lists whose reads 401 without a session.
  const signedIn = useSignedIn();
  const location = useLocation();
  const [scope, setScope] = useState<GistScope>(signedIn ? "yours" : "public");
  const [showCreate, setShowCreate] = useState(false);

  useEffect(() => {
    if (searchParams.get("new") === "1") {
      setShowCreate(true);
    }
  }, [searchParams]);

  const closeCreate = () => {
    setShowCreate(false);
    if (searchParams.has("new")) {
      const next = new URLSearchParams(searchParams);
      next.delete("new");
      setSearchParams(next, { replace: true });
    }
  };

  return (
    <div>
      <PageTitle
        icon={<GistIcon size={20} />}
        title="Gists"
        meta="Code snippets and notes."
        actions={
          signedIn ? (
            <Button variant="primary" size="sm" onClick={() => setShowCreate(true)}>
              New gist
            </Button>
          ) : (
            <ButtonLink variant="primary" size="sm" to={loginPath(location)}>
              New gist
            </ButtonLink>
          )
        }
      />

      <Tabs<GistScope>
        items={
          signedIn
            ? [
                { key: "yours", label: "Yours" },
                { key: "public", label: "Public" },
                { key: "starred", label: "Starred" },
              ]
            : [{ key: "public", label: "Public" }]
        }
        active={scope}
        onChange={setScope}
      />

      <GistList scope={scope} />
      {showCreate && <CreateGistDialog onClose={closeCreate} />}
    </div>
  );
}

function gistsQueryFn(scope: GistScope) {
  switch (scope) {
    case "public":
      return fetchPublicGists;
    case "starred":
      return fetchStarredGists;
    case "yours":
    default:
      return fetchGists;
  }
}

/**
 * Card-style gist rows mirroring gist.github.com: description linking to the
 * permalink page, visibility, file count, updated time, and a snippet preview
 * of the first file.
 */
function GistList({ scope }: { scope: GistScope }) {
  const queryClient = useQueryClient();
  const [filter, setFilter] = useState("");
  const [mutationError, setMutationError] = useState<string | null>(null);

  // Anonymous visitors have no viewer (the read would 401); owner-only row
  // actions simply stay hidden.
  const signedIn = useSignedIn();
  const viewer = useQuery({
    queryKey: ["current-user"],
    queryFn: ({ signal }) => fetchCurrentUser(signal),
    enabled: signedIn,
  });
  const { data, isLoading, isError } = useQuery({
    queryKey: ["gists", scope],
    queryFn: gistsQueryFn(scope),
    refetchInterval: (query) =>
      isRateLimited(query.state.error) || isForbidden(query.state.error) ? false : 5000,
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => deleteGist(id),
    onSuccess: () => {
      setMutationError(null);
      queryClient.invalidateQueries({ queryKey: ["gists"] });
    },
    onError: (err: Error) => setMutationError(err.message),
  });

  const filtered = useMemo(() => {
    if (!data) return [];
    const q = filter.trim().toLowerCase();
    if (!q) return data;
    return data.filter(
      (g) =>
        g.description.toLowerCase().includes(q) ||
        Object.keys(g.files).some((name) => name.toLowerCase().includes(q)),
    );
  }, [data, filter]);

  if (isError) return <InlineError title="Failed to load gists" />;
  if (isLoading || !data) return <Spinner label="loading gists" />;

  return (
    <div>
      {mutationError && <ErrorBanner>{mutationError}</ErrorBanner>}
      <input
        type="search"
        value={filter}
        onChange={(e) => setFilter(e.target.value)}
        placeholder="Filter gists…"
        aria-label="Filter gists"
        className="mb-4 w-full"
        style={{ maxWidth: "20rem", fontSize: "0.85rem" }}
      />
      {data.length === 0 ? (
        <Blankslate icon={<GistIcon size={28} />} title="No gists yet">
          Create one with the “New gist” button.
        </Blankslate>
      ) : filtered.length === 0 ? (
        <Blankslate icon={<GistIcon size={28} />} title="No matches">
          No gist matches “{filter}”.
        </Blankslate>
      ) : (
        <div className="flex flex-col gap-3">
          {filtered.map((gist, i) => (
            <GistCard
              key={gist.id}
              gist={gist}
              // Only hydrate snippet previews for the first screenful of rows.
              eager={i < 30}
              canDelete={!!viewer.data?.login && viewer.data.login === gist.owner?.login}
              onDelete={async () => {
                if (await confirmAction(`Delete gist ${gist.id}?`, { title: "Delete gist", confirmLabel: "Delete" })) {
                  deleteMut.mutate(gist.id);
                }
              }}
              deleting={deleteMut.isPending}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function GistCard({
  gist,
  eager,
  canDelete,
  onDelete,
  deleting,
}: {
  gist: BleephubGist;
  eager: boolean;
  canDelete: boolean;
  onDelete: () => void;
  deleting: boolean;
}) {
  const fileNames = Object.keys(gist.files);
  const firstFile = fileNames[0];
  return (
    <Box style={{ padding: "0.85rem 1rem" }}>
      <div className="flex flex-wrap items-center gap-2">
        {gist.owner?.login && <Avatar login={gist.owner.login} src={gist.owner.avatar_url} size={24} />}
        <Link
          to={`/ui/gists/${gist.id}`}
          style={{
            color: "var(--color-accent)",
            fontWeight: 600,
            fontSize: "0.95rem",
            textDecoration: "none",
            display: "inline-block",
            lineHeight: "1.625rem",
          }}
        >
          {gist.owner?.login ? `${gist.owner.login} / ` : ""}
          {firstFile ?? gist.id}
        </Link>
        {gist.public ? <StateLabel state="open">public</StateLabel> : <StateLabel state="closed">secret</StateLabel>}
        <span className="ml-auto inline-flex items-center gap-3" style={{ fontSize: "0.78rem", color: "var(--color-fg-muted)" }}>
          <span className="inline-flex items-center gap-1">
            <FileIcon size={13} /> {fileNames.length} file{fileNames.length === 1 ? "" : "s"}
          </span>
          <span>
            Updated <RelativeTime iso={gist.updated_at} />
          </span>
          {canDelete && (
            <Button size="sm" variant="danger" onClick={onDelete} disabled={deleting} aria-label={`Delete gist ${gist.id}`}>
              delete
            </Button>
          )}
        </span>
      </div>
      <Link to={`/ui/gists/${gist.id}`} style={{ textDecoration: "none", color: "inherit" }}>
        <p className="mt-1" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          {gist.description || "(no description)"}
        </p>
        {firstFile && <GistSnippet gist={gist} filename={firstFile} eager={eager} />}
      </Link>
    </Box>
  );
}

/**
 * First-file snippet preview. List responses omit file content (as on real
 * GitHub), so the preview hydrates the gist lazily — concurrency-capped,
 * cached — and shows the first few lines.
 */
const SNIPPET_LINES = 8;

function GistSnippet({ gist, filename, eager }: { gist: BleephubGist; filename: string; eager: boolean }) {
  const inline = gist.files[filename]?.content;
  const detail = useQuery({
    queryKey: ["gist-snippet", gist.id],
    queryFn: () => limitedGhFetch<BleephubGist>(`/api/v3/gists/${encodeURIComponent(gist.id)}`),
    enabled: eager && inline == null,
    staleTime: 60_000,
    retry: false,
  });
  const content = inline ?? detail.data?.files?.[filename]?.content;
  if (content == null) return null;
  const lines = content.split("\n");
  const snippet = lines.slice(0, SNIPPET_LINES).join("\n");
  return (
    <div className="mt-2" style={{ maxHeight: "10rem", overflow: "hidden", borderRadius: "var(--radius-md)", border: "1px solid var(--color-border)" }}>
      <CodeHighlight code={snippet} path={filename} style={{ margin: 0, fontSize: "0.75rem" }} />
    </div>
  );
}

function CreateGistDialog({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient();
  const [description, setDescription] = useState("");
  const [isPublic, setIsPublic] = useState(false);
  const nextFileId = useRef(0);
  const [files, setFiles] = useState<{ id: number; filename: string; content: string }[]>(() => [
    { id: nextFileId.current++, filename: "", content: "" },
  ]);
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: () => {
      const fileMap: Record<string, { content: string }> = {};
      files.forEach((f) => {
        if (f.filename.trim()) fileMap[f.filename.trim()] = { content: f.content };
      });
      return createGist({
        description,
        public: isPublic,
        files: fileMap,
      });
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["gists"] });
      onClose();
    },
    onError: (err: Error) => setError(err.message),
  });

  const updateFile = (idx: number, patch: Partial<{ filename: string; content: string }>) => {
    setFiles((cur) => cur.map((f, i) => (i === idx ? { ...f, ...patch } : f)));
  };

  const valid = files.some((f) => f.filename.trim());

  return (
    <Modal title="Create gist" onClose={onClose}>
      <FormLabel id="gist-desc">Description</FormLabel>
      <input
        id="gist-desc"
        type="text"
        value={description}
        onChange={(e) => setDescription(e.target.value)}
        className="mb-4 w-full"
      />

      <label className="mb-4 inline-flex items-center gap-2">
        <input
          type="checkbox"
          checked={isPublic}
          onChange={(e) => setIsPublic(e.target.checked)}
        />
        <span style={{ fontSize: "0.82rem" }}>Public gist</span>
      </label>

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
            value={file.content}
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
        <Button size="sm" variant="secondary" onClick={() => setFiles((cur) => [...cur, { id: nextFileId.current++, filename: "", content: "" }])}>
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
          disabled={mutation.isPending || !valid}
          variant="primary"
        >
          {mutation.isPending ? "Creating…" : "Create gist"}
        </Button>
      </DialogActions>
    </Modal>
  );
}
