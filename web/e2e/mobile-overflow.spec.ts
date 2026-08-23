import { test, expect, type Page } from "./fixtures.js";

// ─── Mobile (iPhone-class) horizontal-overflow gate ──────────────────────────
//
// Regression gate for phone-width responsiveness: at 375×812 no page may
// scroll horizontally at the PAGE level. Wide content (code, diffs, tables,
// tab rows) must scroll inside its own overflow-x-auto container instead —
// exactly github.com's behaviour. The historical failure modes this guards:
//   - the global header refusing to shrink (search input min-content +
//     non-collapsing quick links forced 664–764px on every route), and
//   - auto grid tracks inheriting min-content from wide descendants (the
//     profile contribution graph) because grid children lacked min-w-0.
//
// Deliberately lightweight: no axe, a handful of representative routes, one
// layout assertion per route.

const ADMIN_TOKEN = "bleephub-admin-token-00000000000000000000";
const BASE = "http://localhost:15555";

test.use({ viewport: { width: 375, height: 812 } });

// Concrete identifiers filled in by beforeAll so dynamic routes aren't empty.
const seeded = {
  owner: "admin",
  repo: "mobile-overflow",
  issueNumber: 1,
  stressIssueNumber: 2,
  pullNumber: 3,
};

async function api(
  page: Page,
  method: string,
  apiPath: string,
  body?: Record<string, unknown>,
): Promise<{ ok: boolean; status: number; json: unknown; text: string }> {
  const bodyJson = body === undefined ? null : JSON.stringify(body);
  return page.evaluate(
    async ({ base, method, apiPath, token, bodyJson }) => {
      const res = await fetch(base + apiPath, {
        method,
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
        },
        ...(bodyJson === null ? {} : { body: bodyJson }),
      });
      const text = await res.text();
      let json: unknown = null;
      try {
        json = text ? JSON.parse(text) : null;
      } catch {
        json = null;
      }
      return { ok: res.ok, status: res.status, json, text };
    },
    { base: BASE, method, apiPath, token: ADMIN_TOKEN, bodyJson },
  );
}

test.beforeAll(async ({ browser }) => {
  const context = await browser.newContext({ baseURL: BASE });
  const page = await context.newPage();
  await page.request.post("/auth/token", { headers: { Authorization: `Bearer ${ADMIN_TOKEN}` } });
  await page.goto("/ui/");

  // 409/422 = already seeded by a previous run against a reused server; fine.
  const repo = `/api/v3/repos/${seeded.owner}/${seeded.repo}`;
  await api(page, "POST", "/api/v3/user/repos", {
    name: seeded.repo,
    description: "mobile-overflow e2e fixture",
    auto_init: true,
  });
  // A long unbroken line makes the issue body + blob exercise in-card scrolling.
  const longLine =
    "someVeryLongUnbrokenIdentifier_0123456789_0123456789_0123456789_0123456789_0123456789";
  const issueRes = await api(page, "POST", `${repo}/issues`, {
    title: "Mobile overflow seed issue with a long wrapping title for the detail page",
    body: `Seed body.\n\n    ${longLine}\n`,
  });
  if (issueRes.ok && issueRes.json && typeof issueRes.json === "object") {
    seeded.issueNumber = (issueRes.json as { number: number }).number;
  }
  // Hostile content: a 220-char unbroken title (title wrap), an unbroken body
  // word (markdown wrap), and an extreme label name (sidebar select width) —
  // each has broken page width before.
  const unbroken = "W".repeat(220);
  const stressRes = await api(page, "POST", `${repo}/issues`, {
    title: unbroken,
    body: `Unbroken ${unbroken}${unbroken} word.`,
  });
  if (stressRes.ok && stressRes.json && typeof stressRes.json === "object") {
    seeded.stressIssueNumber = (stressRes.json as { number: number }).number;
  }
  await api(page, "POST", `${repo}/labels`, {
    name: "an-extremely-long-label-name-that-goes-on-and-on-forever-and-ever",
    color: "5319e7",
  });
  const mainRef = await api(page, "GET", `${repo}/git/ref/heads/main`);
  if (mainRef.ok && mainRef.json) {
    const sha = (mainRef.json as { object: { sha: string } }).object.sha;
    await api(page, "POST", `${repo}/git/refs`, { ref: "refs/heads/mobile-feature", sha });
    await api(page, "PUT", `${repo}/contents/WIDE.md`, {
      message: "Add wide doc",
      content: Buffer.from(`# Wide\n\n    const x = ${longLine};\n`).toString("base64"),
      branch: "mobile-feature",
    });
    const pullRes = await api(page, "POST", `${repo}/pulls`, {
      title: "Mobile overflow seed pull request",
      head: "mobile-feature",
      base: "main",
      body: "Seed PR for the mobile-overflow gate.",
    });
    if (pullRes.ok && pullRes.json && typeof pullRes.json === "object") {
      seeded.pullNumber = (pullRes.json as { number: number }).number;
    }
  }
  await context.close();
});

// Route builders resolve after beforeAll fills the seeded numbers.
const ROUTES: { label: string; route: () => string }[] = [
  { label: "dashboard", route: () => "/ui/" },
  { label: "profile", route: () => `/ui/${seeded.owner}` },
  { label: "repo-home", route: () => `/ui/${seeded.owner}/${seeded.repo}` },
  { label: "issues-list", route: () => `/ui/${seeded.owner}/${seeded.repo}/issues` },
  {
    label: "issue-detail",
    route: () => `/ui/${seeded.owner}/${seeded.repo}/issues/${seeded.issueNumber}`,
  },
  {
    label: "issue-detail-hostile-content",
    route: () => `/ui/${seeded.owner}/${seeded.repo}/issues/${seeded.stressIssueNumber}`,
  },
  {
    label: "pull-files",
    route: () => `/ui/${seeded.owner}/${seeded.repo}/pulls/${seeded.pullNumber}/files`,
  },
  {
    label: "blob",
    route: () => `/ui/${seeded.owner}/${seeded.repo}/blob/mobile-feature/WIDE.md`,
  },
  { label: "discussions", route: () => `/ui/${seeded.owner}/${seeded.repo}/discussions` },
  { label: "repo-settings", route: () => `/ui/${seeded.owner}/${seeded.repo}/settings` },
  { label: "account-settings", route: () => "/ui/account" },
];

for (const { label, route } of ROUTES) {
  test(`no page-level horizontal overflow at 375px: ${label}`, async ({ page }) => {
    await page.goto(route(), { waitUntil: "networkidle" });
    const metrics = await page.evaluate(() => {
      const scrollWidth = document.scrollingElement?.scrollWidth ?? 0;
      const innerWidth = window.innerWidth;
      // Name the widest unclipped leaf elements so a failure says WHAT
      // overflowed, not just that something did.
      const offenders: string[] = [];
      if (scrollWidth > innerWidth + 2) {
        for (const el of Array.from(document.querySelectorAll("*"))) {
          const r = el.getBoundingClientRect();
          if (r.right <= innerWidth + 4 || r.width < 30) continue;
          let anc = el.parentElement;
          let clipped = false;
          while (anc) {
            const o = getComputedStyle(anc).overflowX;
            if (o === "auto" || o === "hidden" || o === "scroll") {
              clipped = true;
              break;
            }
            anc = anc.parentElement;
          }
          if (clipped) continue;
          const widerChild = Array.from(el.children).some(
            (c) => c.getBoundingClientRect().right > innerWidth + 4,
          );
          if (widerChild) continue;
          offenders.push(
            `${el.tagName}.${String(el.className).slice(0, 50)} '${(el.textContent ?? "")
              .trim()
              .slice(0, 40)}' w=${Math.round(r.width)}`,
          );
        }
      }
      return { scrollWidth, innerWidth, offenders: [...new Set(offenders)].slice(0, 5) };
    });
    expect(
      metrics.scrollWidth,
      `document scrollWidth ${metrics.scrollWidth} exceeds viewport ${metrics.innerWidth} on ${route()}; offenders: ${metrics.offenders.join(" | ")}`,
    ).toBeLessThanOrEqual(metrics.innerWidth + 2);
  });
}
