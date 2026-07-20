import assert from "node:assert/strict";
import { chromium } from "@playwright/test";

const password = process.env.SHAUTH_BOOTSTRAP_ADMIN_PASSWORD;
assert.ok(password, "SHAUTH_BOOTSTRAP_ADMIN_PASSWORD is required");
const validatorUsername = process.env.SHAUTH_VALIDATOR_USERNAME;
assert.ok(validatorUsername, "SHAUTH_VALIDATOR_USERNAME is required");
const nonAuthenticCredentialSentinel = process.env.BLEEPHUB_NON_AUTHENTIC_CREDENTIAL_SENTINEL;
assert.ok(nonAuthenticCredentialSentinel, "BLEEPHUB_NON_AUTHENTIC_CREDENTIAL_SENTINEL is required");
assert.notEqual(nonAuthenticCredentialSentinel, password, "negative-probe sentinel must differ from the Shauth password");
const primaryPort = requiredPort("BLEEPHUB_SSO_PRIMARY_PORT");
const secondaryPort = requiredPort("BLEEPHUB_SSO_SECONDARY_PORT");
assert.notEqual(primaryPort, secondaryPort, "Bleephub SSO ports must be distinct");
const primaryOrigin = `http://localhost:${primaryPort}`;
const secondaryOrigin = `http://127.0.0.1:${secondaryPort}`;
const shauthOrigin = "http://localhost:8080";

const browser = await chromium.launch({
  headless: true,
  executablePath: process.env.PLAYWRIGHT_EXECUTABLE_PATH || undefined,
});
const errors = [];
try {
  await assertCredentialBoundary(browser);

  const context = await browser.newContext();
  const page = await context.newPage();
  const credentialBoundary = await installCredentialBoundary(context, page);
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(message.text());
  });
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("requestfailed", (request) => errors.push(`${request.url()}: ${request.failure()?.errorText ?? "request failed"}`));
  page.on("request", (request) => {
    const target = new URL(request.url());
    if (target.hostname !== "localhost" && target.hostname !== "127.0.0.1" && !target.hostname.endsWith(".localhost")) {
      errors.push(`external runtime dependency: ${target.origin}${target.pathname}`);
    }
  });

  // Bleephub rejects non-authentic values through every app-local password
  // and token authentication surface without creating a browser session.
  const localCredentialAttempt = await context.request.post(`${primaryOrigin}/auth/local`, {
    data: { login: validatorUsername, password: nonAuthenticCredentialSentinel },
  });
  assert.equal(localCredentialAttempt.status(), 401);
  const legacyLocalCredentialAttempt = await context.request.post(`${primaryOrigin}/login`, {
    form: { login: validatorUsername, password: nonAuthenticCredentialSentinel },
  });
  assert.equal(legacyLocalCredentialAttempt.status(), 401);
  for (const authorization of [
    `Bearer ${nonAuthenticCredentialSentinel}`,
    `token ${nonAuthenticCredentialSentinel}`,
    `Basic ${Buffer.from(`${validatorUsername}:${nonAuthenticCredentialSentinel}`).toString("base64")}`,
  ]) {
    const tokenCredentialAttempt = await context.request.get(`${primaryOrigin}/api/v3/user`, {
      headers: { Authorization: authorization },
    });
    assert.equal(tokenCredentialAttempt.status(), 401, `${authorization.split(" ", 1)[0]} validator credential was accepted`);
  }
  const anonymousSession = await context.request.get(`${primaryOrigin}/auth/session`);
  assert.equal(anonymousSession.status(), 200);
  assert.deepEqual(await anonymousSession.json(), { authenticated: false });
  assert.equal(
    (await context.cookies(primaryOrigin)).some((cookie) => cookie.name === "_gh_sess"),
    false,
    "Shauth validator credentials must not create a Bleephub browser session",
  );

  // Direct entry starts the real OIDC authorization-code flow. The Shauth
  // password login establishes the one shared identity-provider session.
  await page.goto(`${primaryOrigin}/ui/`);
  await page.locator("#username").fill(validatorUsername);
  await page.locator("#password").fill(password);
  await page.getByRole("button", { name: "Sign in with password" }).click();
  await page.waitForURL(`${primaryOrigin}/ui/`);
  await page.getByRole("button", { name: "Open user menu" }).click();
  await page.getByText("Signed in as").waitFor();
  await page.getByText(validatorUsername, { exact: true }).waitFor();
  assert.equal(await page.getByText("Access token", { exact: true }).count(), 0);
  await assertAuthenticated(context, primaryOrigin, true);

  // The deployment-neutral validation coordinate fails closed and exposes an
  // exact, visible, accessible app-local logout control for Shauth validation.
  await page.goto(`${primaryOrigin}/auth/validation`);
  await page.getByRole("heading", { name: "Bleephub is authenticated" }).waitFor();
  const validationSignOut = page.getByRole("button", { name: "Sign out", exact: true });
  assert.equal(await validationSignOut.getAttribute("type"), "submit");
  const validationSignOutForm = validationSignOut.locator("xpath=ancestor::form");
  assert.equal(await validationSignOutForm.getAttribute("action"), "/auth/logout");
  assert.equal(await validationSignOutForm.getAttribute("method"), "post");
  await assertAuthenticated(context, primaryOrigin, true);

  // The packages page exercises the authenticated-user package endpoint with
  // GitHub's mandatory package_type parameter instead of returning HTTP 400.
  const packages = page.waitForResponse((response) => {
    const target = new URL(response.url());
    return target.pathname === "/api/v3/user/packages" && target.searchParams.get("package_type") === "container";
  });
  await page.goto(`${primaryOrigin}/ui/packages`);
  assert.equal((await packages).status(), 200);
  await page.getByText("No packages yet.").waitFor();

  // Catalog entry uses the already authenticated Shauth session. It must not
  // render another credential form or expose Bleephub's legacy token form.
  await page.goto(`${shauthOrigin}/apps`);
  await page.locator(`a[href="${secondaryOrigin}/ui/"]`).click();
  await page.waitForURL(`${secondaryOrigin}/ui/`);
  await page.getByRole("button", { name: "Open user menu" }).waitFor();
  assert.equal(await page.locator("#username").count(), 0);
  assert.equal(await page.getByText("Access token", { exact: true }).count(), 0);
  await assertAuthenticated(context, secondaryOrigin, true);

  // RP-Initiated Logout returns to a persistent page on the initiating
  // Bleephub origin while the provider revokes every correlated relying-party
  // session. The page remains inert on reload and exposes one explicit,
  // application-local recovery control.
  await page.goto(`${primaryOrigin}/ui/`);
  await page.getByRole("button", { name: "Open user menu" }).click();
  const signedOutNavigation = page.waitForResponse(
    (response) => response.url() === `${primaryOrigin}/ui/signed-out` && response.request().isNavigationRequest(),
  );
  await page.getByRole("menuitem", { name: "Sign out" }).click();
  await page.waitForURL(`${primaryOrigin}/ui/signed-out`);
  assert.equal((await signedOutNavigation).status(), 200);
  await page.getByRole("heading", { name: "You are signed out" }).waitFor();
  await page.getByText("Bleephub", { exact: true }).waitFor();
  let signInControl = page.getByRole("link", { name: "Sign in with Shauth" });
  assert.equal(
    await signInControl.getAttribute("href"),
    "/auth/shauth?return_to=%2Fui%2F",
    "signed-out recovery must use Bleephub's same-origin Shauth starter",
  );
  let signOutControl = page.getByRole("button", { name: "Sign out", exact: true });
  assert.equal(await signOutControl.getAttribute("type"), "submit");
  let signOutForm = signOutControl.locator("xpath=ancestor::form");
  assert.equal(await signOutForm.getAttribute("action"), "/auth/logout");
  assert.equal(await signOutForm.getAttribute("method"), "post");
  await assertAuthenticated(context, primaryOrigin, false);
  await assertAuthenticated(context, secondaryOrigin, false);

  const reload = await page.reload();
  assert.equal(reload?.status(), 200);
  assert.equal(page.url(), `${primaryOrigin}/ui/signed-out`);
  await page.getByRole("heading", { name: "You are signed out" }).waitFor();
  signInControl = page.getByRole("link", { name: "Sign in with Shauth" });
  assert.equal(await signInControl.getAttribute("href"), "/auth/shauth?return_to=%2Fui%2F");
  signOutControl = page.getByRole("button", { name: "Sign out", exact: true });
  signOutForm = signOutControl.locator("xpath=ancestor::form");
  assert.equal(await signOutForm.getAttribute("action"), "/auth/logout");
  assert.equal(await signOutForm.getAttribute("method"), "post");

  // Recovery traverses Bleephub's own starter, prompts at Shauth after global
  // logout, and returns to a fully authenticated Bleephub UI without exposing
  // the legacy access-token form.
  await signInControl.click();
  await page.waitForURL((url) => url.origin === shauthOrigin && url.pathname === "/login");
  await page.locator("#username").fill(validatorUsername);
  await page.locator("#password").fill(password);
  await page.getByRole("button", { name: "Sign in with password" }).click();
  await page.waitForURL(`${primaryOrigin}/ui/`);
  await page.getByRole("button", { name: "Open user menu" }).waitFor();
  assert.equal(await page.getByText("Access token", { exact: true }).count(), 0);
  await assertAuthenticated(context, primaryOrigin, true);

  // Once recovered, direct entry to the second relying party is silent SSO:
  // it returns to the application without another credential prompt.
  await page.goto(`${secondaryOrigin}/ui/`);
  await page.waitForURL(`${secondaryOrigin}/ui/`);
  await page.getByRole("button", { name: "Open user menu" }).waitFor();
  assert.equal(await page.locator("#username").count(), 0);
  await assertAuthenticated(context, secondaryOrigin, true);

  // Provider-initiated logout is the complementary global-revocation path.
  // It invalidates both relying-party sessions, and a direct application visit
  // then fails closed at Shauth's real credential form.
  await page.goto(`${shauthOrigin}/logout`);
  await page.getByRole("heading", { name: "Sign out everywhere?" }).waitFor();
  await page.getByRole("button", { name: "Sign out everywhere" }).click();
  await waitForAuthenticationState(context, primaryOrigin, false);
  await waitForAuthenticationState(context, secondaryOrigin, false);

  await page.goto(`${primaryOrigin}/ui/`);
  await page.waitForURL((url) => url.origin === shauthOrigin && url.pathname === "/login");
  await page.locator("#username").waitFor();
  await page.waitForLoadState("networkidle");
  assert.deepEqual(credentialBoundary.handlerErrors, [], "Shauth credential boundary failed internally");
  assert.deepEqual(credentialBoundary.violations, [], "Shauth password crossed the identity-provider boundary");
  assert.deepEqual(errors, []);
} finally {
  await browser.close();
}

async function assertCredentialBoundary(browser) {
  const headerContext = await browser.newContext({
    extraHTTPHeaders: {
      Authorization: `Basic ${Buffer.from(`${validatorUsername}:${password}`).toString("base64")}`,
    },
  });
  try {
    const page = await headerContext.newPage();
    const boundary = await installCredentialBoundary(headerContext, page);
    await assert.rejects(page.goto(`${primaryOrigin}/api/v3/user`), /ERR_BLOCKED_BY_CLIENT/);
    assert.deepEqual(boundary.handlerErrors, []);
    assert.deepEqual(boundary.violations, [{ method: "GET", url: `${primaryOrigin}/api/v3/user` }]);
  } finally {
    await headerContext.close();
  }

  const mutatedTargetContext = await browser.newContext();
  try {
    const page = await mutatedTargetContext.newPage();
    const boundary = await installCredentialBoundary(mutatedTargetContext, page);
    await page.setContent(`
      <form method="post" action="${shauthOrigin}/login"
        onsubmit="this.action='${primaryOrigin}/auth/local'">
        <input name="username">
        <input name="password" type="password">
        <button type="submit">Submit</button>
      </form>
    `);
    await page.locator('[name="username"]').fill(validatorUsername);
    await page.locator('[name="password"]').fill(password);
    const failedRequest = page.waitForEvent("requestfailed");
    await page.getByRole("button", { name: "Submit" }).click();
    await failedRequest;
    assert.deepEqual(boundary.handlerErrors, []);
    assert.deepEqual(boundary.violations, [{ method: "POST", url: `${primaryOrigin}/auth/local` }]);
  } finally {
    await mutatedTargetContext.close();
  }

  const redirectContext = await browser.newContext();
  try {
    const redirectTarget = `${primaryOrigin}/credential-sink?password=${encodeURIComponent(password)}`;
    const page = await redirectContext.newPage();
    const boundary = await installCredentialBoundary(redirectContext, page, {
      redirectLoginTo: redirectTarget,
    });
    await page.setContent(`
      <form method="post" action="${shauthOrigin}/login">
        <input name="username">
        <input name="password" type="password">
        <button type="submit">Submit</button>
      </form>
    `);
    await page.locator('[name="username"]').fill(validatorUsername);
    await page.locator('[name="password"]').fill(password);
    const failedRequest = page.waitForEvent("requestfailed");
    await page.getByRole("button", { name: "Submit" }).click();
    await failedRequest;
    assert.deepEqual(boundary.handlerErrors, []);
    assert.deepEqual(boundary.violations, [{ method: "POST", url: redirectTarget }]);
  } finally {
    await redirectContext.close();
  }
}

async function installCredentialBoundary(context, page, { redirectLoginTo } = {}) {
  const violations = [];
  const handlerErrors = [];
  let redirectedLogin = false;
  const session = await context.newCDPSession(page);
  await session.send("Fetch.enable", { patterns: [{ urlPattern: "*", requestStage: "Request" }] });
  session.on("Fetch.requestPaused", async (event) => {
    try {
      const request = event.request;
      if (!requestContainsCredential(request)) {
        await session.send("Fetch.continueRequest", { requestId: event.requestId });
        return;
      }

      const target = new URL(request.url);
      const isShauthLogin =
        request.method === "POST" &&
        target.origin === shauthOrigin &&
        target.pathname === "/login" &&
        target.search === "" &&
        target.hash === "";
      if (!isShauthLogin) {
        violations.push({ method: request.method, url: request.url });
        await session.send("Fetch.failRequest", { requestId: event.requestId, errorReason: "BlockedByClient" });
        return;
      }

      if (redirectLoginTo && !redirectedLogin) {
        redirectedLogin = true;
        await session.send("Fetch.fulfillRequest", {
          requestId: event.requestId,
          responseCode: 307,
          responseHeaders: [{ name: "Location", value: redirectLoginTo }],
        });
        return;
      }
      await session.send("Fetch.continueRequest", { requestId: event.requestId });
    } catch (error) {
      handlerErrors.push(error instanceof Error ? error.message : String(error));
    }
  });
  return { handlerErrors, violations };
}

function requestContainsCredential(request) {
  const basicCredential = Buffer.from(`${validatorUsername}:${password}`).toString("base64");
  const encodedPassword = encodeURIComponent(password);
  const encodedCredential = Buffer.from(password).toString("base64");
  const payloads = [request.url, request.postData ?? "", ...Object.entries(request.headers).flat()];
  return payloads.some(
    (payload) =>
      payload.includes(password) ||
      payload.includes(encodedPassword) ||
      payload.includes(encodedCredential) ||
      payload.includes(basicCredential),
  );
}

async function assertAuthenticated(context, origin, expected) {
  const response = await context.request.get(`${origin}/auth/session`);
  assert.equal(response.status(), 200);
  const body = await response.json();
  assert.equal(body.authenticated, expected, `Bleephub ${origin} authentication state`);
  if (expected) {
    assert.equal(body.user.login, validatorUsername);
    assert.equal(body.user.email, "admin@localhost.test");
  }
}

async function waitForAuthenticationState(context, origin, expected) {
  let lastError;
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try {
      await assertAuthenticated(context, origin, expected);
      return;
    } catch (error) {
      lastError = error;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
  }
  throw lastError;
}

function requiredPort(name) {
  const value = process.env[name];
  assert.match(value ?? "", /^\d+$/, `${name} is required`);
  const port = Number(value);
  assert.ok(port > 0 && port <= 65535, `${name} must be a valid TCP port`);
  return port;
}
