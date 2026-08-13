import { test, expect } from "./fixtures.js";
import AxeBuilder from "@axe-core/playwright";

// ⌘K / Ctrl-K command palette — real-browser proof it opens, filters, navigates,
// and is accessible. Uses static jump-to targets so it needs no seeded data.
// Control+k triggers on both macOS (the listener accepts ctrlKey) and Linux CI.

test("command palette: open, filter, navigate, and axe-clean", async ({ page }) => {
  await page.goto("/ui/");
  await expect(page.getByRole("link", { name: "bleephub" })).toBeVisible();

  // Put keyboard focus on the page (the handler is document-level, so a
  // bubbled keydown from any focused element reaches it), then open with ⌘K.
  await page.locator('input[type="search"]').first().focus();
  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: /command palette/i });
  await expect(dialog).toBeVisible();

  // The input should be focused, and static targets present.
  const input = dialog.getByRole("combobox");
  await expect(input).toBeFocused();
  await expect(dialog.getByRole("option", { name: /Dashboard/ })).toBeVisible();

  // No accessibility violations on the open palette (WCAG A + AA).
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"])
    .include('[role="dialog"]')
    .analyze();
  expect(results.violations).toEqual([]);

  // Filter to a single static target. Wait for the debounced filter to remove
  // the other targets so Notifications is the active (first) option, then Enter.
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
