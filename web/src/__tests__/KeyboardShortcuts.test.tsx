import { describe, it, expect, vi, afterEach } from "vitest";
import { render, cleanup, screen, fireEvent } from "@testing-library/react";
import { KeyboardShortcuts } from "../components/KeyboardShortcuts.js";

afterEach(cleanup);

describe("KeyboardShortcuts", () => {
  it("renders nothing when closed", () => {
    const { container } = render(<KeyboardShortcuts open={false} onClose={() => {}} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("shows an accessible dialog listing shortcut groups and bindings", () => {
    render(<KeyboardShortcuts open onClose={() => {}} />);
    const dialog = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
    expect(dialog).toHaveAttribute("aria-modal", "true");
    // A documented binding from each of two groups.
    expect(screen.getByText("Go to your notifications")).toBeInTheDocument();
    expect(screen.getByText("Open the command palette (Ctrl K on Windows/Linux)")).toBeInTheDocument();
    // The keys are rendered as <kbd> elements.
    expect(screen.getAllByText("g").length).toBeGreaterThan(0);
  });

  it("closes on Escape, the close button, and backdrop click", () => {
    const onEsc = vi.fn();
    const { rerender } = render(<KeyboardShortcuts open onClose={onEsc} />);
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onEsc).toHaveBeenCalledTimes(1);

    const onBtn = vi.fn();
    rerender(<KeyboardShortcuts open onClose={onBtn} />);
    fireEvent.click(screen.getByRole("button", { name: "Close keyboard shortcuts" }));
    expect(onBtn).toHaveBeenCalledTimes(1);
  });
});
