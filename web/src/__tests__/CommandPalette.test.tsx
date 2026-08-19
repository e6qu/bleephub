import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, useLocation } from "react-router";
import { CommandPalette } from "../components/CommandPalette.js";

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

function LocationProbe() {
  const loc = useLocation();
  return <div data-testid="loc">{loc.pathname + loc.search}</div>;
}

function renderPalette(onClose = vi.fn()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/ui/"]}>
        <CommandPalette open onClose={onClose} viewerLogin="octocat" />
        <Routes>
          <Route path="*" element={<LocationProbe />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { onClose };
}

describe("CommandPalette", () => {
  it("shows static jump-to targets and filters them by query", async () => {
    renderPalette();
    // dialog + combobox present
    expect(screen.getByRole("dialog", { name: /command palette/i })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /Dashboard/ })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /Notifications/ })).toBeInTheDocument();

    fireEvent.change(screen.getByRole("combobox"), { target: { value: "notif" } });
    await waitFor(() => {
      expect(screen.getByRole("option", { name: /Notifications/ })).toBeInTheDocument();
      expect(screen.queryByRole("option", { name: /^Dashboard/ })).not.toBeInTheDocument();
    });
  });

  it("searches repositories and navigates to the chosen one on Enter", async () => {
    mockFetch.mockImplementation((url: RequestInfo | URL) => {
      const u = url.toString();
      if (u.includes("/search/repositories")) {
        return Promise.resolve(
          jsonResponse({ total_count: 1, incomplete_results: false, items: [{ id: 7, full_name: "octocat/hello", description: "hi" }] }),
        );
      }
      if (u.includes("/search/users")) {
        return Promise.resolve(jsonResponse({ total_count: 0, incomplete_results: false, items: [] }));
      }
      return Promise.resolve(jsonResponse({ total_count: 0, incomplete_results: false, items: [] }));
    });
    renderPalette();

    fireEvent.change(screen.getByRole("combobox"), { target: { value: "hello" } });
    const repoOption = await screen.findByRole("option", { name: /octocat\/hello/ });
    expect(repoOption).toBeInTheDocument();

    fireEvent.click(repoOption);
    await waitFor(() => {
      expect(screen.getByTestId("loc").textContent).toBe("/ui/repos/octocat/hello");
    });
  });

  it("navigates to the active static target on Enter", async () => {
    renderPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "notif" } });
    // Wait for the debounced filter to settle so Notifications is the only
    // (and therefore active) option before pressing Enter.
    await waitFor(() =>
      expect(screen.queryByRole("option", { name: /Dashboard/ })).not.toBeInTheDocument(),
    );
    fireEvent.keyDown(screen.getByRole("combobox"), { key: "Enter" });
    await waitFor(() => {
      expect(screen.getByTestId("loc").textContent).toBe("/ui/notifications");
    });
  });

  it("scopes the Issues / Pull requests targets to the signed-in viewer", async () => {
    renderPalette();
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "your issues" } });
    const option = await screen.findByRole("option", { name: /Your issues/ });
    fireEvent.click(option);
    await waitFor(() => {
      expect(decodeURIComponent(screen.getByTestId("loc").textContent!)).toBe(
        "/ui/search?type=issues&q=is:issue author:octocat",
      );
    });
  });

  it("closes on Escape", async () => {
    const { onClose } = renderPalette();
    fireEvent.keyDown(screen.getByRole("combobox"), { key: "Escape" });
    expect(onClose).toHaveBeenCalled();
  });
});
