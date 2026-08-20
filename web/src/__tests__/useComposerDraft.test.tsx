import { describe, it, expect, vi, afterEach } from "vitest";
import { renderHook, cleanup } from "@testing-library/react";
import { useComposerDraft, clearComposerDraft } from "../hooks/useComposerDraft.js";

afterEach(() => {
  cleanup();
  sessionStorage.clear();
  vi.restoreAllMocks();
});

/** onChange is observed (not fed back); rerender with new props to simulate typing. */
function renderDraftHook(key: string | null, initialValue = "") {
  const onChange = vi.fn();
  const utils = renderHook(
    ({ k, v }: { k: string | null; v: string }) => useComposerDraft(k, v, onChange),
    { initialProps: { k: key, v: initialValue } },
  );
  return { onChange, ...utils };
}

describe("useComposerDraft", () => {
  it("restores a stored draft into an empty composer on mount", () => {
    sessionStorage.setItem("bleephub:draft:t:1", "half-typed thought");
    const { onChange } = renderDraftHook("t:1");
    expect(onChange).toHaveBeenCalledWith("half-typed thought");
  });

  it("does not clobber a composer that already has text", () => {
    sessionStorage.setItem("bleephub:draft:t:1", "stored");
    const { onChange } = renderDraftHook("t:1", "already typing");
    expect(onChange).not.toHaveBeenCalled();
    // The existing text wins and writes through over the stale draft.
    expect(sessionStorage.getItem("bleephub:draft:t:1")).toBe("already typing");
  });

  it("writes value changes through and removes the entry when cleared", () => {
    const { rerender } = renderDraftHook("t:1");
    rerender({ k: "t:1", v: "hello" });
    expect(sessionStorage.getItem("bleephub:draft:t:1")).toBe("hello");
    rerender({ k: "t:1", v: "hello world" });
    expect(sessionStorage.getItem("bleephub:draft:t:1")).toBe("hello world");
    // Blank (whitespace-only counts) drops the draft.
    rerender({ k: "t:1", v: "   " });
    expect(sessionStorage.getItem("bleephub:draft:t:1")).toBeNull();
  });

  it("switches drafts when the key changes (reply retargeting)", () => {
    sessionStorage.setItem("bleephub:draft:t:reply:2", "reply draft");
    const { onChange, rerender } = renderDraftHook("t:1");
    rerender({ k: "t:1", v: "main draft" });
    // Retarget with an emptied composer: the other key's draft is restored.
    rerender({ k: "t:reply:2", v: "" });
    expect(onChange).toHaveBeenCalledWith("reply draft");
    // The first key's draft survives untouched.
    expect(sessionStorage.getItem("bleephub:draft:t:1")).toBe("main draft");
  });

  it("does nothing with a null key", () => {
    sessionStorage.setItem("bleephub:draft:t:1", "stored");
    const { onChange, rerender } = renderDraftHook(null);
    expect(onChange).not.toHaveBeenCalled();
    rerender({ k: null, v: "typed" });
    expect(sessionStorage.length).toBe(1); // nothing new written
  });

  it("keeps working when storage writes throw (quota)", () => {
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceededError");
    });
    const { rerender } = renderDraftHook("t:1");
    expect(() => rerender({ k: "t:1", v: "hello" })).not.toThrow();
  });

  it("clearComposerDraft removes the stored entry", () => {
    sessionStorage.setItem("bleephub:draft:t:1", "posted text");
    clearComposerDraft("t:1");
    expect(sessionStorage.getItem("bleephub:draft:t:1")).toBeNull();
  });
});
