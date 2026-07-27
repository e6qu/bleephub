import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// Paths are resolved from this file, not from vitest's root. A relative
// "./src/test-setup.ts" is resolved against the root, which is the parent
// workspace when this package runs as one of its projects — that silently
// loaded the parent's setup file and left this package's cleanup(),
// sessionStorage and matchMedia polyfills uninstalled.
const dir = (relative: string) => fileURLToPath(new URL(relative, import.meta.url));

export default defineConfig({
  test: {
    name: "core",
    root: dir("."),
    environment: "jsdom",
    // jsdom's webstorage (localStorage / sessionStorage) is gated on an
    // origin — without `url` it defaults to about:blank and any access
    // throws. Pin to localhost so useTheme + any future storage-aware
    // hooks work in tests.
    environmentOptions: {
      jsdom: { url: "http://localhost/" },
    },
    setupFiles: [dir("./src/test-setup.ts")],
    include: ["src/__tests__/**/*.{test,spec}.{ts,tsx}"],
    exclude: ["e2e/**", "node_modules/**"],
  },
});
