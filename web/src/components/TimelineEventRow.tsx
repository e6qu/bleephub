import type { ReactNode } from "react";
import type { GithubTimelineItem } from "../types.js";
import { DotFillIcon } from "./octicons.js";

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
      <DotFillIcon size={14} style={{ marginTop: "0.15rem", color: "var(--color-fg-subtle)" }} />
      <span className="min-w-0 flex-1">
        <span style={{ color: "var(--color-fg)", fontWeight: 600 }}>{actor}</span> {text}
        {when && <span> · {new Date(when).toLocaleString()}</span>}
      </span>
    </div>
  );
}
