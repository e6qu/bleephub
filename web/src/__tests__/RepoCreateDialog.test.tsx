import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RepoCreateDialog } from "../components/RepoCreateDialog.js";

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

function renderDialog(props: Partial<Parameters<typeof RepoCreateDialog>[0]> = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <RepoCreateDialog open onClose={() => {}} onCreated={() => {}} {...props} />
    </QueryClientProvider>,
  );
}

describe("RepoCreateDialog template source", () => {
  it("generates from the selected template via POST .../generate instead of create", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.startsWith("/api/v3/user/repos") && (!init || init.method === undefined || init.method === "GET")) {
        // Only the template repo is is_template.
        return Promise.resolve(
          jsonResponse([
            { id: 1, full_name: "admin/starter", is_template: true },
            { id: 2, full_name: "admin/plain", is_template: false },
          ]),
        );
      }
      if (url === "/api/v3/repos/admin/starter/generate" && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ id: 3, full_name: "admin/from-template" }, 201));
      }
      return Promise.resolve(jsonResponse([]));
    });

    renderDialog();

    // Only the is_template repo is offered.
    const select = await screen.findByRole("combobox", { name: "Repository template" });
    expect(screen.queryByRole("option", { name: "admin/plain" })).toBeNull();
    fireEvent.change(select, { target: { value: "admin/starter" } });

    fireEvent.change(screen.getByPlaceholderText("name"), { target: { value: "from-template" } });
    fireEvent.click(screen.getByRole("button", { name: "Create repository" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/repos/admin/starter/generate" && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
      expect(JSON.parse(String(post![1].body))).toEqual({
        name: "from-template",
        description: "",
        private: false,
      });
    });
    // No plain create call was made.
    expect(
      mockFetch.mock.calls.find((c) => String(c[0]) === "/api/v3/user/repos" && c[1]?.method === "POST"),
    ).toBeUndefined();
  });

  it("creates a plain repo when no template is chosen", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.startsWith("/api/v3/user/repos") && init?.method === "POST") {
        return Promise.resolve(jsonResponse({ id: 4, full_name: "admin/plainnew" }, 201));
      }
      if (url.startsWith("/api/v3/user/repos")) return Promise.resolve(jsonResponse([]));
      return Promise.resolve(jsonResponse([]));
    });

    renderDialog();

    fireEvent.change(await screen.findByPlaceholderText("name"), { target: { value: "plainnew" } });
    fireEvent.click(screen.getByRole("button", { name: "Create repository" }));

    await waitFor(() => {
      const post = mockFetch.mock.calls.find(
        (c) => String(c[0]) === "/api/v3/user/repos" && c[1]?.method === "POST",
      );
      expect(post).toBeDefined();
    });
  });
});
