import type { ReactNode } from "react";
import type { GithubTimelineItem } from "../types.js";
import { Avatar } from "./Avatar.js";
import { RelativeTime } from "./RelativeTime.js";
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
  RepoIcon,
  BranchIcon,
} from "./octicons.js";

const iconProps = { size: 14, style: { marginTop: "0.15rem", color: "var(--color-fg-subtle)" } };

// Octicons the shared icon set lacks; local to the timeline rows.
function GitCommitIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" width={14} height={14} fill="currentColor" style={iconProps.style}>
      <path d="M11.93 8.5a4.002 4.002 0 0 1-7.86 0H.75a.75.75 0 0 1 0-1.5h3.32a4.002 4.002 0 0 1 7.86 0h3.32a.75.75 0 0 1 0 1.5Zm-1.43-.75a2.5 2.5 0 1 0-5 0 2.5 2.5 0 0 0 5 0Z" />
    </svg>
  );
}

function PinIcon() {
  return (
    <svg aria-hidden="true" viewBox="0 0 16 16" width={14} height={14} fill="currentColor" style={iconProps.style}>
      <path d="M4.456.734a1.75 1.75 0 0 1 2.826.504l.613 1.327a3.081 3.081 0 0 0 2.084 1.707l2.454.584c1.332.317 1.8 1.972.832 2.94L11.06 10l3.72 3.72a.748.748 0 0 1-.332 1.265.75.75 0 0 1-.729-.205L10 11.06l-2.204 2.205c-.968.968-2.623.5-2.94-.832l-.584-2.454a3.081 3.081 0 0 0-1.707-2.084l-1.327-.613a1.75 1.75 0 0 1-.504-2.826Z" />
    </svg>
  );
}

// github.com renders each timeline event with a distinct octicon in the gutter;
// mirror that instead of a single generic dot. Unmapped events keep the dot.
function eventIcon(event: string): ReactNode {
  switch (event) {
    case "reviewed":
      return <EyeIcon {...iconProps} />;
    case "labeled":
    case "unlabeled":
      return <TagIcon {...iconProps} />;
    case "assigned":
    case "unassigned":
      return <PeopleIcon {...iconProps} />;
    case "closed":
      return <IssueClosedIcon {...iconProps} />;
    case "reopened":
      return <IssueOpenedIcon {...iconProps} />;
    case "merged":
      return <MergedIcon {...iconProps} />;
    case "locked":
    case "unlocked":
      return <LockIcon {...iconProps} />;
    case "cross-referenced":
    case "commented":
      return <CommentIcon {...iconProps} />;
    case "committed":
      return <GitCommitIcon />;
    case "head_ref_force_pushed":
    case "head_ref_deleted":
    case "head_ref_restored":
      return <BranchIcon {...iconProps} />;
    case "transferred":
      return <RepoIcon {...iconProps} />;
    case "pinned":
    case "unpinned":
      return <PinIcon />;
    default:
      return <DotFillIcon {...iconProps} />;
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

const shortSha = (sha: string) => sha.slice(0, 7);

/** A short-sha rendered as a link when the event carries a web URL. Small
 * link targets get inline-block + tall line-height for WCAG target size. */
function ShaRef({ sha, href }: { sha: string; href?: string | undefined }) {
  if (!href) {
    return <code style={{ fontSize: "0.78rem" }}>{shortSha(sha)}</code>;
  }
  return (
    <a
      href={href}
      className="inline-block"
      style={{
        color: "var(--color-accent)",
        textDecoration: "none",
        fontFamily: "var(--font-mono)",
        fontSize: "0.78rem",
        lineHeight: "1.625rem",
      }}
    >
      {shortSha(sha)}
    </a>
  );
}

// A single non-comment timeline event (labeled / assigned / renamed / reviewed /
// closed / …) as github.com renders it in an issue or PR conversation. Shared by
// the issue and pull-request conversations so both interleave events identically.
export function TimelineEventRow({ item }: { item: GithubTimelineItem }) {
  // "committed" rows have a git author instead of an account actor.
  const actorAccount = item.actor ?? item.user ?? null;
  const actor = actorAccount?.login ?? (item.event === "committed" ? item.author?.name : undefined);
  const when = item.created_at ?? item.submitted_at ?? (item.event === "committed" ? item.author?.date : null) ?? null;

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
    case "committed": {
      // First line of the commit message, the way github.com shows it.
      const subject = (item.message ?? "").split("\n")[0] ?? "";
      text = (
        <>
          added a commit{subject ? <> — {subject}</> : null}
          {item.sha && (
            <>
              {" "}
              <ShaRef sha={item.sha} href={item.html_url} />
            </>
          )}
        </>
      );
      break;
    }
    case "head_ref_force_pushed": {
      const before = item.before ?? null;
      const after = item.after ?? item.commit_id ?? null;
      text = (
        <>
          force-pushed the head branch
          {before && after ? (
            <>
              {" "}
              from <ShaRef sha={before} /> to <ShaRef sha={after} href={item.html_url} />
            </>
          ) : after ? (
            <>
              {" "}
              to <ShaRef sha={after} href={item.html_url} />
            </>
          ) : null}
        </>
      );
      break;
    }
    case "head_ref_deleted":
      text = <>deleted the head branch</>;
      break;
    case "head_ref_restored":
      text = <>restored the head branch</>;
      break;
    case "transferred":
      text = <>transferred this issue</>;
      break;
    case "pinned":
      text = <>pinned this issue</>;
      break;
    case "unpinned":
      text = <>unpinned this issue</>;
      break;
    case "locked":
      text = (
        <>
          locked{item.lock_reason ? <> as <strong>{item.lock_reason}</strong></> : null} and limited
          conversation to collaborators
        </>
      );
      break;
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
        {actorAccount?.login && (
          <span className="mr-1 inline-flex align-middle">
            <Avatar login={actorAccount.login} src={actorAccount.avatar_url} size={16} />
          </span>
        )}
        <span style={{ color: "var(--color-fg)", fontWeight: 600 }}>{actor}</span> {text}
        {when && (
          <span>
            {" "}
            · <RelativeTime iso={when} />
          </span>
        )}
      </span>
    </div>
  );
}
