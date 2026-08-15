import { useMemo, useState } from "react";
import { useParams, useNavigate, Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { RepoHeader } from "../components/PageHeader.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
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
} from "../api.js";
import type { GithubWikiPage } from "../types.js";

/**
 * Repository wiki. github.com puts a Wiki tab on repos with wikis enabled; the
 * simulator backs it with a per-repo page store (see internal/server/gh_wiki.go).
 * A left rail lists pages; the main pane views a page's markdown or edits it.
 * Write actions (New/Edit/Delete) call the store and surface a 403 for viewers
 * without push access rather than being hidden.
 */
export function WikiPage() {
  const { owner = "", repo = "", slug: routeSlug } = useParams<{
    owner: string;
    repo: string;
    slug?: string;
  }>();
  const counts = useOpenCounts(owner, repo);
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [editing, setEditing] = useState<null | { slug: string; title: string; body: string; isNew: boolean }>(null);

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
    mutationFn: (input: { slug: string; title: string; body: string }) =>
      putWikiPage(owner, repo, input.slug, { title: input.title, body: input.body }),
    onSuccess: (saved) => {
      qc.invalidateQueries({ queryKey: ["wiki-pages", owner, repo] });
      qc.invalidateQueries({ queryKey: ["wiki-page", owner, repo] });
      setEditing(null);
      navigate(`/ui/repos/${owner}/${repo}/wiki/${encodeURIComponent(saved.slug)}`);
    },
  });

  const remove = useMutation({
    mutationFn: (slug: string) => deleteWikiPage(owner, repo, slug),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["wiki-pages", owner, repo] });
      navigate(`/ui/repos/${owner}/${repo}/wiki`);
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
          onNew={startNew}
        />
        <div>
          {editing ? (
            <WikiEditor
              initial={editing}
              pending={save.isPending}
              error={<MutationError of={save} />}
              onCancel={() => setEditing(null)}
              onSave={(title, body) => {
                const slug = editing.isNew
                  ? wikiSlug(title) || "page"
                  : editing.slug;
                save.mutate({ slug, title, body });
              }}
            />
          ) : (
            <WikiView
              pagesQ={pagesQ}
              pageQ={pageQ}
              activeSlug={activeSlug}
              onNew={startNew}
              onEdit={startEdit}
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
  onNew,
}: {
  owner: string;
  repo: string;
  pages: GithubWikiPage[] | undefined;
  activeSlug: string | undefined;
  onNew: () => void;
}) {
  return (
    <aside className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <SectionLabel>Pages</SectionLabel>
        <Button size="sm" onClick={onNew}>
          <PlusIcon size={13} /> New
        </Button>
      </div>
      {pages && pages.length > 0 ? (
        <nav aria-label="Wiki pages">
          <ul style={{ listStyle: "none", margin: 0, padding: 0 }}>
            {pages.map((p) => (
              <li key={p.slug}>
                <Link
                  to={`/ui/repos/${owner}/${repo}/wiki/${encodeURIComponent(p.slug)}`}
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
  onNew,
  onEdit,
  onDelete,
  removeError,
}: {
  pagesQ: { isLoading: boolean; isError: boolean; error: unknown; data: GithubWikiPage[] | undefined };
  pageQ: { isLoading: boolean; isError: boolean; error: unknown; data: GithubWikiPage | undefined };
  activeSlug: string | undefined;
  onNew: () => void;
  onEdit: (p: GithubWikiPage) => void;
  onDelete: (p: GithubWikiPage) => void;
  removeError: React.ReactNode;
}) {
  if (pagesQ.isLoading) return <Spinner label="loading wiki" />;
  if (pagesQ.isError) return <InlineError title="Failed to load wiki" detail={String(pagesQ.error)} />;
  if (!activeSlug || (pagesQ.data && pagesQ.data.length === 0)) {
    return (
      <Blankslate icon={<BookIcon size={28} />} title="Welcome to the wiki">
        <p>This repository’s wiki has no pages yet.</p>
        <div className="mt-3">
          <Button variant="primary" onClick={onNew}>
            <PlusIcon size={14} /> Create the first page
          </Button>
        </div>
      </Blankslate>
    );
  }
  if (pageQ.isLoading) return <Spinner label="loading page" />;
  if (pageQ.isError || !pageQ.data) {
    return <InlineError title="Failed to load page" detail={String(pageQ.error)} />;
  }
  const page = pageQ.data;
  return (
    <article>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <h1 style={{ fontSize: "1.5rem", fontWeight: 600, margin: 0 }}>{page.title}</h1>
        <div className="flex items-center gap-2">
          <Button size="sm" onClick={() => onEdit(page)}>Edit</Button>
          <Button size="sm" variant="danger" onClick={() => onDelete(page)}>Delete</Button>
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
        Last edited {new Date(page.updated_at).toLocaleString()}
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
  onSave: (title: string, body: string) => void;
}) {
  const [title, setTitle] = useState(initial.title);
  const [body, setBody] = useState(initial.body);
  const slugPreview = useMemo(() => (title.trim() ? wikiSlug(title) || "page" : ""), [title]);

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (title.trim()) onSave(title.trim(), body);
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
