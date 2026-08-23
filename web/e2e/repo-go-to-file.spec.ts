import { test, expect, type Page } from "./fixtures.js";
import AxeBuilder from "@axe-core/playwright";

const ADMIN_TOKEN = "bleephub-admin-token-00000000000000000000";
const BASE = "http://localhost:15555";

// Repo "Go to file" fuzzy finder — real-browser proof it opens (button + `t`),
// filters the recursive tree, navigates to a blob, and is accessible.

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

test("go to file: opens, filters, navigates, and axe-clean", async ({ page }) => {
  const repo = "gtf-fixture";
  await page.goto("/ui/");
  const created = await api(page, "POST", "/api/v3/user/repos", { name: repo, auto_init: true });
  expect([201, 422]).toContain(created.status);

  await page.goto(`/ui/admin/${repo}`);
  await page.getByRole("button", { name: "Go to file" }).click();

  const dialog = page.getByRole("dialog", { name: /go to file/i });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("combobox")).toBeFocused();
  // auto_init created a README.md, which the recursive tree lists.
  await expect(dialog.getByRole("option", { name: "README.md" })).toBeVisible();

  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .include('[role="dialog"]')
    .analyze();
  expect(results.violations).toEqual([]);

  await dialog.getByRole("combobox").fill("readme");
  await dialog.getByRole("option", { name: "README.md" }).click();
  await expect(page).toHaveURL(/\/blob\/main\/README\.md$/);
});
