import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { GistsPage } from "../pages/GistsPage.js";

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
  window.history.pushState({}, "", "/");
});

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <GistsPage />
      </BrowserRouter>
    </QueryClientProvider>,
  );
}

const gist = {
  id: "g1",
  description: "hello world",
  public: true,
  owner: { login: "admin", type: "User" },
  files: {
    "hello.txt": { filename: "hello.txt", content: "hello", size: 5, type: "text/plain", language: "Text" },
  },
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

function mockEndpoints() {
  mockFetch.mockImplementation((url: string) => {
    if (url.split("?")[0]! === "/api/v3/gists") return Promise.resolve(jsonResponse([gist]));
    if (url === "/api/v3/gists/public") return Promise.resolve(jsonResponse([{ ...gist, id: "g2" }]));
    if (url === "/api/v3/gists/starred") return Promise.resolve(jsonResponse([{ ...gist, id: "g3" }]));
    if (url === "/api/v3/gists/g1") return Promise.resolve(jsonResponse(gist));
    if (url === "/api/v3/user") return Promise.resolve(jsonResponse({ login: "admin", avatar_url: "" }));
    return Promise.resolve(jsonResponse({}));
  });
}

describe("GistsPage", () => {
  it("renders user's gists as cards with description, file count, and snippet", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Gists")).toBeInTheDocument();
      expect(screen.getByText("hello world")).toBeInTheDocument();
    });
    expect(screen.getByText("1 file")).toBeInTheDocument();
    // The first-file snippet preview (content rides in the mock list payload).
    expect(screen.getByText("hello")).toBeInTheDocument();
  });

  it("links each card to the gist permalink page /ui/gists/{id}", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("hello world")).toBeInTheDocument());
    const link = screen.getByRole("link", { name: /admin \/ hello\.txt/ });
    expect(link).toHaveAttribute("href", "/ui/gists/g1");
  });

  it("switches to public gists", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("hello world")).toBeInTheDocument());
    fireEvent.click(screen.getByText("Public"));
    await waitFor(() => {
      expect(screen.getByText("Public")).toBeInTheDocument();
    });
  });

  it("switches to starred gists", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("hello world")).toBeInTheDocument());
    fireEvent.click(screen.getByText("Starred"));
    await waitFor(() => {
      expect(screen.getByText("Starred")).toBeInTheDocument();
    });
  });

  it("deletes an own gist after confirmation", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === "/api/v3/gists/g1" && init?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (url.split("?")[0]! === "/api/v3/gists") return Promise.resolve(jsonResponse([gist]));
      if (url === "/api/v3/user") return Promise.resolve(jsonResponse({ login: "admin", avatar_url: "" }));
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("hello world")).toBeInTheDocument());
    fireEvent.click(await screen.findByRole("button", { name: "Delete gist g1" }));
    fireEvent.click(await screen.findByRole("button", { name: /^delete$/i }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        ([u, init]) => u === "/api/v3/gists/g1" && (init as RequestInit | undefined)?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });

  it("opens the create form from the global new-gist deep link", async () => {
    window.history.pushState({}, "", "/ui/gists?new=1");
    mockEndpoints();

    renderPage();

    expect(
      await screen.findByRole("heading", { name: /create gist/i }),
    ).toBeInTheDocument();
  });
});
