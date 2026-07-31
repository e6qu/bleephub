import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { StateToggle } from "../components/StateToggle.js";
import { Tabs } from "../components/ui.js";

describe("selection controls", () => {
  it("exposes state filters as pressed buttons", () => {
    render(
      <StateToggle
        value="open"
        options={["open", "closed"] as const}
        labels={{ open: "Open", closed: "Closed" }}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByRole("button", { name: "Open" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "Closed" })).toHaveAttribute("aria-pressed", "false");
  });

  it("exposes tabs and moves selection with arrow keys", () => {
    const onChange = vi.fn();
    render(
      <Tabs
        items={[
          { key: "summary", label: "Summary" },
          { key: "details", label: "Details" },
        ]}
        active="summary"
        onChange={onChange}
      />,
    );
    const summary = screen.getByRole("tab", { name: "Summary" });
    expect(summary).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(summary, { key: "ArrowRight" });
    expect(onChange).toHaveBeenCalledWith("details");
    expect(screen.getByRole("tab", { name: "Details" })).toHaveFocus();
  });
});
