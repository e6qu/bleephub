import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { DiscussionsPage } from "../pages/DiscussionsPage.js";

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

function renderAt(path: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/repos/:owner/:repo/discussions" element={<DiscussionsPage />} />
          <Route path="/ui/repos/:owner/:repo/discussions/:number" element={<DiscussionsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const category = { id: "DGC_kgDO00000001", name: "General", emoji: ":speech_balloon:", description: "", isAnswerable: false };

function discussion(number: number, title: string) {
  return {
    id: `D_kgDO${String(number).padStart(8, "0")}`,
    number,
    title,
    bodyText: "body",
    author: { login: "admin", avatarUrl: "" },
    category,
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    comments: { totalCount: 0 },
  };
}

describe("DiscussionsPage list", () => {
  it("renders the discussion list", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/discussions/pinned")) {
        return Promise.resolve(jsonResponse([]));
      }
      if (u.includes("/repos/admin/test")) {
        return Promise.resolve(jsonResponse({ id: 1, node_id: "R_kgDO00000001" }));
      }
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (body.query.includes("discussionCategories")) {
          return Promise.resolve(jsonResponse({ data: { repository: { discussionCategories: { nodes: [category] } } } }));
        }
        return Promise.resolve(
          jsonResponse({
            data: {
              repository: {
                discussions: {
                  nodes: [discussion(1, "First discussion")],
                  totalCount: 1,
                  pageInfo: { hasNextPage: false, endCursor: null },
                },
              },
            },
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/discussions");
    await waitFor(() => {
      expect(screen.getByText("First discussion")).toBeInTheDocument();
    });
  });

  it("shows category filters", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/discussions/pinned")) {
        return Promise.resolve(jsonResponse([]));
      }
      if (u.includes("/repos/admin/test")) {
        return Promise.resolve(jsonResponse({ id: 1, node_id: "R_kgDO00000001" }));
      }
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (body.query.includes("discussionCategories")) {
          return Promise.resolve(jsonResponse({ data: { repository: { discussionCategories: { nodes: [category] } } } }));
        }
        return Promise.resolve(
          jsonResponse({ data: { repository: { discussions: { nodes: [], totalCount: 0, pageInfo: { hasNextPage: false, endCursor: null } } } } }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/discussions");
    await waitFor(() => {
      expect(screen.getByText(/General/i)).toBeInTheDocument();
    });
  });
});

describe("DiscussionsPage detail", () => {
  it("renders the discussion when found", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (body.query.includes("discussionCategories")) {
          return Promise.resolve(jsonResponse({ data: { repository: { discussionCategories: { nodes: [category] } } } }));
        }
        return Promise.resolve(
          jsonResponse({
            data: {
              repository: {
                discussion: {
                  ...discussion(7, "A real discussion"),
                  body: "details",
                  bodyHTML: "<p>details</p>",
                  comments: { nodes: [], totalCount: 0 },
                },
              },
            },
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/discussions/7");
    await waitFor(() => {
      expect(screen.getByText("A real discussion")).toBeInTheDocument();
    });
  });

  it("edits a discussion's title and body via updateDiscussion", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (body.query.includes("updateDiscussion")) {
          return Promise.resolve(jsonResponse({ data: { updateDiscussion: { discussion: { id: "D1", title: "Edited title" } } } }));
        }
        if (body.query.includes("discussionCategories")) {
          return Promise.resolve(jsonResponse({ data: { repository: { discussionCategories: { nodes: [category] } } } }));
        }
        return Promise.resolve(jsonResponse({
          data: { repository: { discussion: {
            ...discussion(7, "A real discussion"), body: "details", bodyHTML: "<p>details</p>", bodyText: "details", comments: { nodes: [], totalCount: 0 },
          } } },
        }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/discussions/7");
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Edited title" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => {
      const mut = mockFetch.mock.calls.find(
        ([u2, i2]) => u2.toString().includes("/api/graphql") && String((i2 as RequestInit)?.body).includes("updateDiscussion"),
      );
      expect(mut).toBeTruthy();
      expect(JSON.parse(String((mut![1] as RequestInit).body)).variables.input.title).toBe("Edited title");
    });
  });

  it("locks a discussion via the lockLockable mutation", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (body.query.includes("lockLockable")) {
          return Promise.resolve(jsonResponse({ data: { lockLockable: { lockedRecord: { locked: true } } } }));
        }
        if (body.query.includes("discussionCategories")) {
          return Promise.resolve(jsonResponse({ data: { repository: { discussionCategories: { nodes: [category] } } } }));
        }
        return Promise.resolve(jsonResponse({
          data: { repository: { discussion: {
            ...discussion(7, "A real discussion"), locked: false, body: "details", bodyHTML: "<p>details</p>", comments: { nodes: [], totalCount: 0 },
          } } },
        }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/discussions/7");
    fireEvent.click(await screen.findByRole("button", { name: "Lock conversation" }));
    await waitFor(() => {
      const mut = mockFetch.mock.calls.find(
        ([u2, i2]) => u2.toString().includes("/api/graphql") && String((i2 as RequestInit)?.body).includes("lockLockable"),
      );
      expect(mut).toBeTruthy();
    });
  });

  it("changes a discussion's category on edit via updateDiscussion", async () => {
    const other = { id: "DGC_kgDO00000002", name: "Ideas", emoji: ":bulb:", description: "", isAnswerable: false };
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (body.query.includes("updateDiscussion")) {
          return Promise.resolve(jsonResponse({ data: { updateDiscussion: { discussion: { id: "D1", title: "A real discussion" } } } }));
        }
        if (body.query.includes("discussionCategories")) {
          return Promise.resolve(jsonResponse({ data: { repository: { discussionCategories: { nodes: [category, other] } } } }));
        }
        return Promise.resolve(jsonResponse({
          data: { repository: { discussion: {
            ...discussion(7, "A real discussion"), body: "details", bodyHTML: "<p>details</p>", bodyText: "details", comments: { nodes: [], totalCount: 0 },
          } } },
        }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/discussions/7");
    fireEvent.click(await screen.findByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("Category"), { target: { value: other.id } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => {
      const mut = mockFetch.mock.calls.find(
        ([u2, i2]) => u2.toString().includes("/api/graphql") && String((i2 as RequestInit)?.body).includes("updateDiscussion"),
      );
      expect(mut).toBeTruthy();
      expect(JSON.parse(String((mut![1] as RequestInit).body)).variables.input.categoryId).toBe(other.id);
    });
  });

  it("shows a not-found state for a missing discussion", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (body.query.includes("discussionCategories")) {
          return Promise.resolve(jsonResponse({ data: { repository: { discussionCategories: { nodes: [category] } } } }));
        }
        return Promise.resolve(
          jsonResponse({
            data: { repository: { discussion: null } },
            errors: [{ message: "Could not resolve to a Discussion with the number of 999.", type: "NOT_FOUND" }],
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/discussions/999");
    // A GraphQL NOT_FOUND on the primary read renders the in-shell 404 state,
    // not the raw error banner (the banner stays for non-404 failures —
    // covered in notFoundPages.test.tsx).
    await waitFor(() => {
      expect(screen.getByText(/discussion #999 not found/i)).toBeInTheDocument();
    });
    expect(screen.queryByText(/failed to load discussion #999/i)).not.toBeInTheDocument();
  });
});

function pinnedDiscussion(number: number, title: string) {
  return {
    number,
    title,
    user: { login: "admin" },
    category: { name: "General", emoji: ":speech_balloon:" },
    created_at: "2026-01-01T00:00:00Z",
    comments: 0,
  };
}

/** Mocks the detail route's endpoints: pinned list, repo detail (with the
 * given permissions), and the discussion GraphQL queries. */
function mockDetailRoute({
  pinned,
  permissions,
}: {
  pinned: ReturnType<typeof pinnedDiscussion>[];
  permissions?: { admin: boolean; push: boolean; pull: boolean };
}) {
  mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
    const u = url.toString();
    if (u.includes("/discussions/pinned")) {
      return Promise.resolve(jsonResponse(pinned));
    }
    if (u.includes("/repos/admin/test")) {
      return Promise.resolve(
        jsonResponse({ id: 1, node_id: "R_kgDO00000001", ...(permissions ? { permissions } : {}) }),
      );
    }
    if (u.includes("/api/graphql")) {
      const body = JSON.parse((init?.body as string) ?? "{}");
      if (body.query.includes("discussionCategories")) {
        return Promise.resolve(jsonResponse({ data: { repository: { discussionCategories: { nodes: [category] } } } }));
      }
      return Promise.resolve(jsonResponse({
        data: { repository: { discussion: {
          ...discussion(7, "A real discussion"), body: "details", bodyHTML: "<p>details</p>", comments: { nodes: [], totalCount: 0 },
        } } },
      }));
    }
    return Promise.resolve(jsonResponse([]));
  });
}

describe("DiscussionsPage pinned discussions", () => {
  it("renders pinned discussion cards from the pinned endpoint", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/discussions/pinned")) {
        return Promise.resolve(jsonResponse([pinnedDiscussion(2, "Pinned one"), pinnedDiscussion(5, "Pinned two")]));
      }
      if (u.includes("/repos/admin/test")) {
        return Promise.resolve(jsonResponse({ id: 1, node_id: "R_kgDO00000001" }));
      }
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (body.query.includes("discussionCategories")) {
          return Promise.resolve(jsonResponse({ data: { repository: { discussionCategories: { nodes: [category] } } } }));
        }
        return Promise.resolve(
          jsonResponse({ data: { repository: { discussions: { nodes: [], totalCount: 0, pageInfo: { hasNextPage: false, endCursor: null } } } } }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/repos/admin/test/discussions");
    await waitFor(() => {
      expect(screen.getByText("Pinned one")).toBeInTheDocument();
    });
    expect(screen.getByText("Pinned discussions")).toBeInTheDocument();
    expect(screen.getByText("Pinned two")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Pinned one/ })).toHaveAttribute(
      "href",
      "/ui/repos/admin/test/discussions/2",
    );
  });

  it("pins a discussion by PUTting the numbers list including its number", async () => {
    mockDetailRoute({
      pinned: [pinnedDiscussion(2, "Other pinned")],
      permissions: { admin: false, push: true, pull: true },
    });
    renderAt("/ui/repos/admin/test/discussions/7");
    fireEvent.click(await screen.findByRole("button", { name: "Pin discussion" }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        ([u2, i2]) => u2.toString().includes("/discussions/pinned") && (i2 as RequestInit)?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse(String((put![1] as RequestInit).body))).toEqual({ numbers: [2, 7] });
    });
  });

  it("unpins a discussion by PUTting the numbers list without its number", async () => {
    mockDetailRoute({
      pinned: [pinnedDiscussion(7, "A real discussion"), pinnedDiscussion(2, "Other pinned")],
      permissions: { admin: false, push: true, pull: true },
    });
    renderAt("/ui/repos/admin/test/discussions/7");
    fireEvent.click(await screen.findByRole("button", { name: "Unpin discussion" }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        ([u2, i2]) => u2.toString().includes("/discussions/pinned") && (i2 as RequestInit)?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse(String((put![1] as RequestInit).body))).toEqual({ numbers: [2] });
    });
  });

  it("hides the pin control without push permission", async () => {
    mockDetailRoute({
      pinned: [pinnedDiscussion(2, "Other pinned")],
      permissions: { admin: false, push: false, pull: true },
    });
    renderAt("/ui/repos/admin/test/discussions/7");
    await screen.findByRole("button", { name: "Lock conversation" });
    await waitFor(() => {
      const repoCall = mockFetch.mock.calls.some(([u2]) => u2.toString().includes("/repos/admin/test") && !u2.toString().includes("pinned"));
      expect(repoCall).toBe(true);
    });
    expect(screen.queryByRole("button", { name: "Pin discussion" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Unpin discussion" })).toBeNull();
  });

  it("disables pinning when 4 discussions are already pinned", async () => {
    mockDetailRoute({
      pinned: [1, 2, 3, 4].map((n) => pinnedDiscussion(n, `Pinned ${n}`)),
      permissions: { admin: false, push: true, pull: true },
    });
    renderAt("/ui/repos/admin/test/discussions/7");
    const btn = await screen.findByRole("button", { name: "Pin discussion" });
    expect(btn).toBeDisabled();
    expect(btn).toHaveAttribute("title", "A repository can have at most 4 pinned discussions");
  });
});

// ─── Mark-as-answer gating (author-or-write) ──────────────────────────────

describe("DiscussionsPage mark-as-answer gating", () => {
  const answerable = { id: "DGC_kgDO00000009", name: "Q&A", emoji: ":pray:", description: "", isAnswerable: true };

  function mockAnswerableDetail({
    permissions,
    viewerLogin,
  }: {
    permissions: { admin: boolean; push: boolean; pull: boolean };
    viewerLogin: string;
  }) {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.includes("/discussions/pinned")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: viewerLogin }));
      if (u.includes("/repos/admin/test")) {
        return Promise.resolve(jsonResponse({ id: 1, node_id: "R_kgDO00000001", permissions }));
      }
      if (u.includes("/api/graphql")) {
        const body = JSON.parse((init?.body as string) ?? "{}");
        if (body.query.includes("discussionCategories")) {
          return Promise.resolve(jsonResponse({ data: { repository: { discussionCategories: { nodes: [answerable] } } } }));
        }
        return Promise.resolve(
          jsonResponse({
            data: {
              repository: {
                discussion: {
                  ...discussion(7, "How do I frobnicate?"),
                  category: answerable,
                  body: "details",
                  bodyText: "details",
                  comments: {
                    nodes: [
                      {
                        id: "DC_kgDO00000001",
                        author: { login: "helper" },
                        body: "an answer",
                        createdAt: "2026-01-02T00:00:00Z",
                        isAnswer: false,
                        reactionGroups: [],
                        replies: { nodes: [] },
                      },
                    ],
                    totalCount: 1,
                  },
                },
              },
            },
          }),
        );
      }
      return Promise.resolve(jsonResponse([]));
    });
  }

  it("hides Mark as answer from a pull-only outsider (Reply stays)", async () => {
    mockAnswerableDetail({ permissions: { admin: false, push: false, pull: true }, viewerLogin: "reader" });
    renderAt("/ui/repos/admin/test/discussions/7");
    expect(await screen.findByText("an answer")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reply" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Mark as answer" })).not.toBeInTheDocument();
  });

  it("shows Mark as answer to the discussion author without push", async () => {
    // discussion() is authored by admin.
    mockAnswerableDetail({ permissions: { admin: false, push: false, pull: true }, viewerLogin: "admin" });
    renderAt("/ui/repos/admin/test/discussions/7");
    expect(await screen.findByRole("button", { name: "Mark as answer" })).toBeInTheDocument();
  });

  it("shows Mark as answer to a writer who is not the author", async () => {
    mockAnswerableDetail({ permissions: { admin: false, push: true, pull: true }, viewerLogin: "maintainer" });
    renderAt("/ui/repos/admin/test/discussions/7");
    expect(await screen.findByRole("button", { name: "Mark as answer" })).toBeInTheDocument();
  });
});
