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
