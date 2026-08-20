import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { IssueSidebar } from "../components/IssueSidebar.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

const project = { id: 11, number: 3, title: "Roadmap", short_description: null };

// Reusable base mock: everything empty except the two project endpoints, which
// the test overrides per-case for membership vs non-membership.
function baseMock(itemsForProject3: unknown[], perms = { admin: true, push: true, pull: true }) {
  mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
    const u = url.toString();
    const method = init?.method ?? "GET";
    // Viewer-role gating reads the repo payload's permissions.
    if (u.endsWith("/api/v3/repos/acme/app")) {
      return Promise.resolve(jsonResponse({ id: 1, owner: { login: "acme", type: "Organization" }, permissions: perms }));
    }
    if (u.endsWith("/orgs/acme/projectsV2")) return Promise.resolve(jsonResponse([project]));
    if (u.includes("/orgs/acme/projectsV2/3/items") && method === "GET") return Promise.resolve(jsonResponse(itemsForProject3));
    if (u.includes("/orgs/acme/projectsV2/3/items") && method === "POST") return Promise.resolve(jsonResponse({ id: 99, content_type: "Issue", content: null }, 201));
    if (u.includes("/orgs/acme/projectsV2/3/items/")) return Promise.resolve(new Response(null, { status: 204 })); // DELETE
    if (u.includes("/graphql")) return Promise.resolve(jsonResponse({ data: {} }));
    return Promise.resolve(jsonResponse([]));
  });
}

function renderSidebar() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <IssueSidebar
          owner="acme"
          repo="app"
          ownerType="Organization"
          number={5}
          kind="issue"
          assignees={[]}
          labels={[]}
          milestone={null}
          participants={[]}
        />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("IssueSidebar Projects", () => {
  it("lists a project the issue belongs to and removes it via DELETE", async () => {
    baseMock([{ id: 42, content_type: "Issue", content: { number: 5, html_url: "http://x/acme/app/issues/5" } }]);
    renderSidebar();

    // The membership renders (not the old "None yet" stub).
    expect(await screen.findByText("Roadmap")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Remove from project Roadmap/ }));

    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0].toString().includes("/orgs/acme/projectsV2/3/items/42") && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });

  it("offers a project the issue is not in and adds it via POST", async () => {
    baseMock([]); // no matching item → project is addable
    renderSidebar();

    const select = await screen.findByRole("combobox", { name: "Add to project" });
    fireEvent.change(select, { target: { value: "3" } });

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/orgs/acme/projectsV2/3/items") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
      expect(JSON.parse(String(post![1]!.body))).toMatchObject({ type: "Issue", owner: "acme", repo: "app", number: 5 });
    });
  });
});

describe("IssueSidebar viewer-role gating", () => {
  it("renders projects read-only (and no Lock section) without push access", async () => {
    baseMock(
      [{ id: 42, content_type: "Issue", content: { number: 5, html_url: "http://x/acme/app/issues/5" } }],
      { admin: false, push: false, pull: true },
    );
    renderSidebar();

    // The membership still shows — as plain text.
    expect(await screen.findByText("Roadmap")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Remove from project Roadmap/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: "Add to project" })).not.toBeInTheDocument();
    // Lock conversation is write-only.
    expect(screen.queryByRole("button", { name: /Lock conversation/ })).not.toBeInTheDocument();
  });
});
