import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// Absolute, resolved from this file — see core/vitest.config.ts for why a
// relative setup path is unsafe once projects are involved.
const dir = (relative: string) => fileURLToPath(new URL(relative, import.meta.url));

export default defineConfig({
  test: {
    projects: [
      {
        test: {
          name: "app",
          root: dir("."),
          environment: "jsdom",
          setupFiles: [dir("./src/test-setup.ts")],
          include: ["src/__tests__/**/*.{test,spec}.{ts,tsx}"],
          exclude: ["e2e/**", "node_modules/**"],
        },
      },
      dir("./core/vitest.config.ts"),
    ],
  },
});
