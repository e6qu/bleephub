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
| Register and manage runners | `/ui/runners` |
| Read and act on notifications | `/ui/notifications` |
| Search across the instance | `/ui/search` |

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

The GitHub-compatible API is substantially larger than the browser product.
Endpoint-level parity and deliberate API-only automation surfaces continue to
be ratcheted by the OpenAPI shape/behavior suites; this inventory prevents an
API endpoint from being mistaken for a broken browser link or an undiscoverable
human journey.
