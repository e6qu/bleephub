import { test, expect } from "./fixtures.js";

// Assert the app degrades to a visible error surface, never a blank SPA; a
// fulfilled 500 keeps the console clean so `pageerror` is the only crash signal.
test.beforeEach(({ page }, testInfo) => {
  page.on("pageerror", (err) => {
    throw new Error(`Uncaught page error in ${testInfo.title}: ${err.message}`);
  });
});

test.describe("Error handling / fault injection", () => {
  test("a nonexistent repo's Insights renders a visible error, not a blank page", async ({
    page,
  }) => {
    await page.goto("/ui/admin/this-repo-does-not-exist-xyz/insights");
    await page.waitForLoadState("networkidle");

    // The param-driven breadcrumb renders with no fetch — the shell survives.
    await expect(
      page.getByRole("link", { name: "this-repo-does-not-exist-xyz" }),
    ).toBeVisible();
    // Each Insights panel 404s and degrades to its own InlineError.
    await expect(page.getByText(/Failed to load/i).first()).toBeVisible();
  });

  test("an injected 500 on the repos list degrades to a visible error", async ({ page }) => {
    // Fulfil (do not abort) so the browser sees a normal HTTP 500.
    await page.route("**/api/v3/user/repos**", (route) =>
      route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ message: "Internal Server Error" }),
      }),
    );

    await page.goto("/ui/repos");
    await page.waitForLoadState("networkidle");

    await expect(page.getByText(/Failed to load repositories/i)).toBeVisible();
    await expect(page.getByRole("link", { name: "bleephub" })).toBeVisible();
  });

  test("an injected 500 on the server metrics endpoint degrades the Operations console to a visible error", async ({ page }) => {
    // The console's counters come from the server's own metrics surface.
    await page.route("**/internal/metrics**", (route) =>
      route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ message: "Internal Server Error" }),
      }),
    );

    await page.goto("/ui/operations");
    await page.waitForLoadState("networkidle");

    // A failed metrics fetch must degrade to a visible InlineError, not blank.
    await expect(page.getByText(/Failed to load overview/i)).toBeVisible();
    await expect(page.getByRole("link", { name: "bleephub" })).toBeVisible();
  });

  test("an injected 500 on the recent-workflows fetch leaves the console's counters intact", async ({ page }) => {
    await page.route("**/api/v3/user/repos**", (route) =>
      route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ message: "Internal Server Error" }),
      }),
    );

    await page.goto("/ui/operations");
    await page.waitForLoadState("networkidle");

    // Only the recent-workflows table depends on the repo list, so counters
    // must survive its failure while the table degrades on its own.
    await expect(page.getByText(/Failed to load workflows/i)).toBeVisible();
    await expect(page.getByText("Connected runners")).toBeVisible();
  });
});
