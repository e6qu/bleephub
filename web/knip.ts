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
    // @bleephub/ui-core is a component library: the package `exports` map (the
    // src/*/index.ts barrels) IS its public contract, so knip is left to treat
    // it as the surface rather than as call sites to be justified.
    //
    // includeEntryExports was tried here and is wrong for this workspace. It
    // reports every barrel re-export the SPA does not happen to import — 60
    // findings, all of them index.ts lines rather than implementations, and all
    // false: BackendApp, SimulatorApp, ResourceListPage, DataTable, Modal,
    // LogViewer and the rest are exercised by core/src/__tests__, which import
    // the modules directly and so never touch the barrel line knip flags. A
    // gate that fires on a library's own public API teaches people to silence
    // it. Dead code *within* the workspace — a module no barrel and no test
    // reaches — is still reported under the default setting, which is the
    // check that has value here.
  },
};

export default config;
