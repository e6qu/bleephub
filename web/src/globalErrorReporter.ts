// No ErrorBoundary sees an uncaught promise rejection; surface it via
// console.error, which the telemetry and e2e suites observe.
// Returns an uninstall function for deterministic test teardown.
export function installUnhandledRejectionReporter(target: Window = window): () => void {
  const onRejection = (event: PromiseRejectionEvent) => {
    console.error("Unhandled promise rejection:", event.reason);
  };
  target.addEventListener("unhandledrejection", onRejection);
  return () => target.removeEventListener("unhandledrejection", onRejection);
}
