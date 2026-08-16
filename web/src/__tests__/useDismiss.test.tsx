import { describe, it, expect, afterEach, vi } from "vitest";
import { render, cleanup, screen, fireEvent } from "@testing-library/react";
import { useState } from "react";
import { useDismiss } from "../hooks/useDismiss.js";

function Menu({ onClose }: { onClose: () => void }) {
  const [open, setOpen] = useState(true);
  const close = () => {
    setOpen(false);
    onClose();
  };
  const ref = useDismiss<HTMLDivElement>(open, close);
  return (
    <div>
      <div ref={ref} data-testid="wrap">
        <button onClick={() => setOpen((v) => !v)}>trigger</button>
        {open && <div role="menu">menu open</div>}
      </div>
      <button data-testid="outside">outside</button>
    </div>
  );
}

afterEach(() => cleanup());

describe("useDismiss", () => {
  it("closes on Escape", () => {
    const onClose = vi.fn();
    render(<Menu onClose={onClose} />);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("closes on outside mousedown but not on inside click", () => {
    const onClose = vi.fn();
    render(<Menu onClose={onClose} />);
    // A click inside the wrapper does not dismiss.
    fireEvent.mouseDown(screen.getByText("menu open"));
    expect(onClose).not.toHaveBeenCalled();
    // A mousedown outside the wrapper dismisses.
    fireEvent.mouseDown(screen.getByTestId("outside"));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
