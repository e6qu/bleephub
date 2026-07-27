# Bleephub bug ledger

512 findings from a full-surface audit: 120 blockers, 287 major, 105 minor. Every entry carries a
location and a one-sentence claim. Severity is `B` blocker, `M` major, `m` minor.
Status is `open` until the fix lands with a test.

IDs are for this ledger only — per project convention they never appear in
source or comments. The reasoning behind a fix belongs in its commit message.

---

## AUTH — authentication, authorization, secrets, git transport

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| AUTH-001 | B | gh_apps_perms.go:100-128 | `requirePerm` is a credential-shape gate, not an authorization gate: for a classic PAT or any browser session it evaluates no predicate and falls through to the handler | partial — resource check now enforced; classic-PAT scope enforcement still open |
| AUTH-002 | B | gh_apps_perms.go:113 | `token.Scopes` is stored, persisted and emitted as `X-OAuth-Scopes` but enforced nowhere — a PAT scoped `read:org` can delete organizations | open |
| AUTH-003 | B | auth.go:19-29, agents.go:33,180, broker.go:112 | The entire `/_apis/` runner protocol has no authentication; `ghHeadersMiddleware` does not even run on it | open |
| AUTH-004 | B | secrets_vars_inject.go:39 | Job messages carry every org/repo/environment secret in plaintext to any caller who can poll the broker | open |
| AUTH-005 | B | auth.go:267-282 | Runtime tokens are `{"alg":"none"}` with an empty signature and a one-year expiry | open |
| AUTH-006 | B | artifacts.go:1017-1059 | `repoForRuntimeToken` trusts the unverified `sub` claim; forging a token is base64 encoding | open |
| AUTH-007 | B | secrets.go:230-304 | Repository secret handlers never resolve the repo and never check admin; `PUT /repos/anything/at-all/actions/secrets/X` returns 201 | open |
| AUTH-008 | B | secrets.go:230, store_repos.go:145 | Case-variant paths write a shadow scope key the injector never reads: operator sets a production secret, gets 201, job runs without it | open |
| AUTH-009 | B | handle_mgmt.go:75-102 | `GET /internal/oauth/state` returns every live OAuth authorization code with its owning user ID to any authenticated caller — account takeover | fixed |
| AUTH-010 | B | handle_mgmt.go:20-27 | 7 of 23 `/internal/` routes omit `requireSiteAdmin`, including every private repo's metadata and unmasked job logs | fixed |
| AUTH-011 | B | identity.go:622-636, gh_oauth.go:488 | The CSRF token is set equal to the session cookie value and printed into the OAuth consent page HTML, defeating HttpOnly | open |
| AUTH-012 | B | git_http.go:292, git_ssh.go:170, gh_branch_protection.go:1225 | Branch protection is never consulted by either git transport; `branchProtectionForRepo` has no caller outside its own file | open |
| AUTH-013 | B | gh_oauth.go:568-599 | `redirect_uri` is taken verbatim and never compared to the app's registered callback — authorization-code interception and open redirect | open |
| AUTH-014 | B | gh_apps_store.go:481,492 | Client secrets compared with `!=`, reachable unauthenticated and unrate-limited — byte-at-a-time timing oracle | fixed |
| AUTH-015 | B | gh_middleware.go:172-175 | Every JWT verification failure is discarded and the request continues as anonymous; invalid `ghp_`/`gho_` tokens likewise | open |
| AUTH-016 | M | gh_oauth.go:165, identity.go:577 | `browserLoginUser` accepts any PAT as a password with no scope check, laundering a restricted credential into an unrestricted session | open |
| AUTH-017 | M | gh_apps_perms.go:135,217 | Public repo + GET bypasses scope, resource-owner and repository-selection checks — a PAT scoped to org A reads org B's Actions variable values | open |
| AUTH-018 | M | secrets.go:22, actions_store.go:84, persistence.go:38 | Secret plaintexts and the X25519 private key that opens them are written unencrypted into adjacent rows of the same table | open |
| AUTH-019 | M | store.go createTokenLocked | PATs are stored keyed *by* the raw token value; one DB read is total credential compromise | open |
| AUTH-020 | M | secrets_vars_inject.go:39, handle_mgmt.go:218 | No `::add-mask::` handling and no log scrubbing anywhere — a secret echoed by a job is readable by any authenticated user | open |
| AUTH-021 | M | identity.go:598, store.go:70 | External identities are keyed on the mutable provider username, not `(issuer, subject)`; `SiteAdmin` is overwritten by whichever provider logged in last | open |
| AUTH-022 | M | rbac.go:90-101 | Any active org member gets push to every repo in the org regardless of role, team or base permission | open |
| AUTH-023 | M | git_http.go:98-229 | Nonexistent repo returns 404, existing-but-unauthorized returns 401 — a private-repository existence oracle | open |
| AUTH-024 | M | git_http.go:337, git_ssh.go:231 | Receive-pack errors matched by `strings.Contains(err, "EOF")`, then post-push hooks and workflow triggers fire for refs never written | open |
| AUTH-025 | M | git_ssh.go:52-71 | SSH listener spawns an unbounded goroutine per connection with no handshake deadline and no cap | open |
| AUTH-026 | M | ssh-gateway/main.go:60-95 | Read deadline cleared after the banner check so an idle connection holds one of 32 slots forever; limiter map never evicted | open |
| AUTH-027 | M | identity.go:577, gh_middleware.go:249 | No rate limiting on any auth endpoint; the `X-RateLimit-*` headers are hardcoded constants | open |
| AUTH-028 | M | handle_mgmt.go:410, identity.go:240 | Logins accepted with no allowlist, no case folding, no NFKC and no confusable check — `Alice`, `alice` and Cyrillic `аlice` are three accounts | open |
| AUTH-029 | M | auth.go:22,34-62 | Runner registration is unauthenticated and mints a "management token" that is never stored or checked | open |
| AUTH-030 | m | identity.go:459 | The OAuth client secret is reused as the HMAC key for the state cookie | open |
| AUTH-031 | m | identity.go:642,690 | Logout Origin check is conditional on Shauth being configured, and `/ui/signed-out` destroys the session on a plain GET | open |
| AUTH-032 | m | identity.go:635 | `Secure` inferred from a string prefix, copy-pasted at five sites, no `__Host-` prefix | open |
| AUTH-033 | m | gh_oauth.go:538 | CSRF token compared with `!=` while `identity.go:446` correctly uses `subtle.ConstantTimeCompare` | fixed |
| AUTH-034 | m | git_ssh.go:74-96 | `mustAtoi` cannot fail; discards both `Sscanf` returns and yields 0 — a latent fail-open in the SSH auth path | open |
| AUTH-035 | m | store.go:3222 | SSH key lookup re-parses every registered key per attempt under RLock; unparseable keys silently skipped; no `MaxAuthTries` | open |
| AUTH-036 | m | secrets.go:295 | Repo secret delete discards the existence result, returns 204 for a secret that never existed, and writes a forged audit event | open |
| AUTH-037 | m | identity.go:250 | Login never invalidates the presented session; no rotation on privilege change, so a demoted admin keeps admin for 12h | open |
| AUTH-038 | m | identity.go:759 | Front-channel logout page sets `frame-ancestors *` on a GET with a session-destroying side effect | open |
| AUTH-039 | m | identity.go:235-244 | Five-line comment defending a one-line assignment, arguing which provider string becomes the account key instead of not using one | open |
| AUTH-040 | M | persistence.go:290, dqlite-node/main.go:108 | dqlite inter-node transport has no authentication and no TLS, contradicting the "keeps the wire protocol private" claim | open |

## REST — the `/api/v3` surface

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| REST-001 | B | gh_repos_objects.go:23-26,572 | Private repo contents, commits and README readable by an anonymous caller; `lookupReadableRepoFromPath` exists and is not used | fixed |
| REST-002 | B | gh_repos_refs.go:16-20,236,287 | Branch, tag and ref listings for private repos likewise anonymous | fixed |
| REST-003 | B | gh_labels_rest.go:127-477 | Labels and milestones for private repos anonymous — roadmap and security leakage | fixed |
| REST-004 | B | gh_branch_protection.go:243 | All 14 protection GETs anonymous | fixed |
| REST-005 | B | gh_actions_extras.go:104, gh_workflows_rest.go:92 | Private build logs, artifact bytes and workflow inventory anonymous | fixed |
| REST-006 | B | gh_checks_rest.go:229 | Private CI results anonymous | fixed |
| REST-007 | B | gh_import.go:81 | Commit-author emails and large-file paths anonymous | fixed |
| REST-008 | B | gh_repos_git.go:288-678 | Any logged-in user can force-push or delete `refs/heads/main` on any repository | open |
| REST-009 | B | gh_hooks_rest.go:69-82 | Any authenticated user installs a webhook on any private repo and exfiltrates every subsequent payload | open |
| REST-010 | B | gh_branch_protection.go:141 | All 41 branch-protection routes unauthorized — privilege escalation against the merge gate in the same file | open |
| REST-011 | B | gh_dependabot.go:22,242,259 | 13 Dependabot secret handlers with no collaborator or admin check | open |
| REST-012 | B | gh_apps_rest.go:916-973 | Installation repository allowlist mutable by any user on an arbitrary installation ID; `user` is resolved and never used | open |
| REST-013 | B | gh_actions_permissions.go:257-263 | Map write performed under a read lock — `fatal error: concurrent map writes`, unrecoverable | fixed |
| REST-014 | B | gh_notifications.go:89, gh_rulesets.go:135, gh_migrations.go:414, gh_apps_oauth_mgmt.go:323 | Eight unsynchronized reads of maps written under the write lock — `fatal error: concurrent map read and map write` | partial — 9 verified sites converted; 6 more sit inside a held lock and need per-site work |
| REST-015 | B | gh_codespaces.go:139, gh_gists_rest.go:436, +9 more | Live store pointers rendered outside the lock; `populateGistURLs` writes a request-derived base URL into the shared object on every GET | open |
| REST-016 | B | gh_search.go:94-149 | Every unrecognized search qualifier is silently discarded, returning the unfiltered universe with `incomplete_results: false` | open |
| REST-017 | B | gh_search.go:189,339 | Search results gathered by ranging a Go map and never ordered before slicing — pages overlap and omit nondeterministically | open |
| REST-018 | B | gh_checks_rest.go:144-306 | Check runs readable and PATCHable across tenants by integer guess; the sibling rerequest handler checks correctly | open |
| REST-019 | B | gh_deployments.go:564 | Any deployment status readable through any repo path | open |
| REST-020 | B | gh_pr_comments.go:484-541 | Any PR review comment readable, editable and deletable; the delete handler never resolves the repo | open |
| REST-021 | B | gh_issue_moderation.go:25-83 | Any issue comment rewritable or deletable; `editorID` is recorded but not enforced | open |
| REST-022 | B | gh_repos_objects.go:273-289 | `PUT /contents` decodes `sha` and never reads it — every write is an unconditional overwrite, last writer silently wins | open |
| REST-023 | B | gh_import.go:201-234 | Source import fetches an arbitrary URL synchronously in the request goroutine with no scheme allowlist, no IP filtering and no timeout | open |
| REST-024 | B | gh_codespaces.go:24,149 | `GET /users/{username}/codespaces` is invented, unauthenticated, and enumerates another user's codespaces | open |
| REST-025 | B | gh_gists_rest.go:139-365 | Six gist sub-resource handlers skip the visibility check; `GET /gists/{id}/{sha}` returns full secret-gist contents to anonymous callers | open |
| REST-026 | B | gh_search.go:429, gh_rest.go:173 | `userToJSON` has no nil guard and is called with a deleted user — confirmed nil dereference | open |
| REST-027 | B | gh_projects_classic.go:659-681 | `col` guarded on one line and dereferenced twice on the next | open |
| REST-028 | B | gh_campaigns.go:244, gh_network_configurations.go:173, gh_code_security_configurations.go:733 | Mutators return nil on concurrent delete and the caller dereferences unconditionally | open |
| REST-029 | B | gh_issues_rest.go:130,189 | Pull requests are invisible in the issues list and in `GET /issues/{number}`; `issueToJSON` has no `pull_request` key | open |
| REST-030 | B | gh_request_decode.go:85 | `&json.UnmarshalTypeError{Type: nil}` — confirmed by execution to panic the moment anything formats it | fixed |
| REST-031 | M | whole lane | One `ETag` in 117 files and it is never honored; no `If-None-Match`, no 304, no `X-Poll-Interval`, no `X-GitHub-Media-Type` | open |
| REST-032 | M | gh_middleware.go:234, gh_api_insights.go:111 | `ghResponseWriter` erases `Flusher`/`Hijacker`, so the downstream `Flush()` is a silent no-op | open |
| REST-033 | M | gh_pagination.go:19-35 | `page=abc`, `page=0`, `per_page=-1` silently become defaults | open |
| REST-034 | M | gh_pagination.go:274 | `Link` header URLs are relative; GitHub emits absolute | open |
| REST-035 | M | gh_code_security_configurations.go:243 | Truncate-only pagination ignores `page` entirely — page 2 is unreachable | open |
| REST-036 | M | gh_enterprise_code_security.go:414 | `strconv.Atoi` on an opaque base64 cursor, error discarded — returns page 1 forever | open |
| REST-037 | M | gh_enterprise_dependabot.go:96 | Open-coded slicing drops the overflow guard — large `page` yields a negative index and a slice-bounds panic | open |
| REST-038 | M | ~60 handlers | List endpoints return whole collections with no pagination and no `Link` | open |
| REST-039 | M | gh_issues_rest.go:112-166 | `milestone`, `creator`, `mentioned`, `sort`, `direction`, `since` dropped; invalid `state` silently becomes OPEN; unknown assignee returns the unfiltered list | open |
| REST-040 | M | gh_repos_objects.go:41-93 | Commit list drops six filters and hard-stops at 30 first-parent commits, so `?page=2` is always empty | open |
| REST-041 | M | gh_checks_rest.go:234 | `filter` misread as a conclusion, so GitHub's documented default `?filter=latest` returns an empty list | open |
| REST-042 | M | gh_secret_scanning.go:190, gh_dependabot.go:622 | Org alert endpoints drop every filter while the repo siblings parse them | open |
| REST-043 | M | gh_code_scanning.go:82,191,640 | `tool_guid` dropped in three places and hardcoded `nil` in the renderer | open |
| REST-044 | M | gh_rulesets.go | `r.URL.Query()` never called; `includes_parents` defaults true and is dropped | open |
| REST-045 | M | gh_issues_rest.go:440, gh_pr_comments.go:460, gh_commit_comments_rest.go:213 | `since` dropped on every comment list — the documented incremental-sync mechanism | open |
| REST-046 | M | gh_apps_oauth_mgmt.go:166-208 | Token-scoping endpoint decodes permissions and repositories and drops all three, returning an unnarrowed token with a 200 | open |
| REST-047 | M | gh_pulls_rest.go:1235 | `PUT /pulls/{n}/update-branch` is a pure no-op returning 202; the body is copied to `io.Discard` | open |
| REST-048 | M | gh_pulls_rest.go:896-919 | `team_reviewers` decoded, counted, never used — 201 with `requested_teams: []` | open |
| REST-049 | M | gh_codespaces.go:943 | `Location` declared and never passed; five more create fields absent entirely; unknown machine name silently no-ops | open |
| REST-050 | M | gh_migrations.go:634 | All six `exclude_*` flags stored, echoed and never consulted | open |
| REST-051 | M | gh_runner_groups.go:94 | `restricted_to_workflows` never decoded and the response hardcodes no-restriction | open |
| REST-052 | M | gh_custom_properties.go:41 | `values_editable_by` and `require_explicit_values` validated, stored, echoed, never enforced | open |
| REST-053 | M | gh_branch_protection.go:410 | `required_approving_review_count: 0` — a valid GitHub configuration — deletes the rule and returns 200 | open |
| REST-054 | M | gh_actions_permissions.go:1009 | `enabled` omitted (a required field) silently disables Actions for the repository | open |
| REST-055 | M | gh_branch_protection.go:1195 | An empty request body wipes every required status check with a 200 — a transport hiccup disables the merge gate | open |
| REST-056 | M | gh_branch_protection.go:104,720 | Restrictions bodies decoded as objects where GitHub sends string arrays; responses emit 3-field stubs | open |
| REST-057 | M | gh_branch_protection.go:596,619 | POST aliased to PUT destroys entries not resent; the three DELETE handlers never read the body and remove everything | open |
| REST-058 | M | gh_statuses_rest.go:89 | A typo'd commit-status state becomes `pending` forever, permanently blocking required-status-check merges | open |
| REST-059 | M | gh_pulls_rest.go:540 | `{"event":"APPROVED"}` silently produces an unsubmitted PENDING draft with a 200 | open |
| REST-060 | M | gh_notifications.go:30-129 | A malformed `last_read_at` falls back to `time.Now()` — a typo marks everything read | open |
| REST-061 | M | 30+ sites | `strconv.Atoi` errors discarded on path and query values | open |
| REST-062 | M | gh_enterprise_teams.go:219, gh_pulls_rest.go:550 | Bulk operations validate inside the mutation loop and partially commit before returning 422 | open |
| REST-063 | M | gh_repos_compare.go:299 | A missing git storer makes `compare` report two refs identical — a CI gate keyed on that passes on a broken server | open |
| REST-064 | M | gh_repos_refs.go:32,245 | Missing storage makes branch/tag/ref lists return `200 []`, indistinguishable from an empty repo | open |
| REST-065 | M | gh_dependency_graph.go:152 | A schema-invalid dependency snapshot is persisted and answered 201 | open |
| REST-066 | M | gh_enterprise_dependabot.go:22 | An authorization failure degrades into `200 []` so a non-owner concludes there are no security alerts | open |
| REST-067 | M | gh_pr_comments.go:656, gh_commit_comments_rest.go:381 | `author_association: "OWNER"` hardcoded on every review and commit comment | open |
| REST-068 | M | gh_search.go:742,860 | Results hard-capped at 1000 then reported with `incomplete_results: false` and a truncated `total_count`; `score` hardcoded 1.0 | open |
| REST-069 | M | gh_pulls_rest.go:1577 | `mergeable` is never null — UNKNOWN collapses to false, which clients read as "conflicting" and stop polling | open |
| REST-070 | M | gh_pulls_rest.go:1573 | `review_comments` counts reviews, not review comments | open |
| REST-071 | M | gh_issues_rest.go:1131 | `issueToJSON` missing `author_association`, `timeline_url`, `pull_request`, `draft`, `closed_by`, `sub_issues_summary`, `parent`, `type` | open |
| REST-072 | M | gh_branch_protection.go:17 | `omitempty` on rule members makes a disabled rule vanish from the response | open |
| REST-073 | M | gh_repos_objects.go:663 | Directory listings missing six members; symlinks and submodules typed `"dir"`; an empty directory serializes as JSON `null` | open |
| REST-074 | M | gh_repos_compare.go:158 | `commitToJSON` emits `{base}/repos/…`, which is not a registered route — every URL it hands out 404s | open |
| REST-075 | M | gh_hooks_rest.go:468 | Hook URLs built from `"http://" + r.Host`, plaintext behind TLS | open |
| REST-076 | M | 8 endpoints | Actions settings PUTs return 200+body where GitHub returns 204, while newer endpoints in the same file correctly 204 | open |
| REST-077 | M | gh_pat_web.go:313 | `||` short-circuit means nothing is written and net/http emits 200 with a zero-length body where 404 is correct | open |
| REST-078 | M | gh_teams_rest.go:717, gh_teams_legacy_rest.go:394 | Discarded `decodeJSONBody` result: the handler continues after a 400 was written, grants access, then writes 204 | open |
| REST-079 | M | gh_api_insights.go:77-275 | Every API request takes the full store write lock and appends a durable row with no cap or eviction | open |
| REST-080 | M | gh_apps_perms.go:327 | Permission decisions return on the first matching installation while ranging a map — nondeterministic authorization | open |
| REST-081 | M | gh_teams_rest.go:308 | `handleListChildTeams` resolves the user, nil-checks it, never uses it; secret teams enumerable | open |
| REST-082 | M | gh_orgs_rest.go:240, store_orgs.go:270 | `GET /users/{username}/orgs` is unauthenticated and ignores membership visibility — private org memberships leak | open |
| REST-083 | M | gh_repos_rest.go:1299 | Starred-repo listing applies neither `canReadRepo` nor the fine-grained PAT filter | open |
| REST-084 | M | gh_repos_rest.go:45-65 | `repoFromRequest` is the prologue for 15 handlers and checks nothing | open |
| REST-085 | M | gh_rulesets.go:11-14 | Repo ruleset create/update/delete carry no `requirePerm` at all | open |
| REST-086 | M | gh_network_configurations.go:59 | `POST /internal/orgs/{org}/network-settings` has no authentication of any kind | open |
| REST-087 | M | gh_reactions.go:335 | Reaction delete never compares the reaction's owner to the caller | open |
| REST-088 | M | gh_orgs_rest.go:281 | A pending, never-accepted invitee can create org repos; `MembersCanCreateRepositories` is never consulted | open |
| REST-089 | M | gh_workflows_rest.go:166 | Runs attributed to a workflow file by comparing the human-authored `name:` string — two files named `CI` cross-contaminate billing totals | open |
| REST-090 | M | gh_repos_objects.go:589 | `?ref=` hardcodes `NewBranchReferenceName`, so tags and SHAs 404; `resolveGitRef` handles all three and is used elsewhere | open |
| REST-091 | M | gh_repos_objects.go, gh_repos_commit_reads.go | `Accept: vnd.github.raw|html|object|diff|patch|sha` accepted and ignored; five Accept inspections in 117 files | open |
| REST-092 | M | gh_pr_comments.go:539 vs :632 | Reactions written and read under one parent-type key and deleted under another, orphaning rows forever | open |
| REST-093 | M | gh_repos_archive.go:128 | A mid-stream archive failure returns 200 with a truncated archive though the whole repo was already buffered | open |
| REST-094 | M | server.go:666 | `writeJSON` ignores the encode error after `WriteHeader` — the choke point every handler uses | open |
| REST-095 | M | gh_apps_oauth_mgmt.go:126 | A failed token revocation reports success | open |
| REST-096 | M | gh_enterprise_actions.go:20 | Enterprise settings never key on the path enterprise, conflating all tenants | open |
| REST-097 | M | gh_custom_properties.go:487 | The same slice header written into every repo's property map — N repos alias one array | open |
| REST-098 | M | server.go:69, gh_releases.go:464 | `{p1}/{p2}` dispatchers make ~a dozen real endpoints invisible to `RegisteredRoutes()`, defeating the mechanism's stated purpose | open |
| REST-099 | M | gh_releases.go:165 | Draft releases are not filtered from the list, exposing unpublished notes to any reader | open |
| REST-100 | M | gh_actions_permissions.go:269 | `SetLabels` assigns the same ID to every label because the ID generator scans a slice the loop never updates | open |
| REST-101 | M | gh_repos_archive.go:216 | `writeZip` materializes symlinks as regular files, so zipball and tarball of one ref differ | open |
| REST-102 | M | gh_repos_compare.go:898 | Merge commits attributed to a hardcoded `GitHub <noreply@github.com>` despite an authenticated user being available | open |
| REST-103 | M | gh_statuses_rest.go:210 | Combined status keys on the raw ref while statuses are stored by SHA — always an empty `success` | open |
| REST-104 | M | gh_statuses_rest.go:264 | No `status` webhook is ever emitted, so `on: status` workflows never fire | open |
| REST-105 | M | gh_teams_rest.go:276 | Team rename re-slugs with no collision check, shadowing an existing team | open |
| REST-106 | M | gh_org_hooks_rest.go:33 | Installation tokens have no membership row, so the whole org-hooks surface is closed to apps holding `organization_hooks:write` | open |
| REST-107 | M | gh_org_events.go:216 | `GET /orgs/{org}/events` unauthenticated and public-only; `/public_events` unregistered | open |
| REST-108 | M | gh_repos_rest.go:200 | `handleMergeUpstream` hard-resets the target ref rather than merging, in an if/else whose branches are byte-identical | open |
| REST-109 | m | gh_rest.go:116 | Rate-limit response is static and omits seven documented resource buckets | open |
| REST-110 | m | 125 sites | Only 5 `Location` headers against 125 `201 Created` responses | open |
| REST-111 | m | gh_org_pat_admin.go:42, gh_private_registries.go:26 | Live credentials carry marshalable JSON tags and are excluded from responses only by hand-written renderers | open |
| REST-112 | m | gh_secret_scanning.go:369, gh_dependabot.go:770 | ~95 lines of commented-out handlers including unreachable middleware | open |

## GQL — GraphQL

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| GQL-001 | B | gh_graphql.go:138-143 | `POST /api/graphql` serves anonymous requests; real GitHub 401s every unauthenticated request | fixed |
| GQL-002 | B | gh_repos_graphql.go:693, store_repos.go:1249 | `user(login:).repositories` has no visibility filter and each leaked node is a live handle into issues, PRs, discussions and releases | fixed |
| GQL-003 | B | gh_repos_graphql.go:901 + 22 siblings | No mutation in the lane calls any permission helper — any authenticated user can delete any repository | partial — deleteRepository now checks canAdminRepo; the other 22 mutations still open |
| GQL-004 | B | gh_discussions_graphql.go:934,959 | Two discussion answer mutations skip authentication entirely | open |
| GQL-005 | B | gh_pulls_graphql.go:1682 | `expectedHeadOid` — the `--match-head-commit` safety interlock — accepted and ignored | open |
| GQL-006 | B | gh_issues_graphql.go:1242 | `updateIssue` drops `labelIds`, `assigneeIds` and `milestoneId` and returns a success payload showing the unchanged issue | open |
| GQL-007 | B | gh_issues_graphql.go:1248 | `state` is an unvalidated string written straight into the store; `issueStateEnum` exists 700 lines above | open |
| GQL-008 | M | 4 connections | `orderBy` accepted and ignored, breaking `gh issue list --sort`, `gh pr list` and `gh repo list --sort` | open |
| GQL-009 | M | gh_repos_graphql.go:684 | `ownerAffiliations` accepted and never read | open |
| GQL-010 | M | gh_pulls_graphql.go:937 | `commits(last:1)` returns the *first* commit, so `gh` reads check status off the wrong commit on multi-commit PRs | open |
| GQL-011 | M | gh_pulls_graphql.go:842-1153 | Five more PR sub-connections accept Relay arguments and return the whole list | open |
| GQL-012 | M | gh_pulls_graphql.go:1488 | `search(last:)` returns the first N; `before` undeclared; the non-null `type` argument never read | open |
| GQL-013 | M | gh_pulls_graphql.go:2931 | `sort:` silently dropped while every other unhandled qualifier sets `impossible` | open |
| GQL-014 | M | gh_pulls_graphql.go:2928 | `review-requested:` hardcoded impossible on a false claim — the data is read 300 lines away | open |
| GQL-015 | M | gh_issues_graphql.go:583 | Four of five `IssueFilters` members ignored, so `filterBy:{states:[CLOSED]}` returns open issues | open |
| GQL-016 | M | gh_issues_graphql.go:761, gh_discussions_graphql.go:537 | An unresolvable filter value widens the result set to everything | open |
| GQL-017 | M | gh_issues_graphql.go:1025 | Unknown node IDs silently dropped from label/assignee/milestone updates while `issueTypeId` in the same resolver errors | open |
| GQL-018 | M | gh_repos_graphql.go:1174 | `decodeCursor` returns 0 on any malformed cursor — clients loop forever or silently truncate | open |
| GQL-019 | M | gh_repos_graphql.go:1170 | Cursors are bare list indices with no connection or entity identity, so inserts shift every subsequent page | open |
| GQL-020 | M | gh_pagination.go:106,134 | `first`+`last` silently ignores `last`; `first`+`before` silently ignores `before` | open |
| GQL-021 | M | gh_pagination.go:216 | Page-size bounds clamped where the comment itself says GitHub rejects | open |
| GQL-022 | M | gh_issues_graphql.go:1677 | Eager pre-pagination truncates at build time and then misreports `totalCount` | open |
| GQL-023 | M | gh_discussions_graphql.go:1101, gh_releases.go:110 | Release and reaction mint identical global IDs | open |
| GQL-024 | M | 9 scanners | Node IDs carry the database ID in plaintext and nothing parses them — every mutation walks an entire map under the global lock | open |
| GQL-025 | M | gh_graphql.go:165 | No depth or complexity limit with four type cycles, reachable unauthenticated | open |
| GQL-026 | M | gh_pulls_graphql.go:3136 | `search(first:1)` fully renders every issue and PR in the instance before discarding all but one | open |
| GQL-027 | M | gh_issues_graphql.go:1511 | Rendering one page of 100 issues walks the global comment map 100 times | open |
| GQL-028 | M | gh_issues_graphql.go:1630 | Thirteen unsynchronized package-level schema memos shared across server instances | open |
| GQL-029 | M | gh_issues_graphql.go:2484 | A live `*Store` smuggled through the resolver graph as an untyped map entry | open |
| GQL-030 | M | gh_discussions_graphql.go:885 | Nil dereference when a comment outlives its soft-deleted discussion | open |
| GQL-031 | M | gh_graphql.go:95, gh_orgs_graphql.go:103 | Three root fields return bare null where GitHub returns a `NOT_FOUND` error, contradicting the file's own header comment | open |
| GQL-032 | M | gh_graphql.go:80 | `viewer` resolves to null for anonymous callers; it is `User!` on GitHub | open |
| GQL-033 | M | gh_graphql.go:215 | Validation prepass discards its result, both branches return nil, and the document is parsed twice per request | open |
| GQL-034 | M | whole lane | Zero interfaces declared; no `Query.node`, `nodes` or `rateLimit` — the canonical Relay refetch path fails outright | open |
| GQL-035 | M | 11 types | Fabricated type names (`PRComment`, `IssueOrder2`, `PullRequestOrderDirection`) break fragments and generated clients | open |
| GQL-036 | M | 18 sites | `PageInfo` forked eighteen ways; two omit `hasPreviousPage`/`startCursor` though the data is computed | open |
| GQL-037 | M | gh_pulls_graphql.go:477 | `RequestedReviewer` union declares Bot and Team and unconditionally returns User | open |
| GQL-038 | M | 12 payloads | `clientMutationId` absent, so a client that sends and selects it gets a validation error | open |
| GQL-039 | M | gh_issues_graphql.go:1191 | `AddCommentPayload.subject` typed as Issue and nulled for PRs | open |
| GQL-040 | M | gh_issues_graphql.go:1877, gh_pulls_graphql.go:2517 | `authorAssociation` hardcoded OWNER | open |
| GQL-041 | M | gh_issues_graphql.go:1872 | Comment `url` is the empty string where GitHub returns `URI!` | open |
| GQL-042 | M | 9 sites | Only `Repository.url` honours the external URL; every other entity returns a relative path | open |
| GQL-043 | M | gh_pulls_graphql.go:2529 | `reviewDecision` derived from all reviews rather than the latest per author, so an approval never clears CHANGES_REQUESTED | open |
| GQL-044 | M | gh_issues_graphql.go:935 | `assignableUsers` returns every account on the instance on a false claim that the membership graph is unmodelled | open |
| GQL-045 | M | gh_pulls_graphql.go:1034 | `autoMergeRequest`, `mergeCommit`, `resolvedBy` and `isPinned` are unconditional nulls or falses contradicting stored state | open |
| GQL-046 | m | gh_repos_graphql.go:104 | `hasIssuesEnabled` registered twice; `AddFieldConfig` overwrites silently | open |
| GQL-047 | m | gh_repos_graphql.go:1008 | Input decode panics rather than returning an error with a path | open |
| GQL-048 | m | gh_pagination.go:163 | Two incompatible conventions for reading integer arguments across 14 sites | open |
| GQL-049 | m | gh_projects_v2_graphql.go:402 | Three unlock paths with no `defer` — a panic leaks the store read lock | open |
| GQL-050 | m | gh_discussions_graphql.go:1071 | `bodyHTML` is escaped plaintext in `<p>` tags; goldmark is already a dependency | open |
| GQL-051 | m | gh_graphql.go:174 | Full query text logged on every errored request | open |

## ACT — Actions engine, runners, webhooks

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| ACT-001 | B | artifacts.go:890 | `Content-Range` end is unbounded — a 1-byte body can allocate a terabyte | open |
| ACT-002 | B | artifacts.go:305,632, timeline.go:38 | Artifact and log endpoints unauthenticated with sequential IDs; omitting the run ID returns every artifact across every repository | open |
| ACT-003 | B | actions.go:52,180 | Unauthenticated action-tarball endpoint returns a gzip of any private repository at HEAD | open |
| ACT-004 | B | jobs.go:353-369 | Matrix expansion shares one env map across combinations, so every job gets the last combination's values — but only when the job declares `env:`, which the existing test does not | fixed |
| ACT-005 | B | jobs.go:341,374 | The `needs` rewrite depends on map iteration order and fails nondeterministically about half the time | fixed |
| ACT-006 | B | jobs.go:342 | `include:`-only matrices are silently dropped; `ExpandMatrix` handles them and is never reached | fixed |
| ACT-007 | B | webhooks.go:265 | Fork PRs never trigger any workflow; the whole fork-approval machinery is unreachable | open |
| ACT-008 | B | webhooks.go:260,465 | Workflow files are read from repository HEAD, not the triggering ref | open |
| ACT-009 | B | workflow_triggers.go:308 | A package-global regex cache written with no mutex from concurrent trigger evaluation — `fatal error: concurrent map writes` | open |
| ACT-010 | B | workflows.go:706, broker.go:154 | `LockedUntil` is written three times and never read; the pending message is removed before the response is flushed, so a dropped connection loses the job permanently | open |
| ACT-011 | B | webhooks.go:80,183,204 | Webhook delivery is a full SSRF: user-controlled URL, no IP validation, redirects followed, unbounded goroutines, no per-hook ordering | open |
| ACT-012 | M | workflows.go:402 | Concurrency admission is a TOCTOU across two lock acquisitions and ignores non-running holders | open |
| ACT-013 | M | workflows.go:417,1166 | `cancel-in-progress` does not cancel pending runs, and the queue releases oldest-first so a loaded group replays a stale backlog | open |
| ACT-014 | M | workflow.go:137-161 | `permissions:`, `defaults:`, `run-name:` and job-level `concurrency:` are accepted by the parser and discarded; GITHUB_TOKEN scope is fixed regardless | open |
| ACT-015 | M | workflow.go:103-112 | `steps.*.env` and `steps.*.shell` are parsed with zero read sites; `working-directory`, `continue-on-error` and `timeout-minutes` are not even fields | open |
| ACT-016 | M | jobs.go:368 | Matrix values round-trip through `map[string]string`, so `matrix.version == 3` is false where GitHub says true | open |
| ACT-017 | M | workflows_msg.go:127 | Internal `__matrix_*` keys leak into every matrix job's shell environment | open |
| ACT-018 | M | matrix.go:20 | `include` applied before `exclude`, reversing GitHub's documented order | fixed |
| ACT-019 | M | workflows.go:315 | Matrix group membership reverse-engineered from the job key's textual shape, so unrelated jobs named `build_1`/`build_2` share fail-fast | open |
| ACT-020 | M | workflows.go:80,323 | `max-parallel` stored per run rather than per matrix group, last writer wins | open |
| ACT-021 | M | workflows.go:834,1037 | `continue-on-error` does not prevent the run from failing — its entire documented purpose | fixed |
| ACT-022 | M | workflows.go:1230 | A completion arriving with no result is silently recorded as successful | open |
| ACT-023 | M | workflow_call.go:374,461 | Broken `with:`, output and secret templates are logged and skipped, so a called workflow runs with empty inputs or unauthenticated | open |
| ACT-024 | M | workflow_call.go:398 | `"yes"` coerces to false for a boolean input and `"12abc"` to 12 for a number; `choice` options unvalidated | open |
| ACT-025 | M | workflow_call.go:197 | A reusable workflow's own top-level `env:` is dropped | open |
| ACT-026 | M | webhooks.go:249 | ~40 `on:` events parse and never fire; only push, pull_request, repository_dispatch, release and schedule are ever dispatched | open |
| ACT-027 | M | webhooks.go:290 | `pull_request` runs report the head branch ref and SHA instead of the merge ref | open |
| ACT-028 | M | workflows_msg.go:601 | Step conditions omit GitHub's implicit `success() &&` guard, so steps run after an earlier failure | open |
| ACT-029 | M | expressions.go:93 | Status-function detection is a lowercased substring scan — `contains(msg,'always()')` is misread as a gate | open |
| ACT-030 | M | workflows.go:643,1276 | The timeout clock starts at queue time, and a timed-out job concludes `cancelled` where GitHub says `failure` | open |
| ACT-031 | M | broker.go:318, actions_events.go:62 | Cancellations and lifecycle webhooks are dropped when a buffer is full — a cancelled job keeps running while the API reports otherwise | open |
| ACT-032 | M | actions_events.go:27,100 | Event payloads rendered at drain time from live pointers: a `queued` event can carry `completed` state, and the reads race the engine | open |
| ACT-033 | M | webhooks_payloads.go:120 | `forced: false` asserted though it was computed one line earlier; `commits`, `head_commit`, `compare` and `pusher` empty, so `[skip ci]` silently misbehaves | open |
| ACT-034 | M | workflows.go:264 | `run_number` is the global run ID, so the first run of a new workflow reports a five-digit number | open |
| ACT-035 | M | broker.go:148,289 | A session with no agent object skips the busy check, satisfies hosted-runner labels, drains the queue and produces uncancellable jobs | open |
| ACT-036 | M | expressions.go:427 | Object and array filters (`.*`, `[*]`) are standard GitHub syntax and fail the job | open |
| ACT-037 | m | webhooks_store.go:26 | `insecure_ssl` accepted, persisted, echoed and never applied | open |
| ACT-038 | m | webhooks.go:79 | Hook fields read off a shared pointer outside the lock; the same race was recognised and fixed for one field only | open |
| ACT-039 | m | actions.go:65 | A malformed body produces a successful empty response, so the runner fails later with an opaque error | open |
| ACT-040 | m | actions.go:20,147 | Action tarball cache keyed by mutable ref, never invalidated, unbounded, holding full tarballs in memory | open |
| ACT-041 | m | timeline.go:307 | Timeline attachments read and discarded — `$GITHUB_STEP_SUMMARY` content never appears anywhere | open |
| ACT-042 | m | timeline.go:202, artifacts.go:548, secret_scanning_ingest.go:57 | Unbounded `io.ReadAll` on log, artifact and scanning paths; the 4 MiB log cap is applied after the full read | open |
| ACT-043 | m | workflows.go:1248 | `cancelTimeout` written without the store lock; watcher goroutines leak for runs that never resolve | open |
| ACT-044 | m | workflows.go:746, broker.go:169, artifacts.go:1028 | Linear scans over every job and workflow on hot paths, never garbage collected — completed job messages holding plaintext secrets are retained indefinitely | open |
| ACT-045 | m | workflows_msg.go:173 | Secret values inserted into a regex mask unescaped, so a secret with metacharacters is rejected and left unmasked | open |
| ACT-046 | m | matrix.go:135 | Matrix display names use sorted keys rather than declaration order, breaking required-status-check contexts | open |
| ACT-047 | m | matrix.go:110 | Include/exclude matching compares values via `fmt.Sprintf`, so `1` and `"1"` are equal | open |
| ACT-048 | m | actions_events.go:151,389 | Startup-failure runs emit no check suite and an empty conclusion; the merge gate's `startup_failure` branch is dead code | open |
| ACT-049 | m | workflow_schedule.go:35 | The dispatcher re-reads and re-parses every workflow file of every repository every minute; parse errors swallowed | open |
| ACT-050 | m | dependabot_ingest.go:148 | Any non-numeric version segment is silently treated as not vulnerable | open |
| ACT-051 | m | workflows_msg.go:280 | `runner.os`, `arch` and `name` are constants; the github context omits `workflow_ref` and `job_workflow_sha`, which are load-bearing for OIDC claims | open |
| ACT-052 | m | artifacts.go:966 | The cache download token — documented as the sole access control — is compared with `!=`; no eviction, and `LastAccessedAt` is faked | partial — constant-time compare landed; eviction and LastAccessedAt still open |

## STORE — persistence, storage, dqlite, S3

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| STORE-001 | B | persistence.go:71 | `PutBatch` is used at 4 call sites against 398 `MustPut` and 206 `MustDelete`; `DeleteRepo` alone issues 54 independent transactions | open |
| STORE-002 | B | persistence.go:400 | Mutators change memory before persisting and `log.Fatalf` on failure — `os.Exit(1)` from a handler with the lock held and the mutation half-applied | open |
| STORE-003 | B | s3fs.go:584 | `s3File.Lock()` returns nil; go-git relies on it for ref compare-and-set and `packed-refs` rewrites | open |
| STORE-004 | B | s3fs.go:356,146 | `Chroot` shares one active-file map across every repository keyed by chroot-relative paths — repo A's read returns repo B's bytes | open |
| STORE-005 | B | store_repos.go:336 | Rename re-registers the same path-bound storer under the new key, so all later git I/O targets the vanished prefix | open |
| STORE-006 | B | store_repos.go:310 | The rename aborts only on `EEXIST`, which POSIX `rename(2)` never returns; every real failure is ignored | open |
| STORE-007 | B | store_orgs.go:227, store.go:1147 | A partial cascade writes a permanent poison pill: the server refuses to boot forever after the next restart, with no repair path | open |
| STORE-008 | B | store.go:2752 | ~70 ID counters rebuilt as `max(surviving)+1`; for attestations and package files that ID is the S3 object key, so a new entity inherits deleted bytes | open |
| STORE-009 | B | store.go:930 | The in-memory graph is the source of truth with no read-through, invalidation or leader election, while the code assumes multiple replicas | open |
| STORE-010 | B | store.go:1554 | Boot performs a write pass that force-cancels every non-completed run — against a shared database one replica starting cancels another's live runs | open |
| STORE-011 | B | dqlite-node/main.go:108 | No auth, no TLS, hijack error discarded, and an unbuffered send from a handler started before the node exists | open |
| STORE-012 | B | persistence.go:39 | No schema version, no migration runner; rows are raw marshalled structs so a rename decodes to zero and a downgrade drops fields | open |
| STORE-013 | B | store_codespaces.go:182, timeline.go:210, store_repos.go:249 | The global store lock is held across `docker pull` (180s), S3 prefix operations (120s) and every Raft round-trip | open |
| STORE-014 | B | store_repos.go:368 | `DeleteRepo` destroys bytes before metadata with an early return at each of six steps and no compensation | open |
| STORE-015 | B | store_codespaces.go:270,686 | A workspace guard makes codespaces permanently undeletable, and `DeleteRepo` propagates the error — the repository can never be deleted | open |
| STORE-016 | M | all store files | No optimistic concurrency anywhere; the pattern exists on one type and was not generalized | open |
| STORE-017 | M | gh_repos_git.go:688 | Zero `CheckAndSetReference` calls against 20+ `SetReference` sites — concurrent pushes silently lose one another | open |
| STORE-018 | M | s3fs.go:211,369 | Rename is copy-then-delete with no rollback; a failed delete reports failure after the copy succeeded | open |
| STORE-019 | M | object_bytes.go:18 | The byte-store interface is `[]byte`-only in both directions, and no upload route has a size limit | open |
| STORE-020 | M | object_bytes.go:56, persistence.go:255 | No checksum on any S3 put or get; `synchronous(NORMAL)` means acknowledged writes can be lost with no stated contract | open |
| STORE-021 | M | store_copilot_spaces.go:93 + 9 more | Getters return live pointers that writers mutate in place; the `Stargazers` map case is a hard panic, and blind writes resurrect deleted rows | open |
| STORE-022 | M | store_projects_classic.go:34, store_rulesets.go:28 | `json:"-"` silently means "not durable": board ordering and ruleset version history are lost on reload | open |
| STORE-023 | M | store_notifications.go:554, store_repo_reads.go:86 | Whole unbounded collections stored as one row and rewritten on every mutation — including on every `git clone` | open |
| STORE-024 | M | persistence.go:436, gh_api_insights.go:271 | `List` is the only read primitive and materializes whole buckets; one durable row per API request ever served | open |
| STORE-025 | M | store.go:958, persistence.go:211 | Sessions are never reaped and every logout scans and decodes every historical session row inside a write transaction | open |
| STORE-026 | M | store_packages.go:304, store_code_scanning.go:883 | Blob writes have no compensation and reuse IDs, silently aliasing orphaned objects | open |
| STORE-027 | M | store_secret_scanning.go:228 + 3 | Bulk mutations validate inside the loop and commit a map-iteration-order subset before returning 422 | open |
| STORE-028 | M | store_orgs.go:247 + 4 | Delete paths enumerate secondary indexes, so soft-deleted packages orphan their S3 bytes and cancelled subscriptions leave access live | open |
| STORE-029 | M | store_workflow_files.go:51 | The primary key hashes the mutable repo name; rename re-persists under the old ID and the next registration duplicates the row | open |
| STORE-030 | M | store_workflow_files.go:180 | A discarded read error writes empty YAML over a good row, which the type's own comment says makes dispatch 422 forever | open |
| STORE-031 | M | store_codespaces.go:372 | The encrypted codespace secret value is never referenced and the endpoint returns 201 | open |
| STORE-032 | M | store_rulesets.go:269 | Three ruleset-suite endpoints discard their arguments and return nil | open |
| STORE-033 | M | store_secret_scanning.go:253 | `validateSecretScanningTransition` ends in an unconditional nil return, making its own state machine dead code | open |
| STORE-034 | M | store_teams_people.go:208, store_copilot.go:42 | GET endpoints take the exclusive lock and perform durable deletes; a persist failure during a read kills the server | open |
| STORE-035 | M | git_storage.go:27 | A transient S3 client failure is memoized for the process lifetime with no reset | open |
| STORE-036 | M | persistence.go:386 | Unbounded retry treats every error as "waiting for quorum", including a malformed address map that can never succeed | open |
| STORE-037 | M | s3fs.go:93 vs :190 | `Open` misses the `NotFound` shape, and go-git discards the read error — a transient S3 failure reads as "ref does not exist" and a live branch is overwritten | open |
| STORE-038 | M | s3fs.go:181,261 | The active buffer is invisible to `Stat` and `ReadDir`; a failed `Close` leaves a phantom file and leaks memory | open |
| STORE-039 | M | store_issues.go:126 | Every PR event is written first under the issue parent type, then rewritten — a crash in the window files it against an unrelated issue | open |
| STORE-040 | M | store_orgs.go:552 | `UpdateTeam` has no slug collision check, orphaning the shadowed team from every index-driven operation | open |
| STORE-041 | M | store_codespaces.go:175 | Container and workspace created before any durable record exists, with no startup reconciler | open |
| STORE-042 | M | store_packages.go:275 | Missing the "persistence implies object store" guard its sibling has, so metadata replicates while bytes stay on one node | open |
| STORE-043 | M | store_projects_v2.go:782 | Field update re-mints every option ID, dangling every item's stored value | open |
| STORE-044 | M | store_issues.go:718 + 3 | Caller slices and maps adopted by reference across the store boundary | open |
| STORE-045 | M | store_repos.go:1029 + 2 | In-place `s[:0]` filtering rewrites the backing array readers already hold | partial — fixed in gh_actions_permissions.go |
| STORE-046 | M | store_pulls.go:521 | Full scans where an index exists, giving path-dependent answers between sibling endpoints | open |
| STORE-047 | m | persistence.go:416 | One process-wide mutex guards every operation and covers marshalling, unlike `PutBatch` | open |
| STORE-048 | m | s3fs.go:426 | One delete per object with no paginator under a fixed wall-clock budget covering all pages | open |
| STORE-049 | m | s3fs.go:73 | `Chroot` enforces no boundary and ref names are unvalidated at the handler | open |
| STORE-050 | m | store_secret_scanning.go:586 | A literal `null` row written when the key was just deleted, reloading as a permanent tombstone | open |
| STORE-051 | m | store_rulesets.go:209 | Comment promises ID ordering; the code returns randomized map order under offset pagination | open |
| STORE-052 | m | store_repos.go:449 | Deletes against keys that can never exist, occupying the place real cleanup would go | open |
| STORE-053 | m | store_migrations.go:202 | The `*Locked` suffix carries two opposite meanings, risking self-deadlock on a non-reentrant mutex | open |
| STORE-054 | m | store.go:919 | `loadFromPersistence` mutates every map with no lock and opens git storage serially per repo | open |
| STORE-055 | m | dqlite-node/main.go:36 | Hardcoded listen port while the advertised address is configurable; fixed voter count | open |
| STORE-056 | m | persistence.go:269 | The address map is resolved once at construction — the very drift the package exists to handle | open |
| STORE-057 | m | store_code_scanning.go:832 | Unchecked slice index panics under the global write lock | open |
| STORE-058 | m | store.go:3358 | Panic wrappers around fallible methods whose error-returning variants already exist | open |
| STORE-059 | m | store_discussions.go:250 | Tombstones retained forever and scanned globally on every create | open |
| STORE-060 | m | store_copilot.go:226 | A pure read takes the write lock and inserts a never-persisted entry | open |

## CORE — server lifecycle, observability, configuration

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| CORE-001 | B | cmd/bleephub/main.go:51, server.go:518 | No signal handling anywhere in the server binary and `srv.Shutdown` is never called; as PID 1 the process dies instantly mid-request | open |
| CORE-002 | B | cmd/bleephub/main.go:33 | `defer obs.Shutdown` is unreachable because every exit path goes through `os.Exit`; telemetry is never flushed | open |
| CORE-003 | B | server.go:524 | No panic-recovery middleware; a panic yields no 500, no log, no span, and can leave the store mutex held | fixed |
| CORE-004 | B | server.go:549 | `BPH_TLS_CERT` without `BPH_TLS_KEY` silently serves plaintext HTTP on the port the operator believes is TLS | fixed |
| CORE-005 | B | workflow_schedule.go:21, git_ssh.go:52 + 10 | Fifteen-plus goroutines with no owner; the cron dispatcher is an unstoppable `for { time.Sleep }` and the SSH listener is never closed | open |
| CORE-006 | M | server.go:655 | The response wrapper implements neither `Flusher` nor `Hijacker`, so `Flush()` is a silent no-op server-wide | open |
| CORE-007 | M | otel.go:128 | Log records emitted with `context.Background()`, so logs and traces can never be correlated | open |
| CORE-008 | M | otel.go:103,87 | A log line that fails to unmarshal is reported as written and vanishes; runtime-metric startup failure discarded | open |
| CORE-009 | M | server.go:532 | No global request-body cap and no `MaxHeaderBytes`; one handler out of hundreds bounds its body | open |
| CORE-010 | M | server.go:535 | A paragraph-long comment justifies having no read or write deadline, leaving a slow-body Slowloris open | open |
| CORE-011 | M | ui_embed.go:1, ci.yml:23 | `-tags noui` on every test invocation means the entire SPA handler is never compiled or tested in CI | open |
| CORE-012 | M | ui_noembed.go:5, server.go:414 | Under `noui` the root redirect points at a `/ui/` nothing serves, producing a warning and a 404 for a route the server advertised | open |
| CORE-013 | M | ui_embed.go:12, Makefile:11 | 73 committed build artifacts with no CI check that they match a fresh build | open |
| CORE-014 | M | persistence.go:400 | `MustPut` calls `log.Fatalf` from inside a request handler; with one task and no minimum healthy percent that is a full outage on one transient write error | open |
| CORE-015 | M | persistence.go:386 | Unbounded startup retry before any listener exists and before signal handling, invisible to health checks | open |
| CORE-016 | M | server.go:445 | `/health` returns a hardcoded `ok`, touches no dependency, and leaks version and enterprise slug unauthenticated; no readiness endpoint exists | open |
| CORE-017 | M | workflows.go:809 | Job duration measured from the workflow's creation time, inflating every job in a multi-job run | fixed |
| CORE-018 | M | metrics.go:14 | Job durations ring-buffered under a mutex and read by nothing | fixed |
| CORE-019 | M | workflows.go:238 + 3 | Zero `RecordError`/`SetStatus` calls repo-wide, so traces cannot distinguish success from failure | open |
| CORE-020 | M | server.go:433 | Raw query strings logged at WARN on a server that implements OAuth — codes, client secrets and tokens reach logs and telemetry | fixed |
| CORE-021 | M | otel.go:41, terraform/main.tf:792 | No `OTEL_*` variable in the task definition, so the whole telemetry stack is inert in the shipped deployment | open |
| CORE-022 | M | otel.go:41 | Per-signal OTLP endpoint variables are honoured by the exporters but not by the enable gate | open |
| CORE-023 | M | main.go:23, server.go:122 | Invalid `--log-level` and `BLEEPHUB_MAX_WORKFLOWS` silently replaced with defaults; 40+ scattered `os.Getenv` calls across 12 files | open |
| CORE-024 | M | actions_test.go:276 | The test helper hand-builds a partial `*Server`, producing eight nil-guards in production code and leaving `NewServer` untested | open |
| CORE-025 | M | main.go:36, persistence.go:395 | ANSI console logging in production plus 29 stdlib `log` calls that bypass zerolog, the level filter and the telemetry bridge | partial — net/http ErrorLog now routed through zerolog; console-vs-JSON and the 29 stdlib log calls still open |
| CORE-026 | M | internal/emojigen/main.go:42 | A third-party tarball downloaded by mutable tag with no checksum, no pinned commit and no client timeout, then committed | open |
| CORE-027 | m | server.go:665 | `writeJSON` discards the encode error, shipping a truncated body with a success status | fixed |
| CORE-028 | m | server.go:530 | Every HTTP span is named `bleephub` with no route attribute | open |
| CORE-029 | m | metrics.go:76 | `ReadMemStats` stops the world while holding the metrics mutex | fixed |
| CORE-030 | m | metrics.go:9 | JSON tags on a struct that is never marshalled, implying a contract that does not exist | open |
| CORE-031 | m | emojiart.go:94 | Byte-index slicing on names that may be multibyte | open |
| CORE-032 | m | ui_embed.go:21 | `/ui/` bypasses the route registry, weakening the enumerable-surface invariant to an undocumented exception | open |

## WEB — frontend

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| WEB-001 | B | api.ts:279-386, OverviewPage.tsx:18 | Metrics computed client-side by walking every repo, every run page and every run's jobs, re-run every 3 seconds — one tab self-DoSes the server | open |
| WEB-002 | B | 12 pages | 19 state-changing controls, including security and destructive ones, have no error handling and never render an error | open |
| WEB-003 | B | components/ui.tsx:382 | The only dialog primitive, used at 40 sites, has no dialog role, no focus trap, no Escape and leaves the page tabbable | open |
| WEB-004 | B | App.tsx:70-111 | The app renders nothing until the session probe resolves, with no timeout; a network error is indistinguishable from signed-out | open |
| WEB-005 | B | tsconfig.e2e.json:2 | Extends a path outside the repository that does not exist, and nothing references the file — 967 lines of e2e TypeScript never type-checked | open |
| WEB-006 | B | vitest.config.ts:7 | The 13 core test suites never execute and are never type-checked; a further 11 e2e tests are unreachable from any config | open |
| WEB-007 | M | vite.config.ts:30 | The dev proxy omits `/auth` and `/settings`, so `bun run dev` cannot authenticate | open |
| WEB-008 | M | RepoSocialPage.tsx:96 | React state mutated inside a query function, so the 5-second refetch discards every page the user loaded | open |
| WEB-009 | M | DeploymentsPage.tsx:96 + 2 | "Load more" has no in-flight guard, so two clicks append the same page twice | open |
| WEB-010 | M | api.ts, ~64 sites | Not one fetch accepts or forwards an abort signal, and there is no request timeout anywhere | open |
| WEB-011 | M | api.ts:148, DiscussionsPage.tsx:490 | A bearer token in localStorage alongside three unsanitised HTML sinks, safe only via an untested server-side coupling | open |
| WEB-012 | M | api.ts | 3,552 lines with ten near-identical request helpers and 34 inline fetches, each subtly different | open |
| WEB-013 | M | api.ts:202, types.ts | Every response is cast, never validated, against 2,153 hand-written lines with no generator | open |
| WEB-014 | M | api.ts:172 | A 401 on any background poll triggers a full-page navigation from inside a fetch helper, discarding location and racing itself | open |
| WEB-015 | M | LoginPage.tsx:16 | Provider discovery fails open to `{}`, rendering credential UI the instance will reject | open |
| WEB-016 | M | ci.yml:25-39, knip.ts | Four quality scripts unwired, and the knip config structurally cannot report dead code in the core workspace | open |
| WEB-017 | M | web/core | 8 of ~35 exports consumed; ~1,500 lines built and type-checked every run and invisible to knip | open |
| WEB-018 | M | App.tsx:108 | `ToastProvider` wraps the app with zero consumers — a notification system that can never render | open |
| WEB-019 | M | tsconfig.base.json | Seven strict-adjacent flags off, and five `eslint-disable` comments suppressing a linter that is not installed | open |
| WEB-020 | M | AppHeader.tsx:43 | Menus declare `role="menu"` and implement none of the keyboard contract; sign-out lives inside one | open |
| WEB-021 | M | core/Modal.tsx:42 | A jsdom workaround shipped in production behind an empty catch, degrading the dialog to non-modal | open |
| WEB-022 | M | App.tsx:107 | One error boundary above the router, so any page crash replaces the whole app; no reset path and no unhandled-rejection handler | open |
| WEB-023 | M | ListControls.tsx:246 | Inline `outline: none` on the primary search field with no compensating focus ring | open |
| WEB-024 | M | api.ts:561 | 409 and 404 treated as success on installation lifecycle calls | open |
| WEB-025 | M | OAuthPage.tsx:26 | The popup return value is discarded and the UI reports success when the popup was blocked | open |
| WEB-026 | m | 11 pages | 29 form controls with no accessible name | open |
| WEB-027 | m | components/ui.tsx:439 | `FormLabel`'s id is optional, so 11 sites render a label pointing at nothing | open |
| WEB-028 | m | ui.tsx:322, StateToggle.tsx:26 | Selection conveyed only by colour and weight; no roles, no `aria-pressed`, and a missing button type | open |
| WEB-029 | m | 11 pages | 20 destructive actions gated on `window.confirm`, which returns false once the user suppresses dialogs | open |
| WEB-030 | m | GistsPage.tsx:461 | Index keys on editable, removable rows | open |
| WEB-031 | m | ActionsPage.tsx:540 | An uncleaned 1-second timer firing against an unmounted component | open |
| WEB-032 | m | RepoDetailPage.tsx:467 | Clipboard failure is invisible on the insecure origins this server serves by default | open |
| WEB-033 | m | OAuthPage.tsx:51 | Untyped JSON and a live bearer token rendered into a visible code block | open |
| WEB-034 | m | api.ts:2082 | Only the first GraphQL error is reported, thrown as a bare error so not-found detection can never match | open |
| WEB-035 | m | LoginPage.tsx:58 | A duplicated, weaker copy of the server's return-to validation | open |
| WEB-036 | m | Shell.tsx:82 | Build identity read from env vars the local build path never sets, printing "development" while `/health` has the truth | open |
| WEB-037 | m | web/README.md:38 | Documents a route that has no entry in the router | open |
| WEB-038 | m | Avatar.tsx:19 | No error fallback, no referrer policy and no lazy loading on remote avatars | open |
| WEB-039 | m | AppHeader.tsx:482 | Three form-encoded state-changing requests with no CSRF token | open |
| WEB-040 | m | vite.config.ts:8 | No bundle budget and the markdown stack lands in the catch-all chunk | open |
| WEB-041 | m | AppHeader.tsx:353 | A defensive default contradicting a non-optional declared type | open |
| WEB-042 | m | ClassroomPage.tsx | Seven components written as single lines of 1,000–2,100 characters | open |
| WEB-043 | m | api.ts | 228 of ~300 path interpolations skip encoding while 73 do not | open |
| WEB-044 | m | App.tsx:95 | `Suspense fallback={null}` blanks the app when a lazy chunk fails after a deploy | open |

## TEST — test-suite quality

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| TEST-001 | B | ci.yml:23 | `-race` appears nowhere in the repository, while five tests name the race detector as their assertion mechanism | open |
| TEST-002 | B | webhook_lastresp_race_test.go:13 | Zero assertions — the only such file of 210; cannot fail without the race detector | open |
| TEST-003 | B | dqlite_integration_test.go:1 | Its build tag appears exactly once in the repository: on its own first line; the harness it references does not exist | open |
| TEST-004 | B | sdk-tests, terraform/wake, test/terraform-sockerless | Three modules and ~2,676 lines of tests never executed by CI, any Makefile target, any script or any Dockerfile | open |
| TEST-005 | B | 28 targets | `-fuzz` is never invoked and the corpus covers 3 of 28 targets, so the route fuzzer is a 24-case table test | open |
| TEST-006 | B | fuzz_http_test.go:14 | The fixture is built once outside the fuzz closure, so the fuzzer will delete its own seed data and silently collapse coverage | open |
| TEST-007 | B | App.tsx:138-167 | Ten routed pages, including every security and admin page, have zero unit and zero e2e coverage | open |
| TEST-008 | B | bleephub_test.go:29 | 102 files share one mutable server; zero `t.Parallel()` calls across 212 files, and assertions have already been deleted to accommodate it | open |
| TEST-009 | B | actions_test.go:276 | A six-field hand-built server used by 60 files against a fourteen-field production constructor, through a middleware chain production never assembles | open |
| TEST-010 | M | fuzz_http_helpers_test.go:260 | The fuzzer varies authentication and then asserts only "not 500", so a 200 to an anonymous delete would pass | open |
| TEST-011 | M | bleephub_test.go:67 | The shared suite runs with persistence disabled, and no test creates state over HTTP, restarts, and reads it back over HTTP | open |
| TEST-012 | M | gh_issues_test.go:10 + 13 | Setup helpers discard the response status, so a failed setup surfaces as a misleading later failure or a passing empty test | open |
| TEST-013 | M | webhooks_test.go:304-909 | Sleep used as synchronization at 31 sites, ~35s of wall clock, while the correct poll-with-deadline pattern exists in the same file | open |
| TEST-014 | M | webhooks_test.go:160 | `t.Fatalf` called from an HTTP handler goroutine, where it cannot reliably fail the test and may hang | open |
| TEST-015 | M | ~1,640 sites | Unchecked type assertions against 97 comma-ok forms; one panic terminates the binary and discards 1,320 other results | open |
| TEST-016 | M | load_sustained_test.go:271 | Emits 1,500 events and asserts only that the count is non-zero | open |
| TEST-017 | M | stress_lockgraph_test.go:104 | Transport errors increment nothing, so the failure gate is false and the error is discarded | open |
| TEST-018 | M | ~20 helpers | Twenty near-duplicate request helpers, each wiring a different middleware subset and error policy | open |
| TEST-019 | M | webhooks_test.go:42 + 5 | The unit suite makes real outbound signed POSTs to `example.com` on every CI run | open |
| TEST-020 | M | openapi_shape_validator_test.go:50 | The validator's coverage counters are incremented and never read, so a detached observer would pass silently | open |
| TEST-021 | M | ui_embed.go:1 | The SPA handler is compiled out of every Go test run and has no test of any kind | open |
| TEST-022 | M | ci.yml, vitest configs | No coverage measurement in either language | open |
| TEST-023 | M | run-integration.sh:946 | The expected-conclusion parameter is never overridden at any of 11 call sites, so no workflow step is ever made to fail | open |
| TEST-024 | M | bleephub_ecs_test.go:80, Dockerfile:83 | Test dependencies cloned from a branch tip and downloaded without checksums | open |
| TEST-025 | M | README.md:301,443,14 | Three documented claims about the test architecture contradict the code | fixed |
| TEST-026 | M | bleephub.spec.ts:322 | Five e2e setup calls with fixed names and an unconditional catch, plus an empty-state assertion depending on file ordering | open |
| TEST-027 | M | core/e2e, e2e/shauth-sso.mjs | Orphaned e2e suites unreachable from any config or script | open |
| TEST-028 | M | hooks.test.tsx:56 | A test named for interval refetching has no fake timers and would pass if the interval were deleted | open |
| TEST-029 | M | 4 specs | Every write action on Discussions and Codespaces, both credential surfaces, and every OAuth error branch untested | open |
| TEST-030 | m | sdk-tests/repositories_test.go:79 | Post-delete checks accept any error, so a 500 passes a "want 404" assertion | open |
| TEST-031 | m | fuzz_emoji_test.go:54 | Control-byte inputs — the interesting cases — are silently dropped, though the correct builder exists | open |
| TEST-032 | m | stress_crud_test.go:119 | The stress seed is time-based, never logged and not overridable, so failures cannot be replayed | open |
| TEST-033 | m | 4 files | 24 bug IDs in test comments, violating the project rule that production code already follows | open |
| TEST-034 | m | auth_hardening_test.go:93 | Asserting "not 401" passes for 404 and 500 | open |
| TEST-035 | m | run-gh-test.sh:109, run-integration.sh:1417 | Three `|| true` on auth login, a sleep among poll loops, and a banner claiming 14 tests where 13 exist | open |

## PARITY — GitHub API fidelity

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| PAR-001 | B | two allowlist copies | The validator reads a relative path, so the documented root copy is dead and has already diverged; the only provenance anchors live in the dead file | open |
| PAR-002 | B | gh_codespaces.go:987 | Two invented members emitted on fourteen endpoints and deleted on two, with the code's own comments stating they are undocumented; 28 allowlist entries paper over them | open |
| PAR-003 | B | openapi_shape_validator_test.go:114 | Schema selection ranges a map, so validation is coin-flip nondeterministic on two operations | open |
| PAR-004 | B | sdk-tests/go.mod | The 18-file conformance suite — the strongest wire-fidelity evidence in the repo — never runs | open |
| PAR-005 | B | update-github-openapi.sh:9 | The spec every parity gate measures against is fetched from a moving branch with no commit, date or checksum, and nothing detects drift | open |
| PAR-006 | B | openapi_shape_validator_test.go:189 | An undocumented status code is never reported and silently disables body validation for that exchange | open |
| PAR-007 | B | gh_api_definition_test.go:88-130 | At least 13 route-allowlist entries are invented paths or wrong methods, each asserted with boilerplate and no citation | open |
| PAR-008 | M | gh_codespaces.go | An invented wildcard shadows three real GitHub endpoints, which now 404 | open |
| PAR-009 | M | openapi_shape_validator_test.go:346 | Null accepted for any member, so a non-nullable required field can be null | open |
| PAR-010 | M | openapi_shape_validator_test.go:414 | No enum, format or constraint checking of any kind | open |
| PAR-011 | M | openapi_shape_validator_test.go:45 | No coverage floor; zero observations would pass, and most test files route around the observer | open |
| PAR-012 | M | openapi_shape_validator_test.go:20 | Three stated invariants — citation, bug ID, only-shrinks — are enforced by nothing and are all currently false | open |
| PAR-013 | M | openapi_shape_validator_test.go:134 | Requests, parameters, headers, error responses, empty bodies and non-JSON responses are all unvalidated | open |
| PAR-014 | M | spec `/copilot-spaces*` | 28 real operations with no route — the only wholly unimplemented family | open |
| PAR-015 | M | README.md:444 | Claims one allowlist entry where there are 60, claims the list only shrinks, and links the dead copy | fixed |
| PAR-016 | M | README.md:443 | Claims paths cannot be invented, omitting an 80-entry escape hatch that is in active use | fixed |
| PAR-017 | M | BLEEPHUB_GITHUB_API_PARITY.md:22 | Claims the surface covers the description (measured 93.0%) and credits a test that admits inventions | open |
| PAR-018 | M | specs/simulator-surfaces/ | 19 cited handlers do not exist, and the status legend cannot express "not implemented" | open |
| PAR-019 | M | BLEEPHUB_GH_CLI.md:115,146 | Two cross-references point at spec content that does not exist | open |
| PAR-020 | M | BLEEPHUB_GH_CLI.md:62-79 | Five commands in the supported table have zero occurrences in the harness | open |
| PAR-021 | M | BLEEPHUB_GH_CLI.md:78 | `gh release download` documented as supported and as returning empty assets | open |
| PAR-022 | m | allowlist:37-50 | Twelve entries are provably unreachable and inflate the count | open |
| PAR-023 | m | Dockerfile.gh-test:6 | The compatibility harness installs an unpinned `gh` while the docs claim a verified version | open |
| PAR-024 | m | emoji assets | Two vendored artifacts with a licence note but no source, commit or checksum | open |
| PAR-025 | m | openapi_shape_validator_test.go:463 | An unreadable allowlist is indistinguishable from an empty one | open |
| PAR-026 | m | openapi_shape_validator_test.go:183 | Internal-URL detection mislabels the operation and false-positives on ordinary repository content | open |
| PAR-027 | m | spec:123, gh doc:142 | Both documents route the reader to tracking files that do not exist | open |
| PAR-028 | m | gh_codespaces.go:990 | Git status and idle timeout hardcoded, and `location: "local"` is not a value GitHub emits | open |

## CI — pipeline, release, deployment, hygiene

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| CI-001 | B | repo root | No LICENSE file, while every published image embeds a CC-BY-4.0 asset archive | open |
| CI-002 | B | publish-container.yml | No release workflow, no semver, no tag, no GitHub Release, no changelog — zero git tags exist | open |
| CI-003 | B | ci.yml:23,37 | Zero static analysis: no vet, lint, gofmt check, race, coverage, govulncheck, module verification or ESLint | partial — gofmt and go vet (both tag sets) now gate; golangci-lint, -race, coverage and govulncheck need real defects fixed first |
| CI-004 | B | scripts/*.sh | Four written quality gates referenced by nothing | open |
| CI-005 | B | scripts/dupl.sh:20 | Scans repo root at depth 1 where no Go files live — it would fail 100% of the time if wired, and has never detected a clone | open |
| CI-006 | B | ci.yml:113 | The sockerless checkout is unpinned while shauth beside it is pinned to a SHA | open |
| CI-007 | B | terraform/versions.tf | No state backend declared, and state contains the SSH host private key and admin token in cleartext, not gitignored | open |
| CI-008 | B | terraform/main.tf:890 | Zero minimum healthy percent, no container health check and no circuit breaker — every deploy is a hard outage with no rollback | open |
| CI-009 | B | terraform/main.tf:885 | `desired_count` has two writers and no `ignore_changes`, so applying while awake terminates the live service | open |
| CI-010 | B | terraform/main.tf:146,368 | Git bucket versioning suspended and no EFS backup policy — no restore path for either durable store | open |
| CI-011 | B | 5 fetches | Five network fetches into shipped images and vendored files with no checksum, including the runner tarball and the gh signing keyring | open |
| CI-012 | B | publish-container.yml:63 | Four public images with no SBOM, signing, provenance or vulnerability scanning | open |
| CI-013 | B | 3 modules | Three Go modules never built or tested, including the only test for the Lambda gating production traffic | fixed |
| CI-014 | B | dqlite-node/main.go:1 | The storage quorum binary is behind a build tag CI never enables and first compiles after merge | open |
| CI-015 | B | publish-container.yml:3 | The release Dockerfiles are never built on pull requests | open |
| CI-016 | M | publish-container.yml:11 | `cancel-in-progress` on a publishing workflow can orphan architecture tags with no manifest | open |
| CI-017 | M | ci.yml:3 | No concurrency group; superseded runs burn ~90 runner-minutes per force-push | fixed |
| CI-018 | M | both workflows | Every action pinned by mutable tag, one by floating major, in a workflow holding `packages: write` | open |
| CI-019 | M | ci.yml:16-116 | Checkout duplicated eight times and the Bun version hardcoded in four places | open |
| CI-020 | M | ci.yml:33 vs Dockerfile.release:1 | CI validates the UI with Bun 1.3.14; the shipped image builds it with 1.2.19 | open |
| CI-021 | M | .gitignore:8 | 73 generated files committed, rewritten by every build, with an in-tree comment asserting they are ignored | open |
| CI-022 | M | scripts/ | Three near-identical script pairs, used inconsistently, one pointing at a directory that does not exist | open |
| CI-023 | M | terraform/main.tf:516 | The wake Lambda can stop any ECS task in the account | open |
| CI-024 | M | terraform/main.tf:982 | The only alarm exists to turn the service off; no SNS, no error alarms, no access logs | open |
| CI-025 | M | scripts/jscpd.sh:11, ci.yml:82 | Unpinned tool fetches at gate time while a sibling tool is correctly pinned | open |
| CI-026 | M | ci.yml:3 | No scheduled run despite multiple externally-mutable inputs, so drift is misattributed to whoever opens the next PR | open |
| CI-027 | M | 6 Dockerfiles | Every base image referenced by mutable tag | open |
| CI-028 | M | Dockerfile.release:33 | The production runtime links against a development PPA with no version pin, and the Jekyll gem tree has no lock | open |
| CI-029 | M | Dockerfile.release:43 | The server image runs as root with no health check while the sibling runner image drops privileges correctly | open |
| CI-030 | M | .github/ | No dependency update automation for five ecosystems | fixed |
| CI-031 | M | README.md | 22 of 46 environment variables undocumented, including every dqlite coordinate and the SSH host key | open |
| CI-032 | M | gemoji_catalog.txt | A vendored dataset with no provenance metadata of any kind | open |
| CI-033 | M | publish-container.yml:190 | Unguarded deletes against four public packages on every merge; an empty keep-set selects everything | open |
| CI-034 | M | terraform/versions.tf | No provider block, so the deployment region and the region driving IAM ARNs can silently disagree | open |
| CI-035 | M | terraform/tests/ | Seven run blocks covering one input dimension of about 25 | open |
| CI-036 | M | terraform/main.tf:885 | No autoscaling and a single-writer ceiling that is documented nowhere | open |
| CI-037 | M | terraform/main.tf:166 | Versioning enabled with no expiration, a 24/7 NAT instance, no budget and no anomaly alarm | open |
| CI-038 | M | ci.yml:118 | Fork pull requests run a fork-built image with the host Docker socket mounted | open |
| CI-039 | M | ci.yml:84-118 | A third of CI cannot run without two other repositories being structurally intact | open |
| CI-040 | M | repo root | No CODEOWNERS, CONTRIBUTING, templates or documented required checks | open |
| CI-041 | m | Dockerfile.release:22 | No `-trimpath`, and two of three shipped binaries carry no version stamp | open |
| CI-042 | m | .gitignore | Missing state files, tfvars, coverage output, env files and OS junk | fixed |
| CI-043 | m | .terraform.lock.hcl | One platform hash while CI runs another, and a locked provider declared nowhere | open |
| CI-044 | m | terraform/versions.tf:2 | Version constraints admit releases CI never tests | open |
| CI-045 | m | terraform/*/Dockerfile | Two unreferenced, non-functional Dockerfiles | open |
| CI-046 | m | publish-container.yml:98 | A tool installed and never used, via the only floating-major action reference | open |
| CI-047 | m | publish-container.yml:7 | `packages: write` granted workflow-wide to jobs that touch no registry | open |
| CI-048 | m | ci.yml:3 | No path filtering, so a README change runs all seven jobs | open |
| CI-049 | m | terraform/main.tf:190 | Account-level public-access blocks disabled to expose one file, and two buckets have no block at all | open |
| CI-050 | m | terraform/main.tf:151 | AWS-managed keys only, with no rotation or revocation control | open |
| CI-051 | m | terraform/main.tf:296 | Public SSH ingress hardcoded with no restriction variable and no IPv6 rule | open |
| CI-052 | m | terraform/main.tf | No `prevent_destroy` on any durable resource | open |
| CI-053 | m | .dockerignore | Committed build output shipped into the build context, then overwritten | open |
| CI-054 | m | scripts/knip.sh:7 | The gate ignores the exit code and decides pass/fail by grepping free text | open |
| CI-055 | m | scripts/jscpd.sh:10 | Disables errexit and matches console wording that upstream is free to change | open |
| CI-056 | m | ci.yml:31-103 | Three jobs perform a cold dependency install while the Go jobs and the Dockerfile cache correctly | open |
| CI-057 | m | Makefile:15, ci.yml:23 | The server suite runs 437s against an 8m timeout — 9% headroom, and a slower runner turns it into a failure that reads as a hang | fixed |

## ARCH — recorded, deliberately not attempted on this branch

| ID | S | Location | Finding | Status |
|---|---|---|---|---|
| ARCH-001 | M | internal/server | A single flat package of 406 files and 181k lines. Splitting it is a real improvement but would conflict with every other change here and is an architectural decision to take on its own | deferred by decision |

## Refuted

Checked and found not to be defects. Recorded so they are not actioned later.

| Claim | Verdict |
|---|---|
| `gh_code_scanning.go:49` registers an unreachable route without the `/api/v3` prefix | Reachable and deliberate. `gh_middleware.go:82-86` activates the API middleware for the `/repos/` and `/code-scanning/` prefixes and cites the reason: the official CodeQL Action posts to the uploads host. Cited provenance for an uploads-host path |
| `math/rand` use is a security weakness | Not a defect. The only non-test use picks a zen quote. All 18 token, secret and ID generation sites use `crypto/rand`, and UUIDs are crypto-backed |
