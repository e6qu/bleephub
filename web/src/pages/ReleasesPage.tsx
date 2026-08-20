import { useEffect, useState, type FormEvent } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import {
  addReleaseReaction,
  createRelease,
  deleteRelease,
  deleteReleaseAsset,
  downloadReleaseAsset,
  fetchAuthenticatedUser,
  fetchRelease,
  fetchReleaseReactions,
  fetchReleases,
  removeReleaseReaction,
  updateRelease,
  uploadReleaseAsset,
  ghPostJSON,
  type ReleasePayload,
} from "../api.js";
import { ReactionBar } from "../components/ReactionBar.js";
import Markdown from "../components/Markdown.js";
import { RelativeTime } from "../components/RelativeTime.js";
import type { GithubRelease, GithubReleaseAsset } from "../types.js";
import { RepoHeader } from "../components/PageHeader.js";
import { RepoNotFound } from "../components/RepoNotFound.js";
import { useRepoPermissions } from "../hooks/useRepoPermissions.js";
import { useSignedIn } from "../session.js";
import { Blankslate, Box, Button, ButtonLink, ErrorBanner, FormLabel, PageTitle } from "../components/ui.js";
import { confirmAction } from "../components/confirmAction.js";
import { DownloadIcon, PlusIcon, TagIcon, TrashIcon } from "../components/octicons.js";

const inputStyle = {
  width: "100%",
  border: "1px solid var(--color-border)",
  borderRadius: "var(--radius-md)",
  background: "var(--color-surface)",
  color: "var(--color-fg)",
  padding: "0.45rem 0.6rem",
} as const;

export function ReleasesPage() {
  const { owner = "", repo = "", releaseId } = useParams<{
    owner: string;
    repo: string;
    releaseId?: string;
  }>();
  const location = useLocation();
  const creating = location.pathname.endsWith("/releases/new");
  const id = releaseId ? Number(releaseId) : 0;
  // /releases/new is a write surface: github.com 404s it for viewers without
  // push access. Wait for the permissions payload before deciding so writers
  // never see a 404 flash.
  const { canPush, loaded } = useRepoPermissions(owner, repo);

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="code" />
      {creating ? (
        !loaded ? (
          <Spinner label="loading repository" />
        ) : canPush ? (
          <ReleaseEditor owner={owner} repo={repo} />
        ) : (
          <RepoNotFound />
        )
      ) : id > 0 ? (
        <ReleaseDetail owner={owner} repo={repo} releaseId={id} />
      ) : (
        <ReleaseList owner={owner} repo={repo} />
      )}
    </div>
  );
}

function ReleaseList({ owner, repo }: { owner: string; repo: string }) {
  // "New release" needs push access; the feed stays readable for everyone.
  const { canPush } = useRepoPermissions(owner, repo);
  const releases = useQuery({
    queryKey: ["releases", owner, repo],
    queryFn: () => fetchReleases(owner, repo),
  });
  if (releases.isLoading) return <Spinner label="loading releases" />;
  if (releases.isError) return <InlineError title="Failed to load releases" detail={String(releases.error)} />;

  return (
    <>
      <PageTitle
        icon={<TagIcon size={22} />}
        title="Releases"
        meta={`${releases.data?.length ?? 0} releases`}
        actions={
          canPush ? (
            <ButtonLink variant="primary" to={`/ui/repos/${owner}/${repo}/releases/new`}>
              <PlusIcon size={14} /> New release
            </ButtonLink>
          ) : undefined
        }
      />
      {(releases.data?.length ?? 0) === 0 ? (
        <Blankslate icon={<TagIcon size={28} />} title="No releases published">
          Create a release from a real repository tag and attach distributable files.
        </Blankslate>
      ) : (
        <div className="flex flex-col gap-4">
          {releases.data!.map((release) => (
            <ReleaseFeedItem
              key={release.id}
              owner={owner}
              repo={repo}
              release={release}
              isLatest={release.id === releases.data!.find((r) => !r.draft && !r.prerelease)?.id}
            />
          ))}
        </div>
      )}
    </>
  );
}

const chipStyle = (color: string, filled = false) =>
  ({
    fontSize: "0.68rem",
    fontWeight: 600,
    color: filled ? "#ffffff" : color,
    background: filled ? color : "transparent",
    border: `1px solid ${color}`,
    borderRadius: "2rem",
    padding: "0.05rem 0.5rem",
    whiteSpace: "nowrap",
  }) as const;

/** GitHub's releases feed entry: title + chips, meta, rendered notes, assets. */
function ReleaseFeedItem({ owner, repo, release, isLatest }: {
  owner: string;
  repo: string;
  release: GithubRelease;
  isLatest: boolean;
}) {
  const archiveBase = `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}`;
  const assetLink = {
    color: "var(--color-accent)",
    textDecoration: "none",
    display: "inline-block",
    lineHeight: "1.625rem",
  } as const;
  return (
    <Box
      header={
        <div className="flex w-full min-w-0 flex-wrap items-center gap-2">
          <Link
            to={`/ui/repos/${owner}/${repo}/releases/${release.id}`}
            className="min-w-0 truncate"
            style={{ fontSize: "1.05rem", fontWeight: 600, color: "var(--color-fg)", textDecoration: "none" }}
          >
            {release.name || release.tag_name}
          </Link>
          {isLatest && <span style={chipStyle("var(--gh-open-solid)", true)}>Latest</span>}
          {release.prerelease && <span style={chipStyle("var(--color-brand-gold)")}>Pre-release</span>}
          {release.draft && <span style={chipStyle("var(--color-fg-muted)")}>Draft</span>}
          <span
            className="flex flex-wrap items-center gap-2"
            style={{ marginLeft: "auto", fontSize: "0.78rem", color: "var(--color-fg-muted)" }}
          >
            <span className="inline-flex items-center gap-1 font-mono">
              <TagIcon size={12} /> {release.tag_name}
            </span>
            {release.author && <span>{release.author.login}</span>}
            {release.published_at ? (
              <span>
                released <RelativeTime iso={release.published_at} />
              </span>
            ) : (
              <span>
                drafted <RelativeTime iso={release.created_at} />
              </span>
            )}
          </span>
        </div>
      }
    >
      <div style={{ padding: "1rem 1.25rem" }}>
        {release.body.trim() ? (
          <div className="markdown-body" style={{ fontSize: "0.88rem" }}>
            <Markdown>{release.body}</Markdown>
          </div>
        ) : (
          <p style={{ margin: 0, fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>No release notes.</p>
        )}
        <div className="mt-4" style={{ borderTop: "1px solid var(--color-border)", paddingTop: "0.7rem" }}>
          <div className="mb-1" style={{ fontSize: "0.8rem", fontWeight: 600 }}>
            Assets <span style={{ color: "var(--color-fg-muted)", fontWeight: 400 }}>{release.assets.length + 2}</span>
          </div>
          <ul style={{ listStyle: "none", margin: 0, padding: 0, fontSize: "0.82rem" }}>
            {release.assets.map((asset) => (
              <li key={asset.id} className="flex flex-wrap items-center gap-2" style={{ padding: "0.15rem 0" }}>
                <Link to={`/ui/repos/${owner}/${repo}/releases/${release.id}`} style={assetLink}>
                  {asset.label || asset.name}
                </Link>
                <span style={{ color: "var(--color-fg-muted)", fontSize: "0.74rem" }}>
                  {asset.size.toLocaleString()} bytes · {asset.download_count} downloads
                </span>
              </li>
            ))}
            {/* GitHub auto-lists the source archives for the release's tag. */}
            <li style={{ padding: "0.15rem 0" }}>
              <a href={`${archiveBase}/zipball/${encodeURIComponent(release.tag_name)}`} style={assetLink}>
                Source code (zip)
              </a>
            </li>
            <li style={{ padding: "0.15rem 0" }}>
              <a href={`${archiveBase}/tarball/${encodeURIComponent(release.tag_name)}`} style={assetLink}>
                Source code (tar.gz)
              </a>
            </li>
          </ul>
        </div>
      </div>
    </Box>
  );
}

// github.com's "Generate release notes" — autogenerates the title + a
// changelog body from the commits since the previous tag. Defined here so it
// rides this lazy chunk rather than weighing on the entry bundle.
const generateReleaseNotes = (owner: string, repo: string, tag_name: string, target_commitish?: string) =>
  ghPostJSON<{ name: string; body: string }>(
    `/api/v3/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/releases/generate-notes`,
    { tag_name, target_commitish },
  );

function ReleaseEditor({ owner, repo, release, onSaved }: { owner: string; repo: string; release?: GithubRelease; onSaved?: (saved: GithubRelease) => void }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [tagName, setTagName] = useState(release?.tag_name ?? "");
  const [target, setTarget] = useState(release?.target_commitish ?? "");
  const [name, setName] = useState(release?.name ?? "");
  const [body, setBody] = useState(release?.body ?? "");
  const [draft, setDraft] = useState(release?.draft ?? false);
  const [prerelease, setPrerelease] = useState(release?.prerelease ?? false);
  // GitHub's "Set as the latest release" checkbox. The release REST object does
  // not expose whether it is currently excluded from latest, so this reflects
  // GitHub's default (eligible/on); unchecking sends make_latest:"false".
  const [makeLatest, setMakeLatest] = useState(true);

  useEffect(() => {
    if (!release) return;
    setTagName(release.tag_name);
    setTarget(release.target_commitish);
    setName(release.name);
    setBody(release.body);
    setDraft(release.draft);
    setPrerelease(release.prerelease);
  }, [release]);

  const save = useMutation({
    mutationFn: async () => {
      const payload: ReleasePayload = {
        tag_name: tagName.trim(),
        target_commitish: target.trim() || undefined,
        name: name.trim(),
        body,
        draft,
        prerelease,
        make_latest: makeLatest ? "true" : "false",
      };
      return release
        ? updateRelease(owner, repo, release.id, payload)
        : createRelease(owner, repo, payload);
    },
    onSuccess: async (saved) => {
	  queryClient.setQueryData(["release", owner, repo, saved.id], saved);
      await queryClient.invalidateQueries({ queryKey: ["releases", owner, repo] });
      if (onSaved) onSaved(saved);
      else navigate(`/ui/repos/${owner}/${repo}/releases/${saved.id}`);
    },
  });

  const genNotes = useMutation({
    mutationFn: () => generateReleaseNotes(owner, repo, tagName.trim(), target.trim() || undefined),
    onSuccess: (notes) => {
      setBody(notes.body ?? "");
      if (!name.trim() && notes.name) setName(notes.name);
    },
  });

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (tagName.trim()) save.mutate();
  };

  return (
    <>
      <PageTitle title={release ? `Edit ${release.name || release.tag_name}` : "Create a new release"} />
      {save.isError && <ErrorBanner>{String(save.error)}</ErrorBanner>}
      <form onSubmit={submit} className="flex flex-col gap-4">
        <div className="grid gap-4 md:grid-cols-2">
          <label><FormLabel>Tag</FormLabel><input aria-label="Tag" required value={tagName} onChange={(e) => setTagName(e.target.value)} style={inputStyle} placeholder="v1.0.0" /></label>
          <label><FormLabel>Target branch or commit</FormLabel><input aria-label="Target branch or commit" value={target} onChange={(e) => setTarget(e.target.value)} style={inputStyle} placeholder="main" /></label>
        </div>
        <label><FormLabel>Release title</FormLabel><input aria-label="Release title" value={name} onChange={(e) => setName(e.target.value)} style={inputStyle} /></label>
        <div>
          <div className="flex items-center justify-between gap-2">
            <FormLabel>Release notes</FormLabel>
            <Button
              type="button"
              size="sm"
              variant="secondary"
              disabled={!tagName.trim() || genNotes.isPending}
              onClick={() => genNotes.mutate()}
            >
              {genNotes.isPending ? "Generating…" : "Generate release notes"}
            </Button>
          </div>
          {genNotes.isError && <ErrorBanner>{String(genNotes.error)}</ErrorBanner>}
          <textarea aria-label="Release notes" value={body} onChange={(e) => setBody(e.target.value)} rows={10} style={{ ...inputStyle, resize: "vertical" }} />
        </div>
        <div className="flex flex-wrap gap-5">
          <label className="inline-flex items-center gap-2"><input type="checkbox" checked={draft} onChange={(e) => setDraft(e.target.checked)} /> Save as draft</label>
          <label className="inline-flex items-center gap-2"><input type="checkbox" checked={prerelease} onChange={(e) => setPrerelease(e.target.checked)} /> Mark as pre-release</label>
          <label className="inline-flex items-center gap-2"><input type="checkbox" checked={makeLatest} onChange={(e) => setMakeLatest(e.target.checked)} /> Set as the latest release</label>
        </div>
        <div className="flex gap-2">
          <Button type="submit" variant="primary" disabled={!tagName.trim() || save.isPending}>{release ? "Save changes" : "Create release"}</Button>
          <Button type="button" onClick={() => navigate(release ? `/ui/repos/${owner}/${repo}/releases/${release.id}` : `/ui/repos/${owner}/${repo}/releases`)}>Cancel</Button>
        </div>
      </form>
    </>
  );
}

function ReleaseDetail({ owner, repo, releaseId }: { owner: string; repo: string; releaseId: number }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState(false);
  // Editing/deleting a release (and managing its assets) needs push access.
  const { canPush } = useRepoPermissions(owner, repo);
  const release = useQuery({ queryKey: ["release", owner, repo, releaseId], queryFn: () => fetchRelease(owner, repo, releaseId) });
  // Anonymous visitors have no viewer (the read would 401).
  const signedIn = useSignedIn();
  const viewerQ = useQuery({ queryKey: ["viewer"], queryFn: fetchAuthenticatedUser, enabled: signedIn });
  const viewerLogin = typeof viewerQ.data?.login === "string" ? viewerQ.data.login : null;
  const remove = useMutation({
    mutationFn: () => deleteRelease(owner, repo, releaseId),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["releases", owner, repo] });
      navigate(`/ui/repos/${owner}/${repo}/releases`);
    },
  });
  if (release.isLoading) return <Spinner label="loading release" />;
  if (release.isError || !release.data) return <InlineError title="Failed to load release" detail={String(release.error)} />;
  if (editing) return <ReleaseEditor owner={owner} repo={repo} release={release.data} onSaved={(saved) => {
    queryClient.setQueryData(["release", owner, repo, releaseId], saved);
    setEditing(false);
  }} />;

  return (
    <>
      <PageTitle
        icon={<TagIcon size={22} />}
        title={release.data.name || release.data.tag_name}
        meta={<><span className="font-mono">{release.data.tag_name}</span>{release.data.draft ? " · Draft" : release.data.prerelease ? " · Pre-release" : " · Published"}</>}
        actions={canPush ? <><Button onClick={() => setEditing(true)}>Edit</Button><Button variant="danger" onClick={async () => { if (await confirmAction("Delete this release and all of its assets?")) remove.mutate(); }}><TrashIcon size={14} /> Delete</Button></> : undefined}
      />
      {remove.isError && <ErrorBanner>{String(remove.error)}</ErrorBanner>}
      {(release.data.author || release.data.published_at) && (
        <div className="mb-4" style={{ fontSize: "0.85rem", color: "var(--color-fg-muted)" }}>
          {release.data.author && <strong style={{ color: "var(--color-fg)" }}>{release.data.author.login}</strong>}
          {release.data.author ? " released this " : "Released "}
          {release.data.published_at && <RelativeTime iso={release.data.published_at} />}
        </div>
      )}
      {release.data.discussion_url && (
        <div className="mb-4" style={{ fontSize: "0.85rem" }}>
          <Link
            to={`/ui/repos/${owner}/${repo}/discussions/${release.data.discussion_url.split("/").pop()}`}
            style={{ color: "var(--color-accent)", textDecoration: "none" }}
          >
            Join the release discussion
          </Link>
        </div>
      )}
      {release.data.body && (
        <div className="markdown-body mb-5">
          <Markdown>{release.data.body}</Markdown>
        </div>
      )}
      <div className="mb-5">
        <ReactionBar
          queryKey={["release-reactions", owner, repo, releaseId]}
          fetchList={() => fetchReleaseReactions(owner, repo, releaseId)}
          add={(content) => addReleaseReaction(owner, repo, releaseId, content)}
          remove={(reactionId) => removeReleaseReaction(owner, repo, releaseId, reactionId)}
          viewerLogin={viewerLogin}
        />
      </div>
      <ReleaseAssets owner={owner} repo={repo} release={release.data} canPush={canPush} />
    </>
  );
}

function ReleaseAssets({ owner, repo, release, canPush }: { owner: string; repo: string; release: GithubRelease; canPush: boolean }) {
  const queryClient = useQueryClient();
  const [file, setFile] = useState<File | null>(null);
  const [label, setLabel] = useState("");
  const upload = useMutation({
    mutationFn: () => uploadReleaseAsset(owner, repo, release.id, file!, label.trim()),
    onSuccess: async () => {
      setFile(null);
      setLabel("");
      await queryClient.invalidateQueries({ queryKey: ["release", owner, repo, release.id] });
    },
  });
  const remove = useMutation({
    mutationFn: (assetId: number) => deleteReleaseAsset(owner, repo, assetId),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["release", owner, repo, release.id] }),
  });
  const download = async (asset: GithubReleaseAsset) => {
    const blob = await downloadReleaseAsset(owner, repo, asset.id);
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = asset.name;
    anchor.click();
    URL.revokeObjectURL(url);
  };

  return (
    <section aria-labelledby="release-assets-heading">
      <h2 id="release-assets-heading" className="mb-3" style={{ fontSize: "1rem", fontWeight: 600 }}>Assets</h2>
      {(upload.isError || remove.isError) && <ErrorBanner>{String(upload.error ?? remove.error)}</ErrorBanner>}
      {release.assets.length > 0 && (
        <Box className="mb-4">
          {release.assets.map((asset, index) => (
            <div key={asset.id} className="flex flex-wrap items-center gap-3" style={{ padding: "0.65rem 1rem", borderBottom: index === release.assets.length - 1 ? "none" : "1px solid var(--color-border)" }}>
              <div className="min-w-0 flex-1"><div style={{ fontWeight: 500 }}>{asset.label || asset.name}</div><div style={{ fontSize: "0.76rem", color: "var(--color-fg-muted)" }}>{asset.name} · {asset.size.toLocaleString()} bytes · {asset.download_count} downloads</div></div>
              <Button size="sm" aria-label={`Download ${asset.name}`} onClick={() => void download(asset)}><DownloadIcon size={14} /></Button>
              {canPush && <Button size="sm" variant="danger" aria-label={`Delete ${asset.name}`} onClick={() => remove.mutate(asset.id)}><TrashIcon size={14} /></Button>}
            </div>
          ))}
        </Box>
      )}
      {canPush && (
        <div className="flex flex-wrap items-end gap-3">
          <label><FormLabel>Asset file</FormLabel><input aria-label="Asset file" type="file" onChange={(e) => setFile(e.target.files?.[0] ?? null)} /></label>
          <label><FormLabel>Label</FormLabel><input aria-label="Asset label" value={label} onChange={(e) => setLabel(e.target.value)} style={{ ...inputStyle, minWidth: 220 }} /></label>
          <Button variant="primary" disabled={!file || upload.isPending} onClick={() => upload.mutate()}><PlusIcon size={14} /> Upload asset</Button>
        </div>
      )}
    </section>
  );
}
