import assert from "node:assert/strict";
import { chromium } from "@playwright/test";

const password = process.env.SHAUTH_BOOTSTRAP_ADMIN_PASSWORD;
assert.ok(password, "SHAUTH_BOOTSTRAP_ADMIN_PASSWORD is required");

const browser = await chromium.launch({
  headless: true,
  executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined,
});
const errors = [];
try {
  const context = await browser.newContext();
  const page = await context.newPage();
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("requestfailed", (request) => errors.push(`${request.url()}: ${request.failure()?.errorText ?? "request failed"}`));
  page.on("request", (request) => {
    const target = new URL(request.url());
    if (target.hostname !== "localhost" && target.hostname !== "127.0.0.1") {
      errors.push(`external runtime dependency: ${target.origin}${target.pathname}`);
    }
  });

  // Direct entry starts the real OIDC authorization-code flow. The Shauth
  // password login establishes the one shared identity-provider session.
  await page.goto("http://localhost:15555/ui/");
  await page.locator("#username").fill("admin");
  await page.locator("#password").fill(password);
  await page.getByRole("button", { name: "Sign in with password" }).click();
  await page.waitForURL("http://localhost:15555/ui/");
  await page.getByRole("button", { name: "Open user menu" }).click();
  await page.getByText("Signed in as").waitFor();
  await page.getByText("admin", { exact: true }).waitFor();
  assert.equal(await page.getByText("Access token", { exact: true }).count(), 0);
  await assertAuthenticated(context, 15555, true);

  // The packages page exercises the authenticated-user package endpoint with
  // GitHub's mandatory package_type parameter instead of returning HTTP 400.
  const packages = page.waitForResponse((response) => {
    const target = new URL(response.url());
    return target.pathname === "/api/v3/user/packages" && target.searchParams.get("package_type") === "container";
  });
  await page.goto("http://localhost:15555/ui/packages");
  assert.equal((await packages).status(), 200);
  await page.getByText("No packages yet.").waitFor();

  // Catalog entry uses the already authenticated Shauth session. It must not
  // render another credential form or expose Bleephub's legacy token form.
  await page.goto("http://localhost:8080/apps");
  await page.locator('a[href="http://localhost:15556/ui/"]').click();
  await page.waitForURL("http://localhost:15556/ui/");
  await page.getByRole("button", { name: "Open user menu" }).waitFor();
  assert.equal(await page.locator("#username").count(), 0);
  assert.equal(await page.getByText("Access token", { exact: true }).count(), 0);
  await assertAuthenticated(context, 15556, true);

  // RP-Initiated Logout returns to the initiating Bleephub instance while the
  // provider propagates logout to every correlated Bleephub relying party.
  await page.goto("http://localhost:15555/ui/");
  await page.getByRole("button", { name: "Open user menu" }).click();
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await page.waitForURL("http://localhost:15555/ui/signed-out");
  await page.getByRole("heading", { name: "You are signed out" }).waitFor();
  await assertAuthenticated(context, 15555, false);
  await assertAuthenticated(context, 15556, false);

  // A direct visit after global logout fails closed and returns to Shauth's
  // actual login form, proving neither relying party retained access.
  await page.goto("http://localhost:15556/ui/");
  await page.waitForURL((url) => url.origin === "http://localhost:8080" && url.pathname === "/login");
  await page.locator("#username").waitFor();
  await page.waitForLoadState("networkidle");
  assert.deepEqual(errors, []);
} finally {
  await browser.close();
}

async function assertAuthenticated(context, port, expected) {
  const response = await context.request.get(`http://localhost:${port}/auth/session`);
  assert.equal(response.status(), 200);
  const body = await response.json();
  assert.equal(body.authenticated, expected, `Bleephub ${port} authentication state`);
  if (expected) {
    assert.equal(body.user.login, "admin");
    assert.equal(body.user.email, "admin@localhost.test");
  }
}
