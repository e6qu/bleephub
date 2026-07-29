// A state-changing control that fails must say so. These cover the shared
// surface itself, then drive two real pages — one security-relevant, one
// destructive — against a rejecting write and assert the failure is visible.
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import type { ReactElement } from "react";

import { MutationError } from "../components/MutationError.js";
import { SecretScanningPage } from "../pages/SecretScanningPage.js";
import { PackagesPage } from "../pages/PackagesPage.js";

const mockFetch = vi.fn();

beforeEach(() => {
  globalThis.fetch = mockFetch as unknown as typeof fetch;
});

afterEach(() => {
  mockFetch.mockReset();
  vi.restoreAllMocks();
});

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function renderAt(routePath: string, entry: string, element: ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[entry]}>
        <Routes>
          <Route path={routePath} element={element} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("MutationError", () => {
  it("renders nothing while every source is clean", () => {
    const { container } = render(<MutationError of={[{ error: null }, { error: undefined }]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the failure as an alert", () => {
    render(<MutationError of={{ error: new Error("server said no") }} />);
    expect(screen.getByRole("alert")).toHaveTextContent("server said no");
  });

  it("reports the first failure among several sources", () => {
    render(
      <MutationError
        of={[{ error: null }, { error: new Error("first") }, { error: new Error("second") }]}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("first");
    expect(screen.queryByText(/second/)).not.toBeInTheDocument();
  });

  it("stringifies a non-Error rejection rather than dropping it", () => {
    render(<MutationError of={{ error: "plain string failure" }} />);
    expect(screen.getByRole("alert")).toHaveTextContent("plain string failure");
  });
});

describe("failing mutations surface on the page", () => {
  it("SecretScanningPage shows an error when resolving an alert fails", async () => {
    const alert = {
      number: 7,
      state: "open",
      secret_type: "github_pat",
      secret_type_display_name: "GitHub PAT",
      resolution: null,
      created_at: "2024-01-01T00:00:00Z",
      html_url: "",
      secret: "x",
    };
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (method === "PATCH") {
        return Promise.resolve(jsonResponse({ message: "Forbidden" }, 403));
      }
      if (url.includes("/locations")) return Promise.resolve(jsonResponse([]));
      if (url.endsWith("/secret-scanning/alerts/7")) return Promise.resolve(jsonResponse(alert));
      if (url.includes("/secret-scanning/alerts")) return Promise.resolve(jsonResponse([alert]));
      return Promise.resolve(jsonResponse([]));
    });

    renderAt(
      "/ui/repos/:owner/:repo/security/secret-scanning",
      "/ui/repos/admin/r/security/secret-scanning",
      <SecretScanningPage />,
    );

    fireEvent.click(await screen.findByRole("button", { name: /#7 GitHub PAT/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Resolve" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/403/);
  });

  it("PackagesPage shows an error when deleting a package fails", async () => {
    const pkg = {
      id: 1,
      name: "widget",
      package_type: "container",
      visibility: "public",
      version_count: 1,
      updated_at: "2024-01-01T00:00:00Z",
      created_at: "2024-01-01T00:00:00Z",
      html_url: "",
      owner: { login: "admin" },
      repository: null,
    };
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (method === "DELETE") {
        return Promise.resolve(jsonResponse({ message: "Internal Server Error" }, 500));
      }
      if (url.includes("/packages")) return Promise.resolve(jsonResponse([pkg]));
      if (url.endsWith("/api/v3/user")) {
        return Promise.resolve(jsonResponse({ login: "admin", id: 1 }));
      }
      return Promise.resolve(jsonResponse([]));
    });

    renderAt("/ui/packages", "/ui/packages", <PackagesPage />);

    fireEvent.click(await screen.findByRole("button", { name: /Delete/i }));
    fireEvent.click(await screen.findByRole("button", { name: "Confirm" }));

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/500/),
    );
  });
});
