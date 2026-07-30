export type AccountType = "User" | "Organization" | string;

const segment = (value: string) => encodeURIComponent(value);
const filePath = (value: string) => value.split("/").map(segment).join("/");

export function accountRoute(login: string, type: AccountType): string {
  return type === "Organization"
    ? `/ui/orgs/${segment(login)}`
    : `/ui/${segment(login)}`;
}

export function repoRoute(owner: string, repo: string): string {
  return `/ui/repos/${segment(owner)}/${segment(repo)}`;
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
