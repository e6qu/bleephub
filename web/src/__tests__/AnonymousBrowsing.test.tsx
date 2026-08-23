import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, cleanup, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { ToastProvider } from "@bleephub/ui-core/components";
import { App } from "../App.js";
import { AppHeader } from "../components/AppHeader.js";
import { RepoHeader } from "../components/PageHeader.js";
import { IssuesPage } from "../pages/IssuesPage.js";
import { SessionContext } from "../session.js";
import { fetchBrowserSession, isLoggedIn } from "../api.js";

// Signed-out App renders: the probe is mocked per test; everything else in
// api.js stays real and goes through the fetch mock below.
vi.mock("../api.js", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api.js")>();
  return { ...actual, isLoggedIn: vi.fn(() => false), fetchBrowserSession: vi.fn() };
});

const mockedProbe = vi.mocked(fetchBrowserSession);
const mockedIsLoggedIn = vi.mocked(isLoggedIn);

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

beforeEach(() => {
  mockedIsLoggedIn.mockReturnValue(false);
  mockedProbe.mockResolvedValue(false);
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: vi.fn(() => ({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  });
});

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
  vi.clearAllMocks();
});

/** URLs of every request the page fired. */
function requestedUrls(): string[] {
  return mockFetch.mock.calls.map((c) => String(c[0]));
}

/** Matches viewer-scoped endpoints that 401 for an anonymous visitor. */
const VIEWER_SCOPED = [
  /\/api\/v3\/user(\/|\?|$)/, // /api/v3/user + /user/... (NOT /users/...)
  /\/api\/v3\/notifications/,
  /\/api\/graphql/,
  /\/ui-data\/repos\/[^/]+\/[^/]+\/viewer/,
  /\/ui-data\/notifications/,
];

function expectNoViewerScopedRequests() {
  const offenders = requestedUrls().filter((u) => VIEWER_SCOPED.some((re) => re.test(u)));
  expect(offenders).toEqual([]);
}

const publicRepo = {
  id: 1,
  name: "test",
  full_name: "admin/test",
  description: "a repo",
  default_branch: "main",
  visibility: "public",
  private: false,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
  pushed_at: "2026-01-02T00:00:00Z",
  stargazers_count: 5,
  subscribers_count: 2,
  forks_count: 1,
  owner: { login: "admin", type: "User" },
  // No `permissions` block: the server omits it for anonymous reads, and the
  // role-gating hooks treat that as no-push/no-admin.
};

/** Anonymous-shaped fetch mock: public reads answer, nothing else exists. */
function mockAnonymousRepoServer() {
  mockFetch.mockImplementation((url: RequestInfo | URL) => {
    const u = String(url);
    if (u.includes("/ui-data/bootstrap/")) {
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    }
    if (/\/api\/v3\/repos\/admin\/test(\?|$)/.test(u)) {
      return Promise.resolve(jsonResponse(publicRepo));
    }
    if (u.includes("/readme")) {
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    }
    if (u.includes("/topics")) return Promise.resolve(jsonResponse({ names: [] }));
    if (u.includes("/languages")) return Promise.resolve(jsonResponse({}));
    if (u.includes("/auth/providers")) return Promise.resolve(jsonResponse({}));
    return Promise.resolve(jsonResponse([]));
  });
}

describe("anonymous browsing (App-level)", () => {
  it("renders a public repo page signed-out with a Sign in header and no viewer-scoped fetches", async () => {
    mockAnonymousRepoServer();
    window.history.pushState({}, "", "/ui/admin/test");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <App />
      </QueryClientProvider>,
    );

    // The shell renders with the anonymous header…
    const signIn = await screen.findByRole("link", { name: "Sign in" });
    expect(signIn.getAttribute("href")).toBe(
      `/ui/login?return_to=${encodeURIComponent("/ui/admin/test")}`,
    );
    // …and none of the signed-in chrome.
    expect(screen.queryByLabelText(/notifications/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Create new…" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open user menu" })).not.toBeInTheDocument();

    // The repo page itself renders (lazy chunk + queries settle).
    await screen.findByText("a repo", undefined, { timeout: 10_000 });

    // Star/Watch/Fork render as sign-in links, not mutation buttons. (The
    // page body also links to the stargazers list, so match on the header
    // action that points at sign-in.)
    const starLinks = await screen.findAllByRole("link", { name: /star/i });
    expect(
      starLinks.some((l) => (l.getAttribute("href") ?? "").includes("/ui/login?return_to=")),
    ).toBe(true);
    expect(screen.queryByRole("button", { name: /unstar|^star/i })).not.toBeInTheDocument();

    // An anonymous page must fire ZERO viewer-scoped (401ing) requests.
    expectNoViewerScopedRequests();
  });

  it("redirects a signed-out visitor from a viewer-scoped route to sign-in", async () => {
    mockAnonymousRepoServer();
    window.history.pushState({}, "", "/ui/notifications");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <App />
      </QueryClientProvider>,
    );

    await waitFor(() => {
      expect(window.location.pathname).toBe("/ui/login");
    });
    expect(new URLSearchParams(window.location.search).get("return_to")).toBe("/ui/notifications");
    expectNoViewerScopedRequests();
  });
});

describe("anonymous AppHeader", () => {
  function renderHeader(signedIn: boolean) {
    mockFetch.mockImplementation(() => Promise.resolve(jsonResponse([])));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <SessionContext.Provider value={signedIn}>
            <MemoryRouter initialEntries={["/ui/admin/test"]}>
              <AppHeader />
            </MemoryRouter>
          </SessionContext.Provider>
        </ToastProvider>
      </QueryClientProvider>,
    );
  }

  it("renders logo + search + Sign in only, and never polls notifications", async () => {
    renderHeader(false);

    expect(screen.getByRole("link", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Search or jump to…")).toBeInTheDocument();
    expect(screen.queryByLabelText(/notifications/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Create new…" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open user menu" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Open global navigation" })).not.toBeInTheDocument();

    // Give any (wrongly) enabled query a tick to fire, then assert silence.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expectNoViewerScopedRequests();
  });

  it("keeps the signed-in chrome when a session exists (regression)", async () => {
    renderHeader(true);
    expect(await screen.findByLabelText(/notifications/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create new…" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Sign in" })).not.toBeInTheDocument();
  });
});

describe("anonymous RepoHeader", () => {
  it("renders Watch/Fork/Star as sign-in links with counts and no viewer-state fetch", async () => {
    mockAnonymousRepoServer();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <SessionContext.Provider value={false}>
          <MemoryRouter initialEntries={["/ui/admin/test"]}>
            <Routes>
              <Route
                path="/ui/:owner/:repo"
                element={<RepoHeader owner="admin" repo="test" active="code" />}
              />
            </Routes>
          </MemoryRouter>
        </SessionContext.Provider>
      </QueryClientProvider>,
    );

    const star = await screen.findByRole("link", { name: /star/i });
    expect(star.getAttribute("href")).toContain("/ui/login?return_to=");
    expect(screen.getByRole("link", { name: /watch/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /fork/i })).toBeInTheDocument();
    // The counts still render from the public social counters.
    await screen.findByText("5");

    await new Promise((resolve) => setTimeout(resolve, 50));
    expectNoViewerScopedRequests();
  });
});

describe("anonymous issue detail", () => {
  it("shows a sign-in box instead of the comment composer and fires no viewer-scoped requests", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = String(url);
      if (u.includes("/ui-data/bootstrap/")) {
        return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
      }
      if (u.includes("/issues/7/comments") || u.includes("/timeline")) {
        return Promise.resolve(jsonResponse([]));
      }
      if (u.includes("/issues/7/reactions")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/issues/7")) {
        return Promise.resolve(
          jsonResponse({
            id: 7,
            node_id: "I_kwDO00000007",
            number: 7,
            title: "A public issue",
            body: "body",
            state: "open",
            user: { login: "admin", avatar_url: "" },
            labels: [],
            assignees: [],
            comments: 0,
            created_at: "2026-01-01T00:00:00Z",
            updated_at: "2026-01-01T00:00:00Z",
            closed_at: null,
          }),
        );
      }
      if (/\/api\/v3\/repos\/admin\/test(\?|$)/.test(u)) {
        return Promise.resolve(jsonResponse(publicRepo));
      }
      return Promise.resolve(jsonResponse([]));
    });

    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <SessionContext.Provider value={false}>
          <MemoryRouter initialEntries={["/ui/admin/test/issues/7"]}>
            <Routes>
              <Route path="/ui/:owner/:repo/issues/:number" element={<IssuesPage />} />
            </Routes>
          </MemoryRouter>
        </SessionContext.Provider>
      </QueryClientProvider>,
    );

    await screen.findByText("A public issue");
    // The composer is replaced by GitHub's signed-out box…
    expect(await screen.findByText(/to comment/i)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.queryByLabelText(/comment/i)).not.toBeInTheDocument();

    await new Promise((resolve) => setTimeout(resolve, 50));
    expectNoViewerScopedRequests();
  });
});
