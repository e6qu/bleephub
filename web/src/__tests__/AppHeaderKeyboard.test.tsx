import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "@bleephub/ui-core/components";
import { AppHeader } from "../components/AppHeader.js";

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

afterEach(() => {
  mockFetch.mockReset();
});

function LocationProbe() {
  const loc = useLocation();
  return <div data-testid="loc">{loc.pathname + loc.search}</div>;
}

function renderHeader(initialEntry = "/", seed?: (client: QueryClient) => void) {
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
  seed?.(client);
  render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[initialEntry]}>
          <AppHeader />
          <LocationProbe />
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
    const second = screen.getByRole("menuitem", { name: "Import repository" });
    await waitFor(() => expect(first).toHaveFocus());

    await user.keyboard("{ArrowDown}");
    expect(second).toHaveFocus();
    await user.keyboard("{End}");
    expect(screen.getByRole("menuitem", { name: "New codespace" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu", { name: "Create new…" })).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it("opens the keyboard-shortcuts sheet on '?'", async () => {
    renderHeader();
    const user = userEvent.setup();
    // GlobalShortcuts loads lazily, so re-press until its listener is attached.
    await waitFor(async () => {
      await user.keyboard("?");
      expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeInTheDocument();
    });
  });

  it("navigates with the global 'g n' sequence", async () => {
    renderHeader();
    const user = userEvent.setup();
    await waitFor(async () => {
      await user.keyboard("gn");
      expect(screen.getByTestId("loc")).toHaveTextContent("/ui/notifications");
    });
  });

  it("focuses the header search on 's' and '/', but not while typing", async () => {
    renderHeader();
    const user = userEvent.setup();
    const search = screen.getByLabelText("Search");
    // GlobalShortcuts loads lazily, so re-press until its listener is attached.
    await waitFor(async () => {
      await user.keyboard("s");
      expect(search).toHaveFocus();
    });

    // While the search input is focused, "/" must type into it, not hijack.
    await user.keyboard("/");
    expect(search).toHaveValue("/");
    expect(search).toHaveFocus();

    // "/" from the page focuses search too.
    (document.activeElement as HTMLElement | null)?.blur();
    await user.keyboard("/");
    expect(search).toHaveFocus();
    // The guarded "/" typed nothing new.
    expect(search).toHaveValue("/");
  });

  it("scopes Issues / Pull requests quick links to the signed-in viewer", async () => {
    renderHeader();
    await waitFor(() => {
      expect(screen.getByRole("link", { name: "Your issues" })).toHaveAttribute(
        "href",
        `/ui/search?type=issues&q=${encodeURIComponent("is:issue author:admin")}`,
      );
    });
    expect(screen.getByRole("link", { name: "Your pull requests" })).toHaveAttribute(
      "href",
      `/ui/search?type=issues&q=${encodeURIComponent("is:pr author:admin")}`,
    );
  });

  it("offers New codespace in the create menu", async () => {
    renderHeader();
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Create new…" }));
    expect(screen.getByRole("menuitem", { name: "New codespace" })).toHaveAttribute("href", "/ui/codespaces");
  });

  it("scopes header search to the current repository", async () => {
    renderHeader("/ui/acme/api");
    expect(screen.getByText("In this repository")).toBeInTheDocument();
    const input = screen.getByLabelText("Search in acme/api");
    const user = userEvent.setup();
    await user.type(input, "flaky test{Enter}");
    await waitFor(() => {
      expect(screen.getByTestId("loc").textContent).toContain("/ui/search");
    });
    // The submitted query is prefixed with the repo qualifier.
    expect(decodeURIComponent(screen.getByTestId("loc").textContent!)).toContain(
      "repo:acme/api flaky test",
    );
  });

  // ─── G57: owner/repo breadcrumb in the global header row ──────────────────
  //
  // github.com's 2023+ chrome carries owner/repo in the header itself, next to
  // the logo, instead of a page-level bar above the repository tabs. It is
  // derived from the pathname, so these also pin the reserved-word
  // disambiguation that keeps /ui/<app page> from parsing as a repository.

  it("shows the owner/repo breadcrumb next to the logo on a repository route", async () => {
    renderHeader("/ui/acme/api/pulls/7/files");
    const crumb = screen.getByRole("navigation", { name: "Repository breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "acme" })).toHaveAttribute("href", "/ui/acme");
    expect(within(crumb).getByRole("link", { name: "api" })).toHaveAttribute("href", "/ui/acme/api");
  });

  it("routes an organization owner in the breadcrumb to the organization page", async () => {
    renderHeader("/ui/acme/api", (client) => {
      // The repo pages fill this key; the header only reads it (skipToken), so
      // a cached organization payload is what redirects the owner link.
      client.setQueryData(["repo", "acme", "api"], {
        full_name: "acme/api",
        owner: { login: "acme", type: "Organization" },
      });
    });
    const crumb = screen.getByRole("navigation", { name: "Repository breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "acme" })).toHaveAttribute("href", "/ui/orgs/acme");
  });

  it("never reads a reserved top-level page as a repository", async () => {
    renderHeader("/ui/settings/organizations");
    expect(screen.queryByRole("navigation", { name: "Repository breadcrumb" })).not.toBeInTheDocument();
    expect(screen.queryByText("In this repository")).not.toBeInTheDocument();
  });

  it("shows no breadcrumb outside a repository", async () => {
    renderHeader("/ui/");
    expect(screen.queryByRole("navigation", { name: "Repository breadcrumb" })).not.toBeInTheDocument();
  });
});
