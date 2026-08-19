import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";

vi.mock("../components/Shell.js", () => ({
  RepoHeader: ({ owner, repo }: { owner: string; repo: string }) => (
    <div>
      {owner}/{repo}
    </div>
  ),
}));
vi.mock("../hooks/useOpenCounts.js", () => ({ useOpenCounts: () => ({}) }));

import { WikiPage } from "../pages/WikiPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderAt(path: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/repos/:owner/:repo/wiki" element={<WikiPage />} />
          <Route path="/ui/repos/:owner/:repo/wiki/:slug" element={<WikiPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const home = {
  slug: "home",
  title: "Home",
  body: "# Welcome\n\nHello wiki",
  author: "admin",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-02-01T00:00:00Z",
};

describe("WikiPage", () => {
  it("lists pages and renders the active page markdown", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/wiki/pages")) return Promise.resolve(jsonResponse([home]));
      if (u.endsWith("/wiki/pages/home")) return Promise.resolve(jsonResponse(home));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/hello/wiki");

    // sidebar link + rendered markdown heading
    expect(await screen.findByRole("link", { name: "Home" })).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Welcome" })).toBeInTheDocument();
    expect(screen.getByText("Hello wiki")).toBeInTheDocument();
  });

  it("shows the create-first-page blankslate for an empty wiki", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/wiki/pages")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/hello/wiki");
    expect(
      await screen.findByRole("button", { name: /create the first page/i }),
    ).toBeInTheDocument();
  });

  it("creates a page via PUT /wiki/pages/{slug} with the derived slug", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/wiki/pages/getting-started") && init?.method === "PUT") {
        return Promise.resolve(
          jsonResponse({ ...home, slug: "getting-started", title: "Getting Started" }, 201),
        );
      }
      if (u.endsWith("/wiki/pages")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/hello/wiki");

    fireEvent.click(await screen.findByRole("button", { name: /create the first page/i }));
    fireEvent.change(await screen.findByLabelText(/wiki page title/i), {
      target: { value: "Getting Started" },
    });
    fireEvent.change(screen.getByLabelText(/wiki page content/i), {
      target: { value: "Some content" },
    });
    fireEvent.click(screen.getByRole("button", { name: /save page/i }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) =>
          c[0].toString().endsWith("/wiki/pages/getting-started") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
    });
  });

  it("sends the edit summary through the save payload", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/wiki/pages/home") && init?.method === "PUT") {
        return Promise.resolve(jsonResponse(home));
      }
      if (u.endsWith("/wiki/pages")) return Promise.resolve(jsonResponse([home]));
      if (u.endsWith("/wiki/pages/home")) return Promise.resolve(jsonResponse(home));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/hello/wiki/home");

    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(await screen.findByLabelText(/wiki edit message/i), {
      target: { value: "clarify intro" },
    });
    fireEvent.click(screen.getByRole("button", { name: /save page/i }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/wiki/pages/home") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse(String((put![1] as RequestInit).body))).toEqual({
        title: "Home",
        body: home.body,
        message: "clarify intro",
      });
    });
  });

  it("lists page revisions and restores an old version with a Restore message", async () => {
    const revisions = [
      { id: 3, slug: "home", title: "Home", editor: "admin", message: "tweak wording", created_at: "2026-03-01T00:00:00Z", body_preview: "# Welcome v3" },
      { id: 2, slug: "home", title: "Home", editor: "octo", message: "", created_at: "2026-02-01T00:00:00Z", body_preview: "# Welcome v2" },
    ];
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/wiki/pages/home/revisions/2")) {
        return Promise.resolve(jsonResponse({ ...revisions[1], body: "# Welcome v2\n\nold body" }));
      }
      if (u.endsWith("/wiki/pages/home/revisions")) return Promise.resolve(jsonResponse(revisions));
      if (u.endsWith("/wiki/pages/home") && init?.method === "PUT") {
        return Promise.resolve(jsonResponse(home));
      }
      if (u.endsWith("/wiki/pages")) return Promise.resolve(jsonResponse([home]));
      if (u.endsWith("/wiki/pages/home")) return Promise.resolve(jsonResponse(home));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/hello/wiki/home");

    fireEvent.click(await screen.findByRole("button", { name: "History" }));
    // Newest-first rows with message, editor and a relative <time>.
    expect(await screen.findByText("tweak wording")).toBeInTheDocument();
    expect(screen.getByText("(no edit summary)")).toBeInTheDocument();
    expect(screen.getByText(/octo ·/)).toBeInTheDocument();
    expect(screen.getByText("# Welcome v2")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Restore revision 2" }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/wiki/pages/home") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse(String((put![1] as RequestInit).body))).toEqual({
        title: "Home",
        body: "# Welcome v2\n\nold body",
        message: "Restore revision 2",
      });
    });
  });
});
