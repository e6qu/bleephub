// These 9 tests do NOT run in this repository, and that is expected.
//
// They drive @bleephub/ui-core's generic operator shell — BackendApp, with its
// Overview / Containers / Resources / Metrics pages — against a Go backend
// serving those routes under /ui/. Bleephub ships no such backend (cmd/ holds
// only `bleephub`, which serves the Bleephub SPA at /ui/), and nothing in
// web/src renders BackendApp, so there is no server to point a Playwright
// project at. web/playwright.config.ts therefore has testDir "./e2e" and never
// reaches web/core/e2e.
//
// They are kept because ui-core is a shared library and these are its
// contract; they are type-checked here via web/tsconfig.e2e.json. The
// components they cover are exercised in this repository by the unit suites
// in web/core/src/__tests__ (BackendApp, SimulatorApp, ResourceListPage,
// hooks), which do run as part of `bun run test`.
import { test, expect } from "@playwright/test";

const TITLE = process.env.BACKEND_TITLE || "Backend";

test.describe("Backend SPA", () => {
  test("redirects / to /ui/", async ({ page }) => {
    const response = await page.goto("/");
    expect(page.url()).toContain("/ui/");
    expect(response?.status()).toBe(200);
  });

  test(`renders ${TITLE} in sidebar`, async ({ page }) => {
    await page.goto("/ui/");
    await expect(page.getByRole("heading", { name: TITLE })).toBeVisible();
  });
});

test.describe("Overview Page", () => {
  test("renders heading", async ({ page }) => {
    await page.goto("/ui/");
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  });

  test("renders KPI cards", async ({ page }) => {
    await page.goto("/ui/");
    await expect(page.getByText("Containers")).toBeVisible();
    await expect(page.getByText("Active Resources", { exact: true })).toBeVisible();
    await expect(page.getByText("Uptime")).toBeVisible();
    await expect(page.getByText("Backend", { exact: true })).toBeVisible();
  });
});

test.describe("Containers Page", () => {
  test("renders table headers", async ({ page }) => {
    await page.goto("/ui/containers");
    await expect(page.getByRole("heading", { name: "Containers" })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: "ID" })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: "Name" })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: "Image" })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: "State" })).toBeVisible();
  });
});

test.describe("Resources Page", () => {
  test("renders table headers", async ({ page }) => {
    await page.goto("/ui/resources");
    await expect(page.getByRole("heading", { name: "Resources" })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: "Container" })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: "Backend" })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: "Type" })).toBeVisible();
    await expect(page.getByRole("columnheader", { name: "Resource ID" })).toBeVisible();
  });
});

test.describe("Metrics Page", () => {
  test("renders heading and KPI cards", async ({ page }) => {
    await page.goto("/ui/metrics");
    await expect(page.getByRole("heading", { name: "Metrics" })).toBeVisible();
    await expect(page.getByText("Goroutines")).toBeVisible();
    await expect(page.getByText("Heap")).toBeVisible();
  });
});

test.describe("Navigation", () => {
  test("sidebar has all 4 nav links", async ({ page }) => {
    await page.goto("/ui/");
    await expect(page.getByRole("link", { name: "Overview" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Containers" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Resources" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Metrics" })).toBeVisible();
  });

  test("navigates between all pages via sidebar", async ({ page }) => {
    await page.goto("/ui/");

    await page.getByRole("link", { name: "Containers" }).click();
    await expect(page.url()).toContain("/ui/containers");
    await expect(page.getByRole("heading", { name: "Containers" })).toBeVisible();

    await page.getByRole("link", { name: "Resources" }).click();
    await expect(page.url()).toContain("/ui/resources");
    await expect(page.getByRole("heading", { name: "Resources" })).toBeVisible();

    await page.getByRole("link", { name: "Metrics" }).click();
    await expect(page.url()).toContain("/ui/metrics");
    await expect(page.getByRole("heading", { name: "Metrics" })).toBeVisible();

    await page.getByRole("link", { name: "Overview" }).click();
    await expect(page.url()).toContain("/ui/");
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  });
});
