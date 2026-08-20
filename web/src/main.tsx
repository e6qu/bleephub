import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { App } from "./App.js";
import { installUnhandledRejectionReporter } from "./globalErrorReporter.js";
import "./index.css";

// Report promise rejections that escape the render tree; ErrorBoundary only
// catches errors thrown during render.
installUnhandledRejectionReporter();

// No default refetchInterval: a signed-in shell mounts 10+ queries, and a
// blanket poll multiplies into ~120 requests/minute per open page — enough to
// exhaust the per-user API rate budget on its own (#113). Views that genuinely
// need live data opt in with an explicit per-query refetchInterval.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Client errors are deterministic — retrying a 404/403 only stretches
      // the spinner before the same answer (github.com 404s immediately).
      // Transient shapes (network faults, 5xx, 429) keep one retry.
      retry: (failureCount, error) => {
        const status = (error as { status?: number }).status;
        if (typeof status === "number" && status >= 400 && status < 500 && status !== 429) {
          return false;
        }
        return failureCount < 1;
      },
      refetchOnWindowFocus: false,
    },
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </StrictMode>,
);
