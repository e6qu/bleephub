import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { BranchProtectionPage } from "../pages/BranchProtectionPage.js";

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
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/ui/repos/admin/bp-repo/settings/branch-protection"]}>
        <Routes>
          <Route path="/ui/repos/:owner/:repo/settings/branch-protection" element={<BranchProtectionPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const repo = {
  id: 1,
  name: "bp-repo",
  full_name: "admin/bp-repo",
  default_branch: "main",
  owner: { login: "admin", type: "User" },
};

const mainProtection = {
  url: "",
  html_url: "",
  required_status_checks: { strict: true, enforcement_level: "non_admins", contexts: ["ci"], checks: [] },
  required_pull_request_reviews: null,
  restrictions: null,
  enforce_admins: { enabled: false },
  allow_force_pushes: { enabled: false },
  allow_deletions: { enabled: false },
};

function route(u: string): Response | null {
  if (u === "/api/v3/repos/admin/bp-repo") return jsonResponse(repo);
  if (u.endsWith("/branches?per_page=100") || u.endsWith("/branches")) {
    return jsonResponse([
      { name: "main", protected: true, commit: { sha: "a" } },
      { name: "dev", protected: false, commit: { sha: "b" } },
    ]);
  }
  if (u.endsWith("/branches/main/protection")) return jsonResponse(mainProtection);
  if (u.includes("/issues") || u.includes("/pulls")) return jsonResponse([]);
  return null;
}

describe("BranchProtectionPage", () => {
  it("lists the current protection rules with a summary", async () => {
    mockFetch.mockImplementation((url: string) => {
      const r = route(url.toString());
      return Promise.resolve(r ?? jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();

    await waitFor(() => expect(screen.getByRole("button", { name: "Edit rule for main" })).toBeInTheDocument());
    expect(screen.getByText("status checks", { exact: true })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete rule for main" })).toBeInTheDocument();
  });

  it("deletes a rule after confirmation", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.endsWith("/branches/main/protection") && opts?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      const r = route(u);
      return Promise.resolve(r ?? jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Delete rule for main" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/branches/main/protection") && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });

  it("adds a rule by branch name, targeting the PUT at that name", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.endsWith("/protection") && opts?.method === "PUT") {
        return Promise.resolve(jsonResponse(mainProtection));
      }
      if (u.endsWith("/branches/release%2F1.x/protection")) {
        return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
      }
      const r = route(u);
      return Promise.resolve(r ?? jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    await waitFor(() => screen.getByLabelText(/branch name for a new rule/i));

    fireEvent.change(screen.getByLabelText(/branch name for a new rule/i), {
      target: { value: "release/1.x" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add rule" }));

    // The form below now targets the typed name (shown as a synthetic option).
    await waitFor(() => {
      expect(screen.getByRole("option", { name: "release/1.x (new rule)" })).toBeInTheDocument();
    });

    fireEvent.click(await screen.findByRole("checkbox", { name: "Protect this branch" }));
    fireEvent.click(screen.getByRole("button", { name: /save changes/i }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/branches/release%2F1.x/protection") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
    });
  });

  it("sends dismissal restrictions inside required_pull_request_reviews", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.endsWith("/branches/main/protection") && opts?.method === "PUT") {
        return Promise.resolve(jsonResponse(mainProtection));
      }
      const r = route(u);
      return Promise.resolve(r ?? jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();

    // main is preselected and protected → form arrives enabled.
    const prCheckbox = await screen.findByRole("checkbox", { name: "Require a pull request before merging" });
    fireEvent.click(prCheckbox);
    fireEvent.click(screen.getByRole("checkbox", { name: "Restrict who can dismiss pull request reviews" }));
    fireEvent.change(screen.getByLabelText(/users who can dismiss/i), { target: { value: "alice\nbob" } });
    fireEvent.change(screen.getByLabelText(/teams who can dismiss/i), { target: { value: "platform" } });
    fireEvent.click(screen.getByRole("button", { name: /save changes/i }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/branches/main/protection") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      const body = JSON.parse(String(put![1].body));
      expect(body.required_pull_request_reviews.dismissal_restrictions).toEqual({
        users: [{ login: "alice" }, { login: "bob" }],
        teams: [{ login: "platform" }],
      });
    });
  });
});
