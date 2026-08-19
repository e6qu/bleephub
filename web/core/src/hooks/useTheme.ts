import { useCallback, useEffect, useState } from "react";

/** "system" follows the OS preference live; light/dark are explicit overrides. */
export type Theme = "light" | "dark" | "system";
/** The concrete theme actually painted on the page. */
export type ResolvedTheme = "light" | "dark";

const STORAGE_KEY = "bleephub:theme";

function readStoredTheme(): Theme {
  if (typeof window === "undefined") return "system";
  const stored = window.localStorage.getItem(STORAGE_KEY);
  // Only explicit overrides are persisted; absence of the key means "system".
  return stored === "light" || stored === "dark" ? stored : "system";
}

function systemTheme(fallback: ResolvedTheme): ResolvedTheme {
  if (typeof window === "undefined") return fallback;
  // Honour an explicit OS preference in either direction; only when the
  // OS expresses none do we use the caller's fallback. Operator tools pass
  // "dark" (the brutalist design-system default); bleephub passes "light"
  // to match GitHub's own light-first default.
  if (window.matchMedia("(prefers-color-scheme: light)").matches) return "light";
  if (window.matchMedia("(prefers-color-scheme: dark)").matches) return "dark";
  return fallback;
}

function applyTheme(theme: ResolvedTheme) {
  if (typeof document === "undefined") return;
  const root = document.documentElement;
  if (theme === "dark") root.classList.add("dark");
  else root.classList.remove("dark");
  root.style.colorScheme = theme;
}

/**
 * useTheme reads the current theme + lets callers change it.
 *
 * `theme` is the user's choice ("light" | "dark" | "system"); `resolvedTheme`
 * is what is actually painted. Picking "system" clears the localStorage
 * override and tracks `prefers-color-scheme` live via a matchMedia listener;
 * picking light/dark persists that override until changed.
 */
export function useTheme(
  defaultTheme: ResolvedTheme = "dark",
): { theme: Theme; resolvedTheme: ResolvedTheme; setTheme: (t: Theme) => void; toggle: () => void } {
  const [theme, setThemeState] = useState<Theme>(readStoredTheme);
  const [systemResolved, setSystemResolved] = useState<ResolvedTheme>(() => systemTheme(defaultTheme));

  const resolvedTheme = theme === "system" ? systemResolved : theme;

  // Track the OS preference live while in system mode. The re-sync on entering
  // system mode matters: the OS may have flipped while an override was active.
  useEffect(() => {
    if (theme !== "system" || typeof window === "undefined") return;
    setSystemResolved(systemTheme(defaultTheme));
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => setSystemResolved(systemTheme(defaultTheme));
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [theme, defaultTheme]);

  // Apply on mount + whenever the resolved theme changes. The initial apply
  // matters because tokens.css's `.dark` class is the only switch the design
  // system listens to.
  useEffect(() => {
    applyTheme(resolvedTheme);
  }, [resolvedTheme]);

  const setTheme = useCallback((next: Theme) => {
    if (typeof window !== "undefined") {
      if (next === "system") window.localStorage.removeItem(STORAGE_KEY);
      else window.localStorage.setItem(STORAGE_KEY, next);
    }
    setThemeState(next);
  }, []);

  const toggle = useCallback(() => {
    setTheme(resolvedTheme === "dark" ? "light" : "dark");
  }, [resolvedTheme, setTheme]);

  return { theme, resolvedTheme, setTheme, toggle };
}
