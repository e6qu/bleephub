import type { KnipConfig } from "knip";

const config: KnipConfig = {
  ignoreExportsUsedInFile: true,
  workspaces: {
    ".": {
      // Both are launched out-of-band by shell, so nothing imports them and
      // knip cannot reach them from the Playwright config: the receiver by
      // e2e/start-server.sh, the SSO driver by scripts/test-shauth-sso.sh.
      entry: ["e2e/webhook-receiver.ts", "e2e/shauth-sso.mjs"],
    },
  },
};

export default config;
