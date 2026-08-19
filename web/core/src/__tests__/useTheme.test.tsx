import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { useTheme } from "../hooks/useTheme.js";

afterEach(() => {
  window.localStorage.clear();
  document.documentElement.classList.remove("dark");
  document.documentElement.style.colorScheme = "";
});

describe("useTheme", () => {
  test("defaults to system, resolving dark when the OS expresses no preference", () => {
    const { result } = renderHook(() => useTheme());
    expect(result.current.theme).toBe("system");
    expect(result.current.resolvedTheme).toBe("dark");
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    expect(document.documentElement.style.colorScheme).toBe("dark");
    // system mode persists no override
    expect(window.localStorage.getItem("bleephub:theme")).toBe(null);
  });

  test("toggle flips to the opposite resolved theme and writes localStorage", () => {
    const { result } = renderHook(() => useTheme());
    act(() => {
      result.current.toggle();
    });
    expect(result.current.theme).toBe("light");
    expect(result.current.resolvedTheme).toBe("light");
    expect(document.documentElement.classList.contains("dark")).toBe(false);
    expect(window.localStorage.getItem("bleephub:theme")).toBe("light");
  });

  test("setTheme overrides and system clears the stored override", () => {
    const { result } = renderHook(() => useTheme());
    act(() => {
      result.current.setTheme("light");
    });
    expect(result.current.theme).toBe("light");
    expect(window.localStorage.getItem("bleephub:theme")).toBe("light");
    act(() => {
      result.current.setTheme("dark");
    });
    expect(result.current.theme).toBe("dark");
    expect(document.documentElement.classList.contains("dark")).toBe(true);
    act(() => {
      result.current.setTheme("system");
    });
    expect(result.current.theme).toBe("system");
    expect(window.localStorage.getItem("bleephub:theme")).toBe(null);
    // matchMedia in tests matches nothing, so system resolves to the default.
    expect(result.current.resolvedTheme).toBe("dark");
  });

  test("reads stored preference on mount", () => {
    window.localStorage.setItem("bleephub:theme", "light");
    const { result } = renderHook(() => useTheme());
    expect(result.current.theme).toBe("light");
    expect(result.current.resolvedTheme).toBe("light");
    expect(document.documentElement.classList.contains("dark")).toBe(false);
  });
});
