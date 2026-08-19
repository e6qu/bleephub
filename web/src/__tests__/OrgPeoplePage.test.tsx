import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { OrgPeoplePage } from "../pages/OrgPeoplePage.js";

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
      <MemoryRouter initialEntries={["/ui/orgs/acme/people"]}>
        <Routes>
          <Route path="/ui/orgs/:org/people" element={<OrgPeoplePage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const alice = {
  id: 1,
  login: "alice",
  type: "User",
  site_admin: false,
  avatar_url: "",
};

describe("OrgPeoplePage publicize membership", () => {
  it("publicizes own membership via PUT /orgs/{org}/public_members/{login}", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (url === "/api/v3/user") return Promise.resolve(jsonResponse(alice));
      if (url.startsWith("/api/v3/orgs/acme/members")) return Promise.resolve(jsonResponse([alice]));
      if (url === "/api/v3/orgs/acme/public_members" && method === "GET")
        return Promise.resolve(jsonResponse([]));
      if (url === "/api/v3/orgs/acme/public_members/alice" && method === "PUT")
        return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(jsonResponse([]));
    });

    renderPage();

    const makePublic = await screen.findByRole("button", { name: "Make membership public" });
    fireEvent.click(makePublic);

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/orgs/acme/public_members/alice" && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
    });
  });
});

describe("OrgPeoplePage admin gate", () => {
  it("hides the invite box and per-member admin controls from non-admin viewers", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/v3/user") return Promise.resolve(jsonResponse(alice));
      // Plain member, not an owner.
      if (url === "/api/v3/user/memberships/orgs/acme")
        return Promise.resolve(jsonResponse({ state: "active", role: "member" }));
      if (url.startsWith("/api/v3/orgs/acme/members")) return Promise.resolve(jsonResponse([alice, bob]));
      return Promise.resolve(jsonResponse([]));
    });

    renderPage();
    await screen.findByText("bob");

    expect(screen.queryByLabelText(/invite a member/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Set role for bob")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Remove bob")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Convert bob to outside collaborator")).not.toBeInTheDocument();
    // The self-service visibility toggle survives the gate.
    expect(screen.getByRole("button", { name: "Make membership public" })).toBeInTheDocument();
  });

  it("shows each member's org role from the ?role=admin members query", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/v3/user") return Promise.resolve(jsonResponse(alice));
      if (url === "/api/v3/user/memberships/orgs/acme")
        return Promise.resolve(jsonResponse({ state: "active", role: "admin" }));
      if (url.includes("/api/v3/orgs/acme/members") && url.includes("role=admin"))
        return Promise.resolve(jsonResponse([alice]));
      if (url.startsWith("/api/v3/orgs/acme/members")) return Promise.resolve(jsonResponse([alice, bob]));
      return Promise.resolve(jsonResponse([]));
    });

    renderPage();
    await screen.findByText("bob");

    // alice (in the ?role=admin result) is labeled Owner; bob's card shows
    // plain "Member" (asserted via the exact card-label text, alice is self →
    // the private-member suffix).
    await waitFor(() => {
      expect(screen.getByText("Owner · private member")).toBeInTheDocument();
    });
    expect(screen.queryByText("Owner · public member")).not.toBeInTheDocument();
  });

  it("lists outside collaborators under their sub-tab", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/v3/user") return Promise.resolve(jsonResponse(alice));
      if (url === "/api/v3/orgs/acme/outside_collaborators")
        return Promise.resolve(jsonResponse([{ id: 9, login: "contractor", type: "User", site_admin: false, avatar_url: "" }]));
      if (url.startsWith("/api/v3/orgs/acme/members")) return Promise.resolve(jsonResponse([alice]));
      return Promise.resolve(jsonResponse([]));
    });

    renderPage();
    await screen.findByText("alice");

    fireEvent.click(screen.getByRole("tab", { name: "Outside collaborators" }));
    expect(await screen.findByText("contractor")).toBeInTheDocument();
  });
});

const bob = { id: 2, login: "bob", type: "User", site_admin: false, avatar_url: "" };

describe("OrgPeoplePage convert to outside collaborator", () => {
  it("converts a member via PUT /orgs/{org}/outside_collaborators/{login}", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (url === "/api/v3/user") return Promise.resolve(jsonResponse(alice));
      // The viewer is an org owner — the admin-only controls must render.
      if (url === "/api/v3/user/memberships/orgs/acme")
        return Promise.resolve(jsonResponse({ state: "active", role: "admin" }));
      if (url.startsWith("/api/v3/orgs/acme/members")) return Promise.resolve(jsonResponse([alice, bob]));
      if (url === "/api/v3/orgs/acme/public_members" && method === "GET")
        return Promise.resolve(jsonResponse([]));
      if (url === "/api/v3/orgs/acme/outside_collaborators/bob" && method === "PUT")
        return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(jsonResponse([]));
    });

    renderPage();

    fireEvent.click(await screen.findByLabelText("Convert bob to outside collaborator"));
    // Confirm the action in the dialog.
    fireEvent.click(await screen.findByRole("button", { name: "Convert" }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/orgs/acme/outside_collaborators/bob" && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
    });
  });
});
