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
      retry: 1,
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
