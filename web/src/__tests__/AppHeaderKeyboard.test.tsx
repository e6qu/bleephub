import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "@bleephub/ui-core/components";
import { AppHeader } from "../components/AppHeader.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

afterEach(() => {
  mockFetch.mockReset();
});

function renderHeader() {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: vi.fn(() => ({
      matches: false,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  });
  mockFetch.mockImplementation((input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/api/v3/user")) {
      return Promise.resolve(new Response(JSON.stringify({ login: "admin" }), { status: 200 }));
    }
    return Promise.resolve(new Response(JSON.stringify([]), { status: 200 }));
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter>
          <AppHeader />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("AppHeader menus", () => {
  it("supports focus entry, arrow navigation, and Escape restoration", async () => {
    renderHeader();
    const user = userEvent.setup();
    const trigger = screen.getByRole("button", { name: "Create new…" });
    await user.click(trigger);

    const first = screen.getByRole("menuitem", { name: "New repository" });
    const second = screen.getByRole("menuitem", { name: "New gist" });
    await waitFor(() => expect(first).toHaveFocus());

    await user.keyboard("{ArrowDown}");
    expect(second).toHaveFocus();
    await user.keyboard("{End}");
    expect(screen.getByRole("menuitem", { name: "New organization" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu", { name: "Create new…" })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });
});
