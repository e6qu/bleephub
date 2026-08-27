import { useEffect, useRef } from "react";

// Session-scoped comment-draft durability: drafts live in sessionStorage under
// `bleephub:draft:{key}`, surviving in-app navigation and reloads but not a new
// browser session.

const STORAGE_PREFIX = "bleephub:draft:";

function storageKeyFor(key: string): string {
  return STORAGE_PREFIX + key;
}

/** Remove a stored draft — call on successful submit. */
export function clearComposerDraft(key: string): void {
  try {
    sessionStorage.removeItem(storageKeyFor(key));
  } catch {
    /* storage unavailable */
  }
}

/**
 * Mirror a controlled composer's value into sessionStorage. On mount (and when
 * `key` changes) an empty composer adopts the stored draft via `onChange`;
 * blank values remove the entry. A null key disables the hook.
 */
export function useComposerDraft(
  key: string | null,
  value: string,
  onChange: (v: string) => void,
): void {
  // Keep the effect from re-running on unstable onChange callbacks.
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const restoredKey = useRef<string | null>(null);

  useEffect(() => {
    if (!key) return;
    const storageKey = storageKeyFor(key);
    if (restoredKey.current !== key) {
      restoredKey.current = key;
      if (value === "") {
        let stored: string | null = null;
        try {
          stored = sessionStorage.getItem(storageKey);
        } catch {
          /* storage unavailable */
        }
        if (stored) {
          // Skip the write pass; the restored value writes itself on re-render.
          onChangeRef.current(stored);
          return;
        }
      }
    }
    try {
      if (value.trim() === "") sessionStorage.removeItem(storageKey);
      else sessionStorage.setItem(storageKey, value);
    } catch {
      /* quota exceeded or storage unavailable — typing keeps working */
    }
  }, [key, value]);
}
