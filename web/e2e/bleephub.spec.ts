import { test, expect, type Page } from "./fixtures.js";
import fs from "fs";
import path from "path";
import { randomUUID } from "crypto";

// Fail on browser console errors or uncaught page exceptions; warnings only log.
test.beforeEach(({ page }, testInfo) => {
  page.on("console", (msg) => {
    const text = msg.text();
    const type = msg.type();
    if (type === "error") {
      throw new Error(`Console error in ${testInfo.title}: ${text}`);
    }
    if (type === "warning") {
      // eslint-disable-next-line no-console
      console.warn(`[browser warning] ${testInfo.title}: ${text}`);
    }
  });
  page.on("pageerror", (err) => {
    throw new Error(`Uncaught page error in ${testInfo.title}: ${err.message}`);
  });
});

// Keep screenshots under Playwright's ignored test-results dir.
const SCREENSHOT_DIR = path.resolve(process.cwd(), "test-results/screenshots");

function ensureScreenshotDir(): void {
  fs.mkdirSync(SCREENSHOT_DIR, { recursive: true });
}

async function shot(page: Page, name: string): Promise<void> {
  ensureScreenshotDir();
  await page.screenshot({
    path: path.join(SCREENSHOT_DIR, `${name}.png`),
    fullPage: true,
  });
}

// ─── helpers ────────────────────────────────────────────────────────────────

const TOKEN = "bleephub-admin-token-00000000000000000000";
const BASE = "http://localhost:15555";
const WEBHOOK_BASE = "http://127.0.0.1:15557";

async function apiPost(page: Page, path: string, body: unknown) {
  return page.evaluate(
    async ({ base, path, token, body }) => {
      const res = await fetch(base + path, {
        method: "POST",
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
        body: JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`${res.status} ${res.statusText} ${await res.text()}`);
      if (res.status === 204) return null;
      const text = await res.text();
      return text ? JSON.parse(text) : null;
    },
    { base: BASE, path, token: TOKEN, body },
  );
}

async function apiPut(page: Page, path: string, body: unknown) {
  return page.evaluate(
    async ({ base, path, token, body }) => {
      const res = await fetch(base + path, {
        method: "PUT",
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
        body: JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`${res.status} ${res.statusText} ${await res.text()}`);
      const text = await res.text();
      return text ? JSON.parse(text) : null;
    },
    { base: BASE, path, token: TOKEN, body },
  );
}

async function apiGet(page: Page, path: string) {
  return page.evaluate(
    async ({ base, path, token }) => {
      const res = await fetch(base + path, {
        headers: { Authorization: `Bearer ${token}` },
      });
      return res.ok ? res.json() : null;
    },
    { base: BASE, path, token: TOKEN },
  );
}

// Fixtures reuse fixed repo names across runs, so tolerate ONLY the re-create
// conflict (422/409) and rethrow any other failure rather than masking it.
function ignoreAlreadyExists(e: unknown): null {
  const msg = String(e);
  if (msg.includes("422") || msg.includes("409") || /already exists/i.test(msg)) {
    return null;
  }
  throw e;
}

// Open the global-nav drawer (hamburger) holding GitHub + Operations destinations.
async function openDrawer(page: Page): Promise<void> {
  await page.getByRole("button", { name: "Open global navigation" }).click();
}

// ─── redirect ───────────────────────────────────────────────────────────────

test.describe("Root redirect", () => {
  test("/ redirects to /ui/", async ({ page }) => {
    const res = await page.goto("/");
    expect(page.url()).toContain("/ui/");
    expect(res?.status()).toBe(200);
    await shot(page, "00-root-redirect");
  });
});

// ─── Operations console (/ui/operations) ──────────────────────────────────────────
// The "System status" console lives at /ui/operations; root /ui/ is the dashboard.

test.describe("Operations console", () => {
  test("renders the System status heading", async ({ page }) => {
    await page.goto("/ui/operations");
    // Brand is a header link; the page title is the h1.
    await expect(page.getByRole("link", { name: "bleephub" })).toBeVisible();
    await expect(page.getByRole("heading", { level: 1 }).filter({ hasText: "System status" })).toBeVisible();
    await shot(page, "01-ops-console");
  });

  test("shows metrics cards", async ({ page }) => {
    await page.goto("/ui/operations");
    await expect(page.getByText("Active Workflows")).toBeVisible();
    await expect(page.getByText("Connected Runners")).toBeVisible();
    await expect(page.getByText("Workflow submissions", { exact: true })).toBeVisible();
    await shot(page, "02-ops-metrics");
  });
});

// ─── Global-nav drawer ───────────────────────────────────────────────────────

test.describe("Global navigation", () => {
  test("the drawer lists the GitHub and Operations destinations", async ({ page }) => {
    await page.goto("/ui/");
    await openDrawer(page);
    const drawer = page.getByRole("navigation", { name: "Global" });
    await expect(drawer.getByRole("link", { name: "Repositories" })).toBeVisible();
    await expect(drawer.getByRole("link", { name: "Classroom" })).toBeVisible();
    await expect(drawer.getByRole("link", { name: "Marketplace" })).toBeVisible();
    await expect(drawer.getByRole("link", { name: "Runners" })).toBeVisible();
    await expect(drawer.getByRole("link", { name: "GitHub Apps" })).toBeVisible();
    await expect(drawer.getByRole("link", { name: "OAuth Apps" })).toBeVisible();
    await expect(drawer.getByRole("link", { name: "Metrics" })).toBeVisible();
    await expect(drawer.getByRole("link", { name: "System status" })).toBeVisible();
    await shot(page, "03-nav-drawer");
  });

  test("navigates between pages via the drawer", async ({ page }) => {
    await page.goto("/ui/");
    const drawer = page.getByRole("navigation", { name: "Global" });

    await openDrawer(page);
    await drawer.getByRole("link", { name: "Runners" }).click();
    await expect(page).toHaveURL(/\/ui\/runners/);
    await expect(page.getByRole("heading", { level: 1 }).filter({ hasText: "Runners" })).toBeVisible();
    await shot(page, "04-runners");

    await openDrawer(page);
    await drawer.getByRole("link", { name: "Repositories" }).click();
    await expect(page).toHaveURL(/\/ui\/repos/);
    await expect(page.getByRole("heading", { level: 1 }).filter({ hasText: "Repositories" })).toBeVisible();
    await shot(page, "05-repos");

    await openDrawer(page);
    await drawer.getByRole("link", { name: "GitHub Apps" }).click();
    await expect(page).toHaveURL(/\/ui\/apps/);
    await expect(page.getByRole("heading", { level: 1 }).filter({ hasText: "Apps" })).toBeVisible();
    await shot(page, "06-apps");

    await openDrawer(page);
    await drawer.getByRole("link", { name: "OAuth Apps" }).click();
    await expect(page).toHaveURL(/\/ui\/oauth/);
    await shot(page, "07-oauth");

    await openDrawer(page);
    await drawer.getByRole("link", { name: "Metrics" }).click();
    await expect(page).toHaveURL(/\/ui\/metrics/);
    await expect(page.getByRole("heading", { level: 1 }).filter({ hasText: /actions throughput/i })).toBeVisible();
    await shot(page, "08-metrics");

    await openDrawer(page);
    await drawer.getByRole("link", { name: "Dashboard" }).click();
    await expect(page).toHaveURL(/\/ui\/$/);
    await shot(page, "09-back-dashboard");
  });
});

test.describe("Repository search", () => {
  test("excludes a topic through shareable GitHub-compatible filters", async ({ page }) => {
    const marker = `search-${randomUUID().slice(0, 12)}`;
    const included = `${marker}-current`;
    const excluded = `${marker}-legacy`;
    await page.goto("/ui/");
    await apiPost(page, "/api/v3/user/repos", {
      name: included,
      description: marker,
    });
    await apiPost(page, "/api/v3/user/repos", {
      name: excluded,
      description: marker,
    });
    await apiPut(page, `/api/v3/repos/admin/${excluded}/topics`, { names: ["web"] });

    await page.goto(`/ui/search?q=${encodeURIComponent(marker)}&type=repositories`);
    await expect(page.getByRole("link", { name: `admin/${included}` })).toBeVisible();
    await expect(page.getByRole("link", { name: `admin/${excluded}` })).toBeVisible();

    const filteredResponse = page.waitForResponse((response) => {
      const requestURL = new URL(response.url());
      return requestURL.pathname.endsWith("/search/repositories") &&
        (requestURL.searchParams.get("q")?.includes("-topic:web") ?? false);
    });
    await page.getByLabel("Excluded repository topic").fill("web");
    expect((await filteredResponse).status()).toBe(200);
    await expect(page).toHaveURL(/exclude_topic=web/);
    await expect(page.getByRole("link", { name: `admin/${included}` })).toBeVisible();
    await expect(page.getByRole("link", { name: `admin/${excluded}` })).toHaveCount(0);
  });
});

test.describe("Repository code and collaboration", () => {
  test("navigates commit filters, ref comparisons, and source archives", async ({ page }) => {
    const name = `code-${randomUUID().slice(0, 12)}`;
    await page.goto("/ui/");
    await apiPost(page, "/api/v3/user/repos", { name, auto_init: true });
    const mainRef = await apiGet(page, `/api/v3/repos/admin/${name}/git/ref/heads/main`);
    await apiPost(page, `/api/v3/repos/admin/${name}/git/refs`, {
      ref: "refs/heads/feature",
      sha: mainRef.object.sha,
    });
    await apiPut(page, `/api/v3/repos/admin/${name}/contents/guide.md`, {
      message: "add guide",
      content: Buffer.from("# Guide\n").toString("base64"),
      branch: "feature",
    });

    await page.goto(`/ui/admin/${name}`);
    await page.getByRole("button", { name: /Code/ }).click();
    await expect(page.getByRole("link", { name: "Download ZIP" })).toHaveAttribute(
      "href",
      `/api/v3/repos/admin/${name}/zipball/main`,
    );
    await expect(page.getByRole("link", { name: "Download TAR.GZ" })).toHaveAttribute(
      "href",
      `/api/v3/repos/admin/${name}/tarball/main`,
    );

    await page.goto(`/ui/admin/${name}/commits`);
    await page.getByLabel("Branch or ref").fill("feature");
    await page.getByLabel("Path").fill("guide.md");
    const filtered = page.waitForResponse((response) => {
      const url = new URL(response.url());
      return url.pathname.endsWith(`/repos/admin/${name}/commits`) &&
        url.searchParams.get("sha") === "feature" &&
        url.searchParams.get("path") === "guide.md";
    });
    await page.getByRole("button", { name: "Apply filters" }).click();
    expect((await filtered).status()).toBe(200);
    await expect(page.getByRole("link", { name: "add guide" })).toBeVisible();

    await page.goto(`/ui/admin/${name}/branches`);
    await page.getByRole("link", { name: "Compare" }).click();
    await expect(page).toHaveURL(new RegExp(`/ui/admin/${name}/compare/main\\.\\.\\.feature`));
    await expect(page.getByText("guide.md")).toBeVisible();
    await expect(page.getByText("ahead", { exact: true })).toBeVisible();
  });
});

test.describe("User menu and packages", () => {
  test("labels personal destinations consistently and submits sign-out", async ({ page }) => {
    await page.goto("/ui/");
    // Prove the signed-in identity and a real sign-out control, not a stub.
    const identity = page.locator("[data-shauth-user]");
    await expect(identity).toBeVisible();
    expect((await identity.getAttribute("data-shauth-user")) ?? "").not.toBe("");
    await page.getByRole("button", { name: "Open user menu" }).click();
    await expect(page.locator("[data-shauth-sign-out]")).toBeVisible();
    const menu = page.getByRole("menu");
    for (const label of ["My profile", "My repositories", "My gists", "My packages", "My codespaces"]) {
      await expect(menu.getByRole("menuitem", { name: label })).toBeVisible();
    }

    const logoutRequest = page.waitForRequest((request) => request.method() === "POST" && new URL(request.url()).pathname === "/auth/logout");
    await menu.getByRole("menuitem", { name: "Sign out" }).click();
    await logoutRequest;
    await expect(page).toHaveURL(/\/ui\/login/);
  });

  test("loads each package tab via the /ui-data aggregation with its package type", async ({ page }) => {
    // Read the /ui-data aggregation (optional package_type) not REST /user/packages,
    // which 400s without a package_type; each tab still narrows via package_type.
    const containerResponse = page.waitForResponse((response) => {
      const requestURL = new URL(response.url());
      return requestURL.pathname.startsWith("/ui-data/users/") && requestURL.pathname.endsWith("/packages") && requestURL.searchParams.get("package_type") === "container";
    });
    await page.goto("/ui/packages");
    expect((await containerResponse).status()).toBe(200);
    await expect(page.getByText("No packages yet.")).toBeVisible();
    await expect(page.getByText("Failed to load packages")).toHaveCount(0);

    const npmResponse = page.waitForResponse((response) => {
      const requestURL = new URL(response.url());
      return requestURL.pathname.startsWith("/ui-data/users/") && requestURL.pathname.endsWith("/packages") && requestURL.searchParams.get("package_type") === "npm";
    });
    await page.getByRole("tab", { name: "npm" }).click();
    expect((await npmResponse).status()).toBe(200);
    await expect(page.getByText("No packages yet.")).toBeVisible();
  });
});

// ─── Dark / light mode toggle ────────────────────────────────────────────────

test.describe("Theme toggle", () => {
  test("saturated GitHub chrome resolves in both light and dark themes", async ({ page }) => {
    await page.goto("/ui/");
    await page.waitForLoadState("networkidle");
    const readChrome = () => {
      const css = getComputedStyle(document.documentElement);
      const header = document.querySelector(".app-header")!;
      const headerStyle = getComputedStyle(header);
      return {
        accent: css.getPropertyValue("--color-accent").trim(),
        blue: css.getPropertyValue("--color-brand-blue").trim(),
        purple: css.getPropertyValue("--color-brand-purple").trim(),
        cyan: css.getPropertyValue("--color-brand-cyan").trim(),
        pink: css.getPropertyValue("--color-brand-pink").trim(),
        headerImage: headerStyle.backgroundImage,
        headerColor: headerStyle.backgroundColor,
      };
    };
    const light = await page.evaluate(readChrome);
    expect(light).toMatchObject({ accent: "#0969da", blue: "#006eff", purple: "#8250df" });
    // G10: chrome is a flat neutral fill in both themes; "none" also catches a
    // re-introduced radial wash.
    expect(light.headerImage).toBe("none");
    expect(light.headerColor).toBe("rgb(246, 248, 250)");

    // The theme toggle lives in the avatar dropdown menu.
    await page.getByRole("button", { name: "Open user menu" }).click();
    const toggle = page.getByRole("menuitem", { name: /(light|dark) theme/i });
    await expect(toggle).toBeVisible();
    await shot(page, "11-theme-toggle");
    await toggle.click();
    await expect(page.locator("html")).toHaveClass(/dark/);
    const dark = await page.evaluate(readChrome);
    expect(dark).toMatchObject({ accent: "#58a6ff", cyan: "#39d0e8", pink: "#ff7bda" });
    expect(dark.headerImage).toBe("none");
    expect(dark.headerColor).toBe("rgb(22, 27, 34)");
    await shot(page, "11b-theme-toggle-dark");
  });
});

test.describe("Fine-grained personal access token settings", () => {
  test("creates a one-time credential in polished light and dark settings", async ({ page }) => {
    await page.goto("/ui/account");
    await page.getByRole("button", { name: "Personal access tokens" }).click();
    const heading = page.getByRole("heading", { name: "Fine-grained personal access tokens" });
    await expect(heading).toBeVisible();
    const hero = heading.locator("..");
    expect(await hero.evaluate((node) => getComputedStyle(node).backgroundImage)).toContain("gradient");
    await page.getByLabel("Token name").fill(`Playwright token ${randomUUID().slice(0, 12)}`);
    await page.getByRole("button", { name: "Generate token" }).click();
    await expect(page.getByText("Your new token")).toBeVisible();
    await expect(page.getByText(/^github_pat_/)).toBeVisible();
    await expect(page.getByText("active", { exact: true })).toBeVisible();
    await shot(page, "11e-fine-grained-token-light");

    await page.getByRole("button", { name: "Open user menu" }).click();
    await page.getByRole("menuitem", { name: /dark theme/i }).click();
    await expect(page.locator("html")).toHaveClass(/dark/);
    expect(await hero.evaluate((node) => getComputedStyle(node).backgroundImage)).toContain("gradient");
    await expect(page.getByText("Your new token")).toBeVisible();
    await shot(page, "11f-fine-grained-token-dark");
  });
});

test.describe("Password and authentication settings", () => {
  // Walk up to but not through enrolment: switching 2FA on would gate the shared
  // admin's /auth/token sessions. Cancelling discards the pending secret, proving
  // the pairing surface is real without leaving state; the Go suite owns the full
  // enrol → verify → recover → disable cycle on its own account.
  test("provisions a scannable authenticator pairing and lists active sessions", async ({ page }) => {
    await page.goto("/ui/account?tab=authentication");

    // The seeded admin has no password yet, so the card offers to set one, not change one.
    await expect(page.getByRole("heading", { name: "Set a password" })).toBeVisible();
    await expect(page.getByLabel("New password", { exact: true })).toBeVisible();
    await expect(page.getByText(/Two-factor authentication is/)).toBeVisible();

    // The session list shows this browser's own session and offers to end it,
    // without ever printing the session cookie.
    await expect(page.getByRole("heading", { name: "Sessions" })).toBeVisible();
    await expect(page.getByText("· this session")).toBeVisible();

    await page.getByRole("button", { name: "Enable two-factor authentication" }).click();

    // Prove a real QR code and setup key, not a switch.
    const qr = page.getByRole("img", { name: /QR code enrolling admin/ });
    await expect(qr).toBeVisible();
    expect(await qr.locator("path").getAttribute("d")).toMatch(/^M\d/);
    await expect(page.getByText("Setup key")).toBeVisible();
    await expect(page.getByLabel("Verification code")).toBeVisible();
    await shot(page, "11g-two-factor-enrollment");

    // The account stays unenrolled until a code is proved, even while pairing shows.
    const settings = await page.request.get("/ui-data/user/authentication");
    const body = (await settings.json()) as { two_factor: { enabled: boolean; pending_enrollment: boolean } };
    expect(body.two_factor).toMatchObject({ enabled: false, pending_enrollment: true });

    await page.getByRole("button", { name: "Cancel" }).click();
    await expect(page.getByRole("button", { name: "Enable two-factor authentication" })).toBeVisible();
  });

  test("stores per-type notification preferences", async ({ page }) => {
    await page.goto("/ui/account?tab=notifications");
    await expect(page.getByRole("heading", { name: "Notification types" })).toBeVisible();

    const savePut = () =>
      page.waitForResponse(
        (response) =>
          response.url().includes("/ui-data/user/notification-settings") &&
          response.request().method() === "PUT",
      );

    const releasesWeb = page.getByRole("checkbox", { name: "Web notifications for Releases" });
    await expect(releasesWeb).toBeChecked();
    const saved = savePut();
    await releasesWeb.click();
    await saved;
    await expect(releasesWeb).not.toBeChecked();

    // The choice is stored per user, not held in the page.
    await page.reload();
    const reloaded = page.getByRole("checkbox", { name: "Web notifications for Releases" });
    await expect(reloaded).not.toBeChecked();
    await expect(page.getByRole("checkbox", { name: "Web notifications for Issues" })).toBeChecked();

    // Leave the account as it was found: this suite shares one admin.
    const restored = savePut();
    await reloaded.click();
    await restored;
    await expect(reloaded).toBeChecked();
  });
});

test.describe("GitHub Classroom transition product", () => {
  test("creates and renders a classroom with saturated light and dark organization", async ({ page }) => {
    await page.goto("/ui/");
    const suffix = randomUUID().slice(0, 12);
    const org = `classroom-e2e-${suffix}`;
    await apiPost(page, "/api/v3/admin/organizations", { login: org, admin: "admin", profile_name: "Classroom E2E" });
    const classroom = await apiPost(page, "/classroom-data/classrooms", { name: "Software Construction", organization: org }) as { id: number };

    await page.goto(`/ui/classrooms/${classroom.id}`);
    await page.waitForLoadState("networkidle");
    await expect(page.getByRole("heading", { name: "Software Construction" })).toBeVisible();
    await expect(page.getByText(`Owned by ${org}`)).toBeVisible();
    await expect(page.getByRole("button", { name: "New assignment" })).toBeVisible();
    await expect(page.getByRole("button", { name: /Roster/ })).toBeVisible();
    const lightSurface = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--color-surface").trim());
    expect(lightSurface).toBeTruthy();
    await shot(page, "11c-classroom-light");

    await page.getByRole("button", { name: "Open user menu" }).click();
    await page.getByRole("menuitem", { name: /dark theme/i }).click();
    await expect(page.locator("html")).toHaveClass(/dark/);
    const darkSurface = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--color-surface").trim());
    expect(darkSurface).not.toBe(lightSurface);
    await expect(page.getByRole("heading", { name: "Software Construction" })).toBeVisible();
    await shot(page, "11d-classroom-dark");
  });
});

// ─── Repos page ─────────────────────────────────────────────────────────────

test.describe("Repos page", () => {
  test("renders the repositories page", async ({ page }) => {
    await page.goto("/ui/repos");
    await page.waitForLoadState("networkidle");
    // Assert page chrome, not an empty list: other tests persist repos across the
    // run, so "empty" is not an ordering-independent precondition.
    await expect(
      page.getByRole("heading", { level: 1 }).filter({ hasText: "Repositories" }),
    ).toBeVisible();
    await shot(page, "12-repos");
  });

  test("shows repo after creation and links to detail", async ({ page }) => {
    await page.goto("/ui/");

    await apiPost(page, "/api/v3/user/repos", {
      name: "test-repo-playwright",
      description: "Playwright test repo",
      private: false,
    });

    await page.goto("/ui/repos");
    await page.waitForLoadState("networkidle");

    const link = page.getByRole("link", { name: /test-repo-playwright/ });
    await expect(link).toBeVisible();
    await shot(page, "13-repos-with-repo");

    await link.click();
    // Cards must land on /ui/{owner}/{repo}, mirroring github.com's /{owner}/{repo}.
    await expect(page).toHaveURL(/\/ui\/[^/]+\/test-repo-playwright$/);
    await shot(page, "14-repo-detail");

    await page.getByLabel("Repository actions").getByRole("button", { name: /Fork/ }).click();
    const forkDialog = page.getByRole("dialog", { name: "Create a new fork" });
    await expect(forkDialog).toBeVisible();
    await expect(forkDialog.getByText(/choose a different owner/i)).toBeVisible();
    await shot(page, "14b-repo-fork-owner");
    await forkDialog.getByRole("button", { name: "Cancel" }).click();
  });

  test("creates a repo through the UI dialog", async ({ page }) => {
    await page.goto("/ui/repos");
    await page.waitForLoadState("networkidle");

    await page.getByRole("button", { name: "New repository" }).click();
    await expect(page.getByRole("heading", { name: "Create a new repository" })).toBeVisible();

    await page.getByLabel("Repository name").fill("ui-created-repo");
    await page.getByLabel("Description").fill("Created from the UI");
    await page.getByRole("button", { name: "Create repository" }).click();

    await expect(page.getByRole("link", { name: /ui-created-repo/ })).toBeVisible();
    await shot(page, "13b-repos-after-ui-create");
  });
});

// ─── Repo detail ─────────────────────────────────────────────────────────────

test.describe("Repo detail page", () => {
  test("shows empty repo with clone instructions", async ({ page }) => {
    await page.goto("/ui/");
    const user = await apiGet(page, "/api/v3/user");
    const owner = (user as { login: string }).login;

    await apiPost(page, "/api/v3/user/repos", {
      name: "detail-test",
      description: "Detail page test",
      private: false,
    }).catch(ignoreAlreadyExists);

    await page.goto(`/ui/${owner}/detail-test`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByRole("link", { name: "detail-test" })).toBeVisible();
    await expect(page.getByText(/this repository is empty/i)).toBeVisible();
    await expect(page.getByRole("button", { name: "HTTPS" })).toBeVisible();
    await expect(page.getByRole("button", { name: "SSH" })).toBeVisible();
    await expect(page.getByRole("button", { name: "GitHub CLI" })).toBeVisible();
    await shot(page, "15-repo-detail-empty");
  });

  test("shows file tree and rendered README for initialized repo", async ({ page }) => {
    await page.goto("/ui/");
    const user = await apiGet(page, "/api/v3/user");
    const owner = (user as { login: string }).login;

    await apiPost(page, "/api/v3/user/repos", {
      name: "readme-test",
      description: "README test",
      private: false,
      auto_init: true,
    }).catch(ignoreAlreadyExists);

    await page.goto(`/ui/${owner}/readme-test`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByText("README.md").first()).toBeVisible();
    // The branch <select> became the filterable RefSwitcher trigger button.
    await expect(page.getByRole("button", { name: "Switch branches or tags" })).toContainText("main");
    await shot(page, "15b-repo-detail-with-readme");
  });

  test("shows configured branch protection and links to its settings", async ({ page }) => {
    const repo = `protected-branch-${randomUUID().slice(0, 12)}`;
    await page.goto("/ui/");
    await apiPost(page, "/api/v3/user/repos", {
      name: repo,
      description: "Protected branch browser journey",
      private: false,
      auto_init: true,
    });
    await apiPut(page, `/api/v3/repos/admin/${repo}/branches/main/protection`, {
      required_status_checks: { strict: true, contexts: ["ci"] },
      required_pull_request_reviews: null,
      enforce_admins: false,
      restrictions: null,
    });

    await page.goto(`/ui/admin/${repo}`);
    // G9: reach the branch listing from the branch/tag switcher, not a second tab row.
    await page.getByRole("button", { name: "Switch branches or tags" }).click();
    await page.getByRole("link", { name: "View all branches" }).click();
    const protectedLink = page.getByRole("link", { name: "protected", exact: true });
    await expect(protectedLink).toBeVisible();
    await protectedLink.click();
    await expect(page).toHaveURL(new RegExp(`/ui/admin/${repo}/settings/branch-protection`));
    await expect(page.getByRole("combobox", { name: "Branch" })).toContainText("main (protected)");
  });

  test("issues tab shows issue list", async ({ page }) => {
    await page.goto("/ui/");
    const user = await apiGet(page, "/api/v3/user");
    const owner = (user as { login: string }).login;

    await apiPost(page, "/api/v3/user/repos", {
      name: "issues-test",
      description: "",
      private: false,
    }).catch(ignoreAlreadyExists);
    await apiPost(page, `/api/v3/repos/${owner}/issues-test/issues`, {
      title: "First Playwright issue",
      body: "Created by Playwright test",
    });

    await page.goto(`/ui/${owner}/issues-test`);
    await page.waitForLoadState("networkidle");
    // Scope to the repo nav so Issues doesn't collide with the global header's quick-link.
    await page.getByRole("navigation", { name: "Repository" }).getByRole("link", { name: /Issues/ }).click();
    await page.waitForLoadState("networkidle");
    await expect(page.getByText("First Playwright issue")).toBeVisible();
    await shot(page, "16-repo-issues-tab");
  });
});

// ─── Issues page ─────────────────────────────────────────────────────────────

test.describe("Issues page", () => {
  test("lists and creates issues", async ({ page }) => {
    await page.goto("/ui/");
    const user = await apiGet(page, "/api/v3/user");
    const owner = (user as { login: string }).login;

    await apiPost(page, "/api/v3/user/repos", {
      name: "issues-direct",
      description: "",
      private: false,
    }).catch(ignoreAlreadyExists);

    await apiPost(page, `/api/v3/repos/${owner}/issues-direct/issues`, {
      title: "Direct issues page test",
    });

    await page.goto(`/ui/${owner}/issues-direct/issues`);
    await page.waitForLoadState("networkidle");
    await expect(page.getByText("Direct issues page test")).toBeVisible();
    await shot(page, "17-issues-page");

    await page.getByRole("button", { name: "New issue" }).click();
    await expect(page.getByPlaceholder("Issue title")).toBeVisible();
    await shot(page, "18-new-issue-modal");

    await page.getByPlaceholder("Issue title").fill("Created from UI");
    await page.getByRole("button", { name: "Create issue" }).click();
    await page.waitForURL(/\/ui\/[^/]+\/[^/]+\/issues\/\d+/);
    await shot(page, "19-issue-detail-after-create");
  });
});

// ─── Pull Requests page ───────────────────────────────────────────────────────

test.describe("Pull Requests page", () => {
  test("shows empty state", async ({ page }) => {
    await page.goto("/ui/");
    const user = await apiGet(page, "/api/v3/user");
    const owner = (user as { login: string }).login;

    await apiPost(page, "/api/v3/user/repos", {
      name: "pulls-direct",
      description: "",
      private: false,
    }).catch(ignoreAlreadyExists);

    await page.goto(`/ui/${owner}/pulls-direct/pulls`);
    await page.waitForLoadState("networkidle");
    await expect(page.getByText(/no open pull requests/i)).toBeVisible();
    await shot(page, "20-pulls-empty");
  });

  test("deep-links to changed files and creates an inline review comment", async ({ page }) => {
    const repo = `pull-review-${randomUUID().slice(0, 12)}`;
    await page.goto("/ui/");
    await apiPost(page, "/api/v3/user/repos", {
      name: repo,
      private: false,
      auto_init: true,
    });
    const main = (await apiGet(page, `/api/v3/repos/admin/${repo}/git/ref/heads/main`)) as {
      object: { sha: string };
    };
    await apiPost(page, `/api/v3/repos/admin/${repo}/git/refs`, {
      ref: "refs/heads/review-me",
      sha: main.object.sha,
    });
    await apiPut(page, `/api/v3/repos/admin/${repo}/contents/review.txt`, {
      message: "Add review target",
      content: btoa("review this line\n"),
      branch: "review-me",
    });
    const pull = (await apiPost(page, `/api/v3/repos/admin/${repo}/pulls`, {
      title: "Review this change",
      head: "review-me",
      base: "main",
    })) as { number: number };

    await page.goto(`/ui/admin/${repo}/pulls/${pull.number}/files`);
    await expect(page).toHaveURL(new RegExp(`/pulls/${pull.number}/files$`));
    await expect(page.getByText("review.txt")).toBeVisible();
    await page.getByRole("button", { name: "Comment on review.txt line 1" }).click();
    await page.getByLabel("Review comment").fill("A browser-authored inline comment.");
    await page.getByRole("button", { name: "Add single comment" }).click();
    await expect(page.getByText(/Comment on review.txt/)).not.toBeVisible();

    const comments = (await apiGet(
      page,
      `/api/v3/repos/admin/${repo}/pulls/${pull.number}/comments`,
    )) as Array<{ body: string; path: string; line: number }>;
    expect(comments).toEqual(
      expect.arrayContaining([
        expect.objectContaining({
          body: "A browser-authored inline comment.",
          path: "review.txt",
          line: 1,
        }),
      ]),
    );
  });
});

// ─── Apps page ───────────────────────────────────────────────────────────────

test.describe("Apps page", () => {
  test("renders app tabs", async ({ page }) => {
    await page.goto("/ui/apps");
    await expect(page.getByRole("heading", { level: 1 }).filter({ hasText: "Apps" })).toBeVisible();
    await expect(page.getByRole("tab", { name: "GitHub Apps" })).toBeVisible();
    await shot(page, "21-apps-page");
  });

  test("opens create-app modal", async ({ page }) => {
    await page.goto("/ui/apps");
    const newAppBtn = page.getByRole("button", { name: /new.*app/i }).first();
    await newAppBtn.click();
    await expect(page.getByRole("heading", { name: /create/i })).toBeVisible();
    await expect(page.getByRole("dialog")).toHaveAttribute("aria-modal", "true");
    await shot(page, "22-create-app-modal");
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog")).toHaveCount(0);
  });

  test("switches to OAuth Apps tab", async ({ page }) => {
    await page.goto("/ui/apps");
    await page.getByRole("tab", { name: "OAuth Apps" }).click();
    await shot(page, "23-oauth-apps-tab");
  });
});

// ─── OAuth page ──────────────────────────────────────────────────────────────

test.describe("OAuth page", () => {
  test("renders device flow and web flow sections", async ({ page }) => {
    await page.goto("/ui/oauth");
    await shot(page, "24-oauth-page");
    await expect(page.url()).toContain("/ui/oauth");
  });
});

// ─── Actions (workflows + runs + detail) ─────────────────────────────────────

test.describe("Actions UI", () => {
  test("submits a workflow and renders the run detail (jobs + logs section)", async ({ page }) => {
    await page.goto("/ui/");
    const repoName = `ci-demo-${randomUUID().slice(0, 12)}`;
    const repoFullName = `admin/${repoName}`;
    await apiPost(page, "/api/v3/user/repos", { name: repoName, auto_init: true });

    // No runner is attached, so logs stay empty; the detail page must still
    // render the job table and logs view.
    const yaml = [
      "name: CI Pipeline",
      "on: workflow_dispatch",
      "jobs:",
      "  build:",
      "    runs-on: ubuntu-latest",
      "    steps:",
      "      - run: echo building",
      "  test:",
      "    runs-on: ubuntu-latest",
      "    needs: build",
      "    steps:",
      "      - run: echo testing",
    ].join("\n");
    await apiPut(page, `/api/v3/repos/${repoFullName}/contents/.github/workflows/ci.yml`, {
      message: "Add GitHub Actions workflow",
      content: Buffer.from(yaml, "utf8").toString("base64"),
      branch: "main",
    });
    await apiPost(page, `/api/v3/repos/${repoFullName}/actions/workflows/ci.yml/dispatches`, { ref: "main", inputs: {} });

    // Target the Runs tab by button role: the page title also contains "runs".
    await page.goto("/ui/workflows");
    await page.waitForLoadState("networkidle");
    await page.getByRole("tab", { name: "Runs" }).click();
    await expect(page.getByText("CI Pipeline").first()).toBeVisible();
    await shot(page, "29-actions-runs");

    await page.getByText("CI Pipeline").first().click();
    await page.waitForURL(/\/ui\/workflows\/.+/);
    await page.waitForLoadState("networkidle");
    await expect(page.getByRole("heading", { name: /Jobs/i })).toBeVisible();
    await shot(page, "30-actions-run-detail");
  });
});

// ─── Metrics page ────────────────────────────────────────────────────────────

test.describe("Metrics page", () => {
  test("shows counters section", async ({ page }) => {
    await page.goto("/ui/metrics");
    await page.waitForLoadState("networkidle");
    await expect(page.getByRole("heading", { level: 1 }).filter({ hasText: /actions throughput/i })).toBeVisible();
    await shot(page, "25-metrics-page");
  });
});

// ─── Release provider ───────────────────────────────────────────────────────

test.describe("Release provider", () => {
  test("creates, edits, uploads, downloads, and deletes through routed UI pages", async ({ page }) => {
    await page.goto("/ui/");
    const user = await apiGet(page, "/api/v3/user");
    const owner = (user as { login: string }).login;
    const repo = "release-ui-playwright";
    await apiPost(page, "/api/v3/user/repos", { name: repo, auto_init: true });

    await page.goto(`/ui/${owner}/${repo}/releases`);
    await page.getByRole("link", { name: "New release" }).click();
    await expect(page).toHaveURL(new RegExp(`/ui/${owner}/${repo}/releases/new$`));
    await page.getByLabel("Tag").fill("v1.0.0");
    await page.getByLabel("Release title").fill("First real release");
    await page.getByLabel("Release notes").fill("Published through the GitHub-compatible UI.");
    await page.getByRole("button", { name: "Create release" }).click();
    await expect(page).toHaveURL(new RegExp(`/ui/${owner}/${repo}/releases/\\d+$`));
    await expect(page.getByRole("heading", { level: 1, name: "First real release" })).toBeVisible();

    const assetBytes = Buffer.from("real release asset bytes", "utf8");
    await page.getByLabel("Asset file").setInputFiles({
      name: "artifact.txt",
      mimeType: "text/plain",
      buffer: assetBytes,
    });
    await page.getByLabel("Asset label").fill("Linux artifact");
    await page.getByRole("button", { name: "Upload asset" }).click();
    await expect(page.getByText("Linux artifact")).toBeVisible();
    await expect(page.getByText(`${assetBytes.length} bytes`)).toBeVisible();

    const download = page.waitForEvent("download");
    await page.getByRole("button", { name: "Download artifact.txt" }).click();
    expect((await download).suggestedFilename()).toBe("artifact.txt");

    await page.getByRole("button", { name: "Edit" }).click();
    await page.getByLabel("Release title").fill("Updated real release");
    await page.getByRole("button", { name: "Save changes" }).click();
    await expect(page.getByRole("heading", { level: 1, name: "Updated real release" })).toBeVisible();

    await page.getByRole("button", { name: "Delete artifact.txt" }).click();
    await expect(page.getByText("Linux artifact")).not.toBeVisible();
    await page.getByRole("button", { name: "Delete" }).click();
    await expect(page.getByRole("dialog", { name: "Confirm action" })).toBeVisible();
    await page.getByRole("button", { name: "Confirm" }).click();
    await expect(page).toHaveURL(new RegExp(`/ui/${owner}/${repo}/releases$`));
    await expect(page.getByText("No releases published")).toBeVisible();
  });
});

// ─── Code security ──────────────────────────────────────────────────────────

test.describe("Code security", () => {
  test("uses the real default-branch commit and renders saturated light/dark CodeQL organization", async ({ page }) => {
    await page.goto("/ui/");
    const user = await apiGet(page, "/api/v3/user");
    const owner = (user as { login: string }).login;
    const repo = `code-security-${randomUUID().slice(0, 12)}`;
    await apiPost(page, "/api/v3/user/repos", { name: repo, auto_init: true });
    const branch = await apiGet(page, `/api/v3/repos/${owner}/${repo}/branches/main`) as { commit: { sha: string } };

    await page.goto(`/ui/${owner}/${repo}/security/code-scanning`);
    await page.waitForLoadState("networkidle");
    await expect(page.getByRole("heading", { level: 1, name: "Code scanning" })).toBeVisible();
    await expect(page.getByText(branch.commit.sha.slice(0, 7), { exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Databases" })).toBeVisible();
    await expect(page.getByText("No CodeQL databases yet")).toBeVisible();

    const sarif = Buffer.from(JSON.stringify({
      version: "2.1.0",
      runs: [{ tool: { driver: { name: "CodeQL" } }, results: [] }],
    }), "utf8");
    await page.getByLabel("SARIF file").setInputFiles({ name: "results.sarif", mimeType: "application/sarif+json", buffer: sarif });
    const uploadResponsePromise = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith(`/repos/${owner}/${repo}/code-scanning/sarifs`));
    await page.getByRole("button", { name: "Upload SARIF" }).click();
    const uploadResponse = await uploadResponsePromise;
    expect(uploadResponse.status()).toBe(202);
    const payload = uploadResponse.request().postDataJSON() as { commit_sha: string; ref: string };
    expect(payload).toEqual(expect.objectContaining({ commit_sha: branch.commit.sha, ref: "refs/heads/main" }));
    expect(payload.commit_sha).not.toMatch(/^0+$/);
    await expect(page.locator(".security-summary").getByText("1 analyses", { exact: true })).toBeVisible();

    const light = await page.locator(".security-hero").evaluate((element) => {
      const hero = getComputedStyle(element);
      const icon = getComputedStyle(document.querySelector(".security-hero-icon")!);
      return {
        surface: getComputedStyle(document.documentElement).getPropertyValue("--color-surface").trim(),
        hero: hero.backgroundImage,
        heroColor: hero.backgroundColor,
        icon: icon.backgroundImage,
      };
    });
    // G10: the hero is flat neutral chrome — no wash on the panel, no gradient on
    // its icon, in either theme.
    expect(light.hero).toBe("none");
    expect(light.icon).toBe("none");
    expect(light.heroColor).toBe("rgb(255, 255, 255)");
    await shot(page, "31-code-security-light");

    await page.getByRole("button", { name: "Open user menu" }).click();
    await page.getByRole("menuitem", { name: /dark theme/i }).click();
    await expect(page.locator("html")).toHaveClass(/dark/);
    const dark = await page.locator(".security-hero").evaluate((element) => ({
      surface: getComputedStyle(document.documentElement).getPropertyValue("--color-surface").trim(),
      hero: getComputedStyle(element).backgroundImage,
      heroColor: getComputedStyle(element).backgroundColor,
    }));
    expect(dark.surface).not.toBe(light.surface);
    expect(dark.hero).toBe("none");
    expect(dark.heroColor).toBe("rgb(13, 17, 23)");
    await expect(page.getByRole("heading", { name: "Databases" })).toBeVisible();
    await shot(page, "32-code-security-dark");
  });
});

// ─── GitHub Marketplace ────────────────────────────────────────────────────

test.describe("GitHub Marketplace", () => {
  test("publishes, purchases, installs, and renders saturated light/dark workflows", async ({ page }) => {
    await page.goto("/ui/apps");
    await page.getByRole("button", { name: "New GitHub app" }).click();
    await page.getByLabel("Name").fill("Marketplace Polish App");
    await page.getByLabel("Description").fill("A real Marketplace producer");
    await page.getByRole("button", { name: "Create app" }).click();
    await expect(page.getByRole("heading", { name: "Save these now" })).toBeVisible();
    await page.getByRole("button", { name: "I copied them" }).click();
    const publisherLink = page.getByRole("link", { name: "marketplace-polish-app" });
    await expect(publisherLink).toBeVisible();
    await publisherLink.click();
    await expect(page.getByRole("heading", { name: "Create Marketplace listing" })).toBeVisible();

    await page.getByLabel("Listing name").fill("Marketplace Polish App");
    await page.getByLabel("Short description").fill("Colorful automation with GitHub-native installation and billing");
    await page.getByLabel("Full description").fill("A polished GitHub Marketplace integration that keeps setup, plans, billing changes, and webhook delivery together.");
    await page.getByLabel("Setup URL").fill("https://example.test/marketplace/setup");
    await page.getByLabel("Payload URL").fill(`${WEBHOOK_BASE}/marketplace`);
    await page.getByLabel("Secret").fill("playwright-marketplace-secret");
    await page.getByRole("button", { name: "Create draft listing" }).click();
    await expect(page.getByRole("heading", { name: "Manage Marketplace listing" })).toBeVisible();

    await page.getByRole("button", { name: "Add plan" }).click();
    await page.getByLabel("Name", { exact: true }).last().fill("Team Candy");
    await page.getByLabel("Description", { exact: true }).last().fill("Private repositories and priority support");
    await page.getByLabel("Pricing model").selectOption("FLAT_RATE");
    await page.getByLabel("Monthly (cents)").fill("1400");
    await page.getByLabel("Yearly (cents)").fill("14000");
    await page.getByLabel(/Offer a 14-day free trial/).check();
    await page.getByRole("button", { name: "Publish plan" }).click();
    await expect(page.getByText("Team Candy", { exact: true })).toBeVisible();
    await page.getByLabel("Publish listing").check();
    await page.getByRole("button", { name: "Save listing" }).click();
    await expect(page.getByText("Published", { exact: true })).toBeVisible();

    await page.goto("/ui/marketplace");
    await expect(page.getByRole("heading", { name: "Build more. Ship brighter." })).toBeVisible();
    await expect(page.getByRole("link", { name: /Marketplace Polish App/ })).toBeVisible();
    const light = await page.locator(".marketplace-hero").evaluate((element) => ({
      surface: getComputedStyle(document.documentElement).getPropertyValue("--color-surface").trim(),
      background: getComputedStyle(element).backgroundImage,
    }));
    expect(light.background).toContain("linear-gradient");
    await shot(page, "33-marketplace-light");

    await page.getByRole("link", { name: /Marketplace Polish App/ }).click();
    await expect(page.getByRole("heading", { name: "Marketplace Polish App" })).toBeVisible();
    await page.getByLabel(/14-day free trial/).check();
    const purchaseResponse = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/ui-data/marketplace/listings/marketplace-polish-app/purchase"));
    await page.getByRole("button", { name: "Complete order and begin installation" }).click();
    expect((await purchaseResponse).status()).toBe(201);
    await expect(page.getByRole("link", { name: /Continue to Marketplace Polish App setup/ })).toBeVisible();
    await expect.poll(async () => {
      const response = await page.request.get(`${WEBHOOK_BASE}/events`);
      const events = await response.json() as Array<{ event: string; body: { action?: string } }>;
      return events.some((event) => event.event === "marketplace_purchase" && event.body.action === "purchased");
    }).toBe(true);
    const installations = await apiGet(page, "/api/v3/user/installations") as { installations: Array<{ app_slug: string }> };
    expect(installations.installations.some((installation) => installation.app_slug === "marketplace-polish-app")).toBe(true);

    await page.getByRole("button", { name: "Open user menu" }).click();
    await page.getByRole("menuitem", { name: /dark theme/i }).click();
    await expect(page.locator("html")).toHaveClass(/dark/);
    const dark = await page.locator(".marketplace-detail-header").evaluate((element) => ({
      surface: getComputedStyle(document.documentElement).getPropertyValue("--color-surface").trim(),
      background: getComputedStyle(element).backgroundImage,
    }));
    expect(dark.surface).not.toBe(light.surface);
    expect(dark.background).toContain("linear-gradient");
    await shot(page, "34-marketplace-dark");
  });
});

// ─── GitHub Sponsors ─────────────────────────────────────────────────────────

test.describe("GitHub Sponsors", () => {
  test("opens a profile, publishes a tier, takes a sponsorship, and bills it", async ({ page }) => {
    // Sponsor an org, not self: an account may not sponsor itself. Name from the
    // worker, not the clock — the clock-dependency gate forbids calendar-sensitive
    // data, and a timestamp would make failures unreproducible on re-run.
    const org = `sponsors-e2e-w${test.info().parallelIndex}r${test.info().retry}`;
    // apiPost runs inside the page, so load the app origin first.
    await page.goto("/ui/sponsors");
    await apiPost(page, "/api/v3/admin/organizations", { login: org, admin: "admin", profile_name: "Sponsors E2E" });

    await page.goto(`/ui/sponsors/${org}`);
    await page.getByRole("button", { name: "Open a Sponsors profile" }).click();
    await expect(page.getByRole("heading", { name: `Sponsor ${org}` })).toBeVisible();

    await page.goto(`/ui/sponsors/${org}/dashboard`);
    await expect(page.getByRole("heading", { name: "Sponsors dashboard" })).toBeVisible();
    await page.getByLabel("Amount (USD per month)").fill("7");
    await page.getByLabel("Description").fill("A coffee a month");
    await page.getByRole("button", { name: "Add tier" }).click();
    await expect(page.getByText("$7.00 a month").first()).toBeVisible();

    await page.getByLabel("Monthly goal (USD)").fill("70");
    await page.getByRole("button", { name: "Set goal" }).click();
    await expect(page.getByRole("progressbar", { name: "Earn $70 per month" })).toBeVisible();

    await page.goto(`/ui/sponsors/${org}`);
    await page.getByRole("button", { name: "Select" }).first().click();
    await expect(page.getByText(/You are sponsoring/)).toBeVisible();

    await page.goto(`/ui/sponsors/${org}/dashboard`);
    await expect(page.getByRole("cell", { name: "admin" }).first()).toBeVisible();
    await expect(page.getByText("$7.00 is awaiting payout.")).toBeVisible();
    await page.getByRole("button", { name: "Run payout" }).click();
    await expect(page.getByText("$0.00 is awaiting payout.")).toBeVisible();

    await page.goto("/ui/sponsors");
    await expect(page.getByRole("heading", { name: /GitHub Sponsors/ })).toBeVisible();
    await expect(page.getByRole("link", { name: org }).first()).toBeVisible();
  });
});

// ─── Dark theme capture ──────────────────────────────────────────────────────

test.describe("Dark theme", () => {
  // Seed the persisted theme to "dark" before any script runs so the app boots dark.
  test.beforeEach(async ({ page }) => {
    await page.addInitScript(() => {
      window.localStorage.setItem("bleephub:theme", "dark");
    });
  });

  test("captures key pages in dark mode", async ({ page }) => {
    await page.goto("/ui/operations");
    await page.waitForLoadState("networkidle");
    await expect(page.getByRole("heading", { level: 1 }).filter({ hasText: "System status" })).toBeVisible();
    await shot(page, "26-dark-ops-console");

    await page.goto("/ui/repos");
    await page.waitForLoadState("networkidle");
    await shot(page, "27-dark-repos");

    const user = await apiGet(page, "/api/v3/user");
    const owner = (user as { login: string }).login;
    await apiPost(page, "/api/v3/user/repos", {
      name: "dark-theme-repo",
      description: "Dark theme repository chrome",
      private: false,
      auto_init: true,
    });
    await page.goto(`/ui/${owner}/dark-theme-repo`);
    await page.waitForLoadState("networkidle");
    await expect(page.getByLabel("Repository actions")).toBeVisible();
    await expect(page.getByRole("button", { name: /Watch/ })).toBeVisible();
    await expect(page.getByRole("button", { name: /Star/ })).toBeVisible();
    await shot(page, "28-dark-issues");
  });
});

// ─── Health endpoint ─────────────────────────────────────────────────────────

test.describe("Health endpoint", () => {
  test("returns 200 JSON", async ({ page }) => {
    const res = await page.goto("/health");
    expect(res?.status()).toBe(200);
  });
});
