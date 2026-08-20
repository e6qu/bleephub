import { createContext, useContext } from "react";

/**
 * Whether the app currently holds a signed-in session. Provided once by App
 * (which owns the session probe); consumed by any component that must not
 * fire viewer-scoped requests — or must render its signed-out variant — for
 * an anonymous visitor.
 *
 * The default is `true` so components rendered outside App (unit tests,
 * storybook-style harnesses) keep their historical signed-in behaviour.
 */
export const SessionContext = createContext(true);

/** True when the viewer is signed in. See {@link SessionContext}. */
export function useSignedIn(): boolean {
  return useContext(SessionContext);
}

/**
 * The sign-in URL that returns the visitor to `loc` after authenticating —
 * the target for every signed-out "Sign in" affordance.
 */
export function loginPath(loc: { pathname: string; search: string; hash: string }): string {
  return `/ui/login?return_to=${encodeURIComponent(loc.pathname + loc.search + loc.hash)}`;
}
