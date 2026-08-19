import { Link } from "react-router";
import { repoCodeRoute } from "../routes.js";

const crumbLink = {
  // Small standalone links: inline-block + 1.625rem line height keeps the
  // WCAG 2.5.8 target size without inflating the row.
  display: "inline-block",
  lineHeight: "1.625rem",
  color: "var(--color-accent)",
  textDecoration: "none",
} as const;

/**
 * GitHub's per-segment path breadcrumb for tree/blob/blame views:
 * repo-name / dir / dir / leaf — every ancestor is a link to its tree at the
 * current ref; the final segment is plain text (aria-current).
 */
export function PathBreadcrumbs({
  owner,
  repo,
  gitRef,
  path,
  trailing,
}: {
  owner: string;
  repo: string;
  gitRef: string;
  path: string;
  /** Optional muted suffix after the crumbs (e.g. "· blame on main"). */
  trailing?: React.ReactNode;
}) {
  const segments = path === "" ? [] : path.split("/");
  const sep = <span style={{ color: "var(--color-fg-muted)" }}>/</span>;
  return (
    <nav aria-label="Breadcrumb" className="flex min-w-0 flex-wrap items-center gap-1" style={{ fontSize: ".875rem" }}>
      <Link to={repoCodeRoute(owner, repo, { kind: "tree", ref: gitRef })} style={{ ...crumbLink, fontWeight: 600 }}>
        {repo}
      </Link>
      {segments.map((seg, i) => {
        const isLast = i === segments.length - 1;
        const upto = segments.slice(0, i + 1).join("/");
        return (
          <span key={upto} className="flex items-center gap-1">
            {sep}
            {isLast ? (
              <span aria-current="page" style={{ fontWeight: 600, lineHeight: "1.625rem" }}>{seg}</span>
            ) : (
              <Link to={repoCodeRoute(owner, repo, { kind: "tree", ref: gitRef, path: upto })} style={crumbLink}>
                {seg}
              </Link>
            )}
          </span>
        );
      })}
      {trailing}
    </nav>
  );
}
