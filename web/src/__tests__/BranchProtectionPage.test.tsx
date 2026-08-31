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
      <MemoryRouter initialEntries={["/ui/admin/bp-repo/settings/branch-protection"]}>
        <Routes>
          <Route path="/ui/:owner/:repo/settings/branch-protection" element={<BranchProtectionPage />} />
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
  permissions: { admin: true, push: true, pull: true },
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
  if (u.endsWith("/branch-protection-patterns")) return jsonResponse([]);
  if (u.includes("/issues") || u.includes("/pulls")) return jsonResponse([]);
  return null;
}

const patternsURL = "/ui-data/repos/admin/bp-repo/branch-protection-patterns";

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

  it("round-trips lock branch, fork syncing, and last-push approval toggles", async () => {
    const fullProtection = {
      ...mainProtection,
      required_pull_request_reviews: {
        required_approving_review_count: 2,
        dismiss_stale_reviews: false,
        require_code_owner_reviews: false,
        require_last_push_approval: true,
      },
      lock_branch: { enabled: true },
      allow_fork_syncing: { enabled: true },
    };
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.endsWith("/branches/main/protection")) {
        if (opts?.method === "PUT") return Promise.resolve(jsonResponse(fullProtection));
        return Promise.resolve(jsonResponse(fullProtection));
      }
      const r = route(u);
      return Promise.resolve(r ?? jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();

    // The three toggles render from the GET's {enabled}/boolean members.
    const lock = await screen.findByRole("checkbox", { name: "Lock branch" });
    expect(lock).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Allow fork syncing" })).toBeChecked();
    expect(
      screen.getByRole("checkbox", { name: "Require approval of the most recent reviewable push" }),
    ).toBeChecked();

    // Flip one off to prove the PUT carries live values, then save.
    fireEvent.click(lock);
    fireEvent.click(screen.getByRole("button", { name: /save changes/i }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/branches/main/protection") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      const body = JSON.parse(String(put![1].body));
      expect(body.lock_branch).toBe(false);
      expect(body.allow_fork_syncing).toBe(true);
      expect(body.required_pull_request_reviews.require_last_push_approval).toBe(true);
    });
  });

  it("saves a wildcard rule via the pattern endpoint with the full array", async () => {
    const patternPuts: unknown[] = [];
    mockFetch.mockImplementation((url: string, opts?: { method?: string; body?: unknown }) => {
      const u = url.toString();
      if (u.endsWith("/branch-protection-patterns")) {
        if (opts?.method === "PUT") {
          patternPuts.push(JSON.parse(String(opts.body)));
          return Promise.resolve(jsonResponse(JSON.parse(String(opts.body))));
        }
        return Promise.resolve(jsonResponse([]));
      }
      const r = route(u);
      return Promise.resolve(r ?? jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    await waitFor(() => screen.getByLabelText(/branch name for a new rule/i));

    fireEvent.change(screen.getByLabelText(/branch name for a new rule/i), {
      target: { value: "release/*" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add rule" }));

    await waitFor(() => {
      expect(screen.getByRole("option", { name: "release/* (pattern)" })).toBeInTheDocument();
    });

    // The editor resets to an unprotected form for the new pattern.
    await waitFor(() =>
      expect(screen.getByRole("checkbox", { name: "Protect this branch" })).not.toBeChecked(),
    );
    fireEvent.click(screen.getByRole("checkbox", { name: "Protect this branch" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Include administrators" }));
    fireEvent.click(screen.getByRole("button", { name: /save changes/i }));

    await waitFor(() => expect(patternPuts).toHaveLength(1));
    const body = patternPuts[0] as { pattern: string; protection: Record<string, unknown> }[];
    expect(body).toHaveLength(1);
    expect(body[0]!.pattern).toBe("release/*");
    expect(body[0]!.protection.enforce_admins).toBe(true);
    expect(String(mockFetch.mock.calls.find((c) => c[1]?.method === "PUT")![0])).toBe(patternsURL);

    // No REST protection call was made for the wildcard name.
    const wildcardRest = mockFetch.mock.calls.find((c) => String(c[0]).includes("/branches/release"));
    expect(wildcardRest).toBeFalsy();
  });

  it("keeps the REST PUT path for exact branch names typed into add rule", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.endsWith("/branches/main/protection") && opts?.method === "PUT") {
        return Promise.resolve(jsonResponse(mainProtection));
      }
      const r = route(u);
      return Promise.resolve(r ?? jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    await waitFor(() => screen.getByLabelText(/branch name for a new rule/i));

    fireEvent.change(screen.getByLabelText(/branch name for a new rule/i), {
      target: { value: "main" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add rule" }));

    // main is protected → the form arrives enabled; just save.
    await waitFor(() =>
      expect(screen.getByRole("checkbox", { name: "Protect this branch" })).toBeChecked(),
    );
    fireEvent.click(screen.getByRole("button", { name: /save changes/i }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/branches/main/protection") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
    });
    const patternPut = mockFetch.mock.calls.find(
      (c) => String(c[0]).endsWith("/branch-protection-patterns") && c[1]?.method === "PUT",
    );
    expect(patternPut).toBeFalsy();
  });

  it("lists pattern rules with a Pattern badge and deletes one by PUTting the remainder", async () => {
    const storedPatterns = [
      { pattern: "release/*", protection: { enforce_admins: { enabled: true } } },
      { pattern: "hotfix/**", protection: { allow_force_pushes: { enabled: true } } },
    ];
    mockFetch.mockImplementation((url: string, opts?: { method?: string; body?: unknown }) => {
      const u = url.toString();
      if (u.endsWith("/branch-protection-patterns")) {
        if (opts?.method === "PUT") return Promise.resolve(jsonResponse(JSON.parse(String(opts.body))));
        return Promise.resolve(jsonResponse(storedPatterns));
      }
      const r = route(u);
      return Promise.resolve(r ?? jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();

    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Delete rule for release/*" })).toBeInTheDocument(),
    );
    expect(screen.getAllByText("Pattern")).toHaveLength(2);
    expect(screen.getByText("hotfix/**")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Delete rule for release/*" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/branch-protection-patterns") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
      expect(JSON.parse(String(put![1].body))).toEqual([
        { pattern: "hotfix/**", protection: { allow_force_pushes: { enabled: true } } },
      ]);
    });
  });

  it("DELETEs the pattern endpoint when removing the last pattern rule", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      const u = url.toString();
      if (u.endsWith("/branch-protection-patterns")) {
        if (opts?.method === "DELETE") return Promise.resolve(new Response(null, { status: 204 }));
        return Promise.resolve(
          jsonResponse([{ pattern: "release/*", protection: { enforce_admins: { enabled: true } } }]),
        );
      }
      const r = route(u);
      return Promise.resolve(r ?? jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Delete rule for release/*" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/branch-protection-patterns") && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
    const put = mockFetch.mock.calls.find(
      (c) => String(c[0]).endsWith("/branch-protection-patterns") && c[1]?.method === "PUT",
    );
    expect(put).toBeFalsy();
  });
});

describe("BranchProtectionPage non-admin guard", () => {
  it("renders a GitHub-style 404 for a non-admin viewer instead of the editor", async () => {
    const viewerRepo = { ...repo, permissions: { admin: false, push: false, pull: true } };
    mockFetch.mockImplementation((url: string) => {
      const u = url.toString();
      if (u === "/api/v3/repos/admin/bp-repo") return Promise.resolve(jsonResponse(viewerRepo));
      const r = route(u);
      return Promise.resolve(r ?? jsonResponse({ message: "Not Found" }, 404));
    });
    renderPage();
    expect(await screen.findByText("This page does not exist")).toBeInTheDocument();
    expect(screen.queryByText("Branch protection rules")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save changes" })).not.toBeInTheDocument();
  });
});
