import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { MigrationsPage } from "../pages/MigrationsPage.js";

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
      <MemoryRouter initialEntries={["/ui/migrations"]}>
        <MigrationsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const importRecord = {
  vcs: "git",
  use_lfs: false,
  vcs_url: "https://example.com/octo/source.git",
  status: "complete",
  status_text: null,
  failed_step: null,
  error_message: null,
  has_large_files: false,
  large_files_size: 0,
  large_files_count: 0,
  import_percent: 100,
  commit_count: 3,
  authors_count: 1,
};

const author = {
  id: 1,
  remote_id: "Octo Cat <octo@example.com>",
  remote_name: "Octo Cat",
  email: "octo@example.com",
  name: "Octo Cat",
};

// The user-migrations list backs the initial page render; default any other GET
// to an empty list so the import flow is what the assertions isolate.
function baseRouter(url: string) {
  const u = url.toString();
  if (u.includes("/import/authors")) return jsonResponse([author]);
  if (u.endsWith("/import")) return jsonResponse(importRecord);
  return jsonResponse([]);
}

describe("MigrationsPage import flow", () => {
  it("starts an import with a PUT carrying the clone URL", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (opts?.method === "PUT") return Promise.resolve(jsonResponse(importRecord, 201));
      return Promise.resolve(baseRouter(url));
    });
    renderPage();

    fireEvent.change(await screen.findByLabelText("Target owner"), { target: { value: "octo" } });
    fireEvent.change(screen.getByLabelText("Target repository"), { target: { value: "dest" } });
    fireEvent.change(screen.getByLabelText("Clone URL"), {
      target: { value: "https://example.com/octo/source.git" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Start import" }));

    await waitFor(() => {
      const put = mockFetch.mock.calls.find(
        (c) => c[0] === "/api/v3/repos/octo/dest/import" && c[1]?.method === "PUT",
      );
      expect(put).toBeDefined();
      expect(JSON.parse(String(put![1].body))).toMatchObject({
        vcs_url: "https://example.com/octo/source.git",
      });
    });
  });

  it("cancels an import with a DELETE", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (opts?.method === "PUT") return Promise.resolve(jsonResponse(importRecord, 201));
      if (opts?.method === "DELETE") return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(baseRouter(url));
    });
    renderPage();

    fireEvent.change(await screen.findByLabelText("Target owner"), { target: { value: "octo" } });
    fireEvent.change(screen.getByLabelText("Target repository"), { target: { value: "dest" } });
    fireEvent.change(screen.getByLabelText("Clone URL"), {
      target: { value: "https://example.com/octo/source.git" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Start import" }));

    // Status panel appears once the target is set and the GET resolves.
    fireEvent.click(await screen.findByRole("button", { name: "Cancel import" }));

    await waitFor(() => {
      const del = mockFetch.mock.calls.find(
        (c) => c[0] === "/api/v3/repos/octo/dest/import" && c[1]?.method === "DELETE",
      );
      expect(del).toBeDefined();
    });
  });

  it("remaps an author with a PATCH to .../import/authors/{id}", async () => {
    mockFetch.mockImplementation((url: string, opts?: { method?: string }) => {
      if (opts?.method === "PUT") return Promise.resolve(jsonResponse(importRecord, 201));
      if (opts?.method === "PATCH") return Promise.resolve(jsonResponse(author));
      return Promise.resolve(baseRouter(url));
    });
    renderPage();

    fireEvent.change(await screen.findByLabelText("Target owner"), { target: { value: "octo" } });
    fireEvent.change(screen.getByLabelText("Target repository"), { target: { value: "dest" } });
    fireEvent.change(screen.getByLabelText("Clone URL"), {
      target: { value: "https://example.com/octo/source.git" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Start import" }));

    // The author row loads from GET .../import/authors once the import GET resolves.
    const emailInput = await screen.findByLabelText("Email");
    fireEvent.change(emailInput, { target: { value: "new@example.com" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => {
      const patch = mockFetch.mock.calls.find(
        (c) =>
          c[0] === "/api/v3/repos/octo/dest/import/authors/1" && c[1]?.method === "PATCH",
      );
      expect(patch).toBeDefined();
      expect(JSON.parse(String(patch![1].body))).toMatchObject({ email: "new@example.com" });
    });
  });
});

describe("MigrationsPage organization scope", () => {
  it("populates the org picker from /user/orgs and loads the chosen org's migrations", async () => {
    mockFetch.mockImplementation((url: string) => {
      const u = url.toString();
      if (u.startsWith("/api/v3/user/orgs")) {
        return Promise.resolve(jsonResponse([{ id: 1, login: "acme" }, { id: 2, login: "initech" }]));
      }
      return Promise.resolve(baseRouter(u));
    });
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: "Organization" }));
    const select = await screen.findByLabelText("Organization");
    expect(screen.getByRole("option", { name: "acme" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "initech" })).toBeInTheDocument();

    fireEvent.change(select, { target: { value: "acme" } });
    await waitFor(() => {
      const get = mockFetch.mock.calls.find((c) => String(c[0]) === "/api/v3/orgs/acme/migrations");
      expect(get).toBeDefined();
    });
  });
});
