import { useMemo } from "react";
import { ErrorBanner } from "./ui.js";

/** A TanStack mutation result, or a plain `{ error }` box. */
export type MutationLike = { error: unknown } | null | undefined;

/** The first failure among the given sources, normalized to an Error. */
export function useMutationError(...sources: MutationLike[]): Error | null {
  const raw = sources.find((source) => source?.error)?.error ?? null;
  return useMemo(() => {
    if (raw === null) return null;
    return raw instanceof Error ? raw : new Error(String(raw));
  }, [raw]);
}

/** Renders the first failure among `of` — the shared error surface for state-changing controls. */
export function MutationError({ of }: { of: MutationLike | MutationLike[] }) {
  const error = useMutationError(...(Array.isArray(of) ? of : [of]));
  if (!error) return null;
  return <ErrorBanner>{error.message}</ErrorBanner>;
}
