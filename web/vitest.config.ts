import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// Absolute, resolved from this file — see core/vitest.config.ts for why a
// relative setup path is unsafe once projects are involved.
const dir = (relative: string) => fileURLToPath(new URL(relative, import.meta.url));

export default defineConfig({
  test: {
    coverage: {
      provider: "v8",
      include: ["src/**/*.{ts,tsx}", "core/src/**/*.{ts,tsx}"],
      exclude: [
        "src/__tests__/**",
        "core/src/__tests__/**",
        "src/test-setup.ts",
        "core/src/test-setup.ts",
        "**/*.d.ts",
      ],
      reporter: ["text-summary", "json-summary"],
      thresholds: {
        statements: 65,
        branches: 62,
        functions: 55,
        lines: 68,
      },
    },
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
