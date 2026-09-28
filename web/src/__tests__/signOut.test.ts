import { describe, expect, it, vi } from "vitest";
// Sign-out is final for the page, so it has this file's module instance to itself.
import { beginSignOut, fetchCurrentUser, fetchRepoDetail, getToken, setToken } from "../api.js";

const mockFetch = vi.fn(
  (_url: RequestInfo | URL, init?: RequestInit) =>
    new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener("abort", () => reject(new Error("aborted")));
    }),
);
globalThis.fetch = mockFetch;

describe("sign-out", () => {
  it("aborts what is in flight and keeps every later request off the network", async () => {
    setToken("token");
    const inFlight = fetchCurrentUser();
    // A per-request signal that stays live must not shield a request from it.
    const live = new AbortController();
    const withSignal = fetchRepoDetail("admin", "repo", live.signal);
    expect(mockFetch).toHaveBeenCalledTimes(2);

    beginSignOut();

    await expect(inFlight).rejects.toThrow(/aborted/);
    await expect(withSignal).rejects.toThrow(/aborted/);
    expect(live.signal.aborted).toBe(false);
    expect(getToken()).toBeNull();

    // The refetch that used to 401 here and send the page to the sign-in
    // redirect, replacing the sign-out navigation.
    await expect(fetchCurrentUser()).rejects.toThrow();
    expect(mockFetch).toHaveBeenCalledTimes(2);
  });
});
