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

/**
 * Viewer-scoped repository permissions from the repo payload's `permissions`
 * block ({admin, push, pull} — the store's capability lattice has no separate
 * triage/maintain tiers). github.com hides what the viewer cannot do, so
 * pages gate their privileged controls on this instead of rendering buttons
 * that would only 403. The repo query is cache-seeded by the bootstrap
 * aggregates, so this is a cache hit on every repo page.
 */
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
