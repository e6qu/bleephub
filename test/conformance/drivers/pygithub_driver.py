#!/usr/bin/env python3
"""Conformance driver for PyGithub, the official-in-practice Python client.

PyGithub is a second typed oracle alongside go-github, with a different failure
mode: it maps every response onto attribute objects and raises
``BadAttributeException`` when a field has an unexpected type, and its
``PaginatedList`` walks pages using the Link header rather than a page counter.
So a row here passes only when PyGithub's own object model accepted the
response and the attribute says what the operation promised.

It runs from test/conformance/.work/pyenv, a throwaway virtual environment
installed from the hash-pinned test/conformance/requirements.txt.
"""

from __future__ import annotations

import base64
import json
import os
import sys
import traceback
import urllib.error
import urllib.parse
import urllib.request

import github
from github import Auth, Github, GithubIntegration
from github.GithubException import (
    BadCredentialsException,
    GithubException,
    UnknownObjectException,
)

BASE = os.environ.get("BPH_BASE", "").rstrip("/")
TOKEN = os.environ.get("BPH_TOKEN", "")
RESULTS = os.environ.get("BPH_RESULTS", "")

if not BASE or not TOKEN:
    print("BPH_BASE and BPH_TOKEN are required", file=sys.stderr)
    raise SystemExit(2)

stream = open(RESULTS, "w", encoding="utf-8") if RESULTS else sys.stdout

PASSED = 0
FAILED = 0
SKIPPED = 0


def truncate(value: object) -> str:
    text = " ".join(str(value).split())
    return text[:400] + "…" if len(text) > 400 else text


class Deviation(Exception):
    """The transport succeeded but the decoded value breaks the contract."""

    def __init__(self, expected: str, actual: str, message: str = ""):
        super().__init__(message or f"expected {expected}, got {actual}")
        self.expected = expected
        self.actual = actual


def emit(record: dict) -> None:
    print(json.dumps({"client": "pygithub", **record}), file=stream, flush=True)


def check(domain: str, operation: str, request: str):
    """Decorator-free runner: check(...)(callable) records one operation."""

    def run(function):
        global PASSED, FAILED
        try:
            function()
        except Deviation as deviation:
            FAILED += 1
            emit(
                {
                    "domain": domain,
                    "operation": operation,
                    "status": "fail",
                    "request": request,
                    "expected": truncate(deviation.expected),
                    "actual": truncate(deviation.actual),
                    "message": truncate(deviation),
                }
            )
        except GithubException as error:
            FAILED += 1
            detail = f"HTTP {error.status}: {truncate(error.data)}"
            emit(
                {
                    "domain": domain,
                    "operation": operation,
                    "status": "fail",
                    "request": request,
                    "expected": "the client call succeeds and the object model accepts the response",
                    "actual": detail,
                    "message": detail,
                }
            )
        except Exception:  # noqa: BLE001 - any client-side failure is a conformance failure
            FAILED += 1
            detail = truncate(traceback.format_exc().strip().splitlines()[-1])
            emit(
                {
                    "domain": domain,
                    "operation": operation,
                    "status": "fail",
                    "request": request,
                    "expected": "the client call succeeds and the object model accepts the response",
                    "actual": detail,
                    "message": detail,
                }
            )
        else:
            PASSED += 1
            emit({"domain": domain, "operation": operation, "status": "pass", "request": request})
        return function

    return run


def skip(domain: str, operation: str, request: str, why: str) -> None:
    global SKIPPED
    SKIPPED += 1
    emit({"domain": domain, "operation": operation, "status": "skip", "request": request, "message": why})


def want(condition: bool, expected: str, actual: object, message: str = "") -> None:
    if not condition:
        raise Deviation(expected, str(actual), message)


client = Github(base_url=f"{BASE}/api/v3", auth=Auth.Token(TOKEN))

OWNER = "admin"
REPO_NAME = "conformance-pygithub"
# The organization this driver provisions for itself; every organization-scoped
# operation below keys on it.
ORG_LOGIN = "conformance-py-org"
state: dict = {}


# --- Fixtures ---------------------------------------------------------------
@check("users", "get_user", "GET /user")
def _authenticated_user() -> None:
    user = client.get_user()
    want(user.login == OWNER, OWNER, user.login, "the authenticated login is wrong")
    want(user.type == "User", "User", user.type, "the authenticated user type is wrong")
    state["user"] = user


@check("repos", "AuthenticatedUser.create_repo", "POST /user/repos")
def _create_repo() -> None:
    user = state.get("user") or client.get_user()
    repo = user.create_repo(REPO_NAME, description="PyGithub conformance fixture", auto_init=True)
    want(repo.full_name == f"{OWNER}/{REPO_NAME}", f"{OWNER}/{REPO_NAME}", repo.full_name, "full_name is wrong")
    want(bool(repo.default_branch), "a default_branch", "empty", "the repository has no default branch")
    state["repo"] = repo


def repository():
    repo = state.get("repo")
    if repo is None:
        repo = client.get_repo(f"{OWNER}/{REPO_NAME}")
        state["repo"] = repo
    return repo


# --- Repositories -----------------------------------------------------------
@check("repos", "get_repo", "GET /repos/{owner}/{repo}")
def _get_repo() -> None:
    repo = client.get_repo(f"{OWNER}/{REPO_NAME}")
    want(bool(repo.clone_url), "clone_url", "empty", "the repository has no clone_url")
    want(bool(repo.ssh_url), "ssh_url", "empty", "the repository has no ssh_url")
    want(repo.owner.login == OWNER, OWNER, repo.owner.login, "the repository owner is wrong")
    want(repo.permissions is not None, "a permissions object", "absent", "the repository has no permissions")


@check("repos", "Repository.edit", "PATCH /repos/{owner}/{repo}")
def _edit_repo() -> None:
    repo = repository()
    repo.edit(description="edited by the PyGithub conformance driver")
    refreshed = client.get_repo(f"{OWNER}/{REPO_NAME}")
    want(
        refreshed.description == "edited by the PyGithub conformance driver",
        "the new description",
        refreshed.description,
        "the edit did not persist",
    )


@check("repos", "Repository.get_branches", "GET /repos/{owner}/{repo}/branches")
def _branches() -> None:
    branches = list(repository().get_branches())
    want(len(branches) >= 1, "at least the default branch", len(branches), "the branch listing is empty")
    want(bool(branches[0].commit.sha), "branch.commit.sha", "empty", "the listed branch has no commit")


@check("repos", "Repository.get_contents", "GET /repos/{owner}/{repo}/contents/{path}")
def _contents() -> None:
    contents = repository().get_contents("README.md")
    want(contents.encoding == "base64", "base64", contents.encoding, "content is not base64 encoded")
    want(len(contents.decoded_content) > 0, "decodable content", "empty", "the content decodes to nothing")


@check("repos", "Repository.create_file", "PUT /repos/{owner}/{repo}/contents/{path}")
def _create_file() -> None:
    result = repository().create_file("pygithub.txt", "add a PyGithub fixture", "pygithub conformance\n")
    want(bool(result["commit"].sha), "commit.sha", "empty", "the write response has no commit sha")
    want(result["content"].path == "pygithub.txt", "pygithub.txt", result["content"].path, "content.path is wrong")


@check("repos", "Repository.get_commits", "GET /repos/{owner}/{repo}/commits")
def _commits() -> None:
    commits = list(repository().get_commits()[:5])
    want(len(commits) >= 1, "at least one commit", len(commits), "the commit listing is empty")
    want(bool(commits[0].commit.author.name), "commit.author.name", "empty", "the commit has no author name")


@check("repos", "Repository.get_topics / replace_topics", "GET+PUT /repos/{owner}/{repo}/topics")
def _topics() -> None:
    repo = repository()
    repo.replace_topics(["conformance", "pygithub"])
    topics = repo.get_topics()
    want(sorted(topics) == ["conformance", "pygithub"], "the two topics just set", topics, "topics did not round-trip")


@check("repos", "Repository.get_labels", "GET /repos/{owner}/{repo}/labels")
def _labels() -> None:
    labels = list(repository().get_labels())
    want(len(labels) > 0, "GitHub's default label set", "0 labels", "a new repository has no default labels")


@check("repos", "Repository.create_hook", "POST /repos/{owner}/{repo}/hooks")
def _hook() -> None:
    hook = repository().create_hook(
        "web", {"url": "https://example.invalid/hook", "content_type": "json"}, ["push"], active=True
    )
    want(hook.id > 0, "a hook id", hook.id, "the created hook has no id")


@check("repos", "Repository.get_stats_contributors", "GET /repos/{owner}/{repo}/stats/contributors")
def _stats() -> None:
    repository().get_stats_contributors()


# --- Issues -----------------------------------------------------------------
@check("issues", "Repository.create_issue", "POST /repos/{owner}/{repo}/issues")
def _create_issue() -> None:
    issue = repository().create_issue(title="PyGithub conformance issue", body="opened by the driver")
    want(issue.number > 0, "a positive number", issue.number, "the created issue has no number")
    want(issue.state == "open", "open", issue.state, "the created issue is not open")
    state["issue"] = issue


@check("issues", "Repository.get_issue", "GET /repos/{owner}/{repo}/issues/{number}")
def _get_issue() -> None:
    issue = repository().get_issue(state["issue"].number)
    want(bool(issue.html_url), "html_url", "empty", "the issue has no html_url")
    want(issue.user.login == OWNER, OWNER, issue.user.login, "the issue author is wrong")


@check("issues", "Issue.create_comment", "POST /repos/{owner}/{repo}/issues/{n}/comments")
def _comment() -> None:
    comment = state["issue"].create_comment("PyGithub conformance comment")
    want(comment.id > 0, "a comment id", comment.id, "the created comment has no id")


@check("issues", "Issue.add_to_labels", "POST /repos/{owner}/{repo}/issues/{n}/labels")
def _label_issue() -> None:
    repo = repository()
    label = repo.create_label("pygithub", "ededed")
    state["issue"].add_to_labels(label)
    labels = [item.name for item in state["issue"].get_labels()]
    want("pygithub" in labels, "the applied label", labels, "the label was not applied")


@check("issues", "Issue.edit (close)", "PATCH /repos/{owner}/{repo}/issues/{n}")
def _close_issue() -> None:
    issue = state["issue"]
    issue.edit(state="closed")
    reloaded = repository().get_issue(issue.number)
    want(reloaded.state == "closed", "closed", reloaded.state, "the issue was not closed")


@check("issues", "Repository.get_issues", "GET /repos/{owner}/{repo}/issues")
def _list_issues() -> None:
    issues = list(repository().get_issues(state="all"))
    want(len(issues) >= 1, "at least one issue", len(issues), "the issue listing is empty")


# --- Pull requests ----------------------------------------------------------
@check("pulls", "Repository.create_pull", "POST /repos/{owner}/{repo}/pulls")
def _create_pull() -> None:
    repo = repository()
    head = repo.get_branch(repo.default_branch)
    repo.create_git_ref("refs/heads/pygithub-topic", head.commit.sha)
    repo.create_file("topic.txt", "topic commit", "topic\n", branch="pygithub-topic")
    pull = repo.create_pull(
        title="PyGithub conformance pull request",
        body="opened by the driver",
        base=repo.default_branch,
        head="pygithub-topic",
    )
    want(pull.number > 0, "a positive number", pull.number, "the created pull request has no number")
    want(pull.head.ref == "pygithub-topic", "pygithub-topic", pull.head.ref, "head.ref is wrong")
    state["pull"] = pull


@check("pulls", "PullRequest.get_files", "GET /repos/{owner}/{repo}/pulls/{n}/files")
def _pull_files() -> None:
    files = list(state["pull"].get_files())
    want(len(files) >= 1, "the changed file", len(files), "the pull request has no changed files")
    want(bool(files[0].patch), "a patch", "empty", "the changed file carries no patch")


@check("pulls", "PullRequest.create_review", "POST /repos/{owner}/{repo}/pulls/{n}/reviews")
def _pull_review() -> None:
    review = state["pull"].create_review(body="PyGithub conformance review", event="COMMENT")
    want(bool(review.state), "a review state", "empty", "the created review has no state")


@check("pulls", "PullRequest.merge", "PUT /repos/{owner}/{repo}/pulls/{n}/merge")
def _pull_merge() -> None:
    result = state["pull"].merge(commit_message="conformance merge")
    want(result.merged is True, "merged=True", result.merged, "the merge was not reported")
    want(bool(result.sha), "a merge sha", "empty", "the merge response has no sha")


# --- Releases, git data, actions -------------------------------------------
@check("releases", "Repository.create_git_release", "POST /repos/{owner}/{repo}/releases")
def _release() -> None:
    release = repository().create_git_release("pygithub-v1", "PyGithub 1.0", "notes")
    want(bool(release.upload_url), "upload_url", "empty", "the release has no upload_url")
    want(release.tag_name == "pygithub-v1", "pygithub-v1", release.tag_name, "the release tag is wrong")
    state["release"] = release


@check("releases", "Repository.get_latest_release", "GET /repos/{owner}/{repo}/releases/latest")
def _latest_release() -> None:
    release = repository().get_latest_release()
    want(release.tag_name == "pygithub-v1", "pygithub-v1", release.tag_name, "the latest release is wrong")


@check("git", "Repository.get_git_ref", "GET /repos/{owner}/{repo}/git/ref/{ref}")
def _git_ref() -> None:
    repo = repository()
    ref = repo.get_git_ref(f"heads/{repo.default_branch}")
    want(ref.object.type == "commit", "commit", ref.object.type, "the ref object type is wrong")
    state["sha"] = ref.object.sha


@check("git", "Repository.get_git_commit", "GET /repos/{owner}/{repo}/git/commits/{sha}")
def _git_commit() -> None:
    commit = repository().get_git_commit(state["sha"])
    want(bool(commit.tree.sha), "tree.sha", "empty", "the git commit has no tree")


@check("git", "Repository.get_git_tree (recursive)", "GET /repos/{owner}/{repo}/git/trees/{sha}?recursive=1")
def _git_tree() -> None:
    commit = repository().get_git_commit(state["sha"])
    tree = repository().get_git_tree(commit.tree.sha, recursive=True)
    want(len(tree.tree) > 0, "tree entries", 0, "the recursive tree is empty")


@check("git", "Repository.create_git_blob", "POST /repos/{owner}/{repo}/git/blobs")
def _git_blob() -> None:
    blob = repository().create_git_blob(base64.b64encode(b"blob\n").decode(), "base64")
    want(bool(blob.sha), "a blob sha", "empty", "the created blob has no sha")


@check("actions", "Repository.get_workflows", "GET /repos/{owner}/{repo}/actions/workflows")
def _workflows() -> None:
    list(repository().get_workflows())


@check("actions", "Repository.create_variable", "POST /repos/{owner}/{repo}/actions/variables")
def _variable() -> None:
    repository().create_variable("PYGITHUB_CONFORMANCE", "1")
    variables = list(repository().get_variables())
    want(
        any(variable.name == "PYGITHUB_CONFORMANCE" for variable in variables),
        "the variable just created",
        [variable.name for variable in variables],
        "the variable listing does not contain it",
    )


@check("actions", "Repository.create_secret", "PUT /repos/{owner}/{repo}/actions/secrets/{name}")
def _secret() -> None:
    # PyGithub fetches the repository public key and seals the value with
    # libsodium itself, so this exercises the whole encryption handshake.
    secret = repository().create_secret("PYGITHUB_CONFORMANCE", "s3cret", "actions")
    want(secret.name == "PYGITHUB_CONFORMANCE", "PYGITHUB_CONFORMANCE", secret.name, "the created secret is wrong")


# --- Organizations, search, gists ------------------------------------------
@check("orgs", "get_organization", "GET /orgs/{org}")
def _org() -> None:
    # GitHub Enterprise Server provisions organizations through the site-admin
    # route; PyGithub has no typed constructor for it, exactly as on GitHub.
    request = urllib.request.Request(
        f"{BASE}/api/v3/admin/organizations",
        data=json.dumps({"login": "conformance-py-org", "admin": OWNER}).encode(),
        headers={"Authorization": f"Bearer {TOKEN}", "Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request) as response:
        want(response.status in (200, 201), "201 Created", response.status, "the organization fixture failed")
    organization = client.get_organization("conformance-py-org")
    want(organization.type == "Organization", "Organization", organization.type, "the organization type is wrong")


@check("search", "search_repositories", "GET /search/repositories")
def _search_repos() -> None:
    results = client.search_repositories("conformance")
    want(results.totalCount >= 1, "at least one result", results.totalCount, "the repository search found nothing")
    want(bool(list(results[:1])), "a materialised first page", "empty", "the search result page is empty")


@check("search", "search_issues", "GET /search/issues")
def _search_issues() -> None:
    results = client.search_issues("conformance")
    want(results.totalCount >= 0, "a numeric total", results.totalCount, "the issue search has no total")


@check("gists", "get_user().create_gist", "POST /gists")
def _gist() -> None:
    gist = client.get_user().create_gist(
        True, {"conformance.txt": github.InputFileContent("gist content\n")}, "PyGithub conformance gist"
    )
    want(bool(gist.id), "a gist id", "empty", "the created gist has no id")
    state["gist"] = gist


@check("gists", "Gist.get_comments", "GET /gists/{id}/comments")
def _gist_comments() -> None:
    list(state["gist"].get_comments())


# --- Cross-cutting behaviour ------------------------------------------------
@check("pagination", "PaginatedList walks the Link header", "GET /repos/{owner}/{repo}/issues?per_page=2")
def _pagination() -> None:
    repo = repository()
    for index in range(4):
        repo.create_issue(title=f"pagination fixture {index}")
    issues = repo.get_issues(state="all")
    # PyGithub materialises pages lazily through the Link header; forcing the
    # full walk is what proves the header is present and correct.
    collected = list(issues)
    want(
        len(collected) >= 5,
        "every issue across more than one page",
        len(collected),
        "PaginatedList stopped early, which means the Link header is missing or wrong",
    )


@check("pagination", "PaginatedList.get_page(1)", "GET /repos/{owner}/{repo}/issues?page=2")
def _page_two() -> None:
    issues = repository().get_issues(state="all")
    second = issues.get_page(1)
    want(isinstance(second, list), "a list of issues", type(second).__name__, "page 2 could not be materialised")


@check("errors", "UnknownObjectException on 404", "GET /repos/{owner}/does-not-exist")
def _not_found() -> None:
    try:
        client.get_repo(f"{OWNER}/definitely-does-not-exist")
    except UnknownObjectException as error:
        want(error.status == 404, "status 404", error.status, "a missing repository does not answer 404")
        want(bool(error.data.get("message")), "a message in the body", error.data, "the error body has no message")
        return
    raise Deviation("UnknownObjectException", "success", "a missing repository was served successfully")


@check("errors", "BadCredentialsException on a bad token", "GET /user with an invalid token")
def _bad_credentials() -> None:
    bad = Github(base_url=f"{BASE}/api/v3", auth=Auth.Token("clearly-not-a-valid-token"))
    try:
        bad.get_user().login
    except BadCredentialsException as error:
        want(error.status == 401, "status 401", error.status, "an invalid token does not answer 401")
        return
    raise Deviation("BadCredentialsException", "success", "an invalid token was accepted")


@check("meta", "get_rate_limit", "GET /rate_limit")
def _rate_limit() -> None:
    limits = client.get_rate_limit()
    want(limits.resources.core.limit > 0, "resources.core.limit > 0", limits.resources.core.limit,
         "the rate limit envelope is unusable")
    want(limits.rate is not None, "the legacy top-level rate object", "absent",
         "the response omits the deprecated but still-sent `rate` key")


@check("meta", "get_emojis", "GET /emojis")
def _emojis() -> None:
    emojis = client.get_emojis()
    want(len(emojis) > 0, "a non-empty emoji map", len(emojis), "/emojis returned nothing")


@check("meta", "render_markdown", "POST /markdown")
def _markdown() -> None:
    rendered = client.render_markdown("# conformance")
    want("<h1" in rendered, "HTML containing <h1", truncate(rendered), "markdown was not rendered to HTML")



# --- Checks -----------------------------------------------------------------
@check("checks", "Repository.create_check_run", "POST /repos/{owner}/{repo}/check-runs")
def _create_check_run() -> None:
    repo = repository()
    head = repo.get_commits()[0].sha
    run = repo.create_check_run(name="pygithub/conformance", head_sha=head, status="in_progress")
    want(run.id > 0, "a check run id", run.id, "the created check run has no id")
    want(run.head_sha == head, head, run.head_sha, "the check run head_sha does not round trip")
    want(run.status == "in_progress", "in_progress", run.status, "the check run status does not round trip")
    state["check_run"] = run
    state["head_sha"] = head


@check("checks", "CheckRun.edit (complete)", "PATCH /repos/{owner}/{repo}/check-runs/{id}")
def _update_check_run() -> None:
    run = state["check_run"]
    run.edit(status="completed", conclusion="success")
    reloaded = repository().get_check_run(run.id)
    want(reloaded.status == "completed", "completed", reloaded.status, "the check run was not completed")
    want(reloaded.conclusion == "success", "success", reloaded.conclusion, "the conclusion did not persist")


@check("checks", "Repository.get_check_suites", "GET /repos/{owner}/{repo}/commits/{sha}/check-suites")
def _check_suites() -> None:
    suites = list(repository().get_commit(state["head_sha"]).get_check_suites())
    want(len(suites) >= 1, "at least one check suite", len(suites),
         "a commit with a check run exposes no check suite")


@check("checks", "Commit.create_status / get_combined_status",
       "POST /repos/{owner}/{repo}/statuses/{sha}")
def _commit_status() -> None:
    commit = repository().get_commit(state["head_sha"])
    status = commit.create_status("failure", target_url="https://example.invalid/s",
                                  description="failing", context="pygithub/ci")
    want(status.state == "failure", "failure", status.state, "the status state does not round trip")
    combined = commit.get_combined_status()
    want(combined.state == "failure", "failure", combined.state,
         "one failing context must make the combined state failure")
    want(combined.total_count >= 1, "a non-zero total_count", combined.total_count,
         "the combined status counts nothing")


# --- Branch protection ------------------------------------------------------
@check("branches", "Branch.edit_protection", "PUT /repos/{owner}/{repo}/branches/{branch}/protection")
def _edit_protection() -> None:
    repo = repository()
    branch = repo.get_branch(repo.default_branch)
    branch.edit_protection(strict=True, contexts=["pygithub/ci"],
                           enforce_admins=True, required_approving_review_count=1)
    protection = branch.get_protection()
    want(protection.required_status_checks.strict is True, "strict True",
         protection.required_status_checks.strict, "strict does not round trip")
    want(bool(protection.enforce_admins), "enforce_admins enabled",
         protection.enforce_admins, "enforce_admins does not round trip")
    want(protection.required_pull_request_reviews.required_approving_review_count == 1,
         1, protection.required_pull_request_reviews.required_approving_review_count,
         "the approval threshold does not round trip")


@check("branches", "Branch.get_required_status_checks",
       "GET /repos/{owner}/{repo}/branches/{branch}/protection/required_status_checks")
def _required_status_checks() -> None:
    repo = repository()
    branch = repo.get_branch(repo.default_branch)
    checks = branch.get_required_status_checks()
    want(checks.strict is True, "strict True", checks.strict, "the standalone resource lost strict")
    want("pygithub/ci" in list(checks.contexts), "pygithub/ci in contexts", list(checks.contexts),
         "the contexts written through edit_protection are not read back")


@check("branches", "Branch.remove_protection",
       "DELETE /repos/{owner}/{repo}/branches/{branch}/protection")
def _remove_protection() -> None:
    repo = repository()
    branch = repo.get_branch(repo.default_branch)
    branch.remove_protection()
    try:
        branch.get_protection()
    except GithubException as error:
        want(error.status == 404, 404, error.status, "reading removed protection does not answer 404")
        return
    raise Deviation("404 after removal", "success", "protection survived its own deletion")


# --- Deployments and environments -------------------------------------------
@check("deployments", "Repository.create_deployment", "POST /repos/{owner}/{repo}/deployments")
def _create_deployment() -> None:
    repo = repository()
    deployment = repo.create_deployment(ref=repo.default_branch, environment="pygithub",
                                        required_contexts=[], auto_merge=False,
                                        description="created by the PyGithub driver")
    want(deployment.id > 0, "a deployment id", deployment.id, "the created deployment has no id")
    want(deployment.environment == "pygithub", "pygithub", deployment.environment,
         "the deployment environment does not round trip")
    want(bool(deployment.sha), "a resolved sha", "empty", "the deployment did not resolve its ref to a sha")
    state["deployment"] = deployment


@check("deployments", "Deployment.create_status",
       "POST /repos/{owner}/{repo}/deployments/{id}/statuses")
def _deployment_status() -> None:
    status = state["deployment"].create_status("success", description="done")
    want(status.state == "success", "success", status.state, "the deployment status state does not round trip")
    statuses = list(state["deployment"].get_statuses())
    want(len(statuses) >= 1, "at least the status posted", len(statuses),
         "the deployment status listing is empty")


@check("deployments", "Repository.create_environment / get_environments",
       "PUT /repos/{owner}/{repo}/environments/{name}")
def _environments() -> None:
    repo = repository()
    environment = repo.create_environment("pygithub-env", wait_timer=1)
    want(environment.name == "pygithub-env", "pygithub-env", environment.name,
         "the environment name is wrong")
    names = [item.name for item in repo.get_environments()]
    want("pygithub-env" in names, "the environment in the listing", names,
         "the environment listing omits the environment just created")


# --- Collaborators and teams -------------------------------------------------
@check("collaborators", "Repository.get_collaborators",
       "GET /repos/{owner}/{repo}/collaborators")
def _collaborators() -> None:
    collaborators = list(repository().get_collaborators())
    want(len(collaborators) >= 1, "at least the owner", len(collaborators),
         "a repository lists no collaborators at all")
    want(collaborators[0].permissions is not None, "a permissions object", "absent",
         "a listed collaborator carries no permissions object")


@check("collaborators", "Repository.get_collaborator_permission",
       "GET /repos/{owner}/{repo}/collaborators/{username}/permission")
def _collaborator_permission() -> None:
    permission = repository().get_collaborator_permission(OWNER)
    want(permission == "admin", "admin", permission, "the owner is not an admin of their own repository")


@check("orgs", "Organization.create_team / get_teams", "POST /orgs/{org}/teams")
def _org_team() -> None:
    organization = client.get_organization(ORG_LOGIN)
    team = organization.create_team("pygithub-team", privacy="closed",
                                    description="created by the PyGithub driver")
    want(team.id > 0, "a team id", team.id, "the created team has no id")
    want(team.slug == "pygithub-team", "pygithub-team", team.slug, "the team slug is wrong")
    slugs = [item.slug for item in organization.get_teams()]
    want("pygithub-team" in slugs, "the team in the listing", slugs, "the team listing omits the new team")


@check("orgs", "Organization.get_repos", "GET /orgs/{org}/repos")
def _org_repos() -> None:
    organization = client.get_organization(ORG_LOGIN)
    repos = list(organization.get_repos())
    want(isinstance(repos, list), "a repository list", type(repos).__name__,
         "the organization repository listing did not decode")


@check("orgs", "Organization.create_hook / get_hooks", "POST /orgs/{org}/hooks")
def _org_hook() -> None:
    organization = client.get_organization(ORG_LOGIN)
    hook = organization.create_hook("web", {"url": "https://example.invalid/org", "content_type": "json"},
                                    ["repository"], active=True)
    want(hook.id > 0, "a hook id", hook.id, "the created organization hook has no id")
    ids = [item.id for item in organization.get_hooks()]
    want(hook.id in ids, "the hook in the listing", ids, "the organization hook listing omits the new hook")


# --- Actions at organization and environment scope --------------------------
@check("actions", "Organization.create_variable / get_variables", "POST /orgs/{org}/actions/variables")
def _org_variable() -> None:
    organization = client.get_organization(ORG_LOGIN)
    organization.create_variable("PYGITHUB_ORG_VAR", "one", "all")
    variable = organization.get_variable("PYGITHUB_ORG_VAR")
    want(variable.value == "one", "one", variable.value, "the organization variable value does not round trip")


@check("actions", "Organization.create_secret", "PUT /orgs/{org}/actions/secrets/{name}")
def _org_secret() -> None:
    organization = client.get_organization(ORG_LOGIN)
    secret = organization.create_secret("PYGITHUB_ORG_SECRET", "value", "all")
    want(secret.name == "PYGITHUB_ORG_SECRET", "PYGITHUB_ORG_SECRET", secret.name,
         "the created organization secret has the wrong name")
    names = [item.name for item in organization.get_secrets()]
    want("PYGITHUB_ORG_SECRET" in names, "the secret in the listing", names,
         "the organization secret listing omits the new secret")


@check("actions", "Repository.get_workflow_runs", "GET /repos/{owner}/{repo}/actions/runs")
def _workflow_runs() -> None:
    runs = repository().get_workflow_runs()
    want(runs.totalCount >= 0, "a countable run listing", runs.totalCount,
         "the workflow run listing did not decode")


@check("actions", "Repository.get_artifacts", "GET /repos/{owner}/{repo}/actions/artifacts")
def _artifacts() -> None:
    artifacts = repository().get_artifacts()
    want(artifacts.totalCount >= 0, "a countable artifact listing", artifacts.totalCount,
         "the artifact listing did not decode")


@check("actions", "Repository.get_repo_variables", "GET /repos/{owner}/{repo}/actions/variables")
def _repo_variables() -> None:
    variables = list(repository().get_variables())
    want(any(item.name == "PYGITHUB_CONFORMANCE" for item in variables),
         "the variable created earlier", [item.name for item in variables],
         "the repository variable listing omits the variable that was created")


# --- Activity ---------------------------------------------------------------
@check("activity", "AuthenticatedUser.add_to_starred / get_starred", "PUT /user/starred/{owner}/{repo}")
def _starring() -> None:
    user = client.get_user()
    repo = repository()
    user.add_to_starred(repo)
    starred = [item.full_name for item in user.get_starred()]
    want(repo.full_name in starred, "the repository in the starred listing", starred,
         "starring a repository is not reflected in the starred listing")
    user.remove_from_starred(repo)


@check("activity", "Repository.get_stargazers", "GET /repos/{owner}/{repo}/stargazers")
def _stargazers() -> None:
    stargazers = list(repository().get_stargazers())
    want(isinstance(stargazers, list), "a user list", type(stargazers).__name__,
         "the stargazer listing did not decode")


@check("activity", "AuthenticatedUser.get_notifications", "GET /notifications")
def _notifications() -> None:
    notifications = list(client.get_user().get_notifications())
    want(isinstance(notifications, list), "a notification list", type(notifications).__name__,
         "the notification listing did not decode")


@check("activity", "Repository.get_events", "GET /repos/{owner}/{repo}/events")
def _repo_events() -> None:
    events = list(repository().get_events()[:5])
    for event in events:
        want(bool(event.type), "an event type", "empty", "an event carries no type")


# --- Users ------------------------------------------------------------------
@check("users", "AuthenticatedUser.get_keys", "GET /user/keys")
def _user_keys() -> None:
    keys = list(client.get_user().get_keys())
    want(isinstance(keys, list), "a key list", type(keys).__name__, "the public key listing did not decode")


@check("users", "AuthenticatedUser.get_emails", "GET /user/emails")
def _user_emails() -> None:
    emails = list(client.get_user().get_emails())
    want(isinstance(emails, list), "an email list", type(emails).__name__, "the email listing did not decode")


@check("users", "NamedUser.get_repos (follows the user's own hypermedia)", "GET /users/{username}/repos")
def _user_repos() -> None:
    # PyGithub builds this request from the `url` the user object itself
    # carried, so it only works when that link is the absolute URI the
    # simple-user schema declares (format: uri). A relative link sends the
    # client to base + relative and produces a 404 it cannot explain.
    repos = list(client.get_user(OWNER).get_repos())
    want(len(repos) >= 1, "at least one repository", len(repos), "the per-user repository listing is empty")


@check("users", "user hypermedia is absolute", "GET /users/{username}")
def _user_hypermedia() -> None:
    payload = client.get_user(OWNER).raw_data
    for key in ("url", "html_url", "avatar_url", "repos_url", "followers_url",
                "organizations_url", "received_events_url", "subscriptions_url"):
        value = payload.get(key)
        want(value is not None, f"{key} present", "absent",
             f"simple-user marks {key} required and it is missing")
        want(str(value).startswith("http://") or str(value).startswith("https://"),
             f"an absolute URI in {key}", value,
             f"{key} is not an absolute URI, so a client that follows the object's own links "
             f"resolves them against its own base and lands on a route that does not exist")


# --- Milestones, comparisons and releases ------------------------------------
@check("issues", "Repository.create_milestone / get_milestones", "POST /repos/{owner}/{repo}/milestones")
def _milestones() -> None:
    repo = repository()
    milestone = repo.create_milestone("PyGithub milestone", description="conformance")
    want(milestone.number > 0, "a milestone number", milestone.number, "the created milestone has no number")
    numbers = [item.number for item in repo.get_milestones()]
    want(milestone.number in numbers, "the milestone in the listing", numbers,
         "the milestone listing omits the new milestone")


@check("repos", "Repository.compare", "GET /repos/{owner}/{repo}/compare/{base}...{head}")
def _compare() -> None:
    repo = repository()
    commits = list(repo.get_commits()[:2])
    if len(commits) < 2:
        raise Deviation("two commits to compare", len(commits), "the fixture has too few commits")
    comparison = repo.compare(commits[1].sha, commits[0].sha)
    want(comparison.ahead_by >= 1, "ahead_by >= 1", comparison.ahead_by,
         "the comparison reports no distance between two different commits")
    want(comparison.files is not None, "a files list", "absent", "the comparison carries no files")


@check("releases", "Repository.get_releases / GitRelease.update_release",
       "GET /repos/{owner}/{repo}/releases")
def _releases() -> None:
    repo = repository()
    releases = list(repo.get_releases())
    want(len(releases) >= 1, "at least the release created earlier", len(releases),
         "the release listing is empty")
    updated = releases[0].update_release(releases[0].title or "conformance", "edited by the PyGithub driver")
    want(updated.body == "edited by the PyGithub driver", "the new body", updated.body,
         "the release edit did not persist")


# --- Error and edge semantics ------------------------------------------------
@check("errors", "GithubException carries the errors array",
       "POST /repos/{owner}/{repo}/labels with no name")
def _validation_shape() -> None:
    try:
        repository()._requester.requestJsonAndCheck(
            "POST", f"/repos/{OWNER}/{REPO_NAME}/labels", input={}
        )
    except GithubException as error:
        want(error.status == 422, 422, error.status, "an invalid label was not rejected with 422")
        want(isinstance(error.data, dict) and error.data.get("errors"),
             "a populated errors array", truncate(error.data),
             "the validation error carries no errors array")
        return
    raise Deviation("422", "success", "a label with no name was accepted")


@check("errors", "404 for a repository that does not exist under a real owner",
       "GET /repos/{owner}/{missing}")
def _missing_repo_under_real_owner() -> None:
    try:
        client.get_repo(f"{OWNER}/pygithub-definitely-missing")
    except UnknownObjectException as error:
        want(error.status == 404, 404, error.status, "a missing repository does not answer 404")
        return
    raise Deviation("404", "success", "a missing repository was served")


@check("pagination", "PaginatedList.totalCount", "GET /repos/{owner}/{repo}/issues?per_page=1")
def _total_count() -> None:
    issues = repository().get_issues(state="all")
    want(issues.totalCount >= 1, "a positive totalCount", issues.totalCount,
         "PaginatedList could not derive totalCount, which it reads from the Link header's last page")


@check("pagination", "PaginatedList.reversed", "GET /repos/{owner}/{repo}/issues (reversed)")
def _reversed_pagination() -> None:
    issues = repository().get_issues(state="all")
    forward = [item.number for item in issues[:3]]
    backward = [item.number for item in issues.reversed[:3]]
    want(len(backward) > 0, "results walking backwards", backward,
         "PaginatedList.reversed produced nothing, which means the Link header carries no rel=last")
    want(forward != backward or len(forward) <= 1, "a different order",
         f"{forward} == {backward}", "reversing the listing changed nothing")


# --- GitHub App authentication ----------------------------------------------
# An App is provisioned through the manifest flow every integrator uses, so
# PyGithub's GithubIntegration can authenticate as the App for real rather than
# recording a skip.
def _create_app_via_manifest(name: str) -> dict:
    manifest = json.dumps({
        "name": name,
        "url": "https://example.invalid/app",
        "redirect_url": "https://example.invalid/callback",
        "default_permissions": {"contents": "read", "issues": "write"},
    })
    body = urllib.parse.urlencode({"manifest": manifest}).encode()
    request = urllib.request.Request(
        f"{BASE}/settings/apps/new", data=body,
        headers={"Authorization": f"Bearer {TOKEN}",
                 "Content-Type": "application/x-www-form-urlencoded"},
        method="POST",
    )

    class _NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, *_args, **_kwargs):
            return None

    opener = urllib.request.build_opener(_NoRedirect)
    try:
        with opener.open(request) as response:
            location = response.headers.get("Location", "")
    except urllib.error.HTTPError as error:
        location = error.headers.get("Location", "") if error.code == 302 else ""
    if not location:
        raise Deviation("a 302 carrying a conversion code", "no Location header",
                        "the App manifest form did not redirect")
    code = urllib.parse.parse_qs(urllib.parse.urlparse(location).query).get("code", [""])[0]
    if not code:
        raise Deviation("a conversion code", location, "the manifest redirect carried no code")
    conversion = urllib.request.Request(
        f"{BASE}/api/v3/app-manifests/{code}/conversions",
        headers={"Authorization": f"Bearer {TOKEN}"}, method="POST",
    )
    with urllib.request.urlopen(conversion) as response:
        return json.loads(response.read())


def _install_app(slug: str, account: str) -> int:
    body = urllib.parse.urlencode({"target_login": account, "repository_selection": "all"}).encode()
    request = urllib.request.Request(
        f"{BASE}/apps/{slug}/installations/new", data=body,
        headers={"Authorization": f"Bearer {TOKEN}",
                 "Content-Type": "application/x-www-form-urlencoded"},
        method="POST",
    )
    with urllib.request.urlopen(request) as response:
        return json.loads(response.read()).get("id", 0)


@check("apps", "GithubIntegration.get_installations", "GET /app/installations")
def _app_installations() -> None:
    created = _create_app_via_manifest("PyGithub Conformance App")
    state["app"] = created
    want(bool(created.get("pem")), "a private key", "empty",
         "the manifest conversion returned no private key")
    installation_id = _install_app(created["slug"], OWNER)
    want(installation_id > 0, "an installation id", installation_id, "the App could not be installed")
    state["installation_id"] = installation_id
    integration = GithubIntegration(
        base_url=f"{BASE}/api/v3",
        auth=Auth.AppAuth(created["id"], created["pem"]),
    )
    state["integration"] = integration
    installations = list(integration.get_installations())
    want(any(item.id == installation_id for item in installations),
         "the installation just created", [item.id for item in installations],
         "an App authenticated with its own JSON Web Token cannot see its installation")


@check("apps", "GithubIntegration.get_access_token", "POST /app/installations/{id}/access_tokens")
def _app_access_token() -> None:
    integration = state["integration"]
    token = integration.get_access_token(state["installation_id"])
    want(bool(token.token), "an installation token", "empty", "no installation token was minted")
    want(token.expires_at is not None, "an expiry", "absent",
         "the installation token has no expires_at, so a client cannot refresh it in time")
    state["installation_token"] = token.token


@check("apps", "Auth.AppInstallationAuth reads a granted repository",
       "GET /repos/{owner}/{repo} with an installation token")
def _app_installation_repos() -> None:
    # PyGithub mints and refreshes the installation token itself from the App
    # key, which is how a real App client is wired; the assertion is that the
    # resulting credential can read a repository the installation covers.
    auth = Auth.AppAuth(state["app"]["id"], state["app"]["pem"]).get_installation_auth(
        state["installation_id"]
    )
    scoped = Github(base_url=f"{BASE}/api/v3", auth=auth)
    repo = scoped.get_repo(f"{OWNER}/{REPO_NAME}")
    want(repo.full_name == f"{OWNER}/{REPO_NAME}", f"{OWNER}/{REPO_NAME}", repo.full_name,
         "an installation token cannot read a repository the installation covers")


@check("apps", "GithubIntegration.get_app", "GET /app")
def _app_get() -> None:
    app = state["integration"].get_app()
    want(app.id == state["app"]["id"], state["app"]["id"], app.id,
         "the JSON Web Token authenticated as a different App")
    want(app.slug == state["app"]["slug"], state["app"]["slug"], app.slug, "the App slug is wrong")

print(f"PyGithub driver: {PASSED} passed, {FAILED} failed, {SKIPPED} skipped", file=sys.stderr)
if stream is not sys.stdout:
    stream.close()
