import { test, expect, type Page } from "./fixtures.js";
import AxeBuilder from "@axe-core/playwright";

const ADMIN_TOKEN = "bleephub-admin-token-00000000000000000000";
const BASE = "http://localhost:15555";

// Repo Settings → Webhooks: real-browser proof the new tab lists, creates, and
// is accessible. Seeds a repo via the API, then drives the settings UI.

async function api(page: Page, method: string, path: string, body?: Record<string, unknown>) {
  const bodyJson = body === undefined ? null : JSON.stringify(body);
  return page.evaluate(
    async ({ base, method, path, token, bodyJson }) => {
      const res = await fetch(base + path, {
        method,
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
        ...(bodyJson === null ? {} : { body: bodyJson }),
      });
      return { ok: res.ok, status: res.status };
    },
    { base: BASE, method, path, token: ADMIN_TOKEN, bodyJson },
  );
}

test("repo settings Webhooks tab: list, create, and axe-clean", async ({ page }) => {
  const repo = "hooks-fixture";
  await page.goto("/ui/");
  // Seed the repo (tolerate 422 if a prior run created it).
  const created = await api(page, "POST", "/api/v3/user/repos", { name: repo, auto_init: true });
  expect([201, 422]).toContain(created.status);

  await page.goto(`/ui/admin/${repo}/settings`);
  await page.getByRole("button", { name: "Webhooks" }).click();

  // The create form is present (this surface previously did not exist).
  const urlInput = page.getByLabel("Payload URL");
  await expect(urlInput).toBeVisible();

  // No accessibility violations on the Webhooks tab (WCAG A + AA).
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .analyze();
  expect(results.violations).toEqual([]);

  // Create a webhook (inactive so no delivery is attempted) and see it listed.
  await urlInput.fill("https://example.com/my-webhook");
  await page.getByRole("checkbox", { name: "Active" }).uncheck();
  await page.getByRole("button", { name: "Add webhook" }).click();

  await expect(page.getByText("example.com/my-webhook")).toBeVisible();
});
