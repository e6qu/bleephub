import { useMemo, useState } from "react";
import { useParams, useNavigate, Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { RepoHeader } from "../components/PageHeader.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
import { useRepoPermissions } from "../hooks/useRepoPermissions.js";
import { Box, Blankslate, Button, SectionLabel } from "../components/ui.js";
import { MutationError } from "../components/MutationError.js";
import Markdown from "../components/Markdown.js";
import { BookIcon, PlusIcon } from "../components/octicons.js";
import { confirmAction } from "../components/confirmAction.js";
import {
  fetchWikiPages,
  fetchWikiPage,
  putWikiPage,
  deleteWikiPage,
  wikiSlug,
  ghFetch,
} from "../api.js";
import { RelativeTime } from "../components/RelativeTime.js";
import { isNotFoundError } from "../components/notFound.js";
import type { GithubWikiPage } from "../types.js";

/** One row of a page's edit history (see internal/server/gh_wiki.go). */
interface WikiRevision {
  id: number;
  slug: string;
  title: string;
  editor: string;
  message: string;
  created_at: string;
  body_preview?: string;
  /** Present only on the single-revision read. */
  body?: string;
}

// Inline fetchers (ghFetch convention) — this page is the only caller, so the
// wrappers ride this lazy chunk instead of api.ts / the entry bundle.
const wikiRevisionsPath = (owner: string, repo: string, slug: string) =>
  `/ui-data/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/wiki/pages/${encodeURIComponent(slug)}/revisions`;
const fetchWikiRevisions = (owner: string, repo: string, slug: string) =>
  ghFetch<WikiRevision[]>(wikiRevisionsPath(owner, repo, slug));
const fetchWikiRevision = (owner: string, repo: string, slug: string, id: number) =>
  ghFetch<WikiRevision>(`${wikiRevisionsPath(owner, repo, slug)}/${id}`);

/**
 * Repository wiki. github.com puts a Wiki tab on repos with wikis enabled; the
 * simulator backs it with a per-repo page store (see internal/server/gh_wiki.go).
 * A left rail lists pages; the main pane views a page's markdown or edits it.
 * Write actions (New/Edit/Delete/Restore) need push access and are hidden from
 * read-only viewers, matching github.com; the wiki stays readable for everyone.
 */
export function WikiPage() {
  const { owner = "", repo = "", slug: routeSlug } = useParams<{
    owner: string;
    repo: string;
    slug?: string;
  }>();
  const counts = useOpenCounts(owner, repo);
  const { canPush } = useRepoPermissions(owner, repo);
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [editing, setEditing] = useState<null | { slug: string; title: string; body: string; isNew: boolean }>(null);
  const [showHistory, setShowHistory] = useState(false);

  const pagesQ = useQuery({
    queryKey: ["wiki-pages", owner, repo],
    queryFn: () => fetchWikiPages(owner, repo),
  });

  // The active page: the route slug, else the first page (Home sorts first).
  const activeSlug = routeSlug ?? pagesQ.data?.[0]?.slug;
  const pageQ = useQuery({
    queryKey: ["wiki-page", owner, repo, activeSlug],
    queryFn: () => fetchWikiPage(owner, repo, activeSlug as string),
    enabled: !!activeSlug && !editing,
  });

  const save = useMutation({
    mutationFn: (input: { slug: string; title: string; body: string; message: string }) => {
      // The PUT handler records an optional `message` edit summary on the
      // revision. putWikiPage's declared payload is {title, body}; passing a
      // wider object through a variable is sound (structural assignability)
      // and keeps api.ts untouched.
      const payload = { title: input.title, body: input.body, message: input.message };
      return putWikiPage(owner, repo, input.slug, payload);
    },
    onSuccess: (saved) => {
      qc.invalidateQueries({ queryKey: ["wiki-pages", owner, repo] });
      qc.invalidateQueries({ queryKey: ["wiki-page", owner, repo] });
      qc.invalidateQueries({ queryKey: ["wiki-revisions", owner, repo] });
      setEditing(null);
      navigate(`/ui/${owner}/${repo}/wiki/${encodeURIComponent(saved.slug)}`);
    },
  });

  const remove = useMutation({
    mutationFn: (slug: string) => deleteWikiPage(owner, repo, slug),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["wiki-pages", owner, repo] });
      navigate(`/ui/${owner}/${repo}/wiki`);
    },
  });

  const startNew = () =>
    setEditing({ slug: "", title: "", body: "", isNew: true });
  const startEdit = (p: GithubWikiPage) =>
    setEditing({ slug: p.slug, title: p.title, body: p.body, isNew: false });

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="wiki" {...counts} />
      <div className="mt-4 grid gap-6 md:grid-cols-[220px_1fr]">
        <WikiSidebar
          owner={owner}
          repo={repo}
          pages={pagesQ.data}
          activeSlug={activeSlug}
          canPush={canPush}
          onNew={startNew}
        />
        <div>
          {editing ? (
            <WikiEditor
              initial={editing}
              pending={save.isPending}
              error={<MutationError of={save} />}
              onCancel={() => setEditing(null)}
              onSave={(title, body, message) => {
                const slug = editing.isNew
                  ? wikiSlug(title) || "page"
                  : editing.slug;
                save.mutate({ slug, title, body, message });
              }}
            />
          ) : showHistory && activeSlug ? (
            <WikiHistory
              owner={owner}
              repo={repo}
              slug={activeSlug}
              canPush={canPush}
              onBack={() => setShowHistory(false)}
            />
          ) : (
            <WikiView
              pagesQ={pagesQ}
              pageQ={pageQ}
              activeSlug={activeSlug ?? ""}
              canPush={canPush}
              onNew={startNew}
              onCreateMissing={(title) =>
                setEditing({ slug: "", title, body: "", isNew: true })
              }
              onEdit={startEdit}
              onHistory={() => setShowHistory(true)}
              onDelete={async (p) => {
                if (await confirmAction(`Delete the wiki page “${p.title}”?`)) {
                  remove.mutate(p.slug);
                }
              }}
              removeError={<MutationError of={remove} />}
            />
          )}
        </div>
      </div>
    </div>
  );
}

function WikiSidebar({
  owner,
  repo,
  pages,
  activeSlug,
  canPush,
  onNew,
}: {
  owner: string;
  repo: string;
  pages: GithubWikiPage[] | undefined;
  activeSlug: string | undefined;
  canPush: boolean;
  onNew: () => void;
}) {
  return (
    <aside className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <SectionLabel>Pages</SectionLabel>
        {canPush && (
          <Button size="sm" onClick={onNew}>
            <PlusIcon size={13} /> New
          </Button>
        )}
      </div>
      {pages && pages.length > 0 ? (
        <nav aria-label="Wiki pages">
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {pages.map((p) => (
              <li key={p.slug}>
                <Link
                  to={`/ui/${owner}/${repo}/wiki/${encodeURIComponent(p.slug)}`}
                  aria-current={p.slug === activeSlug ? "page" : undefined}
                  style={{
                    display: "block",
                    padding: "0.3rem 0.5rem",
                    borderRadius: "var(--radius-sm)",
                    fontSize: "0.85rem",
                    textDecoration: "none",
                    color: p.slug === activeSlug ? "var(--color-fg)" : "var(--color-accent)",
                    background: p.slug === activeSlug ? "var(--color-bg-subtle)" : "transparent",
                    fontWeight: p.slug === activeSlug ? 600 : 400,
                  }}
                >
                  {p.title}
                </Link>
              </li>
            ))}
          </ul>
        </nav>
      ) : (
        <p style={{ fontSize: "0.82rem", color: "var(--color-fg-muted)" }}>No pages yet.</p>
      )}
    </aside>
  );
}

function WikiView({
  pagesQ,
  pageQ,
  activeSlug,
  canPush,
  onNew,
  onCreateMissing,
  onEdit,
  onHistory,
  onDelete,
  removeError,
}: {
  pagesQ: { isLoading: boolean; isError: boolean; error: unknown; data: GithubWikiPage[] | undefined };
  pageQ: { isLoading: boolean; isError: boolean; error: unknown; data: GithubWikiPage | undefined };
  /** "" only on the empty-wiki path, which returns before any use. */
  activeSlug: string;
  canPush: boolean;
  onNew: () => void;
  onCreateMissing: (title: string) => void;
  onEdit: (p: GithubWikiPage) => void;
  onHistory: () => void;
  onDelete: (p: GithubWikiPage) => void;
  removeError: React.ReactNode;
}) {
  if (pagesQ.isLoading) return <Spinner label="loading wiki" />;
  if (pagesQ.isError) return <InlineError title="Failed to load wiki" detail={String(pagesQ.error)} />;
  if (!activeSlug || (pagesQ.data && pagesQ.data.length === 0)) {
    return (
      <Blankslate icon={<BookIcon size={28} />} title="Welcome to the wiki">
        <p>This repository’s wiki has no pages yet.</p>
        {canPush && (
          <div className="mt-3">
            <Button variant="primary" onClick={onNew}>
              <PlusIcon size={14} /> Create the first page
            </Button>
          </div>
        )}
      </Blankslate>
    );
  }
  if (pageQ.isLoading) return <Spinner label="loading page" />;
  if (pageQ.isError || !pageQ.data) {
    // The wiki itself loaded — a 404 here is a URL naming a page that does
    // not exist. github.com offers writers a title-prefilled "create this
    // page" affordance; readers just get the 404 text. The list rail stays.
    if (isNotFoundError(pageQ.error)) {
      const missingTitle = activeSlug.replace(/-+/g, " ").trim() || "this page";
      return (
        <Blankslate
          icon={<BookIcon size={28} />}
          title={canPush ? "New page?" : "This page does not exist"}
        >
          <p>This wiki page does not exist{canPush ? " yet" : ""}.</p>
          {canPush && (
            <div className="mt-3">
              <Button variant="primary" onClick={() => onCreateMissing(missingTitle)}>
                <PlusIcon size={14} /> Create “{missingTitle}”
              </Button>
            </div>
          )}
        </Blankslate>
      );
    }
    return <InlineError title="Failed to load page" detail={String(pageQ.error)} />;
  }
  const page = pageQ.data;
  return (
    <article>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <h1 style={{ fontSize: "1.5rem", fontWeight: 600, margin: 0 }}>{page.title}</h1>
        <div className="flex items-center gap-2">
          {canPush && <Button size="sm" onClick={() => onEdit(page)}>Edit</Button>}
          <Button size="sm" onClick={onHistory}>History</Button>
          {canPush && <Button size="sm" variant="danger" onClick={() => onDelete(page)}>Delete</Button>}
        </div>
      </div>
      {removeError}
      <Box>
        <div style={{ padding: "1rem 1.25rem" }} className="markdown-body">
          {page.body.trim() ? (
            <Markdown>{page.body}</Markdown>
          ) : (
            <p style={{ color: "var(--color-fg-muted)" }}>This page is empty.</p>
          )}
        </div>
      </Box>
      <div className="mt-2" style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}>
        Last edited <RelativeTime iso={page.updated_at} />
        {page.author ? ` by ${page.author}` : ""}
      </div>
    </article>
  );
}

function WikiEditor({
  initial,
  pending,
  error,
  onCancel,
  onSave,
}: {
  initial: { title: string; body: string; isNew: boolean };
  pending: boolean;
  error: React.ReactNode;
  onCancel: () => void;
  onSave: (title: string, body: string, message: string) => void;
}) {
  const [title, setTitle] = useState(initial.title);
  const [body, setBody] = useState(initial.body);
  const [message, setMessage] = useState("");
  const slugPreview = useMemo(() => (title.trim() ? wikiSlug(title) || "page" : ""), [title]);

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (title.trim()) onSave(title.trim(), body, message.trim());
      }}
      className="flex flex-col gap-3"
    >
      <div className="flex flex-col gap-1">
        <label htmlFor="wiki-title" style={{ fontSize: "0.82rem", fontWeight: 600 }}>
          Title
        </label>
        <input
          id="wiki-title"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          placeholder="Page title"
          aria-label="Wiki page title"
        />
        {initial.isNew && slugPreview && (
          <span style={{ fontSize: "0.72rem", color: "var(--color-fg-muted)" }}>
            URL slug: {slugPreview}
          </span>
        )}
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="wiki-body" style={{ fontSize: "0.82rem", fontWeight: 600 }}>
          Content (Markdown)
        </label>
        <textarea
          id="wiki-body"
          value={body}
          onChange={(e) => setBody(e.target.value)}
          rows={16}
          placeholder="Write your page in Markdown…"
          aria-label="Wiki page content"
          style={{ fontFamily: "var(--font-mono)", fontSize: "0.85rem" }}
        />
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="wiki-message" style={{ fontSize: "0.82rem", fontWeight: 600 }}>
          Edit message (optional)
        </label>
        <input
          id="wiki-message"
          value={message}
          onChange={(e) => setMessage(e.target.value)}
          placeholder="Describe this change"
          aria-label="Wiki edit message"
        />
      </div>
      {error}
      <div className="flex items-center gap-2">
        <Button type="submit" variant="primary" disabled={pending || !title.trim()}>
          {pending ? "Saving…" : "Save page"}
        </Button>
        <Button type="button" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  );
}

/**
 * A page's revision history (newest first): edit summary, editor, age and a
 * body preview, plus GitHub's "restore this version" — a PUT of the old body
 * with a "Restore revision {id}" summary.
 */
function WikiHistory({
  owner,
  repo,
  slug,
  canPush,
  onBack,
}: {
  owner: string;
  repo: string;
  slug: string;
  canPush: boolean;
  onBack: () => void;
}) {
  const qc = useQueryClient();
  const revisionsQ = useQuery({
    queryKey: ["wiki-revisions", owner, repo, slug],
    queryFn: () => fetchWikiRevisions(owner, repo, slug),
  });
  const restore = useMutation({
    mutationFn: async (rev: WikiRevision) => {
      // The list rows carry only a preview; fetch the full snapshot to restore.
      const full = await fetchWikiRevision(owner, repo, slug, rev.id);
      const payload = {
        title: full.title,
        body: full.body ?? "",
        message: `Restore revision ${full.id}`,
      };
      return putWikiPage(owner, repo, slug, payload);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["wiki-pages", owner, repo] });
      qc.invalidateQueries({ queryKey: ["wiki-page", owner, repo] });
      qc.invalidateQueries({ queryKey: ["wiki-revisions", owner, repo, slug] });
      onBack();
    },
  });

  return (
    <section aria-label="Page history">
      <div className="mb-3 flex items-center justify-between gap-2">
        <h1 style={{ fontSize: "1.2rem", fontWeight: 600, margin: 0 }}>History</h1>
        <Button size="sm" onClick={onBack}>Back to page</Button>
      </div>
      <MutationError of={restore} />
      {revisionsQ.isLoading ? (
        <Spinner label="loading history" />
      ) : revisionsQ.isError ? (
        <InlineError title="Failed to load history" detail={String(revisionsQ.error)} />
      ) : (revisionsQ.data?.length ?? 0) === 0 ? (
        <p style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>No revisions recorded.</p>
      ) : (
        <Box>
          {revisionsQ.data!.map((rev, i) => (
            <div
              key={rev.id}
              className="flex flex-wrap items-center gap-3"
              style={{
                padding: "0.65rem 1rem",
                fontSize: "0.85rem",
                borderBottom: i < revisionsQ.data!.length - 1 ? "1px solid var(--color-border)" : "none",
              }}
            >
              <div className="min-w-0 flex-1">
                <div style={{ fontWeight: 500, color: "var(--color-fg)" }}>
                  {rev.message || "(no edit summary)"}
                </div>
                <div className="mt-0.5" style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}>
                  {rev.editor || "unknown"} · <RelativeTime iso={rev.created_at} />
                </div>
                {rev.body_preview && (
                  <div
                    className="mt-0.5 truncate"
                    style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)", fontFamily: "var(--font-mono)" }}
                  >
                    {rev.body_preview}
                  </div>
                )}
              </div>
              {canPush && (
                <Button
                  size="sm"
                  aria-label={`Restore revision ${rev.id}`}
                  disabled={restore.isPending}
                  onClick={() => restore.mutate(rev)}
                >
                  Restore
                </Button>
              )}
            </div>
          ))}
        </Box>
      )}
    </section>
  );
}
