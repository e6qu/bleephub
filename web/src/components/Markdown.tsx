import ReactMarkdown, { type Options } from "react-markdown";
import remarkGfm from "remark-gfm";
import { Link, useParams } from "react-router";
import type { ComponentPropsWithoutRef } from "react";

/**
 * Flip the Nth task-list marker (`[ ]`↔`[x]`) in the raw markdown source. The
 * ordinal matches the rendered checkbox order (both are document order), so a
 * clicked checkbox's DOM index maps straight to the source marker to toggle.
 */
export function toggleTaskInMarkdown(source: string, index: number, checked: boolean): string {
  let i = -1;
  return source.replace(/^(\s*(?:[-*+]|\d+\.)\s+)\[([ xX])\]/gm, (whole, prefix: string) => {
    i += 1;
    return i === index ? `${prefix}[${checked ? "x" : " "}]` : whole;
  });
}

// Shared markdown renderer. Defaults to GitHub-flavored markdown. remark-gfm
// emits task-list (`- [ ]`) checkboxes disabled; we give them an accessible name
// (WCAG 4.1.2 / the `label` rule — the item text carries the meaning). When an
// `onToggleTask` handler is supplied (an editable body), the checkboxes become
// interactive: a click reports its document index so the caller can flip the
// matching source marker and persist, matching github.com.
function TaskListInput({
  onToggleTask,
  ...props
}: ComponentPropsWithoutRef<"input"> & { onToggleTask?: ((index: number, checked: boolean) => void) | undefined }) {
  if (props.type === "checkbox") {
    const label = props.checked ? "completed task" : "incomplete task";
    if (onToggleTask) {
      return (
        <input
          {...props}
          aria-label={label}
          disabled={false}
          onChange={(e) => {
            const root = e.currentTarget.closest(".markdown-body");
            const boxes = root ? [...root.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')] : [];
            const idx = boxes.indexOf(e.currentTarget);
            if (idx >= 0) onToggleTask(idx, e.currentTarget.checked);
          }}
        />
      );
    }
    return <input {...props} aria-label={label} />;
  }
  return <input {...props} />;
}

// Minimal mdast shapes — just the fields the alert transform reads/writes — so
// this stays free of a direct dependency on the (transitive) mdast typings.
interface MdNode {
  type: string;
  value?: string;
  url?: string;
  children?: MdNode[];
  data?: { hName?: string; hProperties?: Record<string, unknown> };
}

const ALERT_TYPES = ["NOTE", "TIP", "IMPORTANT", "WARNING", "CAUTION"] as const;
const ALERT_TITLE: Record<string, string> = {
  NOTE: "Note",
  TIP: "Tip",
  IMPORTANT: "Important",
  WARNING: "Warning",
  CAUTION: "Caution",
};

// GitHub alert/callout support: a blockquote whose first line is `[!NOTE]`
// (or TIP / IMPORTANT / WARNING / CAUTION) renders as a coloured admonition
// with a title instead of a plain quote. remark-gfm does not handle these, so
// this small transform rewrites matching blockquotes into a styled <div>. It
// walks the tree by hand rather than pulling in unist-util-visit (a transitive
// dep the bundle does not otherwise declare).
function remarkGithubAlerts() {
  return (tree: MdNode) => {
    for (const node of tree.children ?? []) {
      if (node.type !== "blockquote") continue;
      const firstPara = node.children?.[0];
      const firstText = firstPara?.type === "paragraph" ? firstPara.children?.[0] : undefined;
      if (!firstText || firstText.type !== "text" || firstText.value === undefined) continue;
      const match = /^\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\][ \t]*(\r?\n|$)/i.exec(firstText.value);
      if (!match) continue;
      const type = match[1]!.toUpperCase() as (typeof ALERT_TYPES)[number];
      // Drop the marker line from the body text.
      firstText.value = firstText.value.slice(match[0].length).replace(/^\r?\n/, "");
      node.data = {
        hName: "div",
        hProperties: { className: ["markdown-alert", `markdown-alert-${type.toLowerCase()}`] },
      };
      // Prepend a title line ("Note", "Warning", …) styled by the alert class.
      node.children = [
        {
          type: "paragraph",
          data: { hName: "p", hProperties: { className: ["markdown-alert-title"] } },
          children: [{ type: "text", value: ALERT_TITLE[type] ?? type }],
        },
        ...(node.children ?? []),
      ];
    }
    return tree;
  };
}

// GitHub reference autolinks that remark-gfm doesn't do: `owner/repo#123`,
// `#123` (same-repo issue/PR), `@user` mentions, and full 40-char commit SHAs.
// `#123` and SHAs need repo context; without it they're left as plain text (as
// GitHub also does off a repo). Emails and code spans are skipped — the regex
// requires a non-word/non-`@` boundary and the walk never enters code/links.
interface LinkContext {
  owner: string;
  repo: string;
}
const REF_RE =
  /(?<![\w/])([A-Za-z0-9][\w.-]*\/[A-Za-z0-9][\w.-]*)#(\d+)\b|(?<![\w/])#(\d+)\b|(?<![\w@/`])@([A-Za-z0-9][\w-]*)\b|\b([0-9a-f]{40})\b/g;

function tokenizeRefs(value: string, ctx: LinkContext | undefined): MdNode[] {
  const out: MdNode[] = [];
  let last = 0;
  REF_RE.lastIndex = 0;
  let m: RegExpExecArray | null;
  while ((m = REF_RE.exec(value)) !== null) {
    const full = m[0];
    if (m.index > last) out.push({ type: "text", value: value.slice(last, m.index) });
    let url: string | null = null;
    if (m[1] !== undefined && m[2] !== undefined) url = `/ui/repos/${m[1]}/issues/${m[2]}`;
    else if (m[3] !== undefined && ctx) url = `/ui/repos/${ctx.owner}/${ctx.repo}/issues/${m[3]}`;
    else if (m[4] !== undefined) url = `/ui/${m[4]}`;
    else if (m[5] !== undefined && ctx) url = `/ui/repos/${ctx.owner}/${ctx.repo}/commits/${m[5]}`;
    out.push(url ? { type: "link", url, children: [{ type: "text", value: full }] } : { type: "text", value: full });
    last = m.index + full.length;
  }
  if (last === 0) return [{ type: "text", value }];
  if (last < value.length) out.push({ type: "text", value: value.slice(last) });
  return out;
}

// Plugin factory: remark calls it with the LinkContext as its options argument,
// so it slots into the plugin list as `[remarkGithubRefs, linkContext]`.
function remarkGithubRefs(ctx?: LinkContext) {
  const walk = (node: MdNode) => {
    if (!node.children || node.type === "link" || node.type === "linkReference") return;
    const next: MdNode[] = [];
    for (const child of node.children) {
      if (child.type === "text" && child.value !== undefined) {
        next.push(...tokenizeRefs(child.value, ctx));
      } else {
        if (child.type !== "inlineCode" && child.type !== "code") walk(child);
        next.push(child);
      }
    }
    node.children = next;
  };
  return (tree: MdNode) => {
    walk(tree);
  };
}

// Render internal `/ui/...` links (both authored and autolinked) through the
// router so they navigate in-app instead of triggering a full page load.
function MarkdownLink({ href, children }: ComponentPropsWithoutRef<"a">) {
  if (href && href.startsWith("/ui/")) {
    return <Link to={href}>{children}</Link>;
  }
  return <a href={href}>{children}</a>;
}

export default function Markdown({
  children,
  remarkPlugins,
  components,
  linkContext,
  onToggleTask,
  ...rest
}: Options & { linkContext?: LinkContext; onToggleTask?: ((index: number, checked: boolean) => void) | undefined }) {
  // Every markdown body renders inside a route, so the current :owner/:repo
  // params are the natural reference context for `#123` / SHA autolinks — no
  // caller needs to thread it. An explicit linkContext still wins (cross-repo).
  const params = useParams();
  const ctx: LinkContext | undefined =
    linkContext ?? (params.owner && params.repo ? { owner: params.owner, repo: params.repo } : undefined);
  const input = (props: ComponentPropsWithoutRef<"input">) => (
    <TaskListInput {...props} onToggleTask={onToggleTask} />
  );
  return (
    <ReactMarkdown
      remarkPlugins={remarkPlugins ?? [remarkGfm, remarkGithubAlerts, [remarkGithubRefs, ctx]]}
      components={{ input, a: MarkdownLink, ...components }}
      {...rest}
    >
      {children}
    </ReactMarkdown>
  );
}
