import { test, expect, type Page } from "./fixtures.js";
import AxeBuilder from "@axe-core/playwright";

const ADMIN_TOKEN = "bleephub-admin-token-00000000000000000000";
const BASE = "http://localhost:15555";

// Org Settings landing — real-browser proof it renders the profile form + the
// settings-section links, saves via PATCH, and is accessible.

async function api(page: Page, method: string, path: string, body?: Record<string, unknown>) {
  const bodyJson = body === undefined ? null : JSON.stringify(body);
  return page.evaluate(
    async ({ base, method, path, token, bodyJson }) => {
      const res = await fetch(base + path, {
        method,
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
        ...(bodyJson === null ? {} : { body: bodyJson }),
      });
      return { status: res.status };
    },
    { base: BASE, method, path, token: ADMIN_TOKEN, bodyJson },
  );
}

test("org settings landing: profile form, section links, and axe-clean", async ({ page }) => {
  const org = "settings-org";
  await page.goto("/ui/");
  const created = await api(page, "POST", "/api/v3/admin/organizations", { login: org, admin: "admin" });
  expect([201, 422]).toContain(created.status);

  await page.goto(`/ui/orgs/${org}/settings`);
  await expect(page.getByRole("heading", { name: "Settings", level: 1 })).toBeVisible();
  await expect(page.getByLabel("Display name")).toBeVisible();
  await expect(page.getByRole("link", { name: /Member privileges/ })).toBeVisible();

  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .analyze();
  expect(results.violations).toEqual([]);

  await page.getByLabel("Display name").fill("Settings Org Renamed");
  await page.getByRole("button", { name: /^save$/i }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
});
