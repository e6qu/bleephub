import { describe, expect, it } from "vitest";
// Vite's `?raw` suffix inlines the route table as text (no node:fs, which this
// tsconfig has no types for, and no jsdom import.meta.url rewriting).
import appSource from "../App.tsx?raw";
import { RESERVED_ROOT_SEGMENTS, matchRepoPath, repoRoute } from "../routes.js";

/**
 * Repositories live at `/ui/{owner}/{repo}` (github.com's `/{owner}/{repo}`,
 * relative to this app's `/ui/` site root). The router disambiguates that from
 * the app's own top-level pages by rank — a static segment outscores a dynamic
 * one — but the global header parses the pathname WITHOUT the router, so it
 * relies on the reserved-word list instead. These pin that the two agree.
 */
describe("repository URL namespace", () => {
  it("keeps the reserved-word list in sync with the app's literal /ui/ routes", () => {
    const literals = new Set<string>();
    for (const match of appSource.matchAll(/path="\/ui\/([^"/]*)/g)) {
      const segment = match[1];
      if (segment && segment !== "*" && !segment.startsWith(":")) literals.add(segment);
    }
    expect(literals.size).toBeGreaterThan(0);
    // Every literal first segment must be reserved, or the header would read
    // e.g. /ui/settings/organizations as the repository "settings/organizations".
    const unreserved = [...literals].filter((s) => !RESERVED_ROOT_SEGMENTS.has(s)).sort();
    expect(unreserved).toEqual([]);
  });

  it("reserves the non-route names the SPA mount still owns", () => {
    // Vite emits hashed assets under the base path, and the sign-in page is
    // handled ahead of the route table — neither appears as a <Route>.
    expect(RESERVED_ROOT_SEGMENTS.has("assets")).toBe(true);
    expect(RESERVED_ROOT_SEGMENTS.has("login")).toBe(true);
  });

  it("matches a repository pathname and decodes its segments", () => {
    expect(matchRepoPath("/ui/acme/api")).toEqual({ owner: "acme", repo: "api" });
    expect(matchRepoPath("/ui/acme/api/pulls/7/files")).toEqual({ owner: "acme", repo: "api" });
    expect(matchRepoPath("/ui/a%20co/my%2Brepo")).toEqual({ owner: "a co", repo: "my+repo" });
  });

  it("never matches a reserved page, a bare profile, or a non-/ui path", () => {
    expect(matchRepoPath("/ui/settings/organizations")).toBeNull();
    expect(matchRepoPath("/ui/orgs/acme")).toBeNull();
    expect(matchRepoPath("/ui/operations/audit-log")).toBeNull();
    expect(matchRepoPath("/ui/users/admin")).toBeNull();
    expect(matchRepoPath("/ui/admin")).toBeNull();
    expect(matchRepoPath("/ui/")).toBeNull();
    expect(matchRepoPath("/api/v3/repos/acme/api")).toBeNull();
  });

  it("round-trips repoRoute through matchRepoPath, escaping unsafe names", () => {
    expect(repoRoute("acme", "api")).toBe("/ui/acme/api");
    expect(matchRepoPath(repoRoute("a co", "my+repo"))).toEqual({ owner: "a co", repo: "my+repo" });
  });
});
