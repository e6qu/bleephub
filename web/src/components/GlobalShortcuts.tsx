import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { KeyboardShortcuts } from "./KeyboardShortcuts.js";

/**
 * github.com's "?" shortcuts sheet and `g …` navigation sequences. Code-split
 * off the entry bundle (AppHeader mounts it lazily). Shortcuts are ignored
 * while typing so they never hijack a form field.
 */
export function GlobalShortcuts({ login }: { login: string }) {
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const gPendingRef = useRef(false);

  useEffect(() => {
    const typingIn = (el: EventTarget | null): boolean => {
      const node = el as HTMLElement | null;
      if (!node) return false;
      const tag = node.tagName;
      return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || node.isContentEditable;
    };
    const gTargets: Record<string, string> = {
      h: "/ui/",
      n: "/ui/notifications",
      e: "/ui/search",
      p: login ? `/ui/${login}` : "/ui/",
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey || typingIn(e.target)) {
        gPendingRef.current = false;
        return;
      }
      if (e.key === "?") {
        e.preventDefault();
        setOpen(true);
        gPendingRef.current = false;
        return;
      }
      if (gPendingRef.current) {
        gPendingRef.current = false;
        const to = gTargets[e.key];
        if (to) {
          e.preventDefault();
          navigate(to);
        }
        return;
      }
      // github.com focuses the header search on `s` or `/`.
      if (e.key === "s" || e.key === "/") {
        const input = document.getElementById("global-search-input");
        if (input) {
          e.preventDefault();
          input.focus();
        }
        return;
      }
      if (e.key === "g") {
        gPendingRef.current = true;
        // The `g` prefix stays armed only briefly, matching github.com.
        window.setTimeout(() => {
          gPendingRef.current = false;
        }, 1000);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [navigate, login]);

  return <KeyboardShortcuts open={open} onClose={() => setOpen(false)} />;
}
