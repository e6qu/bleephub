import { Link, useNavigate, useParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { ghFetch, fetchRepoBranches, fetchRepoCommit } from "../api.js";
import type { GithubBlameResult, GithubCommit } from "../types.js";
import { RepoHeader } from "../components/PageHeader.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
import { Box } from "../components/ui.js";
import { Avatar } from "../components/Avatar.js";
import { RelativeTime } from "../components/RelativeTime.js";
import { RefSwitcher } from "../components/RefSwitcher.js";
import { PathBreadcrumbs } from "../components/PathBreadcrumbs.js";
import { repoCodeRoute } from "../routes.js";

// Defined here (not in api.ts) so the blame wrapper rides this lazily-loaded
// chunk rather than weighing on the entry bundle.
const fetchBlame = (owner: string, repo: string, path: string, ref: string) =>
  ghFetch<GithubBlameResult>(
    `/ui-data/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/blame/${path}?ref=${encodeURIComponent(ref)}`,
  );

// Hunk commits are resolved one-per-sha with a small concurrency cap, only to
// find the parent for the "view blame prior to this change" hop.
const MAX_COMMIT_FETCHES = 6;
let inFlight = 0;
const waiters: Array<() => void> = [];
const commitCache = new Map<string, Promise<GithubCommit>>();
function commitBySha(owner: string, repo: string, sha: string): Promise<GithubCommit> {
  const key = `${owner}/${repo}@${sha}`;
  let p = commitCache.get(key);
  if (!p) {
    p = (async () => {
      await new Promise<void>((resolve) => {
        if (inFlight < MAX_COMMIT_FETCHES) {
          inFlight++;
          resolve();
        } else {
          waiters.push(() => {
            inFlight++;
            resolve();
          });
        }
      });
      try {
        return await fetchRepoCommit(owner, repo, sha);
      } finally {
        inFlight--;
        waiters.shift()?.();
      }
    })();
    commitCache.set(key, p);
    p.catch(() => commitCache.delete(key));
  }
  return p;
}

/**
 * GitHub buckets each hunk's age into a small heat scale (newest = hottest);
 * eight steps mapped onto the accent token via color-mix.
 */
function heatColor(bucket: number): string {
  const pct = Math.max(8, 85 - bucket * 11);
  return `color-mix(in srgb, var(--color-accent) ${pct}%, transparent)`;
}

function ageBucket(date: string, oldest: number, newest: number): number {
  const t = new Date(date).getTime();
  if (!Number.isFinite(t) || newest <= oldest) return 7;
  // 0 = newest, 7 = oldest.
  return Math.min(7, Math.max(0, Math.round(((newest - t) / (newest - oldest)) * 7)));
}

/** "View blame prior to this change" — resolves the hunk commit's parent. */
function PriorBlameLink({
  owner,
  repo,
  path,
  sha,
  shortSha,
}: {
  owner: string;
  repo: string;
  path: string;
  sha: string;
  shortSha: string;
}) {
  const q = useQuery({
    queryKey: ["blame-parent", owner, repo, sha],
    queryFn: () => commitBySha(owner, repo, sha),
    staleTime: 5 * 60_000,
  });
  const parent = q.data?.parents?.[0]?.sha;
  if (!parent) return null;
  return (
    <Link
      to={`/ui/repos/${owner}/${repo}/blame/${parent}/${path}`}
      aria-label={`View blame prior to ${shortSha}`}
      title="View blame prior to this change"
      style={{
        display: "inline-block",
        lineHeight: "1.625rem",
        fontSize: ".72rem",
        color: "var(--color-accent)",
        textDecoration: "none",
      }}
    >
      Prior blame
    </Link>
  );
}

export function BlamePage() {
  const params = useParams<{ owner: string; repo: string; ref: string; "*": string }>();
  const owner = params.owner ?? "";
  const repo = params.repo ?? "";
  const ref = params.ref ?? "";
  const path = params["*"] ?? "";
  const counts = useOpenCounts(owner, repo);
  const navigate = useNavigate();

  const query = useQuery({
    queryKey: ["blame", owner, repo, ref, path],
    queryFn: () => fetchBlame(owner, repo, path, ref),
    enabled: !!owner && !!repo && !!ref && !!path,
  });
  const { data: branchList = [] } = useQuery({
    queryKey: ["branches", owner, repo],
    queryFn: () => fetchRepoBranches(owner, repo),
    enabled: !!owner && !!repo,
  });

  const hunkTimes = (query.data?.hunks ?? [])
    .map((h) => new Date(h.date).getTime())
    .filter((t) => Number.isFinite(t));
  const oldest = hunkTimes.length ? Math.min(...hunkTimes) : 0;
  const newest = hunkTimes.length ? Math.max(...hunkTimes) : 0;

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="code" {...counts} />
      <div className="mb-4 flex flex-wrap items-center gap-2" style={{ fontSize: ".84rem" }}>
        <RefSwitcher
          owner={owner}
          repo={repo}
          current={ref}
          branches={branchList.map((b) => b.name)}
          onSelect={(nextRef) => navigate(`/ui/repos/${owner}/${repo}/blame/${nextRef}/${path}`)}
        />
        <PathBreadcrumbs
          owner={owner}
          repo={repo}
          gitRef={ref}
          path={path}
          trailing={<span style={{ color: "var(--color-fg-muted)" }}>· blame</span>}
        />
        <Link
          to={repoCodeRoute(owner, repo, { kind: "blob", ref, path })}
          style={{ display: "inline-block", color: "var(--color-accent)", lineHeight: "1.625rem", marginLeft: "auto" }}
        >
          View file
        </Link>
      </div>

      {query.isLoading && <Spinner label={`blaming ${path}`} />}
      {query.isError && <InlineError title="Failed to load blame" detail={String(query.error)} />}
      {query.data && (
        <Box>
          <div style={{ overflowX: "auto" }}>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: ".8rem" }}>
              <caption className="sr-only">
                Line-by-line commit attribution for {path} on {ref}
              </caption>
              <tbody>
                {query.data.hunks.map((hunk) => {
                  const short = hunk.short_sha;
                  const bucket = ageBucket(hunk.date, oldest, newest);
                  return hunk.lines.map((line, i) => (
                    <tr key={`${hunk.sha}-${hunk.start_line + i}`} style={{ borderTop: i === 0 ? "1px solid var(--color-border)" : "none" }}>
                      {i === 0 && (
                        <td
                          rowSpan={hunk.lines.length}
                          aria-hidden
                          style={{
                            width: 4,
                            minWidth: 4,
                            padding: 0,
                            background: heatColor(bucket),
                          }}
                          title={`Age band ${bucket + 1} of 8 (1 = newest)`}
                        />
                      )}
                      {i === 0 && (
                        <td
                          rowSpan={hunk.lines.length}
                          style={{
                            verticalAlign: "top",
                            padding: ".35rem .6rem",
                            width: "18rem",
                            maxWidth: "18rem",
                            borderRight: "1px solid var(--color-border)",
                            background: "var(--color-bg-subtle)",
                          }}
                        >
                          <div className="flex items-start gap-1.5">
                            <Avatar login={hunk.author} size={18} />
                            <div className="min-w-0">
                              <Link
                                to={`/ui/repos/${owner}/${repo}/commits/${hunk.sha}`}
                                className="font-mono"
                                style={{ display: "inline-block", color: "var(--color-accent)", lineHeight: "1.625rem" }}
                              >
                                {short}
                              </Link>{" "}
                              <span className="truncate" style={{ display: "inline-block", maxWidth: "9.5rem", verticalAlign: "bottom" }}>
                                {hunk.summary}
                              </span>
                              <div style={{ color: "var(--color-fg-muted)", fontSize: ".72rem" }}>
                                {hunk.author}
                                {hunk.date ? (
                                  <>
                                    {" · "}
                                    <RelativeTime iso={hunk.date} />
                                  </>
                                ) : null}
                              </div>
                              <PriorBlameLink owner={owner} repo={repo} path={path} sha={hunk.sha} shortSha={short} />
                            </div>
                          </div>
                        </td>
                      )}
                      <td
                        style={{
                          textAlign: "right",
                          padding: "0 .6rem",
                          width: "3rem",
                          color: "var(--color-fg-muted)",
                          userSelect: "none",
                          verticalAlign: "top",
                        }}
                        className="tabular-nums"
                      >
                        {hunk.start_line + i}
                      </td>
                      <td style={{ padding: "0 .6rem", width: "100%" }}>
                        <pre className="font-mono" style={{ margin: 0, whiteSpace: "pre-wrap", wordBreak: "break-word" }}>
                          {line || " "}
                        </pre>
                      </td>
                    </tr>
                  ));
                })}
              </tbody>
            </table>
          </div>
        </Box>
      )}
    </div>
  );
}
