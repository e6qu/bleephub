import { describe, it, expect, afterEach } from "vitest";
import { render, cleanup, waitFor } from "@testing-library/react";
import { CodeHighlight, highlightLines, languageFromPath } from "../components/CodeHighlight.js";

afterEach(cleanup);

describe("languageFromPath", () => {
  it("maps extensions and special basenames to hljs languages", () => {
    expect(languageFromPath("cmd/bleephub/main.go")).toBe("go");
    expect(languageFromPath("web/src/App.tsx")).toBe("tsx");
    expect(languageFromPath("src/index.js")).toBe("javascript");
    expect(languageFromPath("setup.py")).toBe("python");
    expect(languageFromPath("Dockerfile")).toBe("dockerfile");
    expect(languageFromPath("Dockerfile.release")).toBe("dockerfile");
    expect(languageFromPath("Makefile")).toBe("makefile");
    expect(languageFromPath("config.yml")).toBe("yaml");
    expect(languageFromPath("notes.txt")).toBeUndefined();
    expect(languageFromPath("LICENSE")).toBeUndefined();
  });
});

describe("CodeHighlight", () => {
  it("renders the source as plain text immediately, then upgrades in place", async () => {
    const { container } = render(<CodeHighlight code={'fmt.Println("hi")'} language="go" />);
    // Immediate render: the raw source is visible before the lib loads.
    expect(container.querySelector("pre code")).toHaveTextContent('fmt.Println("hi")');
    await waitFor(() => {
      expect(container.querySelector(".hljs-string")).not.toBeNull();
    });
    expect(container.querySelector("pre code")).toHaveTextContent('fmt.Println("hi")');
  });

  it("escapes HTML in source code — a <script> never becomes an element", async () => {
    const code = '<script>alert("xss")</script>\nconst x = 1;';
    const { container } = render(<CodeHighlight code={code} language="javascript" />);
    await waitFor(() => {
      expect(container.querySelector("pre code span")).not.toBeNull();
    });
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector("pre code")!.innerHTML).toContain("&lt;script&gt;");
  });

  it("stays a plain block for an unknown language", () => {
    const { container } = render(<CodeHighlight code="plain text" path="notes.txt" />);
    expect(container.querySelector("pre code")).toHaveTextContent("plain text");
  });
});

describe("highlightLines", () => {
  it("returns one balanced HTML string per line, re-opening spans that cross lines", async () => {
    const code = "/* first\nsecond */\nvar x = 1;";
    const lines = await highlightLines(code, "javascript");
    expect(lines).toHaveLength(3);
    for (const line of lines) {
      const opens = (line.match(/<span/g) ?? []).length;
      const closes = (line.match(/<\/span>/g) ?? []).length;
      expect(opens).toBe(closes);
    }
    // The block comment colors both of its lines.
    expect(lines[0]).toContain("hljs-comment");
    expect(lines[1]).toContain("hljs-comment");
  });

  it("resolves alias names like tsx through hljs", async () => {
    const lines = await highlightLines("const x: number = 1;", "tsx");
    expect(lines[0]).toContain("hljs-keyword");
  });

  it("falls back to escaped plain lines for an unknown language", async () => {
    const lines = await highlightLines("<b>a</b>\n&x", undefined);
    expect(lines).toEqual(["&lt;b&gt;a&lt;/b&gt;", "&amp;x"]);
  });
});
