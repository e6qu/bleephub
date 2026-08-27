import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { App } from "./App.js";
import { installUnhandledRejectionReporter } from "./globalErrorReporter.js";
import "./index.css";

// ErrorBoundary only catches render-time throws; this reports escaped rejections.
installUnhandledRejectionReporter();

// No default refetchInterval: a blanket poll across the shell's 10+ queries
// would exhaust the per-user rate budget (#113). Live views opt in per query.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Don't retry deterministic 4xx (except 429); keep one retry for
      // network faults, 5xx, 429.
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
