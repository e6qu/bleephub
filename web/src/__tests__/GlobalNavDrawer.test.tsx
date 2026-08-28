import { describe, it, expect, afterEach } from "vitest";
import { render, cleanup, screen, fireEvent, waitFor } from "@testing-library/react";
import { useState } from "react";
import { MemoryRouter } from "react-router";
import { GlobalNavDrawer } from "../components/GlobalNavDrawer.js";

function Harness({ viewerLogin }: { viewerLogin?: string }) {
  const [open, setOpen] = useState(false);
  return (
    <MemoryRouter>
      <button data-testid="trigger" onClick={() => setOpen(true)}>
        menu
      </button>
      <GlobalNavDrawer open={open} onClose={() => setOpen(false)} viewerLogin={viewerLogin} />
    </MemoryRouter>
  );
}

afterEach(() => cleanup());

describe("GlobalNavDrawer focus management", () => {
  it("moves focus into the drawer on open and restores it to the trigger on close", async () => {
    render(<Harness />);
    const trigger = screen.getByTestId("trigger");
    trigger.focus();
    expect(document.activeElement).toBe(trigger);

    fireEvent.click(trigger);
    // Focus lands inside the drawer nav, not on the obscured page behind it.
    await waitFor(() => {
      const nav = screen.getByRole("navigation", { name: "Global" });
      expect(nav.contains(document.activeElement)).toBe(true);
    });

    // Escape closes and returns focus to the trigger.
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("navigation", { name: "Global" })).toBeNull());
    expect(document.activeElement).toBe(trigger);
  });
});

describe("GlobalNavDrawer viewer scoping", () => {
  it("scopes Issues / Pull requests to the signed-in viewer", async () => {
    render(<Harness viewerLogin="octocat" />);
    fireEvent.click(screen.getByTestId("trigger"));
    const issues = await screen.findByRole("link", { name: "Issues" });
    expect(decodeURIComponent(issues.getAttribute("href")!)).toBe(
      "/ui/search?type=issues&q=is:issue author:octocat",
    );
    expect(
      decodeURIComponent(screen.getByRole("link", { name: "Pull requests" }).getAttribute("href")!),
    ).toBe("/ui/search?type=issues&q=is:pr author:octocat");
  });

  it("falls back to unscoped searches when signed out", async () => {
    render(<Harness />);
    fireEvent.click(screen.getByTestId("trigger"));
    const issues = await screen.findByRole("link", { name: "Issues" });
    expect(decodeURIComponent(issues.getAttribute("href")!)).toBe("/ui/search?type=issues&q=is:issue");
  });
});
