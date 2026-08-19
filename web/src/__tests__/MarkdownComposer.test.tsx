import { describe, it, expect, afterEach } from "vitest";
import { render, cleanup, screen, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { useState } from "react";
import { MarkdownComposer } from "../components/MarkdownComposer.js";

afterEach(cleanup);

function Harness({ initial = "" }: { initial?: string }) {
  const [value, setValue] = useState(initial);
  return (
    <MemoryRouter>
      <MarkdownComposer value={value} onChange={setValue} label="Comment body" />
    </MemoryRouter>
  );
}

describe("MarkdownComposer", () => {
  it("renders Write/Preview tabs with tab semantics and the formatting toolbar", () => {
    render(<Harness />);
    const tabs = screen.getAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual(["Write", "Preview"]);
    expect(tabs[0]).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("toolbar", { name: "Formatting" })).toBeInTheDocument();
    for (const label of ["Add heading", "Bold", "Italic", "Insert a quote", "Insert code", "Add a link", "Add a bulleted list", "Add a numbered list", "Add a task list"]) {
      expect(screen.getByRole("button", { name: label })).toBeInTheDocument();
    }
  });

  it("bold toolbar button wraps the selection and re-selects the wrapped text", () => {
    render(<Harness initial="hello world" />);
    const ta = screen.getByRole("textbox", { name: "Comment body" }) as HTMLTextAreaElement;
    ta.setSelectionRange(0, 5);
    fireEvent.click(screen.getByRole("button", { name: "Bold" }));
    expect(ta.value).toBe("**hello** world");
    expect([ta.selectionStart, ta.selectionEnd]).toEqual([2, 7]);
    // Applying it again toggles the wrapping back off.
    fireEvent.click(screen.getByRole("button", { name: "Bold" }));
    expect(ta.value).toBe("hello world");
  });

  it("Cmd/Ctrl+B bolds and Cmd/Ctrl+K inserts a link with the url selected", () => {
    render(<Harness initial="hello" />);
    const ta = screen.getByRole("textbox", { name: "Comment body" }) as HTMLTextAreaElement;
    ta.setSelectionRange(0, 5);
    fireEvent.keyDown(ta, { key: "b", ctrlKey: true });
    expect(ta.value).toBe("**hello**");
    ta.setSelectionRange(2, 7);
    fireEvent.keyDown(ta, { key: "k", metaKey: true });
    expect(ta.value).toBe("**[hello](url)**");
    expect(ta.value.slice(ta.selectionStart, ta.selectionEnd)).toBe("url");
  });

  it("preview tab renders the value as markdown; empty value says so", () => {
    render(<Harness initial="**bold** move" />);
    fireEvent.click(screen.getByRole("tab", { name: "Preview" }));
    const strong = screen.getByText("bold");
    expect(strong.tagName).toBe("STRONG");
    cleanup();
    render(<Harness />);
    fireEvent.click(screen.getByRole("tab", { name: "Preview" }));
    expect(screen.getByText("Nothing to preview")).toBeInTheDocument();
  });

  it("quote and list buttons prefix every selected line", () => {
    render(<Harness initial={"one\ntwo"} />);
    const ta = screen.getByRole("textbox", { name: "Comment body" }) as HTMLTextAreaElement;
    ta.setSelectionRange(0, ta.value.length);
    fireEvent.click(screen.getByRole("button", { name: "Add a numbered list" }));
    expect(ta.value.split("\n")).toEqual(["1. one", "2. two"]);
  });
});
