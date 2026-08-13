# PLAN.md — GitHub functional-parity, theming & accessibility

## Objective (sharpened 2026-08-13)

Make the bleephub web UI a **functional replica of github.com**:

1. **Navigational / structural parity** — every GitHub menu, tab, and control is present **in the same location** with the **same functionality**. Styling may differ; *placement and behavior must match*.
2. **Light/dark mode** — every page and component renders correctly and legibly in **both** themes, with no hardcoded colors that leak.
3. **WCAG 2.2 AA** — measured, gated compliance (contrast, keyboard operability, focus, names/roles/values, etc.).
4. **ARIA / accessibility** — correct semantics: landmarks, roles, labels, live regions, focus management, keyboard nav.

## Honest status reset

The **BUGS.md ledger backlog is closed** (689 fixed + 3 deferred = 692, zero open) and **CI is green**, and the codebase was refactored into five compiler-enforced modules. That is real — but it measured a *feature backlog and conformance gates*, **not** the four objectives above. None of these four has been **systematically verified against github.com**. Earlier "UI parity complete" claims were scoped to the backlog and are **overstated relative to this objective**. This plan is the correction: verify empirically, surface by surface, and close the gaps.

Grounding facts measured today (starting point, not a gap list — Phase 0 produces the gap list):
- **UI surface**: 87 `/ui` routes across 55 page components (inventory below).
- **Dark mode**: infrastructure present (`.dark` class, `useTheme` hook, header toggle, 60 `--color-*` tokens with a `.dark` override) — but **26 hardcoded hex colors** in `pages/`+`components/` will not respond to theme, and **no page is verified per-theme**.
- **Accessibility**: **no automated a11y testing** (no axe/jest-axe; one ad-hoc modal test). 277 ad-hoc `aria-*`/`role=` usages exist but are unverified.

## Method (non-negotiable, from the "wired ≠ working" lesson)

- **Empirical, not wiring audits.** Every claim is proven by driving the running app in Claude-in-Chrome (or Playwright e2e) and capturing evidence — not by reading that a handler exists.
- **Compare to real github.com structure**, not to our own prior assumptions. Phase 0 builds the reference map.
- **One rolling PR per coherent slice.** Each PR carries its evidence (browser journey, both-theme screenshots, axe report). User merges; assistant never merges.
- **File findings as BUGS.md rows** (WEB-/A11Y-/THEME- prefixes) as they're discovered, so the gap is tracked, not hand-waved. Regenerate parity inventory LAST after BUGS.md edits.
- **Gates:** existing Go/web gates stay green; add the new accessibility + theme gates (below) so parity can't silently regress.

---

## Phase 0 — Assessment (measure the gap before fixing) — DO THIS FIRST

Produce three artifacts so the remaining work is sized honestly rather than guessed:

1. **github.com structure reference map** — enumerate GitHub's real navigation surfaces and the menus/controls each contains, so parity is checked against a source of truth:
   - Global: top bar (search + scope, create-new `+` menu, issues/PRs/notifications, profile menu contents), left dashboard nav.
   - Repo: the tab row (Code/Issues/PRs/Actions/Projects/Wiki/Security/Insights/Settings) and each tab's sub-nav + page controls; the repo header (watch/fork/star, about, clone, branch selector, file tree, add-file menu).
   - Org: tab row (Overview/Repositories/Packages/People/Teams/Projects/Insights/Settings) + settings sub-nav.
   - User profile, Settings (the full left-rail of account settings), Notifications, Search results tabs, Explore/Marketplace.
   - **GitHub Classroom** (the old `classroom.github.com`): classroom list, classroom page tabs (Assignments / Students / Settings), the assignment-creation flow (starter repo, deadline, autograding, feedback PR, protected paths), roster management, accept flow, and grade export — mapped to `/ui/classrooms*`.
   - Map each to the bleephub route that should host it (from the inventory below), marking **present / missing / misplaced**.
2. **Accessibility baseline** — wire `@axe-core/playwright` (or jest-axe on rendered pages) and run it across every route; capture the violation inventory by rule and severity. This is the WCAG/ARIA gap, measured.
3. **Theme baseline** — render every route in light and dark; screenshot both; catalog theme defects (illegible contrast, hardcoded-color leaks — start from the 26 known hex literals, unstyled dark surfaces, focus rings invisible in one theme).

Deliverable: a scored gap list (per surface: nav-parity gaps, functionality gaps, a11y violations, theme defects) that turns the rest of this plan from estimate into fact.

---

## Phase 0 — RESULTS (measured 2026-08-13)

Phase 0 is **done**. Three empirical artifacts were produced against the running app.

### 0.1 Accessibility + theme baseline (axe-core, 47 routes × light/dark)

Tooling landed: `web/e2e/a11y-theme-sweep.spec.ts` sweeps 47 routes in both themes with
`@axe-core/playwright` (WCAG 2.0/2.1/2.2 A+AA), writing a machine-readable inventory to
`web/test-results/a11y/`. Run: `cd web && SERVER_BIN=../bleephub-server bunx playwright test a11y-theme-sweep`.

Baseline was **219 violation instances across 40 route/theme pairs**, 4 rules. Fixes in this slice
drove it to **11 instances / 8 pairs, 1 rule** (a 95% reduction). Cleared entirely: `label`
(critical), `definition-list`, `target-size`. See ledger **WEB-064** (the `--color-fg-subtle`
contrast token — the dominant finding, ~200 element-hits from the shared DataTable sort-headers),
**WEB-065** (task-list checkbox labels via a shared Markdown wrapper), **WEB-066** (dl→ul on the
profile/org sidebars), **WEB-067** (24px target sizes).

Remaining (open, tracked): **WEB-068** label-pill contrast (raw label color as text over a tint —
fails AA for light label colors, both themes; needs a luminance-aware foreground like GitHub's) and
**WEB-069** a few residual colored spans (marketplace active-tab, issue state badge, code-scanning
`<code>`). These are the Workstream C contrast pass. No dark-mode-only contrast leaks were found.

### 0.2 github.com nav-parity gap map → Workstream A backlog

Full surface-by-surface map done (top bar, dashboard, repo header+tabs, repo settings, issue/PR,
org, profile, account settings, notifications/search/explore/marketplace/gists). The bar is
"same location + same function, styling may differ." Each item below converts to a `WEB-` ledger
row as its fix lands. **The UI is NOT at nav parity** — two whole surfaces are absent and ~18
menus/controls are missing or non-functional.

**BLOCKERs (whole tab/page absent):**
- **B1 — repo Wiki**: ✅ FIXED (WEB-072) — added a backend page store + REST routes, the Wiki
  tab (gated on `has_wiki`), `/ui/repos/:owner/:repo/wiki[/:slug]` routes, and a WikiPage with a
  page rail + markdown view + create/edit/delete. Go + vitest + Chromium-sweep verified.
- **B2 — user-profile tab row**: ✅ FIXED (WEB-070) — Overview/Repositories/Projects/Packages/Stars
  tab row; Overview renders the profile README, a contribution graph (from the events feed), and a
  recent-activity list. Still open: WEB-071 pinned repositories (needs a backend pin store).

**MAJORs (menu/control missing or non-functional), ranked:**
1. Profile **contribution graph** (calendar heatmap) missing.
2. Profile **pinned repositories** missing.
3. Profile **README** missing.
4. ✅ FIXED (WEB-073) — Global **command palette / ⌘K** jump-to: lazy-loaded dialog+combobox (static targets + live repo/user search, keyboard nav, axe-clean); ⌘K/Ctrl-K in `AppHeader.tsx`.
5. Repo **"Go to file"** fuzzy finder missing.
6. ✅ FIXED (WEB-075) — Repo Settings **Webhooks**: list/create/toggle/ping/delete + deliveries link (Settings → Code and automation); was only a delivery viewer.
7. Repo Settings **Environments** editor missing (Deployments list ≠ env settings).
8. Repo Settings **Actions** settings (General/Runners) missing.
9. **Repo-level Rulesets** missing (rulesets exist org-only).
10. **Org Settings** landing page missing (settings scattered as top-level tabs).
11. **Org Insights** tab missing (Insights is repo-only).
12. ✅ FIXED (WEB-074) — Issue/PR sidebar **Projects**: now lists the org's ProjectsV2, marks/edits (add + remove) this item's membership; was a hardcoded "None yet" stub (`IssueSidebar.tsx`).
13. Account settings: **Password/2FA, Notifications, Profile edit, Account, Appearance** all missing
    (only Emails/keys/PAT/blocked present).
14. Account settings: authorized **Applications** missing; OAuth/GitHub Apps misplaced under Operations.
15. Create **`+`** menu missing Import repository and New project.
16. Avatar menu missing **Your organizations / projects / stars**.
17. Dashboard activity feed is **issues-only**, not the follow/news feed.
18. Add file has no **Upload files** (binary upload) — text-only modal.

**MINORs** (misplaced/partial): repo-settings sub-nav is in-page state, not deep-linkable URLs;
repo tab order (Insights before Security); theme toggle in avatar menu vs Appearance; branch
selector lacks branch/tag search + create-branch; About-edit-gear + contributors absent; search
results lack Wikis/Discussions/Packages tabs; Explore reuses `/ui/search`.

Two items **need a runtime check** before filing: repo Danger-zone completeness
(delete referenced, visibility-change/archive unconfirmed) and header-search qualifier hand-off.

### 0.3 Classroom parity map — see Workstream D

Mapped against the old `classroom.github.com`; REST list + org-scoped auth verified; autograding
wiring provisions a repo and commits the workflow. Full end-to-end (roster/import, group
assignments, accept→provision, grade CSV) still to verify surface by surface.

---

## Workstream A — Navigational / functional parity

For each surface in the Phase-0 map, verify against github.com and fix:
- **Menu presence & location**: every GitHub menu/tab/control exists in the same structural place. Add missing ones; relocate misplaced ones.
- **Functionality**: every GitHub action on that surface is present and *works* (driven in-browser, not just wired). The backend write endpoints largely exist already (prior finding: ~1035 server write routes); most gaps are expected to be frontend wiring/placement, but each must be proven.
- Known candidate areas to scrutinize (from the route inventory — confirm against github.com, don't assume complete): repo **Wiki** (no `/wiki` route present), repo **Settings** sub-nav breadth vs GitHub's, the profile/overview page, org **Insights**, the create-`+` menu contents, repo header controls (branch selector, file-tree add-file/upload, "Go to file"), global command palette.

Slices land per-surface (e.g. "repo header parity", "account-settings left-rail parity", "org tab-row parity").

## Workstream B — Light/dark mode compliance

- Replace the 26 hardcoded hex colors (and any others Phase 0 finds) with `--color-*` tokens; ensure the `.dark` block defines every token used.
- Verify every page in both themes (Phase-0 screenshots become the regression baseline).
- **Gate**: a Playwright pass that loads key routes in both themes and fails on axe color-contrast violations, so a new hardcoded color can't regress theming.

## Workstream C — WCAG 2.2 AA + ARIA

Drive the Phase-0 axe inventory to zero (or justified, documented exceptions):
- **Structure**: one `<main>`, correct landmark roles, logical heading order, skip-link works.
- **Names/roles/values**: every control has an accessible name; icon-only buttons get `aria-label`; form fields have associated `<label>`s; tables use proper headers.
- **Keyboard**: full keyboard operability, visible focus in both themes, no traps, modals trap+restore focus (extend the existing modal a11y test to all dialogs).
- **Dynamic content**: `aria-live` for async load/error/toast; `aria-current` on active nav; `aria-expanded` on menus.
- **Contrast**: AA ratios in both themes (overlaps Workstream B).
- **Gate**: `@axe-core/playwright` run in the Browser-e2e CI job across all routes, failing on new violations (ratchet down from the Phase-0 baseline, never up — mirror the openapi-shape ratchet pattern).

## Workstream D — GitHub Classroom (full parity with the old implementation)

GitHub Classroom is a revived, upstream-**deprecated** product — but its objective is the same as the rest: **full functional parity with GitHub's old Classroom implementation** (`classroom.github.com` as it existed). It has a real reference to match — its navigation, flows, and features — not an "internally coherent" exemption. Status today: REST list + org-scoped auth verified working (list→200, web-create correctly 403s without org-admin); **the full product is unverified against the real Classroom**.

Match the old Classroom's structure and functionality (Phase 0 builds this reference map too):
- **Classroom list** (per organization) and the **create-classroom** flow (pick org, name, connect a roster).
- **Classroom page** with its tabs/sections: **Assignments**, **Students / Roster**, classroom **Settings**, teachers/TAs.
- **Roster**: create from a student-identifier list, link students to GitHub accounts, LMS/import.
- **Assignments**: individual **and** group assignments; the creation flow (title, starter-code template repo, deadline, visibility, editor/Codespaces option, **protected file paths**, **autograding** tests, **feedback pull request**).
- **Accept flow**: invite URL → provisions a repo from the starter template for the student/team.
- **Assignment overview**: accepted-assignments list, per-student repo links, **autograding results/points**, and **grade download (CSV export)**.

Verify each end-to-end in an owned org through both the API and `/ui/classrooms`, matching where each control lives in the real product. Apply the same light/dark + WCAG/ARIA bar. (Other genuinely-non-github surfaces — `/ui/operations/*` operator console, `/ui/metrics` — get the lighter "internally coherent + themed + accessible" check, since those have no github.com analogue.)

---

## Sequencing

1. **Phase 0** (assessment) → the scored gap list. *(next action)*
2. Then interleave A/B/C/D as rolling PRs, **highest-traffic surfaces first** (dashboard, repo Code/Issues/PRs, global nav), each PR proving parity + both themes + zero-new-axe-violations for the surface it touches.
3. Add the two CI gates (axe ratchet, dual-theme contrast) early so progress is protected.

## Out of scope / decisions already made

- **Pixel/visual identity** with github.com is explicitly *not* required (styling may differ) — only structure, placement, functionality, theming, and a11y.
- **Deferred by owner decision** (not work): keep `@bleephub/ui-core` as a library (WEB-016/017); accept TEST-015 (comma-ok sweep net-negative).

## Standing maintenance (unchanged, runs alongside)

Dependabot bumps ≥24h past publish → one consolidated rolling PR; vendored GraphQL/REST contract drift → re-vendor per the documented recipes; CI health (stale parity inventory = regenerate; MinIO/network flakes = re-run). Proven cadence: #270 (Go bumps), #271 (GraphQL drift), #272 (web patches).

---

## Appendix — current `/ui` route inventory (87 routes / 55 pages)

Global: `/ui/` (dashboard), `/ui/:login` (profile), `/ui/account`, `/ui/notifications`, `/ui/search`, `/ui/login`, `/ui/oauth`, `/ui/users/:login`.
Repos: `/ui/repos`, `/ui/repos/:owner/:repo` and tabs — `actions` (+`/runs/:runId`), `blob/:ref/*`, `tree/:ref/*`, `branches`, `tags`, `commits`(+`/:sha`), `compare/:range`, `discussions`(+`/:number`), `deployments`, `forks`, `insights`, `issues`(+`/:number`), `labels`, `milestones`, `packages`, `projects-classic`, `pulls`(+`/:number`,`/checks`,`/commits`,`/files`), `releases`(+`/new`,`/:releaseId`), `security/{advisories,code-scanning,dependabot,secret-scanning}`, `settings`(+`/branch-protection`,`/secrets`), `stargazers`, `watchers`, `codespaces`, `hooks/:hookId/deliveries`.
Orgs: `/ui/orgs/:org` and `copilot`, `governance`, `hooks`(+deliveries), `packages`, `people`, `projects`(+`/:number`), `repos`, `rulesets`, `teams`.
Operator/misc: `/ui/operations`(+`audit-log`,`enterprise`,`orgs`,`teams`,`users`), `/ui/apps`(+marketplace), `/ui/marketplace`(+`/:slug`), `/ui/classrooms`(+`/:classroomId`,`/accept/:inviteCode`), `/ui/codespaces`(+`/:codespaceName`), `/ui/copilot/spaces`, `/ui/gists`, `/ui/metrics`, `/ui/migrations`, `/ui/packages`, `/ui/runners`, `/ui/workflows`(+`/:id`).

**Notable absences to confirm against github.com in Phase 0** (present on GitHub, no route here): repo **Wiki**, repo **Projects (v2 beta boards at repo scope)**, **Explore/Trending**, user **Stars/Projects tabs**, the global **command palette**, repo **Settings** sub-pages beyond branch-protection/secrets (Actions/Pages/Environments/Webhooks/Deploy keys/Moderation/etc. — some exist as separate routes, map them).
