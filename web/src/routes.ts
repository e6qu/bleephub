export type AccountType = "User" | "Organization" | string;

const segment = (value: string) => encodeURIComponent(value);
const filePath = (value: string) => value.split("/").map(segment).join("/");

export function accountRoute(login: string, type: AccountType): string {
  return type === "Organization"
    ? `/ui/orgs/${segment(login)}`
    : `/ui/${segment(login)}`;
}

// Reserved first segments that no account may claim, so consumers parsing the
// pathname without the router (header repo scope, owner/repo breadcrumb) don't
// read `/ui/settings/organizations` as a repository. Every literal first segment
// routed under `/ui/` in App.tsx appears here, plus `assets` (Vite output dir).
export const RESERVED_ROOT_SEGMENTS: ReadonlySet<string> = new Set([
  "account",
  "advisories",
  "apps",
  "assets",
  "classrooms",
  "codespaces",
  "copilot",
  "gists",
  "login",
  "marketplace",
  "metrics",
  "migrations",
  "notifications",
  "oauth",
  "operations",
  "orgs",
  "packages",
  "repos",
  "runners",
  "search",
  "settings",
  "sponsors",
  "users",
  "workflows",
]);

// owner/repo a `/ui/...` pathname addresses, or null for the app's own top-level pages.
export function matchRepoPath(pathname: string): { owner: string; repo: string } | null {
  const matched = /^\/ui\/([^/]+)\/([^/]+)/.exec(pathname);
  if (!matched) return null;
  const owner = decodeURIComponent(matched[1]!);
  if (RESERVED_ROOT_SEGMENTS.has(owner)) return null;
  return { owner, repo: decodeURIComponent(matched[2]!) };
}

export function repoRoute(owner: string, repo: string): string {
  return `/ui/${segment(owner)}/${segment(repo)}`;
}

export type RepoCodeDestination =
  | { kind: "root" }
  | { kind: "commits" }
  | { kind: "branches" }
  | { kind: "tags" }
  | { kind: "compare"; base: string; head: string }
  | { kind: "tree"; ref: string; path?: string }
  | { kind: "blob"; ref: string; path: string };

export function repoCodeRoute(
  owner: string,
  repo: string,
  destination: RepoCodeDestination,
): string {
  const base = repoRoute(owner, repo);
  switch (destination.kind) {
    case "root":
      return base;
    case "commits":
      return `${base}/commits`;
    case "branches":
      return `${base}/branches`;
    case "tags":
      return `${base}/tags`;
    case "compare":
      return `${base}/compare/${segment(destination.base)}...${segment(destination.head)}`;
    case "tree":
      return `${base}/tree/${segment(destination.ref)}${
        destination.path ? `/${filePath(destination.path)}` : ""
      }`;
    case "blob":
      return `${base}/blob/${segment(destination.ref)}/${filePath(destination.path)}`;
  }
}
