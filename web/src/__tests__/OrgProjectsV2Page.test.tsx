import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { OrgProjectsV2Page } from "../pages/OrgProjectsV2Page.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

function renderAt(path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/orgs/:org/projects" element={<OrgProjectsV2Page />} />
          <Route path="/ui/orgs/:org/projects/:number" element={<OrgProjectsV2Page />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("OrgProjectsV2Page", () => {
  it("lists an org's Projects V2", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/orgs/acme/projectsV2")) {
        return Promise.resolve(jsonResponse([{ id: 1, number: 3, title: "Roadmap", short_description: null }]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/projects");
    expect(await screen.findByText("Roadmap")).toBeInTheDocument();
  });

  it("moves an item between board columns via PATCH item field value", async () => {
    const field = {
      id: 100,
      name: "Status",
      data_type: "single_select",
      options: [
        { id: "opt-todo", name: { raw: "To do", html: "To do" } },
        { id: "opt-done", name: { raw: "Done", html: "Done" } },
      ],
    };
    const item = {
      id: 10,
      content_type: "Issue",
      content: { title: "Fix bug", number: 5 },
      fields: [{ id: 100, name: "Status", data_type: "single_select", value: { id: "opt-todo", name: "To do" } }],
    };
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/orgs/acme/projectsV2/3/items/10") && init?.method === "PATCH") {
        return Promise.resolve(jsonResponse({ ...item, fields: [{ ...item.fields[0], value: { id: "opt-done", name: "Done" } }] }));
      }
      if (u.endsWith("/orgs/acme/projectsV2/3/fields")) return Promise.resolve(jsonResponse([field]));
      if (u.endsWith("/orgs/acme/projectsV2/3/items")) return Promise.resolve(jsonResponse([item]));
      if (u.endsWith("/orgs/acme/projectsV2/3")) {
        return Promise.resolve(jsonResponse({ id: 1, number: 3, title: "Roadmap", short_description: null }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/projects/3");

    // The table view is the default, so the single-select cell drives the PATCH.
    const moveSelect = await screen.findByLabelText("Status for item 10");
    fireEvent.change(moveSelect, { target: { value: "opt-done" } });

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/orgs/acme/projectsV2/3/items/10") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeDefined();
      expect(JSON.parse(String(patch![1].body))).toEqual({ fields: [{ id: 100, value: "opt-done" }] });
    });
  });

  it("filters table items by title", async () => {
    const items = [
      { id: 21, content_type: "Issue", content: { title: "Fix the login bug", number: 1 }, fields: [] },
      { id: 22, content_type: "Issue", content: { title: "Write docs", number: 2 }, fields: [] },
    ];
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/orgs/acme/projectsV2/3/fields")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/orgs/acme/projectsV2/3/items")) return Promise.resolve(jsonResponse(items));
      if (u.endsWith("/orgs/acme/projectsV2/3")) {
        return Promise.resolve(jsonResponse({ id: 1, number: 3, title: "Roadmap", short_description: null }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/projects/3");

    expect(await screen.findByText("Write docs")).toBeInTheDocument();
    expect(screen.getByText("Fix the login bug")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Filter items by title"), { target: { value: "docs" } });
    expect(screen.getByText("Write docs")).toBeInTheDocument();
    expect(screen.queryByText("Fix the login bug")).toBeNull();
  });

  it("edits a text field value via PATCH on blur", async () => {
    const field = { id: 200, name: "Notes", data_type: "text", options: [] };
    const item = {
      id: 11,
      content_type: "Issue",
      content: { title: "Doc task", number: 7 },
      fields: [{ id: 200, name: "Notes", data_type: "text", value: "old" }],
    };
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/orgs/acme/projectsV2/3/fields")) return Promise.resolve(jsonResponse([field]));
      if (u.endsWith("/orgs/acme/projectsV2/3/items")) return Promise.resolve(jsonResponse([item]));
      if (u.endsWith("/orgs/acme/projectsV2/3")) {
        return Promise.resolve(jsonResponse({ id: 1, number: 3, title: "Roadmap", short_description: null }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/projects/3");

    const input = await screen.findByLabelText("Notes for item 11");
    fireEvent.change(input, { target: { value: "revised" } });
    fireEvent.blur(input);

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/orgs/acme/projectsV2/3/items/11") && c[1]?.method === "PATCH",
      );
      expect(patch).toBeDefined();
      expect(JSON.parse(String(patch![1].body))).toEqual({ fields: [{ id: 200, value: "revised" }] });
    });
  });

  it("defaults to a table view and switches to the board", async () => {
    const field = {
      id: 100,
      name: "Status",
      data_type: "single_select",
      options: [
        { id: "opt-todo", name: { raw: "To do", html: "To do" } },
        { id: "opt-done", name: { raw: "Done", html: "Done" } },
      ],
    };
    const item = {
      id: 10,
      content_type: "Issue",
      content: { title: "Fix bug", number: 5 },
      fields: [{ id: 100, name: "Status", data_type: "single_select", value: { id: "opt-todo", name: "To do" } }],
    };
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/orgs/acme/projectsV2/3/fields")) return Promise.resolve(jsonResponse([field]));
      if (u.endsWith("/orgs/acme/projectsV2/3/items")) return Promise.resolve(jsonResponse([item]));
      if (u.endsWith("/orgs/acme/projectsV2/3")) {
        return Promise.resolve(jsonResponse({ id: 1, number: 3, title: "Roadmap", short_description: null }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/projects/3");

    // Table view is active by default: a column header per field + a row per item.
    expect(await screen.findByRole("columnheader", { name: "Status" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Title" })).toBeInTheDocument();
    expect(screen.getByRole("rowheader", { name: /Fix bug/ })).toBeInTheDocument();
    // The Table toggle is pressed, Board is not.
    expect(screen.getByRole("button", { name: "Table" })).toHaveAttribute("aria-pressed", "true");

    // Switching to the board reveals the kanban move control instead.
    fireEvent.click(screen.getByRole("button", { name: "Board" }));
    expect(await screen.findByLabelText("Move item 10")).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("creates a draft item via POST .../drafts", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/orgs/acme/projectsV2/3/drafts") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ id: 20, content_type: "DraftIssue", content: null }, 201));
      }
      if (u.endsWith("/orgs/acme/projectsV2/3/items")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/orgs/acme/projectsV2/3")) {
        return Promise.resolve(jsonResponse({ id: 1, number: 3, title: "Roadmap", short_description: null }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/projects/3");

    fireEvent.change(await screen.findByLabelText("draft title"), { target: { value: "Plan the sprint" } });
    fireEvent.click(screen.getByRole("button", { name: "Add draft" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/orgs/acme/projectsV2/3/drafts") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({ title: "Plan the sprint" });
    });
  });

  it("adds an item via POST /orgs/{org}/projectsV2/{n}/items", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/orgs/acme/projectsV2/3/items") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ id: 10, content_type: "Issue", content: null }, 201));
      }
      if (u.endsWith("/orgs/acme/projectsV2/3/items")) return Promise.resolve(jsonResponse([]));
      if (u.endsWith("/orgs/acme/projectsV2/3")) {
        return Promise.resolve(jsonResponse({ id: 1, number: 3, title: "Roadmap", short_description: null }));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderAt("/ui/orgs/acme/projects/3");

    fireEvent.change(await screen.findByLabelText("item repo"), { target: { value: "acme/web" } });
    fireEvent.change(screen.getByLabelText("item number"), { target: { value: "42" } });
    fireEvent.click(screen.getByRole("button", { name: "Add item" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/orgs/acme/projectsV2/3/items") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({ type: "Issue", owner: "acme", repo: "web", number: 42 });
    });
  });
});
