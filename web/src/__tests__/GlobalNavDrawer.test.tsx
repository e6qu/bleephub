import { describe, it, expect, afterEach } from "vitest";
import { render, cleanup, screen, fireEvent, waitFor } from "@testing-library/react";
import { useState } from "react";
import { MemoryRouter } from "react-router";
import { GlobalNavDrawer } from "../components/GlobalNavDrawer.js";

function Harness() {
  const [open, setOpen] = useState(false);
  return (
    <MemoryRouter>
      <button data-testid="trigger" onClick={() => setOpen(true)}>
        menu
      </button>
      <GlobalNavDrawer open={open} onClose={() => setOpen(false)} />
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

    fireEvent.click(trigger); // open
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
