import { describe, it, expect, afterEach } from "vitest";
import { render, cleanup } from "@testing-library/react";
import { LogViewer } from "../components/LogViewer.js";

afterEach(() => {
  cleanup();
});

// Match on textContent, not getByText, so the zero-padded line-number format
// stays decoupled from the assertions.
function preText() {
  return document.querySelector("pre")?.textContent ?? "";
}

describe("LogViewer", () => {
  it("renders log lines with line numbers", () => {
    render(<LogViewer lines={["hello", "world"]} />);
    const text = preText();
    expect(text).toContain("1");
    expect(text).toContain("2");
    expect(text).toContain("hello");
    expect(text).toContain("world");
  });

  it("renders empty state when no lines", () => {
    render(<LogViewer lines={[]} />);
    expect(document.body.textContent).toMatch(/no log output/i);
  });

  it("strips ANSI codes and renders content", () => {
    render(<LogViewer lines={["\x1b[32mOK\x1b[0m"]} />);
    expect(preText()).toContain("OK");
    // The line-number column is also color-styled, so match the span by text.
    const spans = Array.from(document.querySelectorAll('span[style*="color"]'));
    const hit = spans.find((s) => s.textContent === "OK");
    expect(hit).toBeDefined();
  });

  it("escapes HTML in log lines to prevent XSS", () => {
    render(<LogViewer lines={['<script>alert("xss")</script>']} />);
    const pre = document.querySelector("pre");
    expect(pre?.innerHTML).toContain("&lt;script&gt;");
    // The dangerouslySetInnerHTML span must hold only escaped text, no real tag.
    const dangerSpans = Array.from(
      pre?.querySelectorAll("div > span:last-child") ?? [],
    );
    for (const sp of dangerSpans) {
      expect(sp.innerHTML).not.toContain("<script>");
    }
  });

  it("handles reset+color combined sequence", () => {
    render(<LogViewer lines={["\x1b[0;32mGREEN\x1b[0m"]} />);
    const spans = Array.from(document.querySelectorAll('span[style*="color"]'));
    const hit = spans.find((s) => s.textContent === "GREEN");
    expect(hit).toBeDefined();
  });

  it("closes unclosed spans at end of line", () => {
    render(<LogViewer lines={["\x1b[31mUNCLOSED"]} />);
    const spans = Array.from(document.querySelectorAll('span[style*="color"]'));
    const hit = spans.find((s) => s.textContent === "UNCLOSED");
    expect(hit).toBeDefined();
    // An unclosed ANSI span must still balance opens with closes.
    const html = hit?.parentElement?.innerHTML ?? "";
    const opens = (html.match(/<span/g) || []).length;
    const closes = (html.match(/<\/span>/g) || []).length;
    expect(opens).toBe(closes);
  });

  it("handles multi-code ANSI sequences (bold+color)", () => {
    render(<LogViewer lines={["\x1b[1;32mBOLD GREEN\x1b[0m"]} />);
    const span = document.querySelector('span[style*="font-weight"]');
    expect(span).not.toBeNull();
    expect(span?.getAttribute("style")).toContain("font-weight:bold");
    expect(span?.getAttribute("style")).toContain("color:");
  });
});
