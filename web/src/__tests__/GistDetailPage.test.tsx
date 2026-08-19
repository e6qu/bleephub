import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { GistDetailPage } from "../pages/GistDetailPage.js";

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

function renderAt(id = "g1") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[`/ui/gists/${id}`]}>
        <Routes>
          <Route path="/ui/gists/:id" element={<GistDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const gist = {
  id: "g1",
  description: "hello world",
  public: true,
  owner: { login: "admin", type: "User" },
  files: {
    "hello.txt": {
      filename: "hello.txt",
      content: "hello",
      size: 5,
      type: "text/plain",
      language: "Text",
      raw_url: "http://x/raw/g1/hello.txt",
    },
    "notes.md": {
      filename: "notes.md",
      content: "# Heading",
      size: 9,
      type: "text/markdown",
      language: "Markdown",
      raw_url: "http://x/raw/g1/notes.md",
    },
  },
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

function mockEndpoints() {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === "/api/v3/gists/g1") return Promise.resolve(jsonResponse(gist));
    if (url === "/api/v3/gists/g1/star") return Promise.resolve(new Response(null, { status: 204 }));
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

describe("GistDetailPage", () => {
  it("renders every file with a raw link, markdown files as markdown", async () => {
    mockEndpoints();
    renderAt();

    expect(await screen.findByText("hello world")).toBeInTheDocument();
    expect(screen.getByText("hello.txt")).toBeInTheDocument();
    expect(screen.getByText("hello")).toBeInTheDocument();
    // notes.md renders through Markdown → a real heading, not literal "# Heading".
    expect(await screen.findByRole("heading", { name: "Heading" })).toBeInTheDocument();

    const rawLinks = screen.getAllByRole("link", { name: "Raw" });
    expect(rawLinks).toHaveLength(2);
    expect(rawLinks[0]).toHaveAttribute("href", "http://x/raw/g1/hello.txt");
  });

  it("shows star/fork actions (starred state from GET /gists/{id}/star)", async () => {
    mockEndpoints();
    renderAt();
    await waitFor(() => {
      expect(screen.getByText("Unstar")).toBeInTheDocument();
      expect(screen.getByText("Fork")).toBeInTheDocument();
    });
  });

  it("lists comments and posts a new one", async () => {
    mockEndpoints();
    renderAt();
    await screen.findByText("hello world");

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

  it("edits the viewer's own comment via PATCH", async () => {
    mockFetch.mockImplementation((url: string) => {
      if (url === "/api/v3/gists/g1") return Promise.resolve(jsonResponse(gist));
      if (url === "/api/v3/gists/g1/commits") return Promise.resolve(jsonResponse([]));
      if (url === "/api/v3/gists/g1/comments") {
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
    renderAt();
    await screen.findByText("hello world");

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

  it("hides Edit/Delete from a non-owner viewer", async () => {
    mockFetch.mockImplementation((url: string) => {
      if (url === "/api/v3/gists/g1") return Promise.resolve(jsonResponse(gist));
      if (url === "/api/v3/user") return Promise.resolve(jsonResponse({ login: "someoneelse", avatar_url: "" }));
      return Promise.resolve(jsonResponse([]));
    });
    renderAt();
    await screen.findByText("hello world");
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
  });
});
