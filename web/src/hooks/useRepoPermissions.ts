import { useQuery } from "@tanstack/react-query";
import { fetchRepoDetail } from "../api.js";

export interface RepoPermissions {
  /** Repo settings, webhooks, branch protection, danger zone. */
  isAdmin: boolean;
  /** Write affordances: files, merges, releases, labels, triage actions. */
  canPush: boolean;
  /** False only while the repo payload is still loading. */
  loaded: boolean;
}

// Viewer-scoped permissions from the repo payload's `permissions` block. Pages
// gate privileged controls on this so they don't render buttons that only 403.
// The repo query is cache-seeded by the bootstrap aggregates.
export function useRepoPermissions(owner: string, repo: string): RepoPermissions {
  const q = useQuery({
    queryKey: ["repo", owner, repo],
    queryFn: () => fetchRepoDetail(owner, repo),
  });
  const perms = q.data?.permissions;
  return {
    isAdmin: perms?.admin === true,
    canPush: perms?.push === true,
    loaded: q.data !== undefined,
  };
}
