import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { ProfilePage } from "../pages/ProfilePage.js";

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
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ui/users/:login" element={<ProfilePage />} />
          <Route path="/ui/:login" element={<ProfilePage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const profile = {
  login: "octocat",
  id: 1,
  avatar_url: "",
  type: "User",
  site_admin: false,
  name: "The Octocat",
  email: "octo@example.com",
  bio: "builds runners",
  blog: "https://octo.example",
  company: "Acme",
  location: "Internet",
  twitter_username: "octo",
  followers: 5,
  following: 3,
  public_repos: 1,
  created_at: "2026-01-01T00:00:00Z",
};

const repo = {
  id: 1,
  name: "api",
  full_name: "octocat/api",
  description: "the api",
  default_branch: "main",
  visibility: "public",
  private: false,
  updated_at: "2026-02-01T00:00:00Z",
};

function mockProfileEndpoints() {
  mockFetch.mockImplementation((url: RequestInfo | URL) => {
    const u = url.toString();
    if (u.includes("/repos")) return Promise.resolve(jsonResponse([repo], 200, { Link: "" }));
    if (u.includes("/orgs")) return Promise.resolve(jsonResponse([{ login: "acme", id: 2, avatar_url: "", description: "acme" }]));
    return Promise.resolve(jsonResponse(profile));
  });
}

describe("ProfilePage", () => {
  it("renders the profile sidebar, repos, and orgs at /ui/:login", async () => {
    mockProfileEndpoints();
    renderAt("/ui/octocat");

    await waitFor(() => {
      expect(screen.getByText("The Octocat")).toBeInTheDocument();
    });
    expect(screen.getByText("builds runners")).toBeInTheDocument();
    expect(screen.getByText("5")).toBeInTheDocument(); // followers
    expect(await screen.findByText("api")).toBeInTheDocument();
    expect(screen.getByText("Organizations")).toBeInTheDocument();
  });

  it("resolves the same page at /ui/users/:login", async () => {
    mockProfileEndpoints();
    renderAt("/ui/users/octocat");
    await waitFor(() => {
      expect(screen.getByText("The Octocat")).toBeInTheDocument();
    });
    expect(mockFetch).toHaveBeenCalledWith("/api/v3/users/octocat", expect.anything());
  });

  it("surfaces a load error instead of a blank profile", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/repos")) return Promise.resolve(jsonResponse([], 200, { Link: "" }));
      if (u.includes("/orgs")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse({ message: "Not Found" }, 404));
    });
    renderAt("/ui/ghost");
    await waitFor(() => {
      expect(screen.getByText(/failed to load profile/i)).toBeInTheDocument();
    });
  });
});

describe("ProfilePage follow", () => {
  function mockFollow(following: boolean) {
    mockFetch.mockImplementation((url: RequestInfo | URL, init?: RequestInit) => {
      const u = url.toString();
      if (u.endsWith("/user/following/octocat") && init?.method === "PUT") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.endsWith("/user/following/octocat") && init?.method === "DELETE") {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (u.endsWith("/user/following/octocat")) {
        return Promise.resolve(new Response(null, { status: following ? 204 : 404 }));
      }
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "me", id: 9 }));
      if (u.includes("/repos")) return Promise.resolve(jsonResponse([repo], 200, { Link: "" }));
      if (u.includes("/orgs")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse(profile));
    });
  }

  it("follows a user from their profile", async () => {
    mockFollow(false);
    renderAt("/ui/octocat");
    fireEvent.click(await screen.findByRole("button", { name: /^follow$/i }));
    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/user/following/octocat") && c[1]?.method === "PUT",
      );
      expect(put).toBeTruthy();
    });
  });

  it("unfollows when already following", async () => {
    mockFollow(true);
    renderAt("/ui/octocat");
    fireEvent.click(await screen.findByRole("button", { name: /^unfollow$/i }));
    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0].toString().endsWith("/user/following/octocat") && c[1]?.method === "DELETE",
      );
      expect(del).toBeTruthy();
    });
  });

  it("hides the follow button on your own profile", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.endsWith("/api/v3/user")) return Promise.resolve(jsonResponse({ login: "octocat", id: 1 }));
      if (u.includes("/repos")) return Promise.resolve(jsonResponse([repo], 200, { Link: "" }));
      if (u.includes("/orgs")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse(profile));
    });
    renderAt("/ui/octocat");
    await screen.findByText("The Octocat");
    expect(screen.queryByRole("button", { name: /^follow$/i })).not.toBeInTheDocument();
  });
});
