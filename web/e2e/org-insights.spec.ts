import { test, expect, type Page } from "./fixtures.js";
import AxeBuilder from "@axe-core/playwright";

const ADMIN_TOKEN = "bleephub-admin-token-00000000000000000000";
const BASE = "http://localhost:15555";

// Org Insights (GHES API Insights) — real-browser proof the tab/page renders
// its summary + subjects and is accessible, even with no seeded API activity.

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

test("org insights: summary + subjects render and axe-clean", async ({ page }) => {
  const org = "insights-org";
  await page.goto("/ui/");
  const created = await api(page, "POST", "/api/v3/admin/organizations", { login: org, admin: "admin" });
  expect([201, 422]).toContain(created.status);

  await page.goto(`/ui/orgs/${org}/insights`);
  await expect(page.getByRole("heading", { name: "Insights", level: 1 })).toBeVisible();
  await expect(page.getByText("Total API requests")).toBeVisible();

  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .analyze();
  expect(results.violations).toEqual([]);
});
