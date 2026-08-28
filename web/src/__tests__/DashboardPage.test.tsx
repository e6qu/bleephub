import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { DashboardPage } from "../pages/DashboardPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/ui/"]}>
        <Routes>
          <Route path="/ui/" element={<DashboardPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const feedIssue = {
  id: 10,
  number: 7,
  title: "Flaky runner timeout",
  state: "open",
  comments: 2,
  updated_at: "2026-02-01T00:00:00Z",
  html_url: "http://x/acme/api/issues/7",
  repository: { full_name: "acme/api", name: "api", owner: { login: "acme" } },
};

const repo = {
  id: 1,
  name: "api",
  full_name: "acme/api",
  description: "the api",
  default_branch: "main",
  visibility: "public",
  private: false,
  updated_at: "2026-02-01T00:00:00Z",
};

describe("DashboardPage", () => {
  it("renders the top repositories rail and the activity feed", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/api/v3/user/repos")) return Promise.resolve(jsonResponse([repo], 200, { Link: "" }));
      if (u.includes("/received_events"))
        return Promise.resolve(jsonResponse([{ id: "e1", type: "PushEvent", created_at: "2026-02-01T00:00:00Z", actor: { login: "hubot" }, repo: { name: "acme/api" }, payload: { size: 2 } }]));
      if (u.includes("/api/v3/issues")) return Promise.resolve(jsonResponse([feedIssue]));
      if (u.includes("/api/v3/user")) return Promise.resolve(jsonResponse({ id: 1, login: "octocat", type: "User", site_admin: true, created_at: "2026-01-01T00:00:00Z" }));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();

    await waitFor(() => {
      expect(screen.getByText("Flaky runner timeout")).toBeInTheDocument();
    });
    expect(screen.getByText("Following")).toBeInTheDocument();
    expect(await screen.findByText("hubot")).toBeInTheDocument();
    expect(screen.getByText(/pushed 2 commits to/i)).toBeInTheDocument();
    expect(screen.getByText("Your issues")).toBeInTheDocument();
    expect(screen.getByText("System status")).toBeInTheDocument();
  });

  it("shows an honest empty state when there is no activity", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/api/v3/user/repos")) return Promise.resolve(jsonResponse([], 200, { Link: "" }));
      if (u.includes("/received_events")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/api/v3/issues")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/api/v3/user")) return Promise.resolve(jsonResponse({ id: 1, login: "octocat", type: "User", site_admin: true, created_at: "2026-01-01T00:00:00Z" }));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();

    await waitFor(() => {
      expect(screen.getByText(/your feed is quiet/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/no open issues/i)).toBeInTheDocument();
  });

  it("filters the top repositories client-side and shows more via the Link header", async () => {
    const page1 = Array.from({ length: 30 }, (_, i) => ({
      ...repo,
      id: i + 1,
      name: `svc-${i}`,
      full_name: `acme/svc-${i}`,
    }));
    const page2 = [{ ...repo, id: 999, name: "extra", full_name: "acme/extra" }];
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/api/v3/user/repos")) {
        if (u.includes("page=2")) return Promise.resolve(jsonResponse(page2));
        return Promise.resolve(
          jsonResponse(page1, 200, {
            Link: '</api/v3/user/repos?per_page=30&sort=pushed&page=2>; rel="next", </api/v3/user/repos?per_page=30&sort=pushed&page=2>; rel="last"',
          }),
        );
      }
      if (u.includes("/received_events")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/api/v3/issues")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/api/v3/user")) return Promise.resolve(jsonResponse({ id: 1, login: "octocat", type: "User", site_admin: true, created_at: "2026-01-01T00:00:00Z" }));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("acme/svc-0")).toBeInTheDocument());
    // Only the first 8 show before expanding.
    expect(screen.queryByText("acme/svc-9")).toBeNull();

    // Filter runs client-side over the already-fetched list.
    fireEvent.change(screen.getByLabelText("Find a repository"), { target: { value: "svc-12" } });
    expect(screen.getByText("acme/svc-12")).toBeInTheDocument();
    expect(screen.queryByText("acme/svc-0")).toBeNull();
    fireEvent.change(screen.getByLabelText("Find a repository"), { target: { value: "zzz" } });
    expect(screen.getByText(/No repositories match/)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("Find a repository"), { target: { value: "" } });

    // Show more expands the slice and follows the Link header's next page.
    const showMore = screen.getByRole("button", { name: "Show more" });
    fireEvent.click(showMore); // 16 visible
    expect(screen.getByText("acme/svc-9")).toBeInTheDocument();
    fireEvent.click(showMore); // 24 visible
    fireEvent.click(showMore); // 32 → fetches page 2
    await waitFor(() => expect(screen.getByText("acme/extra")).toBeInTheDocument());
    expect(
      mockFetch.mock.calls.some(([u]) => String(u).includes("/api/v3/user/repos") && String(u).includes("page=2")),
    ).toBe(true);
  });

  it("surfaces a feed error instead of swallowing it", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/api/v3/user/repos")) return Promise.resolve(jsonResponse([], 200, { Link: "" }));
      if (u.includes("/received_events")) return Promise.resolve(jsonResponse({ message: "boom" }, 500));
      if (u.includes("/api/v3/issues")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/api/v3/user")) return Promise.resolve(jsonResponse({ id: 1, login: "octocat", type: "User", site_admin: true, created_at: "2026-01-01T00:00:00Z" }));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();

    await waitFor(() => {
      expect(screen.getByText(/failed to load activity feed/i)).toBeInTheDocument();
    });
  });
});
