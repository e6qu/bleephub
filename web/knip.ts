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
    },
    // The `core` workspace (@bleephub/ui-core) is a deliberately general-purpose
    // component library: alongside the primitives Bleephub's own SPA consumes it
    // ships a reusable operator shell (AppShell/BackendApp/SimulatorApp, the
    // Overview/Containers/Resources/Metrics pages, and the API client) that this
    // repo intentionally does NOT mount — see the header of core/e2e/
    // backend-app.spec.ts. Those exports are its public contract, unit-tested in
    // core/src/__tests__ and type-checked here, so knip is left to treat the
    // package `exports` map as the public surface (no includeEntryExports). We do
    // NOT prune ui-core to the SPA's current usage; that is an owner product
    // decision (WEB-016/WEB-017), resolved "keep as library / by design".
  },
};

export default config;
