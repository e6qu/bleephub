import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { PackagesPage } from "../pages/PackagesPage.js";

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
          <Route path="/ui/orgs/:org/packages" element={<PackagesPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const deletedPkg = {
  id: 9,
  name: "ghost-pkg",
  package_type: "container",
  visibility: "private",
  version_count: 1,
  updated_at: "2026-01-01T00:00:00Z",
};

const npmPkg = {
  id: 12,
  name: "web-sdk",
  package_type: "npm",
  visibility: "public",
  url: "",
  html_url: "",
  version_count: 1,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
  owner: { login: "acme", type: "Organization" },
  repository: {
    id: 4,
    node_id: "R_4",
    name: "web",
    full_name: "acme/web",
    description: "",
    homepage: null,
    default_branch: "main",
    visibility: "public",
    private: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    pushed_at: null,
    size: 0,
    owner: { login: "acme", type: "Organization" },
  },
};

describe("PackagesPage detail", () => {
  it("shows per-ecosystem installation instructions and a source-repo link", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/versions")) {
        return Promise.resolve(
          jsonResponse([
            { id: 31, name: "2.1.0", url: "", package_html_url: "", created_at: "2026-01-02T00:00:00Z", updated_at: "2026-01-02T00:00:00Z" },
          ]),
        );
      }
      if (u.includes("state=deleted")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/packages")) return Promise.resolve(jsonResponse([npmPkg]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/packages");

    // Switch to the npm tab, open the package detail.
    fireEvent.click(await screen.findByRole("tab", { name: "npm" }));
    fireEvent.click(await screen.findByRole("button", { name: "web-sdk" }));

    await waitFor(() => {
      expect(screen.getByText("Installation")).toBeInTheDocument();
    });
    // Latest (non-deleted) version fills the npm install command.
    expect(await screen.findByText(/npm install web-sdk@2\.1\.0/)).toBeInTheDocument();
    // Owning repository is linked.
    expect(screen.getByRole("link", { name: "acme/web" })).toHaveAttribute(
      "href",
      "/ui/repos/acme/web",
    );
  });

  it("renders a docker pull command for container packages using this host", async () => {
    const containerPkg = { ...npmPkg, id: 13, name: "svc-image", package_type: "container", repository: null };
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/versions")) {
        return Promise.resolve(
          jsonResponse([
            { id: 32, name: "v5", url: "", package_html_url: "", created_at: "2026-01-02T00:00:00Z", updated_at: "2026-01-02T00:00:00Z" },
          ]),
        );
      }
      if (u.includes("state=deleted")) return Promise.resolve(jsonResponse([]));
      if (u.includes("/packages")) return Promise.resolve(jsonResponse([containerPkg]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/packages");

    fireEvent.click(await screen.findByRole("button", { name: "svc-image" }));
    expect(
      await screen.findByText(new RegExp(`docker pull ${window.location.host}/acme/svc-image:v5`)),
    ).toBeInTheDocument();
  });
});

describe("PackagesPage deleted packages", () => {
  it("lists deleted packages and restores one via POST .../restore", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/packages/container/ghost-pkg/restore") && init?.method === "POST") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.includes("state=deleted")) return Promise.resolve(jsonResponse([deletedPkg]));
      // active list (any package_type) → empty
      if (u.includes("/packages")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/packages");

    const restoreBtn = await screen.findByRole("button", { name: "Restore package ghost-pkg" });
    fireEvent.click(restoreBtn);
    fireEvent.click(await screen.findByRole("button", { name: "Restore" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        ([u, i]) =>
          u.toString().includes("/api/v3/orgs/acme/packages/container/ghost-pkg/restore") &&
          (i as RequestInit)?.method === "POST",
      );
      expect(post).toBeTruthy();
    });
  });
});
