import { useEffect, useRef } from "react";

/*
 * Session-scoped comment-draft durability, github.com-style: typing in a
 * comment box, navigating away and returning restores the half-typed text.
 * Drafts live in sessionStorage under `bleephub:draft:{key}` (same scope the
 * PR "Viewed" files and review-summary drafts already use), so they survive
 * in-app navigation and reloads but not a new browser session.
 */

const STORAGE_PREFIX = "bleephub:draft:";

function storageKeyFor(key: string): string {
  return STORAGE_PREFIX + key;
}

/** Remove a stored draft — call on successful submit so the posted text never resurfaces. */
export function clearComposerDraft(key: string): void {
  try {
    sessionStorage.removeItem(storageKeyFor(key));
  } catch {
    /* storage unavailable */
  }
}

/**
 * Mirror a controlled composer's value into sessionStorage.
 *
 * On mount (and whenever `key` changes, e.g. a reply box retargeting another
 * parent comment), an empty composer adopts the stored draft via `onChange`.
 * Every subsequent value change writes through: non-blank values are stored,
 * blank ones remove the entry (so clearing the box drops the draft). A null
 * key disables the hook entirely — callers without a stable identity (or
 * modal editors GitHub does not persist) opt out by passing null.
 */
export function useComposerDraft(
  key: string | null,
  value: string,
  onChange: (v: string) => void,
): void {
  // The latest onChange without making the effect re-run on unstable callbacks.
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
          // Skip the write pass: the restored value flows back through this
          // effect on the next render and writes itself through then.
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
