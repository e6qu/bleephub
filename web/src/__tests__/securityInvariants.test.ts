import { describe, expect, it } from "vitest";
import apiSource from "../api.ts?raw";
import discussionsSource from "../pages/DiscussionsPage.tsx?raw";

describe("browser credential and rendering invariants", () => {
  it("does not persist API bearer tokens in browser storage", () => {
    // The legacy "bleephub_token" slot may only be deleted, never read or written.
    expect(apiSource).not.toContain('localStorage.getItem("bleephub_token")');
    expect(apiSource).not.toContain('localStorage.setItem("bleephub_token"');
    expect(apiSource).not.toContain("sessionStorage");
  });

  it("does not trust server-provided discussion HTML", () => {
    expect(discussionsSource).not.toContain("dangerouslySetInnerHTML");
    expect(discussionsSource).not.toMatch(/<Markdown[^>]*>\s*\{[^}]*bodyHTML/);
  });
});
