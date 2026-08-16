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
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url.split("?")[0]! === "/api/v3/gists") return Promise.resolve(jsonResponse([gist]));
    if (url === "/api/v3/gists/public") return Promise.resolve(jsonResponse([{ ...gist, id: "g2" }]));
    if (url === "/api/v3/gists/starred") return Promise.resolve(jsonResponse([{ ...gist, id: "g3" }]));
    if (url === "/api/v3/gists/g1") return Promise.resolve(jsonResponse(gist));
    if (url === "/api/v3/gists/g1/star") {
      if (init?.method === "GET") return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(new Response(null, { status: 204 }));
    }
    if (url === "/api/v3/gists/g1/forks") {
      if (init?.method === "POST") return Promise.resolve(jsonResponse(gist, 201));
      return Promise.resolve(jsonResponse([]));
    }
    if (url === "/api/v3/gists/g1/commits") return Promise.resolve(jsonResponse([]));
    if (url === "/api/v3/gists/g1/comments") {
      if (init?.method === "POST") {
        return Promise.resolve(
          jsonResponse({ id: 99, body: "great gist", user: { login: "admin" }, created_at: "2026-01-02T00:00:00Z" }, 201),
        );
      }
      return Promise.resolve(
        jsonResponse([{ id: 1, body: "first!", user: { login: "octocat" }, created_at: "2026-01-01T00:00:00Z" }]),
      );
    }
    if (url === "/api/v3/user") return Promise.resolve(jsonResponse({ login: "admin", avatar_url: "" }));
    return Promise.resolve(jsonResponse({}));
  });
}

describe("GistsPage", () => {
  it("renders user's gists", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Gists")).toBeInTheDocument();
      expect(screen.getByText("hello world")).toBeInTheDocument();
    });
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

  it("opens gist detail and shows star/fork actions", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("hello world")).toBeInTheDocument());
    fireEvent.click(screen.getByText("hello world"));
    await waitFor(() => {
      expect(screen.getByText("Unstar")).toBeInTheDocument();
      expect(screen.getByText("Fork")).toBeInTheDocument();
      expect(screen.getByText("hello.txt")).toBeInTheDocument();
    });
  });

  it("lists gist comments and posts a new one", async () => {
    mockEndpoints();
    renderPage();
    await waitFor(() => expect(screen.getByText("hello world")).toBeInTheDocument());
    fireEvent.click(screen.getByText("hello world"));
    await waitFor(() => expect(screen.getByText("hello.txt")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("tab", { name: "Comments" }));
    await waitFor(() => expect(screen.getByText("first!")).toBeInTheDocument());

    fireEvent.change(screen.getByLabelText("Add a comment"), { target: { value: "great gist" } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));

    await waitFor(() =>
      expect(
        mockFetch.mock.calls.some(
          ([u, init]) => u === "/api/v3/gists/g1/comments" && (init as RequestInit | undefined)?.method === "POST",
        ),
      ).toBe(true),
    );
  });

  it("edits the viewer's own gist comment via PATCH", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url.split("?")[0]! === "/api/v3/gists") return Promise.resolve(jsonResponse([gist]));
      if (url === "/api/v3/gists/g1") return Promise.resolve(jsonResponse(gist));
      if (url === "/api/v3/gists/g1/commits") return Promise.resolve(jsonResponse([]));
      if (url === "/api/v3/gists/g1/comments") {
        if (init?.method === "PATCH") {
          return Promise.resolve(
            jsonResponse({ id: 1, body: "edited body", user: { login: "admin" }, created_at: "2026-01-01T00:00:00Z" }),
          );
        }
        return Promise.resolve(
          jsonResponse([{ id: 1, body: "mine", user: { login: "admin" }, created_at: "2026-01-01T00:00:00Z" }]),
        );
      }
      if (url === "/api/v3/gists/g1/comments/1") {
        return Promise.resolve(
          jsonResponse({ id: 1, body: "edited body", user: { login: "admin" }, created_at: "2026-01-01T00:00:00Z" }),
        );
      }
      if (url === "/api/v3/user") return Promise.resolve(jsonResponse({ login: "admin", avatar_url: "" }));
      return Promise.resolve(jsonResponse({}));
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("hello world")).toBeInTheDocument());
    fireEvent.click(screen.getByText("hello world"));
    await waitFor(() => expect(screen.getByText("hello.txt")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("tab", { name: "Comments" }));
    await waitFor(() => expect(screen.getByText("mine")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Edit comment 1" }));
    fireEvent.change(screen.getByLabelText("Edit comment"), { target: { value: "edited body" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      const patched = mockFetch.mock.calls.find(
        ([u, init]) => u === "/api/v3/gists/g1/comments/1" && (init as RequestInit | undefined)?.method === "PATCH",
      );
      expect(patched).toBeTruthy();
      expect(JSON.parse((patched![1] as RequestInit).body as string)).toEqual({ body: "edited body" });
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
