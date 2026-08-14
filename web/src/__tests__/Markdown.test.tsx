import { describe, it, expect, afterEach, vi } from "vitest";
import { render, cleanup, screen, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import Markdown, { toggleTaskInMarkdown } from "../components/Markdown.js";

describe("toggleTaskInMarkdown", () => {
  const src = "- [ ] first\n- [x] second\n- [ ] third";
  it("flips the Nth task marker without touching the others", () => {
    expect(toggleTaskInMarkdown(src, 0, true)).toBe("- [x] first\n- [x] second\n- [ ] third");
    expect(toggleTaskInMarkdown(src, 1, false)).toBe("- [ ] first\n- [ ] second\n- [ ] third");
    expect(toggleTaskInMarkdown(src, 2, true)).toBe("- [ ] first\n- [x] second\n- [x] third");
  });
});

describe("Markdown task lists", () => {
  it("interactive checkboxes report their index on toggle; read-only otherwise", () => {
    const onToggle = vi.fn();
    const { rerender } = render(
      <MemoryRouter>
        <div className="markdown-body">
          <Markdown onToggleTask={onToggle}>{"- [ ] a\n- [x] b"}</Markdown>
        </div>
      </MemoryRouter>,
    );
    const boxes = screen.getAllByRole("checkbox");
    expect(boxes[1]).not.toBeDisabled();
    fireEvent.click(boxes[1]!);
    expect(onToggle).toHaveBeenCalledWith(1, false);

    // Without the handler the checkboxes stay disabled (read-only).
    rerender(
      <MemoryRouter>
        <div className="markdown-body">
          <Markdown>{"- [ ] a\n- [x] b"}</Markdown>
        </div>
      </MemoryRouter>,
    );
    expect(screen.getAllByRole("checkbox")[0]).toBeDisabled();
  });
});

afterEach(cleanup);

function renderMd(ui: React.ReactNode) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

describe("Markdown autolinks", () => {
  const ctx = { owner: "octo", repo: "hello" };

  it("links #123, @user, owner/repo#9, and 40-char SHAs with repo context", () => {
    const sha = "a".repeat(40);
    renderMd(
      <Markdown linkContext={ctx}>{`See #123 by @octocat, cross other/repo#9, at ${sha}.`}</Markdown>,
    );
    expect(screen.getByRole("link", { name: "#123" })).toHaveAttribute(
      "href",
      "/ui/repos/octo/hello/issues/123",
    );
    expect(screen.getByRole("link", { name: "@octocat" })).toHaveAttribute("href", "/ui/octocat");
    expect(screen.getByRole("link", { name: "other/repo#9" })).toHaveAttribute(
      "href",
      "/ui/repos/other/repo/issues/9",
    );
    expect(screen.getByRole("link", { name: sha })).toHaveAttribute(
      "href",
      `/ui/repos/octo/hello/commits/${sha}`,
    );
  });

  it("does not autolink #123 or SHAs without repo context, but still links @user and owner/repo#n", () => {
    renderMd(<Markdown>{"bare #123 and @octocat and other/repo#5"}</Markdown>);
    expect(screen.queryByRole("link", { name: "#123" })).toBeNull();
    expect(screen.getByRole("link", { name: "@octocat" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "other/repo#5" })).toBeInTheDocument();
  });

  it("does not autolink refs inside code spans or the local part of emails", () => {
    const { container } = renderMd(
      <Markdown linkContext={ctx}>{"`#123` and mail me@example.com"}</Markdown>,
    );
    // The code-span #123 is not turned into an issue link, and the email's
    // `@example` is not turned into an @mention — i.e. no in-app /ui/ links fire.
    // (remark-gfm may still linkify the email to mailto:, which is fine.)
    const internal = [...container.querySelectorAll("a")].filter((a) =>
      (a.getAttribute("href") ?? "").startsWith("/ui/"),
    );
    expect(internal).toHaveLength(0);
  });
});

describe("Markdown", () => {
  it("renders GitHub alerts as titled, typed admonitions", () => {
    const { container } = renderMd(
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
      const { container, unmount } = renderMd(
        <div className="markdown-body">
          <Markdown>{`> [!${marker}]\n> Body.`}</Markdown>
        </div>,
      );
      expect(container.querySelector(`.${cls}`)).not.toBeNull();
      unmount();
    }
  });

  it("leaves an ordinary blockquote untouched", () => {
    const { container } = renderMd(
      <div className="markdown-body">
        <Markdown>{"> just a quote"}</Markdown>
      </div>,
    );
    expect(container.querySelector("blockquote")).not.toBeNull();
    expect(container.querySelector(".markdown-alert")).toBeNull();
  });

  it("still renders task-list checkboxes with accessible names", () => {
    renderMd(
      <div className="markdown-body">
        <Markdown>{"- [x] done\n- [ ] todo"}</Markdown>
      </div>,
    );
    expect(screen.getByRole("checkbox", { name: "completed task" })).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "incomplete task" })).toBeInTheDocument();
  });
});
