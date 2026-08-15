import { test, expect, type Page } from "./fixtures.js";
import AxeBuilder from "@axe-core/playwright";

// Inferred from AxeBuilder itself so no direct dependency on axe-core is needed.
type AxeAnalysis = Awaited<ReturnType<InstanceType<typeof AxeBuilder>["analyze"]>>;
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));

// ─── Phase 0 empirical baseline ───────────────────────────────────────────────
//
// This spec sweeps a representative set of /ui routes in BOTH light and dark
// themes, runs axe-core (WCAG 2.0/2.1/2.2 A + AA), and writes a machine-readable
// violation inventory plus a light/dark theme-defect catalogue. Per-route load
// failures are recorded (not thrown) during the sweep so the catalogue is
// complete; the final "a11y ratchet" test then FAILS the suite if any route in
// either theme carried a violation or failed to load. The baseline is zero, so
// this is a blocking regression gate — drive new findings back to zero rather
// than loosening it.

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
  projectNumber: 0,
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

// Shared axe → ViolationRecord projection, used by both the per-route scan and
// the open-dialog scan so neither drifts (and jscpd sees one copy).
function mapViolations(results: AxeAnalysis): ViolationRecord[] {
  return results.violations.map((v) => ({
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
}

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
    { route: "/ui/settings/organizations", label: "my-organizations" },
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
    { route: `/ui/repos/${o}/${r}/blame/main/README.md`, label: "repo-blame" },
    { route: `/ui/repos/${o}/${r}/branches`, label: "repo-branches" },
    { route: `/ui/repos/${o}/${r}/tags`, label: "repo-tags" },
    { route: `/ui/repos/${o}/${r}/releases`, label: "repo-releases" },
    { route: `/ui/repos/${o}/${r}/releases/new`, label: "repo-release-new" },
    { route: `/ui/repos/${o}/${r}/labels`, label: "repo-labels" },
    { route: `/ui/repos/${o}/${r}/milestones`, label: "repo-milestones" },
    { route: `/ui/repos/${o}/${r}/settings`, label: "repo-settings" },
    { route: `/ui/repos/${o}/${r}/settings/branch-protection`, label: "repo-branch-protection" },
    { route: `/ui/repos/${o}/${r}/security`, label: "repo-security-overview" },
    { route: `/ui/repos/${o}/${r}/security/code-scanning`, label: "repo-code-scanning" },
    { route: `/ui/repos/${o}/${r}/insights`, label: "repo-insights" },
    { route: `/ui/repos/${o}/${r}/discussions`, label: "repo-discussions" },
    { route: `/ui/repos/${o}/${r}/wiki`, label: "repo-wiki" },
    // org
    { route: `/ui/orgs/${org}`, label: "org-overview" },
    { route: `/ui/orgs/${org}/people`, label: "org-people" },
    { route: `/ui/orgs/${org}/teams`, label: "org-teams" },
    { route: `/ui/orgs/${org}/teams/platform`, label: "org-team-detail" },
    { route: `/ui/orgs/${org}/repos`, label: "org-repos" },
    { route: `/ui/orgs/${org}/governance`, label: "org-governance" },
    { route: `/ui/orgs/${org}/rulesets`, label: "org-rulesets" },
    { route: `/ui/orgs/${org}/governance?tab=member-privileges`, label: "org-member-privileges" },
    { route: `/ui/orgs/${org}/governance?tab=actions`, label: "org-actions-settings" },
    { route: `/ui/orgs/${org}/governance?tab=secrets`, label: "org-secrets" },
    { route: `/ui/orgs/${org}/governance?tab=code-security`, label: "org-code-security" },
    { route: `/ui/orgs/${org}/projects/${seeded.projectNumber || 1}`, label: "org-project-detail" },
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

  // issue — the body exercises task-list checkboxes and all five GitHub alert
  // types so the ratchet validates the alert title/border colours in both themes.
  const issueRes = await api(page, "POST", `/api/v3/repos/${seeded.owner}/${seeded.repo}/issues`, {
    title: "Baseline parity issue",
    body:
      "Seed issue for the a11y sweep.\n\n- [ ] item one\n- [x] item two\n\n" +
      "Autolinks: see #1, cc @admin, cross admin/org-parity#1.\n\n" +
      "> [!NOTE]\n> A note callout.\n\n" +
      "> [!TIP]\n> A tip callout.\n\n" +
      "> [!IMPORTANT]\n> An important callout.\n\n" +
      "> [!WARNING]\n> A warning callout.\n\n" +
      "> [!CAUTION]\n> A caution callout.",
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
  // an org team so /orgs/{org}/teams/{slug} renders a team detail for the scan.
  ok("org-team", await api(page, "POST", `/api/v3/orgs/${seeded.org}/teams`, {
    name: "Platform",
  }));
  // an org custom-property schema so the repo Settings › Custom properties tab
  // renders its authoring form (not just the empty state) for the a11y scan.
  ok("org-props", await api(page, "PATCH", `/api/v3/orgs/${seeded.org}/properties/schema`, {
    properties: [
      { property_name: "environment", value_type: "single_select", allowed_values: ["prod", "staging"], description: "Deploy tier" },
      { property_name: "regions", value_type: "multi_select", allowed_values: ["us", "eu"] },
      { property_name: "team", value_type: "string" },
    ],
  }));

  // a Projects V2 board so /orgs/{org}/projects/{n} renders the table view with
  // a real single-select column and an item (GitHub creates projects over
  // GraphQL only). Best-effort: the route still scans clean if any step is gated.
  const orgInfo = await api(page, "GET", `/api/v3/orgs/${seeded.org}`);
  const ownerNodeId =
    orgInfo.ok && orgInfo.json && typeof orgInfo.json === "object"
      ? (orgInfo.json as { node_id?: string }).node_id
      : undefined;
  if (ownerNodeId) {
    const projRes = await api(page, "POST", "/api/graphql", {
      query:
        "mutation($input:CreateProjectV2Input!){createProjectV2(input:$input){projectV2{number}}}",
      variables: { input: { ownerId: ownerNodeId, title: "Parity Roadmap" } },
    });
    const projNumber =
      projRes.ok && projRes.json && typeof projRes.json === "object"
        ? (projRes.json as { data?: { createProjectV2?: { projectV2?: { number?: number } } } }).data
            ?.createProjectV2?.projectV2?.number
        : undefined;
    if (projNumber) {
      seeded.projectNumber = projNumber;
      ok("project-field", await api(page, "POST", `/api/v3/orgs/${seeded.org}/projectsV2/${projNumber}/fields`, {
        name: "Status",
        data_type: "single_select",
        single_select_options: [
          { name: "Todo", color: "GRAY", description: "" },
          { name: "Done", color: "GREEN", description: "" },
        ],
      }));
      // A text field too, so the table renders an editable text input to scan.
      ok("project-text-field", await api(page, "POST", `/api/v3/orgs/${seeded.org}/projectsV2/${projNumber}/fields`, {
        name: "Notes",
        data_type: "text",
      }));
      ok("project-draft", await api(page, "POST", `/api/v3/orgs/${seeded.org}/projectsV2/${projNumber}/drafts`, {
        title: "Draft parity item",
      }));
    } else {
      ok("project", projRes);
    }
  }

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
        // Async list/feed content (dashboard repos + activity, tables) can render
        // after network-idle; give the DOM a beat to settle so axe measures the
        // real, populated page (and matches slower CI) rather than a skeleton.
        await page.waitForTimeout(600);

        const isDark = await page.evaluate(() =>
          document.documentElement.classList.contains("dark"),
        );
        record.themeApplied = theme === "dark" ? isDark : !isDark;

        const results = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
        record.violations = mapViolations(results);

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
        }${record.themeApplied ? "" : " (theme-mismatch)"}` +
          // Surface the offending rule + element selectors on stdout so a CI-only
          // (e.g. Linux-subpixel) regression is diagnosable without the artifact.
          record.violations
            .map((v) => `\n    ! ${v.id}: ${v.targets.join(" | ")}`)
            .join(""),
      );
    }

    // Also scan an OPEN modal surface: the "?" keyboard-shortcuts sheet is an
    // interactive dialog the base-route scans never reach.
    {
      const record: RouteResult = {
        route: "/ui/ (shortcuts dialog)",
        theme,
        url: BASE + "/ui/",
        themeApplied: false,
        loadFailure: false,
        violations: [],
      };
      try {
        await page.goto("/ui/", { waitUntil: "domcontentloaded", timeout: 30_000 });
        await page.waitForSelector("main, [role=main], .app-header", { timeout: 8_000 }).catch(() => {});
        // GlobalShortcuts (which owns the "?" handler) is code-split, so let its
        // chunk load before pressing.
        await page.waitForTimeout(800);
        const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
        await page.keyboard.press("Shift+Slash"); // "?"
        await dialog.waitFor({ state: "visible", timeout: 8_000 });
        const isDark = await page.evaluate(() => document.documentElement.classList.contains("dark"));
        record.themeApplied = theme === "dark" ? isDark : !isDark;
        record.violations = mapViolations(await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze());
      } catch (err) {
        record.loadFailure = true;
        record.error = err instanceof Error ? err.message : String(err);
      }
      collected.push(record);
      // eslint-disable-next-line no-console
      console.log(
        `[scan] ${theme} shortcuts-dialog -> ${
          record.loadFailure ? "LOAD-FAIL" : `${record.violations.length} rules`
        }` + record.violations.map((v) => `\n    ! ${v.id}: ${v.targets.join(" | ")}`).join(""),
      );
    }

    // Also scan the repo Settings → Actions tab, whose controls (permissions
    // radios, workflow-token permissions, fork-PR approval, artifact retention,
    // create/approve-PRs) are gated behind client state, so the base /settings
    // route only ever renders the General tab.
    {
      const route = `/ui/repos/${seeded.owner}/${seeded.repo}/settings (Actions tab)`;
      const record: RouteResult = {
        route,
        theme,
        url: BASE + `/ui/repos/${seeded.owner}/${seeded.repo}/settings`,
        themeApplied: false,
        loadFailure: false,
        violations: [],
      };
      try {
        await page.goto(`/ui/repos/${seeded.owner}/${seeded.repo}/settings`, { waitUntil: "domcontentloaded", timeout: 30_000 });
        await page.waitForSelector("main, [role=main], .app-header", { timeout: 8_000 }).catch(() => {});
        await page.getByRole("button", { name: "Actions", exact: true }).click();
        await page.getByLabel("Fork pull request approval policy").waitFor({ state: "visible", timeout: 8_000 });
        await page.waitForLoadState("networkidle", { timeout: 6_000 }).catch(() => {});
        const isDark = await page.evaluate(() => document.documentElement.classList.contains("dark"));
        record.themeApplied = theme === "dark" ? isDark : !isDark;
        record.violations = mapViolations(await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze());
      } catch (err) {
        record.loadFailure = true;
        record.error = err instanceof Error ? err.message : String(err);
      }
      collected.push(record);
      // eslint-disable-next-line no-console
      console.log(
        `[scan] ${theme} settings-actions -> ${
          record.loadFailure ? "LOAD-FAIL" : `${record.violations.length} rules`
        }` + record.violations.map((v) => `\n    ! ${v.id}: ${v.targets.join(" | ")}`).join(""),
      );
    }

    // Also scan the repo Settings → Rulesets tab, which renders the full inline
    // ruleset authoring editor (targeting conditions, per-rule parameter
    // sub-forms, bypass-actor list) — none of it reachable from a base route.
    {
      const route = `/ui/repos/${seeded.owner}/${seeded.repo}/settings (Rulesets tab)`;
      const record: RouteResult = {
        route,
        theme,
        url: BASE + `/ui/repos/${seeded.owner}/${seeded.repo}/settings`,
        themeApplied: false,
        loadFailure: false,
        violations: [],
      };
      try {
        await page.goto(`/ui/repos/${seeded.owner}/${seeded.repo}/settings`, { waitUntil: "domcontentloaded", timeout: 30_000 });
        await page.waitForSelector("main, [role=main], .app-header", { timeout: 8_000 }).catch(() => {});
        await page.getByRole("button", { name: "Rulesets", exact: true }).click();
        // Expand every parameterised rule so its sub-form is scanned too.
        await page.getByLabel("pull_request").waitFor({ state: "visible", timeout: 8_000 });
        await page.getByLabel("pull_request").check();
        await page.getByLabel("required_status_checks").check();
        await page.getByRole("button", { name: "Add bypass actor" }).click();
        await page.waitForLoadState("networkidle", { timeout: 6_000 }).catch(() => {});
        const isDark = await page.evaluate(() => document.documentElement.classList.contains("dark"));
        record.themeApplied = theme === "dark" ? isDark : !isDark;
        record.violations = mapViolations(await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze());
      } catch (err) {
        record.loadFailure = true;
        record.error = err instanceof Error ? err.message : String(err);
      }
      collected.push(record);
      // eslint-disable-next-line no-console
      console.log(
        `[scan] ${theme} settings-rulesets -> ${
          record.loadFailure ? "LOAD-FAIL" : `${record.violations.length} rules`
        }` + record.violations.map((v) => `\n    ! ${v.id}: ${v.targets.join(" | ")}`).join(""),
      );
    }

    // Also scan the repo Settings → Custom properties tab (org-owned repo, whose
    // org has a seeded schema, so the value-authoring form renders) — a
    // state-gated tab invisible to the base settings scan.
    {
      const record: RouteResult = {
        route: `/ui/repos/${seeded.org}/org-parity/settings (Custom properties tab)`,
        theme,
        url: BASE + `/ui/repos/${seeded.org}/org-parity/settings`,
        themeApplied: false,
        loadFailure: false,
        violations: [],
      };
      try {
        await page.goto(`/ui/repos/${seeded.org}/org-parity/settings`, { waitUntil: "domcontentloaded", timeout: 30_000 });
        await page.waitForSelector("main, [role=main], .app-header", { timeout: 8_000 }).catch(() => {});
        await page.getByRole("button", { name: "Custom properties", exact: true }).click();
        await page.getByLabel("environment").waitFor({ state: "visible", timeout: 8_000 });
        const isDark = await page.evaluate(() => document.documentElement.classList.contains("dark"));
        record.themeApplied = theme === "dark" ? isDark : !isDark;
        record.violations = mapViolations(await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze());
      } catch (err) {
        record.loadFailure = true;
        record.error = err instanceof Error ? err.message : String(err);
      }
      collected.push(record);
      // eslint-disable-next-line no-console
      console.log(
        `[scan] ${theme} settings-custom-properties -> ${
          record.loadFailure ? "LOAD-FAIL" : `${record.violations.length} rules`
        }` + record.violations.map((v) => `\n    ! ${v.id}: ${v.targets.join(" | ")}`).join(""),
      );
    }

    // Also scan the search page's Advanced query-builder form, which is toggled
    // open by a button and so is invisible to the base /ui/search scan.
    {
      const record: RouteResult = {
        route: "/ui/search (advanced form)",
        theme,
        url: BASE + "/ui/search",
        themeApplied: false,
        loadFailure: false,
        violations: [],
      };
      try {
        await page.goto("/ui/search", { waitUntil: "domcontentloaded", timeout: 30_000 });
        await page.waitForSelector("main, [role=main], .app-header", { timeout: 8_000 }).catch(() => {});
        await page.getByRole("button", { name: "Advanced", exact: true }).click();
        await page.getByLabel("With these words").waitFor({ state: "visible", timeout: 8_000 });
        const isDark = await page.evaluate(() => document.documentElement.classList.contains("dark"));
        record.themeApplied = theme === "dark" ? isDark : !isDark;
        record.violations = mapViolations(await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze());
      } catch (err) {
        record.loadFailure = true;
        record.error = err instanceof Error ? err.message : String(err);
      }
      collected.push(record);
      // eslint-disable-next-line no-console
      console.log(
        `[scan] ${theme} search-advanced -> ${
          record.loadFailure ? "LOAD-FAIL" : `${record.violations.length} rules`
        }` + record.violations.map((v) => `\n    ! ${v.id}: ${v.targets.join(" | ")}`).join(""),
      );
    }

    // Also scan the org Code security "New configuration" modal, whose feature-
    // toggle form (name/description + ~10 aria-labelled selects) is only reachable
    // by opening the dialog.
    {
      const record: RouteResult = {
        route: `/ui/orgs/${seeded.org}/governance?tab=code-security (new config dialog)`,
        theme,
        url: BASE + `/ui/orgs/${seeded.org}/governance?tab=code-security`,
        themeApplied: false,
        loadFailure: false,
        violations: [],
      };
      try {
        await page.goto(`/ui/orgs/${seeded.org}/governance?tab=code-security`, { waitUntil: "domcontentloaded", timeout: 30_000 });
        await page.waitForSelector("main, [role=main], .app-header", { timeout: 8_000 }).catch(() => {});
        await page.getByRole("button", { name: "New configuration", exact: true }).click();
        await page.getByLabel("Configuration name").waitFor({ state: "visible", timeout: 8_000 });
        const isDark = await page.evaluate(() => document.documentElement.classList.contains("dark"));
        record.themeApplied = theme === "dark" ? isDark : !isDark;
        record.violations = mapViolations(await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze());
      } catch (err) {
        record.loadFailure = true;
        record.error = err instanceof Error ? err.message : String(err);
      }
      collected.push(record);
      // eslint-disable-next-line no-console
      console.log(
        `[scan] ${theme} code-security-dialog -> ${
          record.loadFailure ? "LOAD-FAIL" : `${record.violations.length} rules`
        }` + record.violations.map((v) => `\n    ! ${v.id}: ${v.targets.join(" | ")}`).join(""),
      );
    }

    expect(collected.length).toBeGreaterThan(0);
  });
}

// ─── ratchet gate ────────────────────────────────────────────────────────────────
// The sweep is now BLOCKING: after both theme passes, fail if any route carries
// an axe violation or failed to load. The baseline was driven to zero (ledger
// WEB-064..069), so any regression — a new hardcoded color, a missing label —
// fails CI here rather than silently accruing. Runs after the two theme tests in
// the same worker, so `collected` is populated.
test("a11y ratchet — zero WCAG violations across all routes and themes", () => {
  const offenders = collected
    .filter((r) => r.loadFailure || r.violations.length > 0)
    .map((r) =>
      r.loadFailure
        ? `${r.route} [${r.theme}] LOAD-FAIL: ${r.error ?? "unknown"}`
        : `${r.route} [${r.theme}] ${r.violations.map((v) => `${v.id}×${v.nodes}`).join(", ")}`,
    );
  expect(
    offenders,
    offenders.length
      ? `axe/theme regressions (drive back to zero, don't loosen the gate):\n  ${offenders.join("\n  ")}`
      : undefined,
  ).toEqual([]);
});

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
