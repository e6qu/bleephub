import type { KnipConfig } from "knip";

const config: KnipConfig = {
  ignoreExportsUsedInFile: true,
  // jscpd is launched by ../scripts/jscpd.sh through its exact local binary;
  // it is intentionally not a package.json script or an imported module.
  ignoreDependencies: ["jscpd"],
  workspaces: {
    ".": {
      // Both are launched out-of-band by shell, so nothing imports them and
      // knip cannot reach them from the Playwright config: the receiver by
      // e2e/start-server.sh, the SSO driver by scripts/test-shauth-sso.sh.
      entry: ["e2e/webhook-receiver.ts", "e2e/shauth-sso.mjs"],
      // Generated from the vendored GitHub OpenAPI description
      // (scripts/gen-web-openapi-types.sh, WEB-013). It exports the full
      // paths/components/operations surface; only the schemas consumed via
      // type aliases in types.ts are referenced, so knip must not flag the
      // rest as unused.
      ignore: ["src/generated/**"],
    },
  },
};

export default config;
