# PLAN — GitHub UX-parity gaps & production performance

## Outcome (implemented 2026-08-19, same PR)

Everything below was implemented in this PR except the explicitly-listed carve-outs. Verified by:
whole-tree typecheck clean, 878 vitest tests green, full Go suite green apart from the documented
local MinIO/OOM environment flakes, the Playwright e2e suite, and a live seeded walkthrough
re-measurement.

**Live re-measurements after the fix:**
- Entry JS wire size 163.8 KB → **37.3 KB** (gzip + `Cache-Control: immutable` on hashed assets,
  ETag+304 on the shell and API; `vendor-yaml` and the new `vendor-hljs` are lazy chunks).
- First-load API fan-out: repo home 16 → **8**, insights 23 → **8**, issue detail 17 → **14**,
  PR detail 22 → **16** (cold full-page loads; warm SPA navigations are lower because the
  `/ui-data/bootstrap/*` aggregates seed the query cache), via 5 new aggregate endpoints whose
  sub-payloads are byte-identical to the standalone endpoints.
- Issues list at 10k issues: **210 ms → 2.3 ms** per request (per-repo sorted index; the dominant
  cost was serializing every issue per request, now only the paginated page renders).
- Browser sessions no longer consume the core REST budget (P3); search stays 30/min with UI
  debounce and a friendly retry state.

**Carve-out closure round (second PR, 2026-08-20):** most of the original carve-outs turned out
implementable and are now closed — auto-merge (the real `enablePullRequestAutoMerge`/`disable…`
GraphQL mutations, REST `auto_merge` payload, merge-when-ready triggers at every check/status/
review/protection completion site, merge-box UI); branch-protection completeness (`lock_branch`,
`allow_fork_syncing`, `require_last_push_approval`, enforced at the ref-write and merge
chokepoints, plus web-style wildcard pattern rules under /ui-data with fnmatch semantics and
exact-rule precedence); convert-issue-to-discussion, pinned discussions (cap 4), and
commit-suggestion apply (all web-only on GitHub → /ui-data); whitespace-ignoring PR diffs
(/ui-data, Myers diff over ws-stripped lines); advisory credits (spec fields, auto-accepted);
environment Team reviewers rendering; webhook `insecure_ssl` (already server-complete — gained
delivery-behavior coverage and form UI); bootstrap aggregates extended (PR sidebar data,
milestones state=all, per_page=100 first pages).

**Third round (2026-08-20):** a visual verification pass over every newly-built surface (zero
console errors end-state) plus the last two implementable deferrals — profile **achievements**
(GitHub's real badge set — Pull Shark, YOLO, Quickdraw, Galaxy Brain, Starstruck, Pair
Extraordinaire — computed per request from store data, served at
`/ui-data/users/{login}/achievements`, profile sidebar badges) and **classic-PAT expiry**
(web-only on GitHub → `POST /ui-data/user/tokens/classic` honoring `expires_at`; expired tokens
refused by the existing auth check; expiry preset picker in the UI). The pass also surfaced and
fixed: discussion category emoji rendered as raw `:shortcode:` text, and the profile/org README
blind probes (the last console-404 sources) replaced by an always-200
`/ui-data/users/{login}/profile-readme` wrapper with byte-identical readme payloads.

**Fourth round (2026-08-20) — viewer-role parity:** a dual-role live audit (site admin vs a
read-only outsider over the same pages) found that NO repository control was role-gated — the
Settings tab, merge button, Add file/Upload, blob Edit/Delete, branch Delete, release/wiki/
Actions write controls, the issue sidebar editors, Lock, the issue overflow menu, label/milestone
CRUD and reviewer management all rendered for a viewer who would only 403. github.com hides what
the viewer cannot do. Now gated end-to-end on the repo payload's viewer-scoped `permissions`
via a shared `useRepoPermissions` hook: admin → settings surfaces (which 404 like GitHub on
direct URLs), push → every write affordance (the lattice has no triage tier), author-rules for
closing/editing own issues/PRs and managing own comments, and GitHub's exact "Only those with
write access to this repository can merge pull requests" notice in the merge box. Verified by
re-running the dual-role audit live: 39 privileged control instances across 12 pages hidden for
the read-only viewer, zero leaks, zero console errors.

**Scale re-measurement (same round):** 12k issues = 62 MB RSS (~2 KB/issue), ~775 writes/s
sustained through persistence, 0.4 ms issue-list reads, 10 ms search, 0.1 s graceful shutdown
and 0.1 s cold boot — the earlier 34 s reload reading was a measurement artifact (raced the
dying process). No scale work needed at this size.

**Fifth round (2026-08-20) — remaining journey classes:**
- **Collaborator tier validated**: the push-but-not-admin case (invited via the real invitation
  flow, accepted through `PATCH /user/repository_invitations/{id}`) shows every write control
  and no Settings tab — zero findings, zero console errors.
- **Anonymous browsing**: the SPA force-redirected every route to login while the server already
  served anonymous public reads (REST 60/hr budget and the /ui-data aggregates both answer
  anonymously). Now signed-out visitors browse public content like github.com: repo code/issues/
  PRs/actions/wiki/releases, profiles, orgs, search and gists render with a Sign-in header,
  read-only reactions, "Sign in to comment" boxes, and Star/Watch/Fork as count links to login;
  viewer-scoped surfaces (dashboard, notifications, settings, operations) still gate, as do the
  surfaces whose data layer requires a user server-side (discussions/GraphQL, security alerts,
  packages, marketplace). 45 viewer-scoped fetches gated so an anonymous page fires zero 4xx.
- **Mobile**: every page horizontally overflowed a 375px viewport; the sole page-level source
  was the global header's non-shrinking flex row (plus profile-grid min-content and the
  PageTitle actions row). Fixed responsive-only — desktop layout byte-identical — with 48 routes
  verified at 375×812 and a new `mobile-overflow.spec.ts` e2e gate (11 routes) in CI.

**Sixth round (2026-08-20) — content robustness, keyboard, dark theme:** a hostile-content
audit (220-char unbroken titles/words, RTL + emoji floods, 14-column tables, 150-char code
lines, extreme label names) found and fixed the page-width breakers GitHub handles: unwrapped
detail H1s, select elements inflated by their longest option (now globally clamped), dashboard
and notification rows crushed by unbreakable titles (which also drove four notification action
buttons under the 24px target-size floor), a min-content dashboard grid track, and a
non-wrapping PR-list filter row. A 45-stop keyboard walk found zero focus traps and zero
invisible focus targets; its only finding — the header search pair suppressing the global
focus ring with inline outline:none — is fixed. Dark theme verified clean over the hostile
pages. The mobile-overflow e2e gate now seeds hostile content itself and names the widest
unclipped elements on failure, and the full-suite spec coupling (each spec's data visible to
the others) is kept deliberately: it caught two of these bugs.

**Seventh round (2026-08-20) — interaction sweep:** ~20 end-to-end flows driven as a user
(labels, milestones, wiki cycle incl. history restore, branch create, compare→PR prefill, blob
edit, comment preview/post/close/reopen, reactions, pin, release edit, notifications save/done,
gist create, environments, rulesets, auto-merge arm, whitespace toggle, commit-suggestion apply
as a real commit) with console/network/error-banner monitoring. Three real bugs found and fixed:
issue timelines emitted every comment twice — once as the comment and once as the stored
"commented" event whose event-space id 404s the reactions endpoint (GitHub has no such event row;
now filtered from timeline and both events endpoints, regression-tested); the SPA's
`comments(first: N)` query against `PullRequestReviewThread` errored — the field took no relay
args — surfacing a visible "Failed to load thread resolution state" banner on PRs with review
threads (args added per the official schema, snapshot regenerated, the sweep test now uses the
client's exact query shape); and the repo-settings Rulesets/Environments tabs blind-probed
`/orgs/{owner}/teams` for user-owned repos (the last console-404 sources — now gated on owner
type). Everything else operated cleanly end-to-end.

**Eighth round (2026-08-20) — robustness sweep (deep links, round-trip fidelity, stale state):**
- Deep-link probing found missing resources spinning ~5s through pointless 404 retries into a raw
  `ApiError: 404` dump, unknown SPA routes silently landing on the dashboard, and case-variant
  URLs 404ing where GitHub resolves case-insensitively. Fixed: the query layer no longer retries
  4xx (except 429); every detail surface renders a GitHub-style 404 (full-page for missing
  owners/repos/gists/profiles, in-repo-shell for missing issues/PRs/releases/runs/discussions/
  refs/paths, and missing wiki slugs offer writers a title-prefilled "New page?" create CTA);
  unknown routes get a real 404 page; and the store resolves repo/user/org names
  case-insensitively through NFKC-folded secondary indexes maintained across create/rename/
  transfer/delete — with canonical casing in payloads, working case-variant git clone URLs, and
  a security audit that fixed four raw-string comparisons the folding would have skewed
  (including a case-variant path bypass of the admin self-demote guard).
- Edit round-trip fidelity verified byte-exact (entities, CRLF, unicode, script tags render
  escaped; open-edit-save changes nothing).
- Stale-state conflicts: closing an already-closed issue converges silently; a stale merge click
  previously dumped `Merge failed: PUT 405: {json}` over live merge controls — merge-box errors
  now surface the API's own message and a failed merge refetches the PR so the box converges to
  the real (merged) state. Breadcrumb casing still echoes the URL rather than canonical — noted,
  cosmetic.

**Ninth round (2026-08-20) — session sweep (leaks, drafts, liveness):**
- Long-session health measured over 480 SPA navigations via CDP: heap plateaus at ~12 MB after
  cache warm-up with ZERO DOM-node or listener growth across the last 360 navigations — no leaks.
- Comment drafts were lost on navigation (github.com restores them): a sessionStorage-backed
  draft layer now restores composer text per conversation (issues/PRs, discussion comments and
  replies, review-thread replies, gist comments), cleared on successful submit.
- The blocked merge box never converged while open (github.com live-updates it) — and the probe
  exposed a server bug beneath: a successful CLASSIC commit status never satisfied a required
  status-check context (only check runs did), so `mergeable_state` stayed "blocked" forever for
  status-reporting CI. Statuses now satisfy contexts and feed pending/failing exactly like check
  runs (regression-tested), and the PR page polls the merge-box queries every 15 s while the PR
  is open (off when merged/closed or backgrounded; sessions are core-budget-exempt). Verified
  live: the box converges ~15 s after the check lands, with no reload.

**Tenth round (2026-08-21) — integrity sweep (fan-out, search truth, pagination):**
- Pagination boundaries and search-qualifier truthfulness both probed clean: per_page caps at
  100 like GitHub, invalid values 422, no phantom `next` link at an exact-page boundary, pages
  past the end return `[]`; and 8 qualifier families (state/is/label/author/in/no:) returned
  zero rows violating their own filter, with `total_count` arithmetic consistent.
- Notification fan-out delivers every GitHub reason (author, mention, subscribed, assign,
  review_requested) — but watching a repository RETROACTIVELY flooded the inbox with every
  pre-existing issue and pull request (measured 4 → 30 threads with zero new activity), because
  threads are derived at read time from watch state. Watching now opens a notification window at
  the subscription's creation time: activity before it is not delivered (explicit per-thread
  subscribes are unaffected, and records with no stamp keep the old inclusive behaviour).
- Webhook fan-out was badly incomplete: `issues` emitted 3 of GitHub's 16 actions,
  `pull_request` 4 of ~19, and a non-draft release fired only `created` where GitHub fans out
  `created`+`published`+`released`. All of it is now implemented behind one shared emitter
  (issues and pull requests cannot diverge again), with the action-specific payload members
  (`label`, `assignee`, `milestone`, `changes`, `requested_reviewer`/`requested_team`), plus
  `pull_request_review.edited` and `pull_request_review_comment.edited/deleted`. Fixing it also
  removed a bogus `issues.edited` fired by a no-op state PATCH and exposed that
  `Find*ByNodeID` returns the LIVE row, so pre-mutation diffs read post-mutation values —
  every before/after emitter now snapshots through `Get*` first. Skipped with evidence:
  merge-queue actions (no feature), `issues.typed/untyped`, discussion lock actions.

**Git-completeness round (2026-08-22):** the sweep moved from the web UI to the git surface itself,
on the principle that anything git can do against github.com and cannot do against bleephub is a
bug, and that all of it must keep working over the S3-backed object storer rather than a real git
binary on a filesystem.

- **Shallow clone, both transports.** `git clone --depth`, `--shallow-since` and `--shallow-exclude`
  all failed outright: bleephub drove go-git's `plumbing/transport/server`, which refuses every
  shallow request (`shallow not supported`) and advertises none of the shallow capabilities, so
  clients gave up at the advertisement. The whole server side of upload-pack is now bleephub's
  (`git_uploadpack.go`), built from go-git *plumbing* (packp/revlist/packfile) over whatever
  `storer.Storer` the repo resolves to — nothing touches a filesystem or shells out. Depth is a BFS
  from the peeled wants; boundary commits emit `shallow`, and a previously-shallow commit whose
  parents now arrive emits `unshallow`, so deepening fetches and `--unshallow` work, not just the
  initial clone. Smart-HTTP and SSH both call the one `serveGitUploadPack`, so a client cannot
  observe a different protocol depending on the URL scheme it dialed.
  Advertised capabilities are exactly the implemented ones (`shallow`, `deepen-since`, `deepen-not`,
  `ofs-delta`, `agent`); `side-band-64k`, `multi_ack*`, `thin-pack`, `include-tag`, `no-progress`
  and `deepen-relative` are deliberately absent, each with its consequence documented at the
  declaration — advertising a capability that is not honoured is worse than omitting it.
- **Git LFS, absent entirely → implemented.** There was no `info/lfs` route at all, so `git lfs`
  clones left 130-byte pointer files in the working tree (the smudge filter's batch call 404'd with
  a non-LFS body) and pushes aborted in the pre-push hook. Now: the v1 batch API, object
  upload/download, and the locking API, with bytes streamed through the existing S3
  `ActionsByteStore` (`PutStream`/`GetStream`, never buffered), content-addressed at
  `lfs/objects/ab/cd/<oid>`. Uploads are rejected unless the bytes actually hash to the advertised
  oid, the oid is validated as a strict SHA-256 digest before it can become a storage key, and
  cross-repository object access is tested rather than assumed.
- **Every `download_url` / `raw_url` was a dead 404.** The contents API, commit and PR file lists
  and compare all advertise `{base}/{owner}/{repo}/raw/{ref}/{path}`, and nothing served that shape;
  an existing test asserted only that the field was *present*, never that it resolved. Now served,
  with the private-repo gate, `{ref}/{path}` ambiguity resolved by probing ref candidates
  longest-first, and `text/plain` + `nosniff` so a raw `.html`/`.svg` cannot execute in-origin.
- **Nine git object/ref fidelity bugs**, each pinned by a test: the literal ref `HEAD` resolved
  nowhere (`?ref=HEAD`, `/git/trees/HEAD`, `/commits/HEAD`, `/tarball/HEAD` all 404'd); archiving
  any repository containing a submodule 500'd (a gitlink was read as a blob — now an empty directory
  in both tar and zip, and deliberately *not* fixed in the shared `flattenTree`, whose merge caller
  needs the gitlink or merges would silently drop submodules); no size ceiling on contents/blobs
  (GitHub's `encoding:"none"` above 1 MB and `403 too_large` above 100 MB); `verification` hardcoded
  to `unsigned` and `POST /git/commits` silently dropping `signature`; the legacy ref listing
  unpaginated; tag order (newest-first, version-aware) and API-shaped archive URLs; gitlink tree
  entries advertising a guaranteed-404 `url`; uncapped directory listings; and `POST /git/blobs`
  capped at 25 MB instead of 100 MB.
- **The test suite was reaching into the macOS login keychain.** Every `git` subprocess in the
  package inherited the developer's configuration, and `credential.helper=osxkeychain` is set in
  both `/opt/homebrew/etc/gitconfig` and `~/.gitconfig`, so authenticating test clones called
  `git-credential-osxkeychain`. It is invisible on CI (no keychain, no personal config) and only
  ever bites whoever runs the suite locally — it was costing 111 of the LFS round-trip's 113
  seconds in blocked credential lookups. Every git subprocess is now hermetic
  (`GIT_CONFIG_NOSYSTEM`, a per-test global config, no prompting), enforced by a ratchet so a new
  test cannot reintroduce it. That test now runs in 1.4 s.

_Verified: full `go test ./...` green (exit 0), gofmt/vet clean, bugs ledger OK (907 rows), parity
inventory regenerated (line-number drift only). Real `git` and `git lfs` binaries drive the shallow,
SSH and LFS tests end-to-end rather than a mocked client._

**Deliberately still not implemented (git):** protocol **v2** (`ls-refs`/`fetch`, which would also
give empty repositories a proper `unborn` HEAD advertisement) and **partial clone**
(`filter=blob:none` / `tree:0`), which is currently ignored rather than refused, so a `--filter`
client silently falls back to a full clone. Both are additive on top of the new upload-pack rather
than rework of it.

**Still not implemented, with reasons:**
- package download counts: absent from GitHub's package-version payload shape.
- tag protection: GitHub retired tag protection rules in favor of rulesets, which bleephub
  already implements with tag targeting — N/A rather than a gap.
- Accepted deviations, documented: the repo sub-tab row (G9), the gradient header (G10),
  breadcrumb placement (G57), 4-toggle notification settings (G102), `/ui/repos/:owner/:repo`
  URL namespace. 2FA is relabeled as a simulated flag rather than growing a fake TOTP flow (G91)
  because production auth is delegated to the SSO IdP.

_The sections below are the original audit, kept for reference; the Section-3 numbers are the
**pre-fix** baseline._

_2026-08-19. Sources: a code-level audit of all 62 `/ui` pages against github.com journeys
(every "missing" claim re-verified by reading the component and grepping all of `web/src`),
a live walkthrough of a freshly built binary seeded with an org, two users, a repo with 11
commits / 1,120 issues / 2 PRs / a release / a wiki, and API + asset benchmarks against that
instance. Ledger IDs are assigned in BUGS.md when an item is picked up; the G/P numbers here
are plan-local only._

## 1. Confirmed live bugs (found in the running app, root-caused)

These broke or misrepresented a journey during the walkthrough — fix before the backlog below.

| # | Sev | Where | What happens | Root cause / fix |
|---|---|---|---|---|
| G1 | B | Repo → Issues tab | Pull requests appear as rows in the Issues list (PRs #121/#122 render above real issues) and inflate the open count. GitHub's UI never shows PRs under Issues. | REST `GET /repos/../issues` includes PRs by GitHub contract; `IssuesPage.tsx` renders the payload verbatim — there is no `pull_request` filter anywhere in the file. Filter out rows carrying `pull_request`, and derive counts the same way. |
| G2 | B | PR detail header | "0 changed files with +0 −0" and tab label "Files changed 0" on a PR that changes 2 files (the Files tab itself renders both diffs correctly). | Server bug, not UI: `GET /pulls/{n}` returns `changed_files: 0, additions: 0, deletions: 0` while `GET /pulls/{n}/files` returns the 2 files. Compute diff stats on the PR payload. |
| G3 | m | Every page footer | Footer literally reads "Published not yet published". | `Shell.tsx:47` falls back to the string `"not yet published"` when `VITE_BLEEPHUB_PUBLISHED_AT` is unset; the embedded build never sets it. Stamp it in `make build` or hide the row when unset. |

## 2. UX / journey gaps vs github.com

Severity: **B** journey broken or major structural divergence · **M** notable divergence · **m** polish.
"(server-backed)" = the server already supports it, UI wiring only. "(server work)" = handler/store
change needed first — per the server-backing rule, do not ship dead controls.

### 2.1 Cross-cutting (one fix, many pages)

| # | Sev | Gap | Fix |
|---|---|---|---|
| G4 | M | **No syntax highlighting anywhere** — blob view, commit/compare diffs, PR diffs, gists, markdown code fences are all monochrome `<pre>`. | Lazy-load a highlighter (shiki/lowlight) in the blob/diff chunks — keep it out of the entry bundle (see P2). |
| G5 | M | **Avatars missing across core journeys** — issue/PR list rows, comment headers, commit rows, participants, stargazers/watchers render logins as plain text. `Avatar` component exists and is barely used. | Reuse `Avatar` in CommentCard, list rows, CommitsList, IssueSidebar participants, RepoSocialPage. |
| G6 | M | **Absolute dates instead of GitHub's relative times** — "8/19/2026" instead of "2 hours ago" on issues, commits, lists. | One `relativeTime` util (or `<time>` component) applied everywhere; keep absolute on hover as GitHub does. |
| G7 | M | **Comment composers are bare textareas** — no Write/Preview tabs, no markdown toolbar, on issues/PRs/discussions/reviews. | Shared tabbed composer wrapping the existing `Markdown` renderer. |
| G8 | m | Counts capped as "50+" (Issues/PR tab badges, list header) where GitHub shows exact counts. | Use totals from search/count endpoints or Link-header last page. |
| G9 | m | Repo pages add a second tab row (Code / Commits / Branches / Tags / Activity / Releases) that github.com does not have; GitHub reaches these via the tree header ("N commits", branch switcher, tags icon). | Structural-parity decision: fold entry points into the code header, or accept and document the extra row. |
| G10 | m | Header/hero use a pastel blue→pink gradient; GitHub chrome is flat neutral in both themes. | Visual-identity decision — accept as brand or flatten for parity. Document either way. |

### 2.2 Repo code browsing

| # | Sev | Gap | Fix |
|---|---|---|---|
| G11 | M | File table rows show icon + name only — no per-file latest commit message/age; "latest commit" banner absent in subdirectories. | Path-scoped `commits?path=` per visible row (batched), banner from path-scoped latest commit. |
| G12 | M | Ref switcher is a plain branches-only `<select>`; tags cannot be browsed as a tree; blob and blame have no ref switcher at all. | Filterable branch/tag dropdown (Branches/Tags tabs) shared by tree/blob/blame headers. |
| G13 | M | No per-segment breadcrumbs in tree (only a ".." button) or blob (path is one static span). | Split path, link each segment via `repoCodeRoute`. |
| G14 | M | Blob view: no line numbers, no `#L42` anchors or shift-click range permalinks. | Numbered anchor gutter; extend "Copy permalink" with the line fragment. |
| G15 | M | Every blob force-decoded as UTF-8 — images/binaries render as mojibake; markdown files have no rendered view. | Branch on type: `<img>` for images, `Markdown` preview toggle for `.md`, size fallback for binaries. |
| G16 | M | Commits list: flat and undated — no "Commits on {date}" grouping, no avatars, no copy-SHA, no per-row browse-tree. | Group by day; add `Avatar`, clipboard button, tree link. |
| G17 | M | Branches page is one flat list — no Default/Active/Stale sections, no ahead/behind, no dates/authors, no per-row New-PR. | Bucket by last-commit age; ahead/behind via compare endpoint. |
| G18 | M | Compare view has no "Create pull request" affordance — its main purpose on GitHub. | Button opening the PullsPage create flow pre-filled with base/head. |
| G19 | M | Blame: no "view blame prior to this change" (reblame from parent), no age heat-strip, no avatars. | Reblame link per hunk (needs parent-ref support — verify server) + heat indicator. |
| G20 | M | Wiki has no page revision history (API surface is list/get/put/delete only). | (server work) revisions endpoint, then a History view. |
| G21 | m | Commit/compare diffs are raw monochrome patches although `PRFilesView` already has a colored line renderer. | Reuse `parseDiffLines`/`diffLineStyle` for commit + compare diffs. |
| G22 | m | Tags list: no creation date, SHA not linked to commit, no link to the associated release. | Add date + links. |
| G23 | m | Releases index shows compact metadata rows — GitHub renders each release's notes, Latest/Pre-release chips, and source-code (zip/tar.gz) pseudo-assets inline. | Render body + chips per entry; append zipball/tarball assets. |
| G24 | m | Stargazers/watchers lists render unlinked plain-text logins. | `Link` + `Avatar` rows (forks list already links). |
| G25 | m | About sidebar lacks a Contributors section (`fetchRepoContributors` exists, used only by Insights). | Add avatar-row section. |

### 2.3 Issues, PRs, discussions

| # | Sev | Gap | Fix |
|---|---|---|---|
| G26 | M | New-issue flow is a title+body modal — no template chooser, no labels/assignees/milestone at creation. | Fetch `.github/ISSUE_TEMPLATE`, add a chooser + metadata pickers (GitHub uses a full-page form). |
| G27 | M | List search box discards free text — `parseQuery` keeps only `key:value` tokens, so typing words into the issues/PR filter does nothing. | Keep unmatched terms as a title/body filter or route to the search endpoint. |
| G28 | M | Issue header/sidebar missing Pin / Transfer / Convert-to-discussion / Delete issue (parity endpoints exist). | (server-backed) header overflow menu. |
| G29 | M | No Subscribe/Unsubscribe notifications section on issue/PR sidebar. | Wire the subscription endpoints. |
| G30 | M | Existing review threads never render inline in the Files-changed diff — they live only in a separate Conversation-tab section; Conversation stacks Timeline / Review comments / Reviews as three blocks with reviews duplicated, instead of one chronological stream. | Fetch review comments in `PRFilesView`, anchor thread cards under diff lines; merge reviews into the timeline by timestamp. |
| G31 | M | "Start a review" batch is React state — switching tabs or reloading silently discards drafted review comments (GitHub creates a server-side PENDING review). | Create a real PENDING review via the API (or persist drafts per-PR in sessionStorage as a stopgap). |
| G32 | M | After merge the UI navigates away; no Delete-branch / Restore-branch offer (GitHub stays on the PR). | Stay on PR; offer `deleteRef`. |
| G33 | M | Merge box: no editable merge/squash commit title+message, no auto-merge toggle. | Commit message fields passed to `mergePR`; auto-merge if server supports. |
| G34 | M | PR list Assignee/Milestone filters permanently empty and "most commented" sort a no-op — `prAccessors` hardcodes `[]`/`null`/`0`. | Populate accessors from the PR payload. |
| G35 | M | Discussions: no upvotes anywhere (list, detail, comments) and no pinned discussions. | (server-backed via GraphQL mutations) wire upvote + pin/unpin, votes-sort. |
| G36 | m | Closed-as-not-planned renders identically to closed-as-completed (same icon/color) though `state_reason` is settable. | Gray "skip" icon for `not_planned` in list + detail. |
| G37 | m | Lock conversation prompts for no reason; API wrapper already supports `lock_reason` (discussions hardcode `"RESOLVED"`). | Reason select. |
| G38 | m | No "Close with comment" — drafted comment is not posted when closing. | Post non-empty composer body before the state mutation. |
| G39 | m | Milestones view rows aren't links; no per-milestone issue list with progress bar. | Link rows to pre-filtered issues list. |
| G40 | m | Suggestion blocks render as plain code fences — no diff render, no "Commit suggestion". | Special-case ```suggestion fences; apply-mutation needs server verify. |
| G41 | m | Reviewers sidebar: no per-reviewer approved/changes-requested state icons, no re-request. | Join `pr-reviews` into the section. |
| G42 | m | PR Commits tab rows static: SHA unlinked, no per-commit status, no avatars. | Link to commit view + status icon. |
| G43 | m | Files tab lacks viewed-checkbox, whitespace toggle, split/unified view, per-file collapse. | Per-file collapse + viewed state first. |
| G44 | m | PR participants = author only (issues derive from comments). | Derive from timeline like the issue side. |
| G45 | m | Timeline fallback prints raw wire names ("head ref force pushed"), bare "committed" rows with no SHA/message. | Map `committed`, `head_ref_force_pushed`, `head_ref_deleted/restored` renderers. |
| G46 | m | Discussions: categories are pills not a sidebar; no search / answered filter / sort; detail page has no right sidebar. | Sidebar + filters on `fetchDiscussionsPage`. |
| G47 | m | Sidebar gear icons are decorative, not buttons (misleading affordance, a11y). | Make them open the pickers or remove. |

### 2.4 Global nav, dashboard, search, notifications

| # | Sev | Gap | Fix |
|---|---|---|---|
| G48 | B | Code search results dead-end: rows are plain spans (no link to the file) and no highlighted match fragments; user and commit results also unclickable. | Link rows (`html_url`/`accountRoute`/commit route already available); request + render `text_matches` (verify server backing). |
| G49 | M | No `s` / `/` keyboard shortcut to focus search (GitHub's most-used shortcut); not in the `?` sheet. | Bind in `GlobalShortcuts`, advertise in the sheet. |
| G50 | M | Header Issues/PRs icons go to unscoped global search (`is:issue` over the whole instance) instead of "your" created/assigned/mentioned dashboards. | Minimum: append `author:{login}`; ideal: dedicated Your-issues/Your-PRs views with involvement tabs. |
| G51 | M | Notifications: no Saved (bookmark) view; Done is one-way with no Done tab to review. | (verify server thread-state backing) add Saved/Done views. |
| G52 | m | Search results tabs show no per-type counts (GitHub sidebar lists counts per type); "Issues & PRs" is one combined tab where GitHub splits them. | Lazy per-type counts; consider splitting tabs. |
| G53 | m | Dashboard: no "Find a repository…" filter over Top repositories (fixed 8, no show-more); no Latest-changes panel (N/A-ish self-hosted). | Client-side filter input + show-more. |
| G54 | m | Header search is a scope-`<select>` + input; GitHub is one input with suggestion overlay, repo-scoped when inside a repo. | Prefill `repo:owner/name` inside repos; qualifier-aware suggestions later. |
| G55 | m | Notifications default to a flat DataTable; GitHub groups by repository by default (grouping exists but is opt-in). | Default `groupByRepo` on, persist choice. |
| G56 | m | "+" create menu lacks New codespace. | Add MenuLink. |
| G57 | m | Owner/repo breadcrumb is a page-level bar rather than inside the global header row (GitHub 2023+ header). | Accept or lift into AppHeader via route matching. |

### 2.5 Profiles, orgs, social

| # | Sev | Gap | Fix |
|---|---|---|---|
| G58 | M | Repo rows/cards everywhere omit language dot, star/fork counts, "Forked from…" (fields already on the payload). | Extend RepoRow/ProfileRepoRow/PinnedCard/RepoPreviewCard. |
| G59 | M | Profile Repositories tab: no Type/Language/Sort dropdowns; search filters only the loaded 30-item page (matches on later pages invisible). Same page-local-search caveat on repo-list/org-repos "Find a repository…". | Server-side query via `buildRepoListURL`; reuse RepoListPage's selects. |
| G60 | M | Gists have no permalink page — detail is modal state inside `/ui/gists` (close/reload loses it); list is an admin-style table; `.md` files not rendered, no highlighting. | Add `/ui/gists/:id` route; render files via `Markdown`/highlighter; card-style list. |
| G61 | M | Org home lacks org profile README and pinned repos; recent-repos grid is mislabeled as the repositories list. | Render `{org}/.github` README; org pin store or relabel "Recently updated". |
| G62 | M | Org People: owner-only controls (invite, change-role, remove, convert) render for every viewer — dead controls that 403; no Members/Outside-collaborators sub-tabs (collaborators live on the bleephub Governance tab). | Gate on viewer's org role via memberships endpoint; surface an Outside-collaborators sub-tab reusing Governance panels. |
| G63 | m | Followers/Following rows show only avatar+login (GitHub: name, bio, location, follow button). | Hydrate rows; reuse `FollowButton`. |
| G64 | m | Stars tab unpaginated (silently truncates >30) with no sort/filter. | Switch to `ghFetchPage`. |
| G65 | m | Teams list flat — no parent→child nesting or per-row member/repo counts. | Group by `parent`, expandable indent. |
| G66 | m | Org People: no 2FA filter (verify `?filter=2fa_disabled` server support); cards show user type instead of org role. | Role via memberships; add filter if backed. |
| G67 | m | Profile sidebar meta rows missing octicons (company/location/email); no Achievements section (needs server, defer). | Pass icons to `MetaRow`. |

### 2.6 Repo/org settings, security, admin

| # | Sev | Gap | Fix |
|---|---|---|---|
| G68 | M | Repository **rename** missing entirely from Settings → General; server PATCH fully supports it. | (server-backed) name field + Rename button, navigate to new URL. |
| G69 | M | No type-to-confirm on delete/archive (plain confirm) and **no confirmation at all on Transfer**. | `expectedText` prop on `confirmAction` ("type owner/repo to confirm"); wrap Transfer. |
| G70 | M | Webhooks cannot be edited after creation (repo + org) — only `{active}` is ever PATCHed. | Reuse the add form as an edit modal calling the existing update wrappers. |
| G71 | M | Branch protection only targets existing branches from a `<select>` — no patterns (`release/*`), no rule list. | Free-text pattern entry + protected-branches list (rulesets already do patterns). |
| G72 | M | "Discussions" feature toggle missing from Settings features (server supports `has_discussions`; DiscussionsPage exists) — discussions can never be enabled from the UI. | (server-backed) add checkbox. |
| G73 | M | Repo "Code security and analysis" has only 3 toggles — no dependency graph / Dependabot security updates / secret-scanning + push-protection / code-scanning setup entry points. | (server work — repo payload lacks `security_and_analysis`) coupled server+UI item. |
| G74 | M | Environment required reviewers display-only; only wait-timer editable. | (verify server accepts `reviewers` on PUT) extend EnvironmentDetail. |
| G75 | m | Merge-button options incomplete: `allow_update_branch`, squash/merge default commit title+message selects (all server-backed). | Add checkbox + two selects. |
| G76 | m | Repo-level `web_commit_signoff_required` missing (org-level exists; server-backed). | One checkbox. |
| G77 | m | Webhook events entered as comma-separated free text; org hook form has no secret field (`createOrgHook` doesn't accept one); no SSL-verification option. | Radio trio + event checkbox grid; add secret to org form + API wrapper. |
| G78 | m | Visibility change is a plain radio in the General form; GitHub puts it in the Danger Zone with typed confirmation. | Move + confirm. |
| G79 | m | Settings sidebar lacks Branches / Tags / Secrets-and-variables entries (reachable only via cards in General). | Add sidebar items. |
| G80 | m | Code-scanning alert rows omit file path/branch. | Render `most_recent_instance.location.path`. |
| G81 | m | Audit log: no `action:`/`actor:`/`created:` qualifier syntax or date range; CSV/JSON export covers only loaded rows; lives as a top-level operator page rather than under org settings. | Qualifier parsing + date pair; document placement. |
| G82 | m | Ruleset bypass actors entered as raw numeric IDs. | Resolve teams/roles into a picker. |
| G83 | m | Branch-protection editor missing: require-last-push-approval, restrict-dismissals, require-deployments, lock-branch (last-push-approval exists in rulesets). | Add toggles (verify server). |

### 2.7 Actions, packages, insights, settings-misc

| # | Sev | Gap | Fix |
|---|---|---|---|
| G84 | B | Runners page manages only `repos[0]` — every query/token/remove hard-scoped to whichever repo sorts first; other repos' runners unreachable. | Repo/org selector, or move runners into repo settings (GitHub's location). |
| G85 | M | Run rows: title is the workflow name (not `display_title`/commit message), no duration; no Actor filter in the filter bar. | Add `display_title` to type+server, `formatDuration`, `?actor=` filter. |
| G86 | M | Log viewer: no search, no timestamp toggle, no raw-logs/download-archive. | Search input over rendered lines; anchor to the logs endpoint. |
| G87 | M | Packages: no install instructions (`docker pull`/`npm install`…) and no package detail page (modal versions table only). | Installation block per ecosystem in the detail view. |
| G88 | M | Insights is one long scroll page — content coverage complete but no left sidebar / per-section routes (structural-parity dimension). | Sidebar with section routes (pattern exists in ActionsPage). |
| G89 | M | Pulse counts silently cap at 50 (first-page `items.length`); no period selector. | Count via search endpoints or Link-header totals. |
| G90 | M | Settings (account) tabs not URL-addressable — one `/ui/account` route with `useState`; refresh/back resets to Public profile. | `/ui/account/:tab` param. |
| G91 | M | "Password & authentication": 2FA is a bare boolean toggle (no TOTP/QR/recovery-code provisioning — misleading as a security affordance); no password change, no sessions list. | Relabel or build minimal TOTP flow; align with the shauth SSO model (if auth is delegated, remove the toggle). |
| G92 | M | No Account section: no username rename, no primary-email change, no account deletion. | "Set as primary" per verified email; Account tab if server supports rename/delete. |
| G93 | m | Workflow-dispatch branch is free text hardcoded "main" (no default-branch lookup, no branch select). | Seed from `default_branch`, render select. |
| G94 | m | Add-runner instructions omit download/`./run.sh` steps (config.sh line only). | Extend the CodeBlock. |
| G95 | m | Run detail: flat job list, no `needs`-based dependency graph. | Indent/annotate `needs` at minimum. |
| G96 | m | Deployments tab flat + always-visible operator create-forms (GitHub groups by environment, no create UI). | Group by environment; tuck forms behind an operator affordance. |
| G97 | m | Marketplace sidebar categories are dead anchors (no matching ids, no filtering). | Filter chips or remove. |
| G98 | m | Appearance lacks "Sync with system" (theme pinned after first explicit pick). | Third "system" value clearing the override. |
| G99 | m | SSH key list shows truncated raw key material — no SHA256 fingerprint, no last-used. | Compute fingerprint client-side; last-used needs server. |
| G100 | m | PAT expiry is a bare date input defaulting to no-expiration (GitHub: preset dropdown, 30d default, warning on none). | Preset select. |
| G101 | m | Authorized-applications revoke list lives on the `/ui/oauth` dev page — undiscoverable from Settings; sidebar also lacks Organizations/Repositories entries. | "Applications" tab reusing `AuthorizedApplications`; add nav links. |
| G102 | m | Notification settings are 4 global toggles (accepted simplification; extend only if server stores per-type prefs). | — |
| G103 | m | Security-advisory form lacks affected-products/CVSS/credits (verify store first); org migrations page requires hand-typing the org login. | Fields if backed; org select from user's orgs. |

**Accepted deviations (document, don't fix):** `/ui/repos/:owner/:repo` URL namespace (deliberate,
vs github.com's root-level `/:owner/:repo`); operator surfaces with no github.com analogue
(`/ui/operations/*`, MetricsPage, WorkflowsPage simulator, webhook-delivery stacked sections);
team discussions absent (GitHub retired them); billing/Copilot-billing parity.

## 3. Performance — measurements

Local M-series dev machine, release-built binary, seeded repo at two scales. Numbers are the
baseline the perf plan is written against, not SLOs.

**Backend (REST, warm, `c` = concurrent connections):**

| Endpoint | Scale | c=1 p50 | c=1 p95 | c=20 p50 | c=20 p95 | c=20 rps |
|---|---|---|---|---|---|---|
| repo object | — | 1.5 ms | 1.7 ms | 2.1 ms | 4.9 ms | 8,110 |
| issues list (per_page=50) | 120 issues | 2.7 ms | 3.6 ms | 7.4 ms | 14 ms | 2,446 |
| issues list (per_page=50) | 1,120 issues | 6.1 ms | 6.7 ms | 26.6 ms | 57.3 ms | 671 |
| issues list (per_page=100, state=all) | 1,120 issues | 6.9 ms | 7.8 ms | — | — | — |
| commits list (50) | 11 commits | 1.6 ms | 1.9 ms | 1.9 ms | 4.0 ms | 9,230 |
| tree recursive | small | 1.5 ms | 1.6 ms | 1.6 ms | 3.5 ms | 11,023 |
| PR files | 2 files | 1.5 ms | 1.6 ms | 1.5 ms | 2.7 ms | 11,912 |

Writes: 1,000 issue creations, 8-way parallel: **1.07 s wall ≈ 940 writes/s**.

**Frontend delivery (from the embedded server):**

- Eager JS on every first paint: `index` 163.8 kB + `vendor-react` 219.1 + `vendor-misc` 163.1 +
  `vendor-yaml` 96.2 + `vendor-tanstack` 85.6 + runtime ≈ **~730 kB raw (~215 kB if gzipped)**.
  `vendor-crypto` (libsodium, 432 kB) is correctly lazy. Full dist: 2.2 MB, 92 assets.
- **No compression**: assets and API responses are served identity even with
  `Accept-Encoding: gzip` (entry JS travels as 163,770 bytes).
- **No cache headers on hashed assets**: `/ui/assets/*-{hash}.js` carries no
  `Cache-Control`/`ETag`/`Last-Modified` — every visit re-downloads everything. (The emoji
  handler already sets `public, max-age=31536000, immutable`, so the precedent exists.)
- **No edge layer compensates**: the app is fronted by API Gateway (HTTP API → VPC link →
  ECS), which neither compresses nor caches; the CloudFront distribution in terraform serves
  only the static startup document (its `caching_disabled` policy is correct for that), so
  every asset byte comes from the origin, uncompressed, on every visit.
- API responses carry no `ETag` → TanStack Query refetches always pay full payloads (GitHub
  serves ETags and 304s).
- Local FCP 16–28 ms; per-navigation wall time dominated by API fan-out, not paint.

**Request fan-out per page (live count during walkthrough):** repo-insights **23** API calls,
PR detail **22**, issue detail **17**, repo home **16**, tree view **15**, commits **11**. At a
realistic 40–80 ms RTT this is the primary production page-latency driver.

**Rate-limit interplay (parity working as designed, but it now binds the first-party UI):**
core budget is 5,000 req/hr per credential — at 16–23 calls per page that is **~220–310 page
views/hour per user** before the UI starts seeing 403s; the search page burns the separate
30/min search budget (hit during testing). GitHub's own web UI does not spend API quota.

## 4. Performance — plan

| # | Priority | Work | Acceptance |
|---|---|---|---|
| P1 | **P0 — delivery** | Serve embedded assets precompressed (gzip at embed-build time; zstd optional) with `Content-Encoding` negotiation; add `Cache-Control: public, max-age=31536000, immutable` to hashed `/ui/assets/*`, `no-cache` + `ETag` on the shell; enable gzip for JSON API responses above ~1 kB. (The app sits behind API Gateway, which passes these headers through — the origin is the whole fix; the terraform CloudFront distribution only serves the startup document and stays as-is.) | Entry JS travels ≤ 40 kB wire; repeat visit re-downloads zero asset bytes. |
| P2 | **P0 — fan-out** | Per-page bootstrap endpoints under `/ui-data` (auto-authed, exempt from the `/api/v3` route gates) for the worst pages: repo-home, issue-detail, PR-detail, insights — one round trip returning what the page's queries need; keep TanStack keys, hydrate from the bundle. Add `ETag`/`If-None-Match` to hot REST GETs so refetches 304. | Repo home ≤ 4 requests, PR detail ≤ 6; e2e `waitForResponse` matchers updated in the same PR (grep vitest + Playwright per the API-path rule). |
| P3 | **P1 — quota** | Decide the first-party-UI rate-limit story: browser-session credentials get a separate (or waived) primary budget, or `/ui-data` reads bypass the REST budget. Also debounce the search page and surface the 403+`Retry-After` as a friendly "you're searching too fast" state instead of an error. | A user clicking through pages all day cannot 403; API parity budgets unchanged for tokens. |
| P4 | **P1 — bundle** | Move `vendor-yaml` (96 kB) out of the eager graph — it is only needed by Actions/workflow parsing; audit `vendor-misc` (163 kB) composition; entry chunk sits at its budget ceiling (163.8 kB), so the highlighter (G4) and any new deps must land in lazy page chunks only. | Eager JS ≤ ~630 kB raw / ~180 kB wire; bundle budget gate stays green. |
| P5 | **P2 — store scaling** | Issues/PR list endpoints scan O(total issues) per request (2.7 ms → 6.1 ms for 10×; extrapolates to ~0.5 s at 100k) and c=20 p95 hits 57 ms (store-lock contention: 4× the c=1 latency). Add per-repo sorted issue indexes for the hot list paths and take locked snapshots before serialization. Re-run this benchmark at 10k issues as the gate. | p50 ≤ 15 ms and c=20 p95 ≤ 60 ms at 10k issues. |
| P6 | **P2 — regression harness** | Commit the seed + benchmark + walkthrough scripts (from this session's scratchpad) under `scripts/perf/`; a manual `make perf` target printing the Section-3 table; optional CI smoke asserting the P1 headers exist. | One command reproduces Section 3. |

Not a problem (measured, no action): write throughput (~940/s), SPA shell latency, static-asset
serve latency, recursive tree, PR files, per-page FCP. Search-bench 403s were the parity search
limit, not a defect.

## 5. Suggested execution order

1. **Live bugs** G1, G2 (+G3 en route) — small, high-embarrassment, testable.
2. **P0 perf** P1 + P2 — biggest production-feel wins; P2's `/ui-data` endpoints are also the
   vehicle for several count-related gaps (G8, G89).
3. **Blockers** G48 (search dead-end), G84 (runners repos[0]).
4. **Cross-cutting UX** G4–G7 — one shared component each, then the per-page M items inherit them.
5. **Journey majors** by traffic: code browsing (G11–G19) → issues/PRs (G26–G35) → settings
   (G68–G74) → nav/search (G49–G51) → profiles/orgs (G58–G62) → Actions (G85–G89) → account (G90–G92).
6. **P1/P2 perf** P3–P6 alongside.
7. **Minors** opportunistically within whichever page a major touches (boy-scout rule).

Ledger discipline: as each G-item is picked up, file it in BUGS.md under WEB- (UI) or its
server family with a fresh ID, per the existing convention; items marked "(server work)" get the
server half verified before any UI control ships.
