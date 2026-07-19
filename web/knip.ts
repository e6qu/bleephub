import type { KnipConfig } from "knip";

const config: KnipConfig = {
  ignoreExportsUsedInFile: true,
  workspaces: {
    ".": {
      entry: ["e2e/webhook-receiver.ts"],
    },
  },
};

export default config;
