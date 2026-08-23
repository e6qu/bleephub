// Screenshot + timing walkthrough of the seeded UI (scripts/perf/seed.sh).
// Logs in with the admin token, visits each core route, saves screenshots and
// a timings.json (navigation timing, FCP, per-page API call count) to OUT_DIR.
//
//   OUT_DIR=/tmp/bleephub-shots node scripts/perf/walkthrough.mjs
//
// Playwright is resolved from web/node_modules (CJS, so createRequire works).
import { createRequire } from "node:module";
import fs from "node:fs";

const require = createRequire(new URL("../../web/package.json", import.meta.url));
const { chromium } = require("@playwright/test");

const BASE = process.env.BLEEPHUB_BASE || "http://localhost:15599";
const TOKEN = process.env.BLEEPHUB_TOKEN || "bleephub-admin-token-00000000000000000000";
const OUT = process.env.OUT_DIR;
if (!OUT) {
  console.error("set OUT_DIR to a writable directory for screenshots + timings.json");
  process.exit(1);
}
fs.mkdirSync(OUT, { recursive: true });

const routes = [
  ["dashboard", "/ui/"],
  ["repo-home", "/ui/admin/hello-app"],
  ["repo-tree-src", "/ui/admin/hello-app/tree/main/src"],
  ["repo-blob", "/ui/admin/hello-app/blob/main/src/server.go"],
  ["repo-commits", "/ui/admin/hello-app/commits"],
  ["repo-branches", "/ui/admin/hello-app/branches"],
  ["issues-list", "/ui/admin/hello-app/issues"],
  ["issue-detail", "/ui/admin/hello-app/issues/7"],
  ["pulls-list", "/ui/admin/hello-app/pulls"],
  ["pr-detail", "/ui/admin/hello-app/pulls/121"],
  ["pr-files", "/ui/admin/hello-app/pulls/121/files"],
  ["releases", "/ui/admin/hello-app/releases"],
  ["wiki", "/ui/admin/hello-app/wiki"],
  ["repo-insights", "/ui/admin/hello-app/insights"],
  ["repo-actions", "/ui/admin/hello-app/actions"],
  ["repo-settings", "/ui/admin/hello-app/settings"],
  ["search", "/ui/search?q=intermittent&type=issues"],
  ["notifications", "/ui/notifications"],
  ["profile", "/ui/admin"],
  ["org-home", "/ui/orgs/acme"],
  ["account-settings", "/ui/account"],
];

const results = [];
const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
const page = await ctx.newPage();

await page.goto(BASE + "/ui/login");
await page.getByLabel(/token/i).fill(TOKEN);
await page.getByRole("button", { name: "Sign in" }).click();
await page.waitForURL("**/ui/");

let i = 0;
for (const [name, path] of routes) {
  i++;
  const apiCalls = [];
  const onResp = (r) => {
    const u = r.url();
    if (u.includes("/api/") || u.includes("/ui-data/")) {
      apiCalls.push({ url: u.replace(BASE, ""), status: r.status() });
    }
  };
  page.on("response", onResp);
  const t0 = Date.now();
  await page.goto(BASE + path, { waitUntil: "load" });
  try {
    await page.waitForLoadState("networkidle", { timeout: 8000 });
  } catch {
    // a polling widget keeps the network busy; the timings below still stand
  }
  const wall = Date.now() - t0;
  page.off("response", onResp);
  const perf = await page.evaluate(() => {
    const nav = performance.getEntriesByType("navigation")[0];
    const fcp = performance.getEntriesByName("first-contentful-paint")[0];
    const res = performance.getEntriesByType("resource");
    return {
      ttfb: Math.round(nav.responseStart),
      domContentLoaded: Math.round(nav.domContentLoadedEventEnd),
      load: Math.round(nav.loadEventEnd),
      fcp: fcp ? Math.round(fcp.startTime) : null,
      resources: res.length,
      transferKB: Math.round(res.reduce((a, r) => a + (r.transferSize || 0), 0) / 1024),
    };
  });
  const errors = apiCalls.filter((c) => c.status >= 400);
  await page.screenshot({ path: `${OUT}/${String(i).padStart(2, "0")}-${name}.png` });
  results.push({ name, path, wallMs: wall, ...perf, apiCallCount: apiCalls.length, apiErrors: errors });
  console.log(`${name}: wall=${wall}ms fcp=${perf.fcp}ms api=${apiCalls.length} err=${errors.length}`);
}

fs.writeFileSync(`${OUT}/timings.json`, JSON.stringify(results, null, 2));
await browser.close();
