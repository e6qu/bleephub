import { test, expect, type Page } from "./fixtures.js";
import AxeBuilder from "@axe-core/playwright";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));

// ─── Phase 0 empirical baseline ───────────────────────────────────────────────
//
// This spec does NOT fix anything and does NOT fail on violations. It sweeps a
// representative set of /ui routes in BOTH light and dark themes, runs axe-core
// (WCAG 2.0/2.1/2.2 A + AA), and writes a machine-readable violation inventory
// plus a light/dark theme-defect catalogue. Route-load failures are recorded as
// findings rather than aborting the sweep.

const ADMIN_TOKEN = "bleephub-admin-token-00000000000000000000";
const BASE = "http://localhost:15555";

// Repo-relative so the sweep is runnable by anyone / in CI. Override with
// A11Y_OUT_DIR to redirect the machine-readable inventory + screenshots.
const OUT_DIR =
  process.env.A11Y_OUT_DIR ?? path.join(HERE, "..", "test-results", "a11y");
const OUT_JSON = path.join(OUT_DIR, "a11y-theme-baseline.json");
const SHOT_DIR = path.join(OUT_DIR, "a11y-screenshots");

const WCAG_TAGS = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"];
const THEMES = ["light", "dark"] as const;
type Theme = (typeof THEMES)[number];

// ─── seeding ──────────────────────────────────────────────────────────────────
// Concrete identifiers filled in by beforeAll so dynamic routes aren't empty.
const seeded = {
  owner: "admin",
  repo: "parity",
  org: "parity-org",
  issueNumber: 0,
  pullNumber: 0,
  classroomId: 0,
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

// ─── collectors ────────────────────────────────────────────────────────────────
type ViolationRecord = {
  id: string;
  impact: string | null;
  description: string;
  help: string;
  helpUrl: string;
  tags: string[];
  nodes: number;
  targets: string[];
};

type RouteResult = {
  route: string;
  theme: Theme;
  url: string;
  themeApplied: boolean; // did <html> actually carry the expected .dark state
  loadFailure: boolean;
  error?: string;
  violations: ViolationRecord[];
};

const collected: RouteResult[] = [];

// ─── route set (~47 routes; one per distinct page type) ─────────────────────────
function buildRoutes(): { route: string; label: string }[] {
  const o = seeded.owner;
  const r = seeded.repo;
  const org = seeded.org;
  const iss = seeded.issueNumber || 1;
  const pr = seeded.pullNumber || 2;
  return [
    { route: "/ui/", label: "dashboard" },
    { route: `/ui/${o}`, label: "profile" },
    { route: `/ui/${o}?tab=repositories`, label: "profile-repositories" },
    { route: `/ui/${o}?tab=projects`, label: "profile-projects" },
    { route: `/ui/${o}?tab=packages`, label: "profile-packages" },
    { route: `/ui/${o}?tab=stars`, label: "profile-stars" },
    { route: "/ui/account", label: "account" },
    { route: "/ui/notifications", label: "notifications" },
    { route: "/ui/search?q=test", label: "search" },
    { route: "/ui/repos", label: "repos-list" },
    { route: "/ui/gists", label: "gists" },
    { route: "/ui/packages", label: "packages" },
    { route: "/ui/runners", label: "runners" },
    { route: "/ui/workflows", label: "workflows" },
    { route: "/ui/marketplace", label: "marketplace" },
    { route: "/ui/codespaces", label: "codespaces" },
    { route: "/ui/migrations", label: "migrations" },
    { route: "/ui/metrics", label: "metrics" },
    { route: "/ui/classrooms", label: "classrooms" },
    { route: "/ui/apps", label: "apps" },
    { route: "/ui/oauth", label: "oauth" },
    // repo tabs
    { route: `/ui/repos/${o}/${r}`, label: "repo-code" },
    { route: `/ui/repos/${o}/${r}/issues`, label: "repo-issues" },
    { route: `/ui/repos/${o}/${r}/issues/${iss}`, label: "repo-issue-detail" },
    { route: `/ui/repos/${o}/${r}/pulls`, label: "repo-pulls" },
    { route: `/ui/repos/${o}/${r}/pulls/${pr}`, label: "repo-pull-detail" },
    { route: `/ui/repos/${o}/${r}/pulls/${pr}/files`, label: "repo-pull-files" },
    { route: `/ui/repos/${o}/${r}/pulls/${pr}/commits`, label: "repo-pull-commits" },
    { route: `/ui/repos/${o}/${r}/pulls/${pr}/checks`, label: "repo-pull-checks" },
    { route: `/ui/repos/${o}/${r}/actions`, label: "repo-actions" },
    { route: `/ui/repos/${o}/${r}/commits`, label: "repo-commits" },
    { route: `/ui/repos/${o}/${r}/branches`, label: "repo-branches" },
    { route: `/ui/repos/${o}/${r}/tags`, label: "repo-tags" },
    { route: `/ui/repos/${o}/${r}/releases`, label: "repo-releases" },
    { route: `/ui/repos/${o}/${r}/labels`, label: "repo-labels" },
    { route: `/ui/repos/${o}/${r}/milestones`, label: "repo-milestones" },
    { route: `/ui/repos/${o}/${r}/settings`, label: "repo-settings" },
    { route: `/ui/repos/${o}/${r}/settings/branch-protection`, label: "repo-branch-protection" },
    { route: `/ui/repos/${o}/${r}/security/code-scanning`, label: "repo-code-scanning" },
    { route: `/ui/repos/${o}/${r}/insights`, label: "repo-insights" },
    { route: `/ui/repos/${o}/${r}/discussions`, label: "repo-discussions" },
    { route: `/ui/repos/${o}/${r}/wiki`, label: "repo-wiki" },
    // org
    { route: `/ui/orgs/${org}`, label: "org-overview" },
    { route: `/ui/orgs/${org}/people`, label: "org-people" },
    { route: `/ui/orgs/${org}/teams`, label: "org-teams" },
    { route: `/ui/orgs/${org}/repos`, label: "org-repos" },
    { route: `/ui/orgs/${org}/governance`, label: "org-governance" },
    // operations console
    { route: "/ui/operations", label: "operations" },
    { route: "/ui/operations/audit-log", label: "operations-audit-log" },
    { route: "/ui/operations/orgs", label: "operations-orgs" },
    { route: "/ui/operations/users", label: "operations-users" },
    { route: "/ui/operations/teams", label: "operations-teams" },
  ];
}

// ─── seed once ──────────────────────────────────────────────────────────────────
test.beforeAll(async ({ browser }) => {
  fs.mkdirSync(SHOT_DIR, { recursive: true });
  const context = await browser.newContext({ baseURL: BASE, ignoreHTTPSErrors: true });
  const page = await context.newPage();
  // The API accepts the admin bearer directly; still exchange a session so the
  // origin is primed like the real browser path.
  await page.request.post("/auth/token", { headers: { Authorization: `Bearer ${ADMIN_TOKEN}` } });
  await page.goto("/ui/");

  const ok = (label: string, res: { ok: boolean; status: number; text: string }) => {
    // 422/409 = "already exists" from a prior run against a reused server; fine.
    if (!res.ok && res.status !== 422 && res.status !== 409) {
      // eslint-disable-next-line no-console
      console.warn(`[seed] ${label} -> ${res.status} ${res.text.slice(0, 200)}`);
    }
  };

  // repo with an initial commit
  ok("repo", await api(page, "POST", "/api/v3/user/repos", {
    name: seeded.repo,
    description: "GitHub-parity a11y baseline fixture",
    auto_init: true,
    private: false,
    has_wiki: true,
  }));

  // a wiki page so the Wiki tab renders content, not just the empty state
  ok("wiki", await api(page, "PUT", `/ui-data/repos/${seeded.owner}/${seeded.repo}/wiki/pages/home`, {
    title: "Home",
    body: "# Welcome\n\nParity wiki fixture page.",
  }));

  // Pin the repo so the profile Overview renders its pinned grid.
  ok("pin", await api(page, "PUT", `/ui-data/users/${seeded.owner}/pinned`, {
    repos: [`${seeded.owner}/${seeded.repo}`],
  }));

  // labels + milestones
  ok("label-1", await api(page, "POST", `/api/v3/repos/${seeded.owner}/${seeded.repo}/labels`, {
    name: "parity-bug",
    color: "d73a4a",
    description: "Something isn't working",
  }));
  ok("label-2", await api(page, "POST", `/api/v3/repos/${seeded.owner}/${seeded.repo}/labels`, {
    name: "parity-enhancement",
    color: "a2eeef",
    description: "New feature or request",
  }));
  ok("milestone-1", await api(page, "POST", `/api/v3/repos/${seeded.owner}/${seeded.repo}/milestones`, {
    title: "v1.0",
    description: "First milestone",
  }));
  ok("milestone-2", await api(page, "POST", `/api/v3/repos/${seeded.owner}/${seeded.repo}/milestones`, {
    title: "v2.0",
  }));

  // issue
  const issueRes = await api(page, "POST", `/api/v3/repos/${seeded.owner}/${seeded.repo}/issues`, {
    title: "Baseline parity issue",
    body: "Seed issue for the a11y sweep.\n\n- [ ] item one\n- [x] item two",
    labels: ["parity-bug"],
  });
  ok("issue", issueRes);
  if (issueRes.ok && issueRes.json && typeof issueRes.json === "object") {
    seeded.issueNumber = (issueRes.json as { number: number }).number;
  }

  // branch + file + pull request
  const mainRef = await api(page, "GET", `/api/v3/repos/${seeded.owner}/${seeded.repo}/git/ref/heads/main`);
  if (mainRef.ok && mainRef.json) {
    const sha = (mainRef.json as { object: { sha: string } }).object.sha;
    ok("branch", await api(page, "POST", `/api/v3/repos/${seeded.owner}/${seeded.repo}/git/refs`, {
      ref: "refs/heads/parity-feature",
      sha,
    }));
    ok("commit", await api(page, "PUT", `/api/v3/repos/${seeded.owner}/${seeded.repo}/contents/PARITY.md`, {
      message: "Add parity doc",
      content: Buffer.from("# Parity\n\nSeed change for PR.\n").toString("base64"),
      branch: "parity-feature",
    }));
    const pullRes = await api(page, "POST", `/api/v3/repos/${seeded.owner}/${seeded.repo}/pulls`, {
      title: "Baseline parity pull request",
      head: "parity-feature",
      base: "main",
      body: "Seed PR for the a11y sweep.",
    });
    ok("pull", pullRes);
    if (pullRes.ok && pullRes.json && typeof pullRes.json === "object") {
      seeded.pullNumber = (pullRes.json as { number: number }).number;
    }
  }

  // organization owned by admin (so the viewer owns it)
  ok("org", await api(page, "POST", "/api/v3/admin/organizations", {
    login: seeded.org,
    admin: "admin",
    profile_name: "Parity Org",
  }));
  // an org repo so org/repos isn't empty
  ok("org-repo", await api(page, "POST", `/api/v3/orgs/${seeded.org}/repos`, {
    name: "org-parity",
    auto_init: true,
  }));

  // classroom (best-effort; feature may be gated)
  const classroomRes = await api(page, "POST", "/classroom-data/classrooms", {
    name: "Parity Classroom",
    organization: seeded.org,
  });
  if (classroomRes.ok && classroomRes.json && typeof classroomRes.json === "object") {
    seeded.classroomId = (classroomRes.json as { id: number }).id;
  } else {
    ok("classroom", classroomRes);
  }

  // eslint-disable-next-line no-console
  console.log(`[seed] issue=#${seeded.issueNumber} pull=#${seeded.pullNumber} classroom=${seeded.classroomId}`);
  await context.close();
});

// ─── per-theme sweep ─────────────────────────────────────────────────────────────
for (const theme of THEMES) {
  test(`a11y sweep — ${theme} theme`, async ({ page }) => {
    test.setTimeout(600_000);

    // Force theme two ways: persisted key (read on mount) + OS media query.
    await page.emulateMedia({ colorScheme: theme });
    await page.addInitScript((t) => {
      window.localStorage.setItem("bleephub:theme", t as string);
    }, theme);

    const routes = buildRoutes();
    for (const { route } of routes) {
      const record: RouteResult = {
        route,
        theme,
        url: BASE + route,
        themeApplied: false,
        loadFailure: false,
        violations: [],
      };
      try {
        await page.goto(route, { waitUntil: "domcontentloaded", timeout: 30_000 });
        // Settle: a main landmark or the app header, then a bounded network idle.
        await page
          .waitForSelector("main, [role=main], .app-header", { timeout: 8_000 })
          .catch(() => {});
        await page.waitForLoadState("networkidle", { timeout: 6_000 }).catch(() => {});

        const isDark = await page.evaluate(() =>
          document.documentElement.classList.contains("dark"),
        );
        record.themeApplied = theme === "dark" ? isDark : !isDark;

        const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
        record.violations = results.violations.map((v) => ({
          id: v.id,
          impact: v.impact ?? null,
          description: v.description,
          help: v.help,
          helpUrl: v.helpUrl,
          tags: v.tags,
          nodes: v.nodes.length,
          targets: v.nodes
            .slice(0, 8)
            .map((n) => (Array.isArray(n.target) ? n.target.join(" ") : String(n.target))),
        }));

        const safe = route.replace(/[^a-z0-9]+/gi, "_").replace(/^_|_$/g, "");
        await page
          .screenshot({
            path: path.join(SHOT_DIR, `${theme}__${safe}.png`),
            fullPage: true,
          })
          .catch(() => {});
      } catch (err) {
        record.loadFailure = true;
        record.error = err instanceof Error ? err.message : String(err);
      }
      collected.push(record);
      // eslint-disable-next-line no-console
      console.log(
        `[scan] ${theme} ${route} -> ${
          record.loadFailure ? "LOAD-FAIL" : `${record.violations.length} rules`
        }${record.themeApplied ? "" : " (theme-mismatch)"}`,
      );
    }
    // Baseline collector: never fail on violations.
    expect(collected.length).toBeGreaterThan(0);
  });
}

// ─── aggregate + emit ────────────────────────────────────────────────────────────
test.afterAll(async () => {
  if (collected.length === 0) return;

  const byImpact: Record<string, number> = { critical: 0, serious: 0, moderate: 0, minor: 0, null: 0 };
  const byTag: Record<string, number> = {};
  const byRule: Record<string, { count: number; impact: string | null; help: string; helpUrl: string; nodes: number }> = {};
  const routesWithViolations = new Set<string>();
  const loadFailures: { route: string; theme: string; error?: string }[] = [];
  const themeMismatches: { route: string; theme: string }[] = [];

  // contrast tracking for theme-leak signal
  const contrastByRouteTheme: Record<string, Record<Theme, number>> = {};

  let totalViolationInstances = 0; // sum of node counts
  let totalViolationRuleHits = 0; // (route,theme,rule) tuples

  for (const rec of collected) {
    if (rec.loadFailure)
      loadFailures.push(
        rec.error === undefined
          ? { route: rec.route, theme: rec.theme }
          : { route: rec.route, theme: rec.theme, error: rec.error },
      );
    if (!rec.themeApplied && !rec.loadFailure) themeMismatches.push({ route: rec.route, theme: rec.theme });
    if (rec.violations.length > 0) routesWithViolations.add(`${rec.route} [${rec.theme}]`);

    for (const v of rec.violations) {
      totalViolationRuleHits += 1;
      totalViolationInstances += v.nodes;
      const impactKey = v.impact ?? "null";
      byImpact[impactKey] = (byImpact[impactKey] ?? 0) + 1;
      for (const t of v.tags) byTag[t] = (byTag[t] ?? 0) + 1;
      const r = (byRule[v.id] ??= { count: 0, impact: v.impact, help: v.help, helpUrl: v.helpUrl, nodes: 0 });
      r.count += 1;
      r.nodes += v.nodes;

      if (v.id === "color-contrast" || v.id === "color-contrast-enhanced") {
        const key = rec.route;
        const slot = (contrastByRouteTheme[key] ??= { light: 0, dark: 0 });
        slot[rec.theme] += v.nodes;
      }
    }
  }

  // dark-mode-only contrast failures = routes where dark has contrast nodes but light has none.
  const darkOnlyContrastRoutes: string[] = [];
  const lightOnlyContrastRoutes: string[] = [];
  let darkContrastNodes = 0;
  let lightContrastNodes = 0;
  for (const [route, slot] of Object.entries(contrastByRouteTheme)) {
    darkContrastNodes += slot.dark;
    lightContrastNodes += slot.light;
    if (slot.dark > 0 && slot.light === 0) darkOnlyContrastRoutes.push(route);
    if (slot.light > 0 && slot.dark === 0) lightOnlyContrastRoutes.push(route);
  }

  const topRules = Object.entries(byRule)
    .sort((a, b) => b[1].count - a[1].count)
    .map(([id, info]) => ({ id, ...info }));

  const worstRoutes = collected
    .filter((r) => !r.loadFailure)
    .map((r) => ({ route: r.route, theme: r.theme, rules: r.violations.length, nodes: r.violations.reduce((s, v) => s + v.nodes, 0) }))
    .sort((a, b) => b.nodes - a.nodes)
    .slice(0, 15);

  const summary = {
    // No generatedAt timestamp: the test-clock gate forbids calendar-sensitive
    // wall-clock data in tests, and a stamp would also churn the baseline JSON.
    axeCoreVersion: "4.13.0",
    axePlaywrightVersion: "4.13.0",
    wcagTags: WCAG_TAGS,
    routesScanned: new Set(collected.map((c) => c.route)).size,
    scans: collected.length,
    themes: THEMES,
    seeded,
    totals: {
      uniqueViolationRules: Object.keys(byRule).length,
      ruleHits: totalViolationRuleHits,
      violationInstances: totalViolationInstances,
      routesWithViolations: routesWithViolations.size,
      loadFailures: loadFailures.length,
      themeMismatches: themeMismatches.length,
    },
    byImpact,
    byWcagTag: Object.fromEntries(Object.entries(byTag).sort((a, b) => b[1] - a[1])),
    colorContrast: {
      totalContrastRuleHits: (byRule["color-contrast"]?.count ?? 0) + (byRule["color-contrast-enhanced"]?.count ?? 0),
      lightContrastNodes,
      darkContrastNodes,
      darkOnlyContrastRoutesCount: darkOnlyContrastRoutes.length,
      darkOnlyContrastRoutes,
      lightOnlyContrastRoutesCount: lightOnlyContrastRoutes.length,
      lightOnlyContrastRoutes,
    },
    topRules,
    worstRoutes,
    loadFailures,
    themeMismatches,
  };

  fs.mkdirSync(OUT_DIR, { recursive: true });
  fs.writeFileSync(OUT_JSON, JSON.stringify({ summary, detail: collected }, null, 2));

  // ── markdown summary to stdout ──
  const lines: string[] = [];
  lines.push("");
  lines.push("================ A11Y + THEME BASELINE ================");
  lines.push(`axe-core ${summary.axeCoreVersion} | WCAG tags: ${WCAG_TAGS.join(", ")}`);
  lines.push(`routes: ${summary.routesScanned} | scans (route×theme): ${summary.scans}`);
  lines.push(
    `unique rules violated: ${summary.totals.uniqueViolationRules} | rule-hits: ${summary.totals.ruleHits} | element instances: ${summary.totals.violationInstances}`,
  );
  lines.push(`routes-with-violations (route×theme): ${summary.totals.routesWithViolations}`);
  lines.push(
    `by impact -> critical ${byImpact.critical} | serious ${byImpact.serious} | moderate ${byImpact.moderate} | minor ${byImpact.minor}`,
  );
  lines.push("");
  lines.push("Top 10 violation rules (by rule-hits across route×theme):");
  topRules.slice(0, 10).forEach((r, i) => {
    lines.push(`  ${String(i + 1).padStart(2)}. ${r.id} — hits ${r.count}, elements ${r.nodes} [${r.impact ?? "n/a"}]`);
  });
  lines.push("");
  lines.push("Worst routes (by element instances):");
  worstRoutes.slice(0, 8).forEach((r) => {
    lines.push(`  - ${r.route} [${r.theme}] — ${r.rules} rules / ${r.nodes} elements`);
  });
  lines.push("");
  lines.push("Theme signal (color-contrast):");
  lines.push(`  light contrast element-hits: ${lightContrastNodes} | dark contrast element-hits: ${darkContrastNodes}`);
  lines.push(`  dark-mode-only contrast-failure routes (theme leaks): ${darkOnlyContrastRoutes.length}`);
  if (darkOnlyContrastRoutes.length) lines.push(`    ${darkOnlyContrastRoutes.join(", ")}`);
  lines.push(`  light-mode-only contrast-failure routes: ${lightOnlyContrastRoutes.length}`);
  if (lightOnlyContrastRoutes.length) lines.push(`    ${lightOnlyContrastRoutes.join(", ")}`);
  lines.push("");
  if (loadFailures.length) {
    lines.push(`Route-load failures: ${loadFailures.length}`);
    loadFailures.forEach((f) => lines.push(`  - ${f.route} [${f.theme}]: ${(f.error ?? "").slice(0, 120)}`));
  } else {
    lines.push("Route-load failures: 0");
  }
  if (themeMismatches.length) {
    lines.push(`Theme-not-applied warnings: ${themeMismatches.length}`);
    themeMismatches.slice(0, 20).forEach((m) => lines.push(`  - ${m.route} [${m.theme}]`));
  }
  lines.push("");
  lines.push(`JSON written: ${OUT_JSON}`);
  lines.push("======================================================");
  // eslint-disable-next-line no-console
  console.log(lines.join("\n"));
});
