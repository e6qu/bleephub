import { describe, it, expect, afterEach } from "vitest";
import { render, cleanup, screen } from "@testing-library/react";
import Markdown from "../components/Markdown.js";

afterEach(cleanup);

describe("Markdown", () => {
  it("renders GitHub alerts as titled, typed admonitions", () => {
    const { container } = render(
      <div className="markdown-body">
        <Markdown>{"> [!WARNING]\n> Be careful here."}</Markdown>
      </div>,
    );
    const alert = container.querySelector(".markdown-alert.markdown-alert-warning");
    expect(alert).not.toBeNull();
    // Title line derived from the marker; blockquote is gone.
    expect(alert?.querySelector(".markdown-alert-title")?.textContent).toBe("Warning");
    expect(container.querySelector("blockquote")).toBeNull();
    // The marker text itself is stripped from the body.
    expect(screen.getByText("Be careful here.")).toBeInTheDocument();
    expect(alert?.textContent).not.toContain("[!WARNING]");
  });

  it("recognises every alert type", () => {
    for (const [marker, cls] of [
      ["NOTE", "markdown-alert-note"],
      ["TIP", "markdown-alert-tip"],
      ["IMPORTANT", "markdown-alert-important"],
      ["CAUTION", "markdown-alert-caution"],
    ] as const) {
      const { container, unmount } = render(
        <div className="markdown-body">
          <Markdown>{`> [!${marker}]\n> Body.`}</Markdown>
        </div>,
      );
      expect(container.querySelector(`.${cls}`)).not.toBeNull();
      unmount();
    }
  });

  it("leaves an ordinary blockquote untouched", () => {
    const { container } = render(
      <div className="markdown-body">
        <Markdown>{"> just a quote"}</Markdown>
      </div>,
    );
    expect(container.querySelector("blockquote")).not.toBeNull();
    expect(container.querySelector(".markdown-alert")).toBeNull();
  });

  it("still renders task-list checkboxes with accessible names", () => {
    render(
      <div className="markdown-body">
        <Markdown>{"- [x] done\n- [ ] todo"}</Markdown>
      </div>,
    );
    expect(screen.getByRole("checkbox", { name: "completed task" })).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "incomplete task" })).toBeInTheDocument();
  });
});
