import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { OrgHooksPage } from "../pages/OrgHooksPage.js";

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

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/ui/orgs/acme/hooks"]}>
        <Routes>
          <Route path="/ui/orgs/:org/hooks" element={<OrgHooksPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const orgHook = {
  id: 3,
  type: "Organization",
  name: "web",
  active: true,
  events: ["push", "issues"],
  config: { url: "https://ci.example.test/org-hook", content_type: "json", insecure_ssl: "0" },
  created_at: "2026-05-01T00:00:00Z",
  updated_at: "2026-05-01T00:00:00Z",
  url: "/api/v3/orgs/acme/hooks/3",
  ping_url: "/api/v3/orgs/acme/hooks/3/pings",
  deliveries_url: "/api/v3/orgs/acme/hooks/3/deliveries",
};

describe("OrgHooksPage", () => {
  it("lists organization webhooks with a deliveries link", async () => {
    mockFetch.mockImplementation((url: string) => {
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) {
        return Promise.resolve(jsonResponse([orgHook]));
      }
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("#3")).toBeInTheDocument());
    expect(screen.getByText(/https:\/\/ci\.example\.test\/org-hook/)).toBeInTheDocument();
    expect(screen.getByText(/events: push, issues/)).toBeInTheDocument();
    expect(mockFetch).toHaveBeenCalledWith(
      "/api/v3/orgs/acme/hooks?per_page=30",
      expect.anything(),
    );

    const link = screen.getByRole("link", { name: /deliveries/i });
    expect(link).toHaveAttribute("href", "/ui/orgs/acme/hooks/3/deliveries");
  });

  it("shows an honest empty state when the org has no webhooks", async () => {
    mockFetch.mockImplementation((url: string) => {
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) {
        return Promise.resolve(jsonResponse([]));
      }
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();

    await waitFor(() =>
      expect(screen.getByText("No organization webhooks")).toBeInTheDocument(),
    );
  });

  it("surfaces list errors instead of swallowing them", async () => {
    mockFetch.mockImplementation(() =>
      Promise.resolve(jsonResponse({ message: "boom" }, 500)),
    );
    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/failed to load organization webhooks/i)).toBeInTheDocument(),
    );
  });
});

describe("OrgHooksPage write actions", () => {
  it("creates a webhook", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.endsWith("/hooks") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ ...orgHook, id: 9 }));
      }
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /new webhook/i }));
    fireEvent.change(await screen.findByLabelText(/payload url/i), {
      target: { value: "https://x.test/h" },
    });
    fireEvent.click(screen.getByRole("button", { name: /add webhook/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/hooks") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      const body = JSON.parse((post![1] as RequestInit).body as string);
      expect(body.config.url).toBe("https://x.test/h");
      expect(body.events).toEqual(["push"]);
    });
  });

  it("pings a webhook", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.endsWith("/hooks/3/pings") && init?.method === "POST") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) return Promise.resolve(jsonResponse([orgHook]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /ping webhook 3/i }));
    await waitFor(() => {
      expect(
        mockFetch.mock.calls.find(
          (c) => String(c[0]).endsWith("/hooks/3/pings") && c[1]?.method === "POST",
        ),
      ).toBeTruthy();
    });
  });

  it("deletes a webhook after confirmation", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.endsWith("/hooks/3") && init?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (url.startsWith("/api/v3/orgs/acme/hooks?")) return Promise.resolve(jsonResponse([orgHook]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /delete webhook 3/i }));
    fireEvent.click(await screen.findByRole("button", { name: /^delete$/i }));
    await waitFor(() => {
      expect(
        mockFetch.mock.calls.find(
          (c) => String(c[0]).endsWith("/hooks/3") && c[1]?.method === "DELETE",
        ),
      ).toBeTruthy();
    });
  });
});
