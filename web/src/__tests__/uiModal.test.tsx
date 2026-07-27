import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, test, vi } from "vitest";
import { Modal } from "../components/ui.js";

function Harness({ onClose }: { onClose: () => void }) {
  return (
    <Modal title="Create a thing" onClose={onClose}>
      <input aria-label="name" />
      <button type="button">Save</button>
    </Modal>
  );
}

function Opener() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Open
      </button>
      {open && (
        <Modal title="Sheet" onClose={() => setOpen(false)}>
          <button type="button">Inside</button>
        </Modal>
      )}
    </>
  );
}

describe("Modal accessibility", () => {
  test("exposes dialog semantics labelled by its title", () => {
    render(<Harness onClose={() => {}} />);
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog).toHaveAccessibleName("Create a thing");
  });

  test("Escape closes the dialog", async () => {
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<Harness onClose={onClose} />);
    await user.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  test("focus moves into the dialog on open", () => {
    render(<Harness onClose={() => {}} />);
    const dialog = screen.getByRole("dialog");
    expect(dialog.contains(document.activeElement)).toBe(true);
  });

  test("Tab cycles within the dialog and never escapes to the page behind", async () => {
    const user = userEvent.setup();
    render(
      <>
        <button type="button">outside before</button>
        <Harness onClose={() => {}} />
        <button type="button">outside after</button>
      </>,
    );
    const dialog = screen.getByRole("dialog");

    // Walk further than the number of focusables inside; focus must stay in.
    for (let i = 0; i < 8; i += 1) {
      await user.tab();
      expect(dialog.contains(document.activeElement)).toBe(true);
    }
    for (let i = 0; i < 8; i += 1) {
      await user.tab({ shift: true });
      expect(dialog.contains(document.activeElement)).toBe(true);
    }
  });

  test("focus returns to the element that opened the dialog on close", async () => {
    const user = userEvent.setup();
    render(<Opener />);
    const opener = screen.getByRole("button", { name: "Open" });
    await user.click(opener);
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(document.activeElement).toBe(opener);
  });

  test("Escape closes only the innermost of stacked dialogs", async () => {
    const outerClose = vi.fn();
    const innerClose = vi.fn();
    const user = userEvent.setup();
    render(
      <Modal title="Outer" onClose={outerClose}>
        <button type="button">outer body</button>
        <Modal title="Inner" onClose={innerClose}>
          <button type="button">inner body</button>
        </Modal>
      </Modal>,
    );
    await user.keyboard("{Escape}");
    expect(innerClose).toHaveBeenCalledTimes(1);
    expect(outerClose).not.toHaveBeenCalled();
  });
});
