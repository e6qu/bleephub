import { useEffect, useRef } from "react";

/**
 * Dismiss-on-outside-click and dismiss-on-Escape for a popup/menu. Attach the
 * returned ref to the element that wraps BOTH the trigger and the popup so a
 * click on the trigger is not treated as "outside". Escape is handled at the
 * document level, so a keyboard user can always dismiss the open popup — the
 * WCAG 2.2 dismissibility behaviour axe cannot see.
 */
export function useDismiss<T extends HTMLElement = HTMLDivElement>(open: boolean, close: () => void) {
  const ref = useRef<T>(null);
  useEffect(() => {
    if (!open) return;
    const onClick = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) close();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") close();
    };
    document.addEventListener("mousedown", onClick);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onClick);
      document.removeEventListener("keydown", onKey);
    };
  }, [open, close]);
  return ref;
}
