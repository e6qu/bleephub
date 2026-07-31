import { describe, expect, it } from "vitest";
import apiSource from "../api.ts?raw";
import discussionsSource from "../pages/DiscussionsPage.tsx?raw";

describe("browser credential and rendering invariants", () => {
  it("does not persist API bearer tokens in browser storage", () => {
    expect(apiSource).not.toContain("localStorage.getItem(TOKEN_KEY)");
    expect(apiSource).not.toContain("localStorage.setItem(TOKEN_KEY");
    expect(apiSource).not.toContain("sessionStorage");
  });

  it("does not trust server-provided discussion HTML", () => {
    expect(discussionsSource).not.toContain("dangerouslySetInnerHTML");
    expect(discussionsSource).not.toMatch(/<Markdown[^>]*>\s*\{[^}]*bodyHTML/);
  });
});
