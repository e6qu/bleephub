import ReactMarkdown, { type Options } from "react-markdown";
import remarkGfm from "remark-gfm";
import type { ComponentPropsWithoutRef } from "react";

// Shared markdown renderer. Defaults to GitHub-flavored markdown and gives the
// disabled checkboxes that remark-gfm emits for task-list items (`- [ ]`) an
// accessible name — otherwise axe flags them as unlabeled form controls
// (WCAG 4.1.2 / the `label` rule). The list-item text carries the meaning, so
// the label just conveys the checkbox's completed/incomplete state.
function TaskListInput(props: ComponentPropsWithoutRef<"input">) {
  if (props.type === "checkbox") {
    return (
      <input
        {...props}
        aria-label={props.checked ? "completed task" : "incomplete task"}
      />
    );
  }
  return <input {...props} />;
}

// Minimal mdast shapes — just the fields the alert transform reads/writes — so
// this stays free of a direct dependency on the (transitive) mdast typings.
interface MdNode {
  type: string;
  value?: string;
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

export default function Markdown({
  children,
  remarkPlugins,
  components,
  ...rest
}: Options) {
  return (
    <ReactMarkdown
      remarkPlugins={remarkPlugins ?? [remarkGfm, remarkGithubAlerts]}
      components={{ input: TaskListInput, ...components }}
      {...rest}
    >
      {children}
    </ReactMarkdown>
  );
}
