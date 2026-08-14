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
    fireEvent.click(screen.getByRole("button", { name: "react with heart" }));
    await waitFor(() => {
      const call = mockFetch.mock.calls.find(
        (c) => String(c[0]).endsWith("/releases/1/reactions") && c[1]?.method === "POST",
      );
      expect(call).toBeDefined();
      expect(JSON.parse(String(call![1].body))).toEqual({ content: "heart" });
    });
  });
});
