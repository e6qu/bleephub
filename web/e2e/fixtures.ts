import { test as base, expect, type Page } from "@playwright/test";

const ADMIN_TOKEN = "bleephub-admin-token-00000000000000000000";

// Every journey owns a browser session. Reusing one storage-state cookie made
// the sign-out journey revoke the server-side session beneath every parallel
// test context, so later journeys depended on scheduling order. Refreshing the
// HttpOnly session through the public token exchange keeps the real browser
// authentication path while making logout and other session mutations local
// to the test that performs them.
export const test = base.extend({
  page: async ({ page }, use) => {
    const response = await page.request.post("/auth/token", {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
    });
    if (!response.ok()) {
      throw new Error(
        `could not create isolated browser session: ${response.status()} ${await response.text()}`,
      );
    }
    await use(page);
  },
});

export { expect };
export type { Page };
