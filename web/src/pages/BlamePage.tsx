import { Link, useParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { ghFetch } from "../api.js";
import type { GithubBlameResult } from "../types.js";
import { RepoHeader } from "../components/Shell.js";
import { useOpenCounts } from "../hooks/useOpenCounts.js";
import { Box } from "../components/ui.js";

// Defined here (not in api.ts) so the blame wrapper rides this lazily-loaded
// chunk rather than weighing on the entry bundle.
const fetchBlame = (owner: string, repo: string, path: string, ref: string) =>
  ghFetch<GithubBlameResult>(
    `/ui-data/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/blame/${path}?ref=${encodeURIComponent(ref)}`,
  );

export function BlamePage() {
  const params = useParams<{ owner: string; repo: string; ref: string; "*": string }>();
  const owner = params.owner ?? "";
  const repo = params.repo ?? "";
  const ref = params.ref ?? "";
  const path = params["*"] ?? "";
  const counts = useOpenCounts(owner, repo);

  const query = useQuery({
    queryKey: ["blame", owner, repo, ref, path],
    queryFn: () => fetchBlame(owner, repo, path, ref),
    enabled: !!owner && !!repo && !!ref && !!path,
  });

  return (
    <div>
      <RepoHeader owner={owner} repo={repo} active="code" {...counts} />
      <div className="mb-4 flex flex-wrap items-center gap-2" style={{ fontSize: ".84rem" }}>
        <Link to={`/ui/repos/${owner}/${repo}`} style={{ color: "var(--color-accent)", textDecoration: "none" }}>
          {owner}/{repo}
        </Link>
        <span style={{ color: "var(--color-fg-muted)" }}>/</span>
        <span className="font-mono">{path}</span>
        <span style={{ color: "var(--color-fg-muted)" }}>· blame on {ref}</span>
        <Link
          to={`/ui/repos/${owner}/${repo}/blob/${ref}/${path}`}
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
                  return hunk.lines.map((line, i) => (
                    <tr key={`${hunk.sha}-${hunk.start_line + i}`} style={{ borderTop: i === 0 ? "1px solid var(--color-border)" : "none" }}>
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
                          <Link
                            to={`/ui/repos/${owner}/${repo}/commits/${hunk.sha}`}
                            className="font-mono"
                            style={{ display: "inline-block", color: "var(--color-accent)", lineHeight: "1.625rem" }}
                          >
                            {short}
                          </Link>{" "}
                          <span className="truncate" style={{ display: "inline-block", maxWidth: "11rem", verticalAlign: "bottom" }}>
                            {hunk.summary}
                          </span>
                          <div style={{ color: "var(--color-fg-muted)", fontSize: ".72rem" }}>
                            {hunk.author}
                            {hunk.date ? ` · ${new Date(hunk.date).toLocaleDateString()}` : ""}
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
