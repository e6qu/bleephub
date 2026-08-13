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

export default function Markdown({
  children,
  remarkPlugins,
  components,
  ...rest
}: Options) {
  return (
    <ReactMarkdown
      remarkPlugins={remarkPlugins ?? [remarkGfm]}
      components={{ input: TaskListInput, ...components }}
      {...rest}
    >
      {children}
    </ReactMarkdown>
  );
}
