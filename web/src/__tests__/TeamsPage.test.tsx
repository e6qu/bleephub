import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { TeamsPage } from "../pages/TeamsPage.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/ui/admin/teams"]}>
        <Routes>
          <Route path="/ui/admin/teams" element={<TeamsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  mockFetch.mockReset();
});

describe("TeamsPage", () => {
  it("loads teams and creates one through the public organization API", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === "/api/v3/user/teams?per_page=100" && !init?.method) {
        return Promise.resolve(jsonResponse([]));
      }
      if (url === "/api/v3/orgs/acme/teams" && init?.method === "POST") {
        return Promise.resolve(
          jsonResponse(
            {
              id: 7,
              slug: "platform",
              name: "Platform",
              description: "Build systems",
              privacy: "closed",
              organization: { login: "acme" },
              created_at: "2026-07-29T00:00:00Z",
            },
            201,
          ),
        );
      }
      return Promise.reject(new Error(`unexpected request: ${init?.method ?? "GET"} ${url}`));
    });

    renderPage();
    expect(await screen.findByText("No teams yet.")).toBeInTheDocument();

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "New team" }));
    const create = screen.getByRole("button", { name: "Create team" });
    expect(create).toBeDisabled();

    await user.type(screen.getByLabelText("Organization login"), "acme");
    await user.type(screen.getByLabelText("Name"), "Platform");
    await user.type(screen.getByLabelText("Description"), "Build systems");
    await user.selectOptions(screen.getByLabelText("Privacy"), "closed");
    expect(create).toBeEnabled();
    await user.click(create);

    await waitFor(() => {
      expect(screen.queryByRole("dialog", { name: "Create team" })).not.toBeInTheDocument();
    });
    const createCall = mockFetch.mock.calls.find(
      ([url, init]) => url === "/api/v3/orgs/acme/teams" && init?.method === "POST",
    );
    expect(createCall).toBeDefined();
    expect(JSON.parse(String(createCall?.[1]?.body))).toEqual({
      name: "Platform",
      description: "Build systems",
      privacy: "closed",
    });
  });

  it("keeps the dialog open and renders an API validation failure", async () => {
    mockFetch.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) === "/api/v3/user/teams?per_page=100" && !init?.method) {
        return Promise.resolve(jsonResponse([]));
      }
      return Promise.resolve(jsonResponse({ message: "Name has already been taken" }, 422));
    });

    renderPage();
    await screen.findByText("No teams yet.");
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "New team" }));
    await user.type(screen.getByLabelText("Organization login"), "acme");
    await user.type(screen.getByLabelText("Name"), "Platform");
    await user.click(screen.getByRole("button", { name: "Create team" }));

    expect(await screen.findByText(/Name has already been taken/i)).toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "Create team" })).toBeInTheDocument();
  });
});
