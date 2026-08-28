import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "@bleephub/ui-core/components";
import { AppHeader } from "../components/AppHeader.js";
import { PageTitle } from "../components/ui.js";

// jsdom does no layout, so 375px behaviour is enforced by the Playwright gate
// (e2e/mobile-overflow.spec.ts); these unit tests pin the class/style decisions it
// depends on so a refactor that drops one fails fast, not as a CI-only overflow.

const mockFetch = vi.fn();
globalThis.fetch = mockFetch;

afterEach(() => {
  mockFetch.mockReset();
});

function renderHeader(initialEntry = "/ui/admin/parity") {
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
        <MemoryRouter initialEntries={[initialEntry]}>
          <AppHeader />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("AppHeader phone-width collapse", () => {
  it("lets the search box shrink instead of forcing page overflow", () => {
    renderHeader();
    const input = screen.getByPlaceholderText("Search or jump to…");
    // min-width:0 overrides the input's ~170px intrinsic minimum; without it the flex row never fits 375px.
    expect(input.style.minWidth).toBe("0px");
    const form = input.closest("form");
    expect(form?.className).toContain("min-w-0");
  });

  it("collapses the search-scope select below sm", () => {
    renderHeader();
    const select = screen.getByLabelText("Search scope");
    expect(select.className).toContain("hidden");
    expect(select.className).toContain("sm:block");
  });

  it("collapses the repo-scope hint below md", () => {
    renderHeader("/ui/admin/parity");
    const hint = screen.getByText("In this repository");
    expect(hint.className).toContain("hidden");
    expect(hint.className).toContain("md:inline");
  });

  it("collapses the Issues / Pull requests quick links below md without inline display", async () => {
    renderHeader();
    // Labels become "Your issues" / "Your pull requests" once the viewer query resolves; findByLabelText waits it out.
    for (const name of ["Your issues", "Your pull requests"]) {
      const link = await screen.findByLabelText(name);
      expect(link.className).toContain("hidden");
      expect(link.className).toContain("md:inline-flex");
      // An inline `display` would defeat the responsive `hidden` utility.
      expect(link.style.display).toBe("");
    }
  });
});

describe("PageTitle actions row", () => {
  it("clamps and wraps the actions row at narrow widths", () => {
    render(
      <PageTitle
        title="Repositories"
        actions={<button type="button">New repository</button>}
      />,
    );
    const actions = screen.getByRole("button", { name: "New repository" }).parentElement;
    expect(actions?.className).toContain("max-w-full");
    expect(actions?.className).toContain("flex-wrap");
    expect(actions?.className).toContain("shrink-0");
  });
});
