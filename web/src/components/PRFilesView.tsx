import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner, InlineError } from "@bleephub/ui-core/components";
import { createPRReviewComment, fetchPRFiles } from "../api.js";
import type { GithubPRFile } from "../types.js";
import { Box, Blankslate, Button, FormLabel } from "./ui.js";
import { FileIcon } from "./octicons.js";

/** Color a unified-diff line by its leading marker. */
function diffLineStyle(line: string): { bg: string; fg: string } {
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

interface ParsedDiffLine {
  text: string;
  oldLine: number | null;
  newLine: number | null;
  commentLine: number | null;
  side: "LEFT" | "RIGHT" | null;
}

/** Convert a unified patch into the old/new line coordinates GitHub's review API expects. */
function parseDiffLines(patch: string): ParsedDiffLine[] {
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
    // split() produces one empty sentinel when the patch ends in a newline.
    // Actual empty source lines still carry the unified-diff marker.
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

interface CommentTarget {
  line: number;
  side: "LEFT" | "RIGHT";
  source: string;
}

function FileDiff({
  file,
  onComment,
}: {
  file: GithubPRFile;
  onComment: (file: GithubPRFile, target: CommentTarget) => void;
}) {
  return (
    <div className="mb-3">
      <Box
        header={
          <span className="flex min-w-0 flex-1 items-center gap-2">
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
          </span>
        }
      >
        {file.patch ? (
          <div style={{ overflowX: "auto" }}>
            {parseDiffLines(file.patch).map((line, i) => {
              const s = diffLineStyle(line.text);
              return (
                <div
                  key={i}
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
                    style={{ width: "3rem", paddingRight: "0.55rem", color: "var(--color-fg-subtle)" }}
                  >
                    {line.newLine ?? ""}
                  </span>
                  <span style={{ width: "2rem", textAlign: "center" }}>
                    {line.commentLine && line.side ? (
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
                          width: "1.25rem",
                          height: "1.25rem",
                          lineHeight: 1,
                        }}
                      >
                        +
                      </button>
                    ) : null}
                  </span>
                  <pre
                    style={{
                      margin: 0,
                      paddingRight: "1rem",
                      whiteSpace: "pre",
                      flex: 1,
                    }}
                  >
                    {line.text || " "}
                  </pre>
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
        )}
      </Box>
    </div>
  );
}

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
  const q = useQuery({
    queryKey: ["pr-files", owner, repo, number],
    queryFn: () => fetchPRFiles(owner, repo, number),
  });
  const commentMutation = useMutation({
    mutationFn: () => {
      if (!target) throw new Error("Select a line first");
      return createPRReviewComment(owner, repo, number, {
        body: body.trim(),
        commit_id: headSha,
        path: target.file.filename,
        line: target.line,
        side: target.side,
      });
    },
    onSuccess: () => {
      setTarget(null);
      setBody("");
      qc.invalidateQueries({ queryKey: ["pr-review-comments", owner, repo, number] });
      qc.invalidateQueries({ queryKey: ["pr-review-threads", owner, repo, number] });
      qc.invalidateQueries({ queryKey: ["pr-timeline", owner, repo, number] });
    },
  });

  if (q.isLoading) return <Spinner label="loading changed files" />;
  if (q.isError) return <InlineError title="Failed to load changed files" detail={String(q.error)} />;
  const files = q.data ?? [];
  if (files.length === 0) {
    return <Blankslate icon={<FileIcon size={26} />} title="No file changes" />;
  }

  const totalAdd = files.reduce((n, f) => n + f.additions, 0);
  const totalDel = files.reduce((n, f) => n + f.deletions, 0);
  return (
    <div>
      <div className="mb-3" style={{ fontSize: "0.83rem", color: "var(--color-fg-muted)" }}>
        Showing {files.length} changed file{files.length === 1 ? "" : "s"} with{" "}
        <span style={{ color: "var(--gh-open)" }}>{totalAdd} additions</span> and{" "}
        <span style={{ color: "var(--color-status-error)" }}>{totalDel} deletions</span>.
      </div>
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
            <FormLabel id="inline-review-comment">Review comment</FormLabel>
            <textarea
              id="inline-review-comment"
              value={body}
              onChange={(event) => setBody(event.target.value)}
              rows={5}
              autoFocus
              style={{ width: "100%" }}
              placeholder="Leave a comment on this line…"
            />
            {commentMutation.isError && (
              <InlineError inline title="Could not add review comment" detail={String(commentMutation.error)} />
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
                  setTarget(null);
                  setBody("");
                  commentMutation.reset();
                }}
              >
                Cancel
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
      {files.map((f) => (
        <FileDiff
          key={f.sha + f.filename}
          file={f}
          onComment={(file, next) => {
            setTarget({ file, ...next });
            setBody("");
            commentMutation.reset();
          }}
        />
      ))}
    </div>
  );
}
