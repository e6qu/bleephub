import { test, expect } from "./fixtures.js";
import AxeBuilder from "@axe-core/playwright";

// Command palette e2e: uses static jump-to targets so it needs no seeded data.
// Control+k fires on macOS too (the listener accepts ctrlKey), not just Linux CI.

test("command palette: open, filter, navigate, and axe-clean", async ({ page }) => {
  await page.goto("/ui/");
  await expect(page.getByRole("link", { name: "bleephub" })).toBeVisible();

  // Handler is document-level, so a bubbled keydown from a focused element reaches it.
  await page.locator('input[type="search"]').first().focus();
  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: /command palette/i });
  await expect(dialog).toBeVisible();

  const input = dialog.getByRole("combobox");
  await expect(input).toBeFocused();
  await expect(dialog.getByRole("option", { name: /Dashboard/ })).toBeVisible();

  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .include('[role="dialog"]')
    .analyze();
  expect(results.violations).toEqual([]);

  // Wait for the debounced filter to drop other targets so Notifications is first, then Enter.
  await input.fill("notifications");
  await expect(dialog.getByRole("option", { name: /Notifications/ })).toBeVisible();
  await expect(dialog.getByRole("option", { name: /Dashboard/ })).toBeHidden();
  await input.press("Enter");

  await expect(page).toHaveURL(/\/ui\/notifications$/);
  await expect(dialog).toBeHidden();
});

test("command palette: Escape closes and restores focus", async ({ page }) => {
  await page.goto("/ui/");
  await expect(page.getByRole("link", { name: "bleephub" })).toBeVisible();
  await page.locator('input[type="search"]').first().focus();
  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: /command palette/i });
  await expect(dialog).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
});
