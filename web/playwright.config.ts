/// <reference types="node" />
import { defineConfig } from "@playwright/test";

const PORT = 15555;
const executablePath = process.env.PLAYWRIGHT_EXECUTABLE_PATH;

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.spec.ts",
  timeout: 30_000,
  retries: 0,
  use: {
    launchOptions: executablePath ? { executablePath } : undefined,
  },
  projects: [
    {
      name: "setup",
      testMatch: "auth.setup.ts",
    },
    {
      name: "chromium",
      use: {
        browserName: "chromium",
        baseURL: `http://localhost:${PORT}`,
        headless: true,
        storageState: ".auth/storage-state.json",
      },
      dependencies: ["setup"],
    },
  ],
  webServer: {
    command: `SERVER_BIN=${process.env.SERVER_BIN || "../bleephub-server"} PORT=${PORT} bash e2e/start-server.sh`,
    port: PORT,
    reuseExistingServer: false,
    timeout: 15_000,
  },
});
