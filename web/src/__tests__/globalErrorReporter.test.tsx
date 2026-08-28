// A rejected promise with no .catch escapes React; the global reporter is the
// only thing that surfaces it. Cover both reporting and that uninstall stops it.
import { describe, it, expect, vi, afterEach } from "vitest";

import { installUnhandledRejectionReporter } from "../globalErrorReporter.js";

function dispatchRejection(reason: unknown) {
  // jsdom lacks a PromiseRejectionEvent constructor; synthesize one carrying `reason`.
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
