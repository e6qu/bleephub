import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { ReleasesPage } from "../pages/ReleasesPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

const asset = { id: 9, name: "artifact.txt", label: "Linux artifact", size: 24, download_count: 0 };
const release = {
  id: 1, tag_name: "v1.0.0", target_commitish: "main", name: "First release", body: "notes",
  draft: false, prerelease: false, created_at: "2026-07-12T00:00:00Z", published_at: "2026-07-12T00:00:00Z",
  author: { login: "admin" }, assets: [asset], upload_url: "", html_url: "", url: "",
};
const repo = {
  id: 1, name: "release", full_name: "admin/release", private: false, visibility: "public", default_branch: "main",
  owner: { login: "admin", type: "User" }, has_issues: true, has_projects: true, has_wiki: true,
  permissions: { admin: true, push: true, pull: true },
};

function response(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), { status, headers: { "Content-Type": "application/json" } });
}

afterEach(() => { cleanup(); mockFetch.mockReset(); });

describe("ReleasesPage", () => {
  it("fills the notes via POST /releases/generate-notes", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith("/repos/admin/release/releases/generate-notes") && init?.method === "POST") {
        return Promise.resolve(response({ name: "v2.0.0", body: "## What's Changed\n* Everything" }));
      }
      if (url === "/api/v3/repos/admin/release") return Promise.resolve(response(repo));
      return Promise.resolve(response([]));
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/ui/repos/admin/release/releases/new"]}><Routes><Route path="/ui/repos/:owner/:repo/releases/new" element={<ReleasesPage />} /></Routes></MemoryRouter></QueryClientProvider>);

    fireEvent.change(await screen.findByLabelText("Tag"), { target: { value: "v2.0.0" } });
    fireEvent.click(screen.getByRole("button", { name: "Generate release notes" }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/releases/generate-notes") && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({ tag_name: "v2.0.0", target_commitish: undefined });
    });
    await waitFor(() =>
      expect((screen.getByLabelText("Release notes") as HTMLTextAreaElement).value).toContain("What's Changed"),
    );
  });

  it("sends make_latest from the 'Set as the latest release' checkbox", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === "/api/v3/repos/admin/release/releases" && init?.method === "POST") {
        return Promise.resolve(response({ ...release, tag_name: "v3.0.0" }, 201));
      }
      if (url === "/api/v3/repos/admin/release") return Promise.resolve(response(repo));
      return Promise.resolve(response([]));
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/ui/repos/admin/release/releases/new"]}><Routes><Route path="/ui/repos/:owner/:repo/releases/new" element={<ReleasesPage />} /></Routes></MemoryRouter></QueryClientProvider>);

    fireEvent.change(await screen.findByLabelText("Tag"), { target: { value: "v3.0.0" } });
    // Checked by default (GitHub's on-by-default); uncheck to exclude from latest.
    fireEvent.click(screen.getByRole("checkbox", { name: "Set as the latest release" }));
    fireEvent.click(screen.getByRole("button", { name: "Create release" }));
    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/repos/admin/release/releases" && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body)).make_latest).toBe("false");
    });
  });

  it("returns from editing with the updated release and its assets intact", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method === "PATCH" && url.endsWith("/releases/1")) return Promise.resolve(response({ ...release, name: "Updated release" }));
      if (url.endsWith("/releases/1")) return Promise.resolve(response(release));
      if (url === "/api/v3/repos/admin/release") return Promise.resolve(response(repo));
      if (url.includes("/ui-data/repos/admin/release/viewer")) return Promise.resolve(response({ starred: false, subscribed: false }));
      if (url.includes("/issues") || url.includes("/pulls") || url.includes("/branches")) return Promise.resolve(response([]));
      return Promise.resolve(response([]));
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/ui/repos/admin/release/releases/1"]}><Routes><Route path="/ui/repos/:owner/:repo/releases/:releaseId" element={<ReleasesPage />} /></Routes></MemoryRouter></QueryClientProvider>);

    expect(await screen.findByRole("button", { name: "Delete artifact.txt" })).toBeVisible();
    // The "author released this on date" line surfaces the release's author + published date.
    expect(screen.getByText(/released this/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    fireEvent.change(screen.getByLabelText("Release title"), { target: { value: "Updated release" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(screen.getByRole("heading", { name: "Updated release" })).toBeVisible());
    expect(screen.getByRole("button", { name: "Delete artifact.txt" })).toBeVisible();
  });

  it("adds a reaction to a release via POST /releases/{id}/reactions", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method === "POST" && url.endsWith("/releases/1/reactions")) {
        return Promise.resolve(
          response({ id: 3, content: "heart", user: { login: "admin" }, created_at: "2026-07-12T00:00:00Z" }, 201),
        );
      }
      if (url.endsWith("/releases/1/reactions")) return Promise.resolve(response([]));
      if (url.endsWith("/releases/1")) return Promise.resolve(response(release));
      if (url === "/api/v3/repos/admin/release") return Promise.resolve(response(repo));
      if (url.endsWith("/user")) return Promise.resolve(response({ login: "admin" }));
      return Promise.resolve(response([]));
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/ui/repos/admin/release/releases/1"]}><Routes><Route path="/ui/repos/:owner/:repo/releases/:releaseId" element={<ReleasesPage />} /></Routes></MemoryRouter></QueryClientProvider>);

    fireEvent.click(await screen.findByRole("button", { name: "add reaction" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "react with heart" }));
    await waitFor(() => {
      const call = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/releases/1/reactions") && c[1]?.method === "POST",
      );
      expect(call).toBeDefined();
      expect(JSON.parse(String(call![1].body))).toEqual({ content: "heart" });
    });
  });

  it("renders the release body as Markdown and links a linked discussion", async () => {
    const withDiscussion = {
      ...release,
      body: "## Highlights\n\nShipped **everything**.",
      discussion_url: "https://bleep.example/admin/release/discussions/7",
    };
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/releases/1/reactions")) return Promise.resolve(response([]));
      if (url.endsWith("/releases/1")) return Promise.resolve(response(withDiscussion));
      if (url === "/api/v3/repos/admin/release") return Promise.resolve(response(repo));
      if (url.endsWith("/user")) return Promise.resolve(response({ login: "admin" }));
      return Promise.resolve(response([]));
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/ui/repos/admin/release/releases/1"]}><Routes><Route path="/ui/repos/:owner/:repo/releases/:releaseId" element={<ReleasesPage />} /></Routes></MemoryRouter></QueryClientProvider>);

    // Markdown heading is rendered as a real <h2>, not literal "## Highlights".
    const heading = await screen.findByRole("heading", { name: "Highlights" });
    expect(heading.tagName).toBe("H2");
    // The linked discussion resolves to the in-app discussion route by number.
    const link = screen.getByRole("link", { name: "Join the release discussion" });
    expect(link.getAttribute("href")).toBe("/ui/repos/admin/release/discussions/7");
  });

  it("renders the index as a feed: notes markdown, chips, and source-code assets", async () => {
    const feed = [
      { ...release, id: 2, tag_name: "v2.0.0-rc.1", name: "Release candidate", prerelease: true, body: "", assets: [] },
      { ...release, id: 1, body: "## Changelog\n\nShipped **things**." },
    ];
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.split("?")[0] === "/api/v3/repos/admin/release/releases") return Promise.resolve(response(feed));
      if (url === "/api/v3/repos/admin/release") return Promise.resolve(response(repo));
      return Promise.resolve(response([]));
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/ui/repos/admin/release/releases"]}><Routes><Route path="/ui/repos/:owner/:repo/releases" element={<ReleasesPage />} /></Routes></MemoryRouter></QueryClientProvider>);

    // Notes render as markdown, not literal "## Changelog".
    const heading = await screen.findByRole("heading", { name: "Changelog" });
    expect(heading.tagName).toBe("H2");
    // Chips: the newest non-draft non-prerelease is Latest; the RC is Pre-release.
    expect(screen.getByText("Latest")).toBeInTheDocument();
    expect(screen.getByText("Pre-release")).toBeInTheDocument();
    // Uploaded asset plus the automatic source archives for the tag.
    expect(screen.getByRole("link", { name: "Linux artifact" })).toBeInTheDocument();
    const zips = screen.getAllByRole("link", { name: "Source code (zip)" });
    expect(zips[0]).toHaveAttribute("href", "/api/v3/repos/admin/release/zipball/v2.0.0-rc.1");
    expect(zips[1]).toHaveAttribute("href", "/api/v3/repos/admin/release/zipball/v1.0.0");
    expect(screen.getAllByRole("link", { name: "Source code (tar.gz)" })[1]).toHaveAttribute(
      "href",
      "/api/v3/repos/admin/release/tarball/v1.0.0",
    );
    // relative published date via <time>
    expect(document.querySelector("time")).not.toBeNull();
  });
});

describe("ReleasesPage read-only viewer gating", () => {
  const viewerRepo = { ...repo, permissions: { admin: false, push: false, pull: true } };

  it("keeps the feed readable but hides New release from a viewer", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.split("?")[0] === "/api/v3/repos/admin/release/releases") return Promise.resolve(response([release]));
      if (url === "/api/v3/repos/admin/release") return Promise.resolve(response(viewerRepo));
      return Promise.resolve(response([]));
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/ui/repos/admin/release/releases"]}><Routes><Route path="/ui/repos/:owner/:repo/releases" element={<ReleasesPage />} /></Routes></MemoryRouter></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "Releases" })).toBeInTheDocument();
    expect(screen.getByText("First release", { exact: false })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /new release/i })).not.toBeInTheDocument();
  });

  it("hides Edit/Delete and asset management on the detail page for a viewer", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/releases/1/reactions")) return Promise.resolve(response([]));
      if (url.endsWith("/releases/1")) return Promise.resolve(response(release));
      if (url === "/api/v3/repos/admin/release") return Promise.resolve(response(viewerRepo));
      if (url.endsWith("/user")) return Promise.resolve(response({ login: "viewer" }));
      return Promise.resolve(response([]));
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/ui/repos/admin/release/releases/1"]}><Routes><Route path="/ui/repos/:owner/:repo/releases/:releaseId" element={<ReleasesPage />} /></Routes></MemoryRouter></QueryClientProvider>);

    // Assets remain downloadable; every write control is gone.
    expect(await screen.findByRole("button", { name: "Download artifact.txt" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /delete$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete artifact.txt" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /upload asset/i })).not.toBeInTheDocument();
  });

  it("404s /releases/new for a viewer, GitHub-style", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/v3/repos/admin/release") return Promise.resolve(response(viewerRepo));
      return Promise.resolve(response([]));
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/ui/repos/admin/release/releases/new"]}><Routes><Route path="/ui/repos/:owner/:repo/releases/new" element={<ReleasesPage />} /></Routes></MemoryRouter></QueryClientProvider>);

    expect(await screen.findByText("This page does not exist")).toBeInTheDocument();
    expect(screen.queryByLabelText("Tag")).not.toBeInTheDocument();
  });
});
