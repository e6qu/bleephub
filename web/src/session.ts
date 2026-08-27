import { createContext, useContext } from "react";

// Provided by App. Default `true` so components rendered outside App (tests) stay signed-in.
export const SessionContext = createContext(true);

export function useSignedIn(): boolean {
  return useContext(SessionContext);
}

/** Sign-in URL that returns the visitor to `loc` after authenticating. */
export function loginPath(loc: { pathname: string; search: string; hash: string }): string {
  return `/ui/login?return_to=${encodeURIComponent(loc.pathname + loc.search + loc.hash)}`;
}
