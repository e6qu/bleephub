import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router";
import { RunnersPage } from "../pages/RunnersPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown) {
  return new Response(JSON.stringify(data), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
  // Scope selection persists in the URL query — reset it between tests.
  window.history.replaceState({}, "", "/");
});

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <RunnersPage />
      </BrowserRouter>
    </QueryClientProvider>,
  );
}

const reposData = [
  {
    id: 1,
    name: "test",
    full_name: "admin/test",
    description: "",
    default_branch: "main",
    visibility: "public",
    private: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
];

const runnersData = {
  total_count: 1,
  runners: [
    {
      id: 7,
      name: "gh-runner-7",
      os: "linux",
      status: "online",
      busy: true,
      labels: [
        { id: 1, name: "self-hosted", type: "read-only" },
        { id: 2, name: "linux", type: "read-only" },
        { id: 3, name: "gpu", type: "custom" },
      ],
    },
  ],
};

/** URL-routed mock — each call gets a fresh Response (bodies are single-read). */
function installMocks() {
  mockFetch.mockImplementation((url: RequestInfo | URL) => {
    const u = url.toString();
    if (u === "/api/v3/user/repos?per_page=100") return Promise.resolve(jsonResponse(reposData));
    if (u.includes("/actions/runners")) return Promise.resolve(jsonResponse(runnersData));
    return Promise.resolve(jsonResponse([]));
  });
}

describe("RunnersPage", () => {
  it("renders the runners heading", async () => {
    installMocks();
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /registered runners/i })).toBeInTheDocument();
    });
  });

  it("lists registered runners from the GitHub Actions Representational State Transfer endpoint", async () => {
    installMocks();
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("gh-runner-7")).toBeInTheDocument();
    });
    expect(screen.getByText("self-hosted, linux, gpu")).toBeInTheDocument();
    expect(screen.getByText("yes")).toBeInTheDocument();
    expect(screen.getByText("online")).toBeInTheDocument();
    const calls = mockFetch.mock.calls.map((c) => c[0].toString());
    expect(calls).toContain("/api/v3/user/repos?per_page=100");
    expect(calls).toContain("/api/v3/repos/admin/test/actions/runners");
    expect(calls).not.toContain("/internal/repos");
    expect(calls).not.toContain("/internal/sessions");
  });

  it("does not call the runner endpoint until a public repository path exists", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u === "/api/v3/user/repos?per_page=100") return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });

    renderPage();
    await waitFor(() => {
      expect(
        screen.getByText("Create a repository to query the GitHub Actions runner registry."),
      ).toBeInTheDocument();
    });

    const calls = mockFetch.mock.calls.map((c) => c[0].toString());
    expect(calls).toContain("/api/v3/user/repos?per_page=100");
    expect(calls.some((c) => c.includes("/actions/runners"))).toBe(false);
    expect(calls).not.toContain("/internal/sessions");
  });
});

describe("RunnersPage scope selection", () => {
  it("targets the repository selected via the searchable picker and persists it in the URL", async () => {
    const twoRepos = [
      ...reposData,
      { ...reposData[0], id: 2, name: "second", full_name: "admin/second" },
    ];
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u === "/api/v3/user/repos?per_page=100") return Promise.resolve(jsonResponse(twoRepos));
      if (u.includes("/actions/runners")) return Promise.resolve(jsonResponse(runnersData));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await screen.findByText("gh-runner-7");
    fireEvent.change(screen.getByLabelText("Find a repository"), { target: { value: "sec" } });
    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "admin/second" } });
    await waitFor(() => {
      const calls = mockFetch.mock.calls.map((c) => c[0].toString());
      expect(calls).toContain("/api/v3/repos/admin/second/actions/runners");
    });
    expect(window.location.search).toContain("repo=admin%2Fsecond");
  });

  it("offers an organization scope backed by the /orgs runner routes", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u === "/api/v3/user/orgs?per_page=100") {
        return Promise.resolve(jsonResponse([{ id: 5, login: "acme", name: "Acme", description: "", members_can_create_repositories: true, created_at: "2026-01-01T00:00:00Z" }]));
      }
      if (u === "/api/v3/user/repos?per_page=100") return Promise.resolve(jsonResponse(reposData));
      if (u.endsWith("/actions/runners/registration-token") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ token: "ORGTOK", expires_at: "2026-01-01T01:00:00Z" }));
      }
      if (u.includes("/actions/runners")) return Promise.resolve(jsonResponse(runnersData));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    await screen.findByText("gh-runner-7");
    fireEvent.change(screen.getByLabelText("Scope"), { target: { value: "org" } });
    await waitFor(() => {
      const calls = mockFetch.mock.calls.map((c) => c[0].toString());
      expect(calls).toContain("/api/v3/orgs/acme/actions/runners");
    });
    expect(window.location.search).toContain("org=acme");
    // Registration token minted against the org route.
    fireEvent.click(await screen.findByRole("button", { name: /add runner/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) =>
          c[0].toString() === "/api/v3/orgs/acme/actions/runners/registration-token" &&
          c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
    });
    expect(await screen.findByText(/ORGTOK/)).toBeInTheDocument();
  });
});

describe("RunnersPage add runner", () => {
  it("generates a registration token and shows the full GitHub-style register block", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/actions/runners/registration-token") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ token: "AABBCC", expires_at: "2026-01-01T01:00:00Z" }));
      }
      if (u === "/api/v3/user/repos?per_page=100") return Promise.resolve(jsonResponse(reposData));
      if (u.includes("/actions/runners")) return Promise.resolve(jsonResponse(runnersData));
      return Promise.resolve(jsonResponse([]));
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /add runner/i }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/actions/runners/registration-token") && c[1]?.method === "POST",
      );
      expect(post).toBeTruthy();
    });
    // Full download/extract/configure/run block, not just config.sh.
    const block = await screen.findByText(/AABBCC/);
    expect(block.textContent).toContain("mkdir actions-runner && cd actions-runner");
    expect(block.textContent).toContain("tar xzf ./actions-runner-linux-x64.tar.gz");
    expect(block.textContent).toContain("./config.sh --url");
    expect(block.textContent).toContain("./run.sh");
    // OS tabs switch the script flavor.
    fireEvent.click(screen.getByRole("tab", { name: "Windows" }));
    expect((await screen.findByText(/AABBCC/)).textContent).toContain("./config.cmd --url");
  });

  it("removes a runner via DELETE after confirming", async () => {
    installMocks();
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Remove runner gh-runner-7" }));
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => /\/actions\/runners\/\d+$/.test(c[0].toString()) && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });
});
