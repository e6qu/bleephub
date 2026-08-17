import type { ReactNode } from "react";
import type { GithubTimelineItem } from "../types.js";
import {
  DotFillIcon,
  EyeIcon,
  TagIcon,
  PeopleIcon,
  IssueClosedIcon,
  IssueOpenedIcon,
  MergedIcon,
  LockIcon,
  CommentIcon,
} from "./octicons.js";

// github.com renders each timeline event with a distinct octicon in the gutter;
// mirror that instead of a single generic dot. Unmapped events keep the dot.
function eventIcon(event: string): ReactNode {
  const props = { size: 14, style: { marginTop: "0.15rem", color: "var(--color-fg-subtle)" } };
  switch (event) {
    case "reviewed":
      return <EyeIcon {...props} />;
    case "labeled":
    case "unlabeled":
      return <TagIcon {...props} />;
    case "assigned":
    case "unassigned":
      return <PeopleIcon {...props} />;
    case "closed":
      return <IssueClosedIcon {...props} />;
    case "reopened":
      return <IssueOpenedIcon {...props} />;
    case "merged":
      return <MergedIcon {...props} />;
    case "locked":
    case "unlocked":
      return <LockIcon {...props} />;
    case "cross-referenced":
    case "commented":
      return <CommentIcon {...props} />;
    default:
      return <DotFillIcon {...props} />;
  }
}

export function reviewStateText(state: string): string {
  switch (state) {
    case "APPROVED":
      return "approved these changes";
    case "CHANGES_REQUESTED":
      return "requested changes";
    case "DISMISSED":
      return "reviewed (dismissed)";
    default:
      return "reviewed";
  }
}

// A single non-comment timeline event (labeled / assigned / renamed / reviewed /
// closed / …) as github.com renders it in an issue or PR conversation. Shared by
// the issue and pull-request conversations so both interleave events identically.
export function TimelineEventRow({ item }: { item: GithubTimelineItem }) {
  const actor = item.actor?.login ?? item.user?.login;
  const when = item.created_at ?? item.submitted_at ?? null;

  let text: ReactNode;
  switch (item.event) {
    case "reviewed":
      text = <>{reviewStateText(item.state ?? "")}</>;
      break;
    case "labeled":
    case "unlabeled":
      text = (
        <>
          {item.event === "labeled" ? "added the" : "removed the"}{" "}
          {item.label ? (
            <span
              className="font-mono"
              style={{
                border: `1px solid #${item.label.color}`,
                borderRadius: "2rem",
                padding: "0 0.45rem",
                fontSize: "0.74rem",
              }}
            >
              {item.label.name}
            </span>
          ) : (
            "unknown"
          )}{" "}
          label
        </>
      );
      break;
    case "assigned":
    case "unassigned":
      text = (
        <>
          {item.event === "assigned" ? "assigned" : "unassigned"}{" "}
          <strong>{item.assignee?.login ?? "unknown"}</strong>
        </>
      );
      break;
    case "renamed":
      text = (
        <>
          changed the title from <em>{item.rename?.from}</em> to <em>{item.rename?.to}</em>
        </>
      );
      break;
    case "cross-referenced": {
      const src = item.source?.issue;
      text = src ? (
        <>
          mentioned this in{" "}
          <strong>
            #{src.number} {src.title}
          </strong>
        </>
      ) : (
        <>mentioned this</>
      );
      break;
    }
    default:
      // Render unrecognised events honestly by their wire name.
      text = <>{item.event.replaceAll("_", " ")}</>;
  }

  return (
    <div
      className="flex items-start gap-2"
      style={{ padding: "0.35rem 0.25rem", fontSize: "0.82rem", color: "var(--color-fg-muted)" }}
    >
      {eventIcon(item.event)}
      <span className="min-w-0 flex-1">
        <span style={{ color: "var(--color-fg)", fontWeight: 600 }}>{actor}</span> {text}
        {when && <span> · {new Date(when).toLocaleString()}</span>}
      </span>
    </div>
  );
}
