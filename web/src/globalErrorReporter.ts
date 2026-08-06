// A promise that rejects with no `.catch` escapes React's render tree entirely:
// no ErrorBoundary sees it, so the failure would vanish into the browser's
// default console line alone. Install a global `unhandledrejection` reporter so
// those rejections are surfaced the same way ErrorBoundary surfaces render
// errors (console.error, which the telemetry and end-to-end suites observe).
//
// Returns an uninstall function so tests can add and remove the listener
// deterministically; production installs it once for the page lifetime.
export function installUnhandledRejectionReporter(target: Window = window): () => void {
  const onRejection = (event: PromiseRejectionEvent) => {
    console.error("Unhandled promise rejection:", event.reason);
  };
  target.addEventListener("unhandledrejection", onRejection);
  return () => target.removeEventListener("unhandledrejection", onRejection);
}
