import { useQuery } from "@tanstack/react-query";
import { fetchRepoIssuesPage, fetchRepoPRsPage } from "../api.js";
import type { GithubIssue } from "../types.js";

/**
 * Open-issue / open-PR counts for the repo tab badges. The list endpoints
 * paginate, so a further page (Link rel="next") shows "N+" rather than a
 * wrong exact count.
 */
export function useOpenCounts(
  owner: string,
  repo: string,
): { issueCount?: number | string | undefined; prCount?: number | string | undefined } {
  const { data: issuePage } = useQuery({
    queryKey: ["issues", owner, repo, "open", "count"],
    queryFn: ({ signal }) => fetchRepoIssuesPage(owner, repo, "open", undefined, signal),
    enabled: !!owner && !!repo,
  });
  const { data: prPage } = useQuery({
    queryKey: ["prs", owner, repo, "open", "count"],
    queryFn: ({ signal }) => fetchRepoPRsPage(owner, repo, "open", undefined, signal),
    enabled: !!owner && !!repo,
  });
  const badge = (page?: { items: unknown[]; nextUrl: string | null }) => {
    if (!page) return undefined;
    return page.nextUrl ? `${page.items.length}+` : page.items.length;
  };
  // The /issues endpoint returns issues AND pull requests (GitHub models a PR as
  // an issue), so the Issues badge must count only real issues — matching
  // IssuesPage's isRealIssue filter. The /pulls endpoint returns only PRs, so
  // the PR badge counts its page directly.
  const issueBadge = (page?: { items: GithubIssue[]; nextUrl: string | null }) => {
    if (!page) return undefined;
    const n = page.items.filter((i) => !i.pull_request).length;
    return page.nextUrl ? `${n}+` : n;
  };
  return { issueCount: issueBadge(issuePage), prCount: badge(prPage) };
}
