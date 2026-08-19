import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { KeyboardShortcuts } from "./KeyboardShortcuts.js";

/**
 * github.com's global keyboard affordances that are NOT the command palette:
 * the "?" shortcuts sheet and the `g …` navigation sequences it documents.
 *
 * Code-split out of the entry bundle (AppHeader mounts it lazily) so this and
 * the shortcuts modal never weigh on first paint. Both are ignored while the
 * user is typing so they can't hijack a form field, and the `g` prefix expires
 * quickly.
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
      // github.com focuses the header search on `s` or `/` (the typing guard
      // above keeps both usable inside form fields).
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
        // The prefix only stays armed briefly, matching github.com.
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
