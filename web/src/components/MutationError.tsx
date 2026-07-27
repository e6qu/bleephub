import { useMemo } from "react";
import { ErrorBanner } from "./ui.js";

/**
 * Anything carrying a rejection: a TanStack Query mutation result, or a plain
 * `{ error }` box for hand-rolled handlers that do not use TanStack.
 */
export type MutationLike = { error: unknown } | null | undefined;

/** The first failure among the given sources, normalized to an Error. */
export function useMutationError(...sources: MutationLike[]): Error | null {
  const raw = sources.find((source) => source?.error)?.error ?? null;
  return useMemo(() => {
    if (raw === null) return null;
    return raw instanceof Error ? raw : new Error(String(raw));
  }, [raw]);
}

/**
 * Renders the first failure among `of`, or nothing when they all succeeded.
 * A state-changing control must never fail silently; this is the one surface
 * every such control reports through.
 */
export function MutationError({ of }: { of: MutationLike | MutationLike[] }) {
  const error = useMutationError(...(Array.isArray(of) ? of : [of]));
  if (!error) return null;
  return <ErrorBanner>{error.message}</ErrorBanner>;
}
