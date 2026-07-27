# Browser user-journey inventory

This is the acceptance inventory for Bleephub's browser product. A journey is
complete when it has a discoverable entry point, a durable URL where one is
useful, loading/empty/error feedback, and a server operation that preserves the
GitHub-compatible REST contract.

## Repository work

| Journey | Browser surface | Backing API |
| --- | --- | --- |
| Discover, create, filter, and open repositories | `/ui/`, `/ui/repos`, `/ui/orgs/:org/repos` | repositories REST API |
| Browse branches and directories | `/ui/repos/:owner/:repo` | contents, branches, and Git data APIs |
| Open a file at a ref | `/ui/repos/:owner/:repo/blob/:ref/*` | contents API |
| Browse commits and inspect a patch | `/ui/repos/:owner/:repo/commits`, `/commits/:sha` | commits API |
| Clone over HTTPS or SSH | repository **Code** menu | smart HTTP and configured SSH Git transports |
| Create and inspect releases | `/releases`, `/releases/new`, `/releases/:releaseId` | releases API |
| Star, watch, fork, and inspect each audience | repository header and `/stargazers`, `/watchers`, `/forks` | starring, watching, and forks APIs |
| Configure repository behavior and security | `/settings`, `/settings/branch-protection`, `/settings/secrets`, `/security/*` | repository, branches, Actions secrets, and security APIs |
| Inspect insights, deployments, hooks, packages, and classic projects | `/insights`, `/deployments`, `/hooks/:id/deliveries`, `/packages`, `/projects-classic` | corresponding GitHub REST APIs |

## Collaboration and automation

| Journey | Browser surface |
| --- | --- |
| List, create, filter, and inspect issues; manage labels and milestones | `/ui/repos/:owner/:repo/issues`, `/labels`, `/milestones` |
| List, create, review, and merge pull requests | `/ui/repos/:owner/:repo/pulls` and `/pulls/:number` |
| List, create, and participate in discussions | `/ui/repos/:owner/:repo/discussions` |
| Inspect workflows, dispatch them, and inspect/cancel/rerun runs | `/ui/workflows`, repository `/actions`, and `/actions/runs/:runId` |
| Read live/completed job logs and rendered step summaries | repository `/actions/runs/:runId` |
| Download or delete run artifacts | run detail and repository `/actions?view=artifacts` |
| Inspect cache usage and delete dependency caches | repository `/actions?view=caches` |
| Register and manage runners | `/ui/runners` |
| Read and act on notifications | `/ui/notifications` |
| Search across the instance | `/ui/search` |

Actions execution preserves the split GitHub exposes between a global run ID
and a per-workflow run number. Reruns keep both identities and increment the
attempt. Matrix values retain YAML scalar types; per-matrix `max-parallel`,
step environment/shell/working-directory/timeout/continue-on-error, implicit
success guards, job timeouts, concurrency replacement, artifacts, caches, logs,
and summaries all flow through the runner-compatible APIs. Finalized
artifact/cache metadata and identifier high-water marks use the durable
SQLite/dqlite store, while archive bytes use the configured Actions object
store.

## Git storage and Pages

| Journey | Browser surface and behavior |
| --- | --- |
| Clone, fetch, browse, commit through APIs, and push | repository **Code** menu and Git smart HTTP/SSH |
| Keep Git repositories available across restarts and replicas | filesystem storage or the configured S3-compatible object store |
| Rename or delete an object-backed repository | repository settings; every paginated object is copied/deleted in bounded batches |
| Enable branch-based Pages | repository **Settings → Pages**, with branch suggestions and `/` or `/docs` source |
| Configure a Pages site | **Settings → Pages** controls build type, source, visibility, custom domain, and HTTPS |
| Request and inspect legacy builds | **Settings → Pages** build history |
| Publish from a GitHub Actions workflow | `actions/upload-pages-artifact` followed by `actions/deploy-pages`; deployment status is visible in Pages settings |
| Serve or remove published content | Pages hostname/path routing backed by the configured object store; disable removes the publication |

Git storage retries transient S3 initialization failures instead of poisoning
the process. Prefix rename lists a stable snapshot before deleting the source,
and prefix deletion repeatedly drains the first page, avoiding continuation
tokens invalidated by mutation. Pages publication metadata is durable and its
content is stored with the same object-storage discipline as other binary
payloads.

## Classroom

| Journey | Browser surface and behavior |
| --- | --- |
| Create the organization required by a classroom | Inline in **New classroom** for site administrators, with `/ui/admin/orgs` as the full management surface |
| Create, open, rename, archive, restore, and delete a classroom | `/ui/classrooms` and `/ui/classrooms/:classroomId` |
| Import/export transition data | Classroom dashboard |
| Replace a roster | Classroom **Roster** dialog |
| Create, edit, close/reopen invitations for, and delete assignments | Classroom detail |
| Configure deadlines and autograding tests | Assignment create/edit dialogs |
| Accept individual and group invitations | `/a/:inviteCode` → `/ui/classrooms/accept/:inviteCode` |
| Generate and open the student's repository | Invitation acceptance hands off to the durable repository URL |

## Codespaces

| Journey | Browser surface and behavior |
| --- | --- |
| Create a user- or repository-scoped codespace | `/ui/codespaces` or repository `/codespaces` |
| List and inspect codespaces | `/ui/codespaces` and `/ui/codespaces/:codespaceName` |
| Start, stop, and delete | List and detail actions |
| Run on an installation without a Docker CLI | Creation falls back to a persisted workspace lifecycle instead of returning 500 |
| Use machines, secrets, export, publish, organization access, and administration automation | GitHub-compatible Codespaces REST API |

The workspace fallback is deliberately an internal storage detail. Public
responses remain within GitHub's Codespaces response schema.

## Identity, organizations, administration, and ecosystem

| Area | Browser surfaces |
| --- | --- |
| Sign-in, sign-out, account settings, and profiles | `/ui/login`, `/ui/account`, `/ui/users/:login`, `/ui/:login` |
| Organization overview, people, teams, repositories, policies, and Copilot policy | `/ui/orgs/:org`, `/people`, `/teams`, `/repos`, `/rulesets`, `/governance`, `/copilot` |
| Site administration | `/ui/admin`, `/users`, `/orgs`, `/teams`, `/enterprise`, `/audit-log` |
| GitHub Apps, OAuth, and Marketplace | `/ui/apps`, `/ui/apps/:publisher/marketplace`, `/ui/oauth`, `/ui/marketplace` |
| Gists, packages, and migrations | `/ui/gists`, `/ui/packages`, `/ui/migrations` |
| Operator status and metrics | `/ui/admin`, `/ui/metrics` |

## Dead ends found by the audit

| Missing or broken journey | Resolution |
| --- | --- |
| Repository files and commits were visible but not openable | Added durable file, commit-list, and commit-detail routes |
| The latest commit count and “more releases” controls discarded clicks | Replaced placeholder handlers with real routes |
| SSH clone was absent when the server transport was not configured | The SSH tab is always discoverable and explains the exact operator configuration when unavailable |
| Classroom creation had no usable name path and failed when no organization existed | Rebuilt the dialog and added contextual organization creation/recovery |
| Existing classrooms and assignments could not be fully managed | Added rename, archive/restore, delete, assignment edit/delete, invitation, deadline, and autograding controls |
| Codespace names did not lead anywhere and `web_url` targeted a missing route | Added the codespace detail/lifecycle route and corrected `web_url` |
| Codespace creation returned 500 when `docker` was not installed | Added a Docker-less persisted workspace fallback while retaining Docker when present |
| Development mode could not complete authentication or Classroom requests | Added `/auth`, `/settings`, `/classroom-data`, and `/a` proxy coverage |
| Paginated social/deployment lists could lose or duplicate pages | Moved social lists to query-owned infinite pagination and guarded deployment pagination |
| Lazy routes, clipboard denial, blocked OAuth popups, and broken avatars failed silently | Added visible loading/errors and resilient fallbacks |
| Artifact and cache bytes survived in object storage but their metadata and numeric IDs were process-local | Moved finalized metadata and atomic ID allocation into SQLite/dqlite, including restart, rename, and repository-delete handling |
| Repository Actions exposed runs but not repository-wide artifact or cache management | Added discoverable artifact download/delete and cache usage/list/delete views |
| Job summaries uploaded by runners were discarded | Persisted bounded summary attachments and render their Markdown on run detail |
| Workflow run numbers used the global run ID and reruns minted inconsistent identity | Added durable per-workflow numbering and preserved ID/number across attempts |
| Matrix typing, group limits, step execution options, implicit conditions, and timeout accounting diverged from GitHub runners | Preserved typed matrix context and wired the runner message and scheduler semantics end to end |
| Concurrency groups replayed stale pending runs and could admit simultaneous submissions | Serialized admission, cancelled superseded pending runs, and promoted only the newest pending run |
| Pages settings exposed only domain/HTTPS controls and offered an invalid manual build action for workflow sites | Added source/build/visibility configuration and build-type-aware controls |
| S3 repository rename/delete could skip keys while mutating paginated listings and issued one delete request per object | Snapshot-before-rename and bounded multi-object delete now cover every page |

The GitHub-compatible API is substantially larger than the browser product.
Endpoint-level parity and deliberate API-only automation surfaces continue to
be ratcheted by the OpenAPI shape/behavior suites; this inventory prevents an
API endpoint from being mistaken for a broken browser link or an undiscoverable
human journey.

## API fidelity regression gates

API coverage is enforced as a set equality, not inferred from a collection of
happy-path tests:

- Every registered `/api/v3` operation must exist in the pinned GitHub OpenAPI
  definition.
- Every operation in that definition must be registered directly or named in
  the exact dispatcher-operation inventory.
- Every registered operation must appear exactly once in the HTTP fuzz route
  inventory, and a reachability test proves the multi-byte selector can select
  every entry.
- Successful responses reached by the test suite are checked against the
  pinned OpenAPI schema.
- The official `go-github` SDK suite boots a real Bleephub server and now runs
  in CI rather than merely compiling.
- Semantics OpenAPI cannot express—qualifier grammar, filtering, ordering,
  validation, and derived label formats—are pinned as compatibility vectors in
  both server regression tests and the SDK suite.

The definition and route gates provide 100% operation-level coverage. They do
not pretend OpenAPI describes server semantics; any discovered dotcom
behavioral difference must become a focused compatibility vector so that exact
request/response behavior cannot regress silently.
