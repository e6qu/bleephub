export type AccountType = "User" | "Organization" | string;

const segment = (value: string) => encodeURIComponent(value);
const filePath = (value: string) => value.split("/").map(segment).join("/");

export function accountRoute(login: string, type: AccountType): string {
  return type === "Organization"
    ? `/ui/orgs/${segment(login)}`
    : `/ui/${segment(login)}`;
}

/**
 * github.com serves a repository at `/{owner}/{repo}`, one segment below the
 * site root, and disambiguates it from its own top-level pages with a reserved
 * word list ("settings", "notifications", "explore", …) that no account may
 * claim. Bleephub's SPA is mounted at `/ui/` (Vite `base`), so its site root is
 * `/ui/` and a repository lives at `/ui/{owner}/{repo}` — the same shape, one
 * mount point down.
 *
 * The router itself needs no help: React Router ranks a static segment above a
 * dynamic one, so `/ui/gists/:id` always beats `/ui/:owner/:repo`. This list
 * exists for the consumers that parse the pathname *without* the router — the
 * global header's repo scope and its owner/repo breadcrumb — which would
 * otherwise read `/ui/settings/organizations` as the repository
 * "settings/organizations".
 *
 * Every literal first segment registered under `/ui/` in App.tsx appears here,
 * plus `assets` (Vite's hashed output directory, served by the Go SPA handler
 * and never routed).
 */
export const RESERVED_ROOT_SEGMENTS: ReadonlySet<string> = new Set([
  "account",
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
  "users",
  "workflows",
]);

/**
 * The owner/repo a `/ui/...` pathname addresses, or null when it addresses one
 * of the app's own top-level pages. Mirrors the `/ui/:owner/:repo` route.
 */
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
