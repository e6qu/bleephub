// A rejected promise with no .catch escapes React entirely; the global reporter
// is the only thing that surfaces it. Cover both that it reports and that
// uninstalling stops it (no leak across the page lifetime).
import { describe, it, expect, vi, afterEach } from "vitest";

import { installUnhandledRejectionReporter } from "../globalErrorReporter.js";

function dispatchRejection(reason: unknown) {
  // jsdom lacks a PromiseRejectionEvent constructor, so synthesize an event of
  // the right type carrying a `reason`, matching the real event shape.
  const event = new Event("unhandledrejection") as Event & { reason: unknown };
  event.reason = reason;
  window.dispatchEvent(event);
}

describe("installUnhandledRejectionReporter", () => {
  afterEach(() => vi.restoreAllMocks());

  it("reports an unhandled rejection via console.error", () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const uninstall = installUnhandledRejectionReporter();
    const reason = new Error("boom");

    dispatchRejection(reason);

    expect(spy).toHaveBeenCalledWith("Unhandled promise rejection:", reason);
    uninstall();
  });

  it("stops reporting after uninstall", () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const uninstall = installUnhandledRejectionReporter();
    uninstall();

    dispatchRejection(new Error("after uninstall"));

    expect(spy).not.toHaveBeenCalled();
  });
});
