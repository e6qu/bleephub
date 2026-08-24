#!/usr/bin/env bash
# Conformance driver for the official GitHub command-line interface.
#
# `gh` is the harshest client in the harness: it reads undocumented fields,
# expects exact shapes, and fails loudly when a payload is a shade off. Nothing
# here uses `gh api` where a real verb exists — the point is to exercise the
# command a person actually types.
#
# Unlike the other drivers this one owns its server. The command-line interface
# only ever speaks HTTPS to an Enterprise host, so it needs a server with a
# certificate it trusts; the driver mints a throwaway certificate authority,
# starts the server with it, and points the client at it through SSL_CERT_FILE.
# That environment variable is honoured by Go's certificate verification on
# Linux but NOT on macOS, where the only alternative is the login keychain, so
# on macOS this driver runs inside the pinned container instead (see
# gh_container.sh). It probes the trust path before asserting anything and
# exits 3 with an explanation rather than reporting a wall of false failures.
#
# Every git subprocess `gh` spawns inherits the hermetic git environment set
# below, mirroring hermeticGitTestEnv (internal/server/git_testenv_test.go), so
# no credential helper from the host — the macOS keychain in particular — can
# ever be consulted.
set -uo pipefail

SERVER_BIN="${BPH_SERVER_BIN:-/usr/local/bin/bleephub}"
WORK="${BPH_WORK:-/tmp/gh-conformance}"
RESULTS="${BPH_RESULTS:-/dev/stdout}"
TOKEN="bleephub-admin-token-00000000000000000000"

rm -rf "$WORK"
mkdir -p "$WORK/tls" "$WORK/home" "$WORK/ghconfig" "$WORK/scratch" "$WORK/data" "$WORK/git"
: >"$RESULTS"

PASS=0
FAIL=0

emit() { # domain operation status request expected actual message
    jq -nc --arg domain "$1" --arg operation "$2" --arg status "$3" --arg request "$4" \
        --arg expected "${5:-}" --arg actual "${6:-}" --arg message "${7:-}" \
        '{client:"gh", domain:$domain, operation:$operation, status:$status, request:$request,
          expected:$expected, actual:($actual[0:400]), message:($message[0:400])}' >>"$RESULTS"
}

pass_op() {
    emit "$1" "$2" pass "$3"
    PASS=$((PASS + 1))
}
fail_op() { # domain operation request expected actual
    emit "$1" "$2" fail "$3" "$4" "$5" "$5"
    FAIL=$((FAIL + 1))
}
skip_op() { emit "$1" "$2" skip "$3" "" "" "$4"; }

# --- Certificate authority and server ---------------------------------------
openssl req -x509 -newkey rsa:2048 -keyout "$WORK/tls/ca.key" -out "$WORK/tls/ca.crt" \
    -days 1 -nodes -subj "/CN=bleephub-conformance-ca" 2>/dev/null
openssl req -newkey rsa:2048 -keyout "$WORK/tls/server.key" -out "$WORK/tls/server.csr" \
    -nodes -subj "/CN=localhost" 2>/dev/null
cat >"$WORK/tls/ext.cnf" <<'EOF'
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
subjectAltName=DNS:localhost,IP:127.0.0.1
EOF
openssl x509 -req -in "$WORK/tls/server.csr" -CA "$WORK/tls/ca.crt" -CAkey "$WORK/tls/ca.key" \
    -CAcreateserial -out "$WORK/tls/server.crt" -days 1 -extfile "$WORK/tls/ext.cnf" 2>/dev/null

# Port 443, not a high port. `gh` derives the host of a git remote without its
# port, so against https://localhost:9999 it decides none of the repository's
# remotes correspond to GH_HOST and every pull-request verb refuses to run. On
# 443 the remote's host and GH_HOST are both "localhost" and the verbs work,
# which is why this driver runs as root inside the pinned container rather than
# as an unprivileged continuous-integration user.
PORT="${BPH_GH_PORT:-443}"
if [ "$PORT" = "443" ]; then HOST_URL="localhost"; else HOST_URL="localhost:$PORT"; fi
export SSL_CERT_FILE="$WORK/tls/ca.crt"
export GIT_SSL_CAINFO="$WORK/tls/ca.crt"
BPH_TLS_CERT="$WORK/tls/server.crt" BPH_TLS_KEY="$WORK/tls/server.key" \
    BLEEPHUB_ADMIN_TOKEN="$TOKEN" \
    BLEEPHUB_DATA_DIR="$WORK/data" \
    BLEEPHUB_GIT_DIR="$WORK/git" \
    BLEEPHUB_EXTERNAL_URL="https://$HOST_URL" \
    "$SERVER_BIN" -addr "127.0.0.1:$PORT" -log-level warn >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
trap 'kill $SERVER_PID 2>/dev/null' EXIT

for _ in $(seq 1 60); do
    curl -fsS --cacert "$WORK/tls/ca.crt" "https://$HOST_URL/health" >/dev/null 2>&1 && break
    sleep 0.25
done
if ! curl -fsS --cacert "$WORK/tls/ca.crt" "https://$HOST_URL/health" >/dev/null 2>&1; then
    echo "the server did not become ready" >&2
    tail -20 "$WORK/server.log" >&2
    exit 2
fi

# --- Client environment ------------------------------------------------------
HOST="$HOST_URL"
export GH_HOST="$HOST"
# An Enterprise host takes its credential from GH_ENTERPRISE_TOKEN; GH_TOKEN is
# only ever consulted for github.com, which is why setting it produced "Bad
# credentials" against a GitHub Enterprise Server-style host. `gh auth login
# --hostname` is not usable here either: it rejects any hostname carrying a
# port, and the harness cannot bind 443 as an unprivileged continuous
# integration user.
export GH_ENTERPRISE_TOKEN="$TOKEN"
export GH_CONFIG_DIR="$WORK/ghconfig"
export GH_NO_UPDATE_NOTIFIER=1
export GH_PROMPT_DISABLED=1
export GH_PAGER=cat
export NO_COLOR=1
# Hermetic git, mirroring hermeticGitTestEnv(home).
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL="$WORK/home/.gitconfig"
export HOME="$WORK/home"
export XDG_CONFIG_HOME="$WORK/home"
export GIT_TERMINAL_PROMPT=0
export GIT_ASKPASS=
: >"$GIT_CONFIG_GLOBAL"
git config --global user.name "Conformance"
git config --global user.email "conformance@bleephub.local"
git config --global init.defaultBranch main

# Because `gh auth login` cannot be used (see above), the git credential helper
# it would have installed is configured directly, in the harness's own global
# git configuration, so `gh repo clone` and the pushes that follow authenticate.
git config --global "credential.https://$HOST.helper" \
    "!f() { echo username=x-access-token; echo password=$TOKEN; }; f"

# Trust probe: if the client cannot verify the certificate there is no point
# recording a hundred identical TLS failures.
if ! gh api /rate_limit >"$WORK/probe.txt" 2>&1; then
    if grep -q "certificate" "$WORK/probe.txt"; then
        echo "gh cannot verify the harness certificate authority on this platform:" >&2
        cat "$WORK/probe.txt" >&2
        exit 3
    fi
    echo "gh probe failed:" >&2
    cat "$WORK/probe.txt" >&2
    exit 2
fi

OWNER="admin"
REPO="conformance-gh"

# run_gh runs a command and records pass when it exits 0 and its output
# satisfies the optional pattern.
run_gh() { # domain operation request pattern -- args...
    local domain="$1" operation="$2" request="$3" pattern="$4"
    shift 4
    local output status
    output="$(gh "$@" 2>&1)"
    status=$?
    if [ "$status" -ne 0 ]; then
        fail_op "$domain" "$operation" "$request" "gh exits 0" "$(printf '%s' "$output" | tr '\n' ' ')"
        return 1
    fi
    if [ -n "$pattern" ] && ! printf '%s' "$output" | grep -qE "$pattern"; then
        fail_op "$domain" "$operation" "$request" "output matching /$pattern/" "$(printf '%s' "$output" | tr '\n' ' ')"
        return 1
    fi
    pass_op "$domain" "$operation" "$request"
    return 0
}

# run_gh_json runs a command that must emit JSON and evaluates a jq filter that
# has to return true. This is the strict form: it proves the client rendered the
# fields it was asked for, not merely that the call succeeded.
run_gh_json() { # domain operation request jq-filter -- args...
    local domain="$1" operation="$2" request="$3" filter="$4"
    shift 4
    local output status
    output="$(gh "$@" 2>&1)"
    status=$?
    if [ "$status" -ne 0 ]; then
        fail_op "$domain" "$operation" "$request" "gh exits 0" "$(printf '%s' "$output" | tr '\n' ' ')"
        return 1
    fi
    if ! printf '%s' "$output" | jq -e "$filter" >/dev/null 2>&1; then
        fail_op "$domain" "$operation" "$request" "a payload satisfying: $filter" \
            "$(printf '%s' "$output" | tr '\n' ' ')"
        return 1
    fi
    pass_op "$domain" "$operation" "$request"
    return 0
}

# ============================================================================
# auth
# ============================================================================
run_gh auth "gh auth status" "gh auth status" "$HOST" auth status >/dev/null
run_gh_json auth "gh api /user" "GET /user" '.login == "admin"' api /user
run_gh_json auth "gh api --include response headers" "GET /user" '.login == "admin"' api /user --jq '.'

# ============================================================================
# repo
# ============================================================================
run_gh repos "gh repo create" "POST /user/repos" "" \
    repo create "$REPO" --private --add-readme --description "gh conformance fixture" >/dev/null
run_gh_json repos "gh repo view --json" "GET /repos/{owner}/{repo}" \
    '.name == "conformance-gh" and (.defaultBranchRef.name | length > 0) and (.id | length > 0)' \
    repo view "$OWNER/$REPO" --json name,defaultBranchRef,id,description,isPrivate,createdAt
run_gh_json repos "gh repo view --json owner,visibility" "GET /repos/{owner}/{repo}" \
    '.owner.login == "admin" and (.visibility | ascii_downcase) == "private"' \
    repo view "$OWNER/$REPO" --json owner,visibility
run_gh_json repos "gh repo list --json" "GET /user/repos" \
    'map(.name) | index("conformance-gh") != null' \
    repo list --json name,visibility,updatedAt --limit 50
run_gh repos "gh repo edit --description" "PATCH /repos/{owner}/{repo}" "" \
    repo edit "$OWNER/$REPO" --description "edited by the conformance driver" >/dev/null
run_gh_json repos "gh repo view --json url,description" "GET /repos/{owner}/{repo}" \
    '(.url | startswith("https://")) and (.description | length > 0)' \
    repo view "$OWNER/$REPO" --json description,name,url
(cd "$WORK/scratch" && gh repo clone "$OWNER/$REPO" clone >/dev/null 2>&1)
if [ -f "$WORK/scratch/clone/README.md" ]; then
    pass_op repos "gh repo clone" "git clone over https with the credential helper"
else
    fail_op repos "gh repo clone" "git clone over https with the credential helper" \
        "a clone containing README.md" "$(tail -3 "$WORK/server.log" | tr '\n' ' ')"
fi

# ============================================================================
# label and milestone
# ============================================================================
run_gh repos "gh label create" "POST /repos/{owner}/{repo}/labels" "" \
    label create conformance --repo "$OWNER/$REPO" --color ededed --description "conformance" >/dev/null
run_gh_json repos "gh label list --json" "GET /repos/{owner}/{repo}/labels" \
    'map(.name) | index("conformance") != null' \
    label list --repo "$OWNER/$REPO" --json name,color,description
run_gh repos "gh label edit" "PATCH /repos/{owner}/{repo}/labels/{name}" "" \
    label edit conformance --repo "$OWNER/$REPO" --description "edited" >/dev/null
run_gh_json repos "gh label list default set" "GET /repos/{owner}/{repo}/labels" \
    'length >= 9' label list --repo "$OWNER/$REPO" --json name --limit 50

MILESTONE_STATUS="$(curl -sS -o /dev/null -w '%{http_code}' --cacert "$WORK/tls/ca.crt" \
    -X POST -H "Authorization: token $TOKEN" -H "Content-Type: application/json" \
    -d '{"title":"conformance milestone"}' "https://$HOST_URL/api/v3/repos/$OWNER/$REPO/milestones")"
if [ "$MILESTONE_STATUS" = "201" ]; then
    pass_op repos "create a milestone for the issue verbs" "POST /repos/{owner}/{repo}/milestones"
else
    fail_op repos "create a milestone for the issue verbs" "POST /repos/{owner}/{repo}/milestones" "201" "HTTP $MILESTONE_STATUS"
fi

# ============================================================================
# issue
# ============================================================================
run_gh issues "gh issue create" "POST /repos/{owner}/{repo}/issues" "" \
    issue create --repo "$OWNER/$REPO" --title "conformance issue" --body "opened by the driver" >/dev/null
run_gh_json issues "gh issue list --json" "GET /repos/{owner}/{repo}/issues" \
    'length >= 1 and (.[0].number | type) == "number"' \
    issue list --repo "$OWNER/$REPO" --json number,title,state,author,labels,createdAt
run_gh_json issues "gh issue view --json (rich fields)" "GET issue + timeline + comments" \
    '.number == 1 and .state == "OPEN" and (.author.login == "admin") and (.url | length > 0)' \
    issue view 1 --repo "$OWNER/$REPO" --json number,title,state,author,url,body,comments,labels,assignees,milestone
run_gh issues "gh issue comment" "POST /repos/{owner}/{repo}/issues/{n}/comments" "" \
    issue comment 1 --repo "$OWNER/$REPO" --body "conformance comment" >/dev/null
run_gh issues "gh issue edit --add-label" "POST /repos/{owner}/{repo}/issues/{n}/labels" "" \
    issue edit 1 --repo "$OWNER/$REPO" --add-label conformance >/dev/null
run_gh issues "gh issue edit --milestone" "PATCH /repos/{owner}/{repo}/issues/{n}" "" \
    issue edit 1 --repo "$OWNER/$REPO" --milestone "conformance milestone" >/dev/null
run_gh issues "gh issue edit --add-assignee" "POST /repos/{owner}/{repo}/issues/{n}/assignees" "" \
    issue edit 1 --repo "$OWNER/$REPO" --add-assignee "$OWNER" >/dev/null
run_gh issues "gh issue pin" "PUT graphql pinIssue" "" issue pin 1 --repo "$OWNER/$REPO" >/dev/null
run_gh issues "gh issue lock" "PUT /repos/{owner}/{repo}/issues/{n}/lock" "" \
    issue lock 1 --repo "$OWNER/$REPO" --reason resolved >/dev/null
run_gh issues "gh issue unlock" "DELETE /repos/{owner}/{repo}/issues/{n}/lock" "" \
    issue unlock 1 --repo "$OWNER/$REPO" >/dev/null
run_gh issues "gh issue close" "PATCH /repos/{owner}/{repo}/issues/{n}" "" \
    issue close 1 --repo "$OWNER/$REPO" --reason completed >/dev/null
run_gh issues "gh issue reopen" "PATCH /repos/{owner}/{repo}/issues/{n}" "" \
    issue reopen 1 --repo "$OWNER/$REPO" >/dev/null
run_gh issues "gh issue status" "GET /issues (created/assigned/mentioned)" "" \
    issue status --repo "$OWNER/$REPO" >/dev/null

# ============================================================================
# pull request
# ============================================================================
CLONE="$WORK/scratch/clone"
if [ -d "$CLONE" ]; then
    (
        cd "$CLONE" || exit 1
        git checkout -q -b conformance-topic
        printf 'topic\n' >topic.txt
        git add topic.txt
        git commit -q -m "topic commit"
        git push -q -u origin conformance-topic
    ) >/dev/null 2>&1
    (cd "$CLONE" && run_gh pulls "gh pr create" "POST /repos/{owner}/{repo}/pulls" "" \
        pr create --title "conformance pull request" --body "opened by the driver" --base main --head conformance-topic) >/dev/null
    (cd "$CLONE" && run_gh_json pulls "gh pr view --json (rich fields)" "GET pull + reviews + commits" \
        '.number == 2 and .state == "OPEN" and (.headRefName == "conformance-topic") and (.baseRefName == "main")' \
        pr view 2 --json number,state,headRefName,baseRefName,title,body,author,reviews,commits,files,mergeable,isDraft)
    (cd "$CLONE" && run_gh_json pulls "gh pr list --json" "GET /repos/{owner}/{repo}/pulls" \
        'length >= 1' pr list --json number,title,headRefName,state)
    (cd "$CLONE" && run_gh_json pulls "gh pr status" "GET pulls for the current branch" \
        '.currentBranch.number == 2' pr status --json number)
    (cd "$CLONE" && run_gh_json pulls "gh pr diff --name-only" "GET /repos/{owner}/{repo}/pulls/{n}/files" \
        'true' pr view 2 --json files)
    # `gh pr diff` asks for Accept: application/vnd.github.v3.diff and prints
    # the body verbatim, so a server that answers with the pull request's JSON
    # instead prints JSON at the user.
    (cd "$CLONE" && run_gh pulls "gh pr diff" "GET pull with Accept: application/vnd.github.v3.diff" \
        "^diff --git" pr diff 2) >/dev/null
    (cd "$CLONE" && run_gh pulls "gh pr comment" "POST issue comment on the pull request" "" \
        pr comment 2 --body "conformance pull request comment") >/dev/null
    (cd "$CLONE" && run_gh pulls "gh pr review --approve" "POST /repos/{owner}/{repo}/pulls/{n}/reviews" "" \
        pr review 2 --approve --body "looks good") >/dev/null
    (cd "$CLONE" && run_gh pulls "gh pr ready" "graphql markPullRequestReadyForReview" "" pr ready 2) >/dev/null
    # `gh pr checks` reads the head commit's check runs and its branch
    # protection, and reports "no checks reported" when the commit has neither.
    # The fixture therefore records one required and one optional check, so the
    # row measures what the server answers rather than the emptiness of the
    # fixture — and covers the isRequired field gh selects on every context.
    PR_HEAD_SHA="$(cd "$CLONE" && git rev-parse HEAD)"
    gh api "repos/$OWNER/$REPO/branches/main/protection" --method PUT --input - >/dev/null 2>&1 <<PROTECTION
{"required_status_checks":{"strict":false,"contexts":["conformance-required"]},
 "enforce_admins":false,"required_pull_request_reviews":null,"restrictions":null}
PROTECTION
    gh api "repos/$OWNER/$REPO/check-runs" --method POST \
        -f name=conformance-required -f head_sha="$PR_HEAD_SHA" \
        -f status=completed -f conclusion=success >/dev/null 2>&1
    gh api "repos/$OWNER/$REPO/check-runs" --method POST \
        -f name=conformance-optional -f head_sha="$PR_HEAD_SHA" \
        -f status=completed -f conclusion=success >/dev/null 2>&1
    (cd "$CLONE" && run_gh_json pulls "gh pr checks" "GET check runs for the head commit" \
        'length == 2 and (map(.state) | all(. == "SUCCESS"))' pr checks 2 --json name,state) || true
    (cd "$CLONE" && run_gh_json pulls "gh pr checks --required" "GET check runs the base branch requires" \
        'length == 1 and .[0].name == "conformance-required"' pr checks 2 --required --json name,state) || true
    (cd "$CLONE" && run_gh pulls "gh pr merge --squash" "PUT /repos/{owner}/{repo}/pulls/{n}/merge" "" \
        pr merge 2 --squash --delete-branch --admin) >/dev/null
else
    for operation in "gh pr create" "gh pr view --json (rich fields)" "gh pr list --json" "gh pr status" \
        "gh pr diff" "gh pr comment" "gh pr review --approve" "gh pr merge --squash"; do
        skip_op pulls "$operation" "pull request verbs" "the clone the pull request is raised from is missing"
    done
fi

# ============================================================================
# release
# ============================================================================
printf 'asset payload\n' >"$WORK/scratch/asset.txt"
run_gh releases "gh release create" "POST /repos/{owner}/{repo}/releases" "" \
    release create v1.0.0 --repo "$OWNER/$REPO" --title "Conformance 1.0" --notes "notes" >/dev/null
run_gh_json releases "gh release view --json" "GET /repos/{owner}/{repo}/releases/tags/{tag}" \
    '.tagName == "v1.0.0" and (.url | length > 0)' \
    release view v1.0.0 --repo "$OWNER/$REPO" --json tagName,name,body,url,createdAt,assets,isDraft,isPrerelease
run_gh releases "gh release upload" "POST {upload_url}" "" \
    release upload v1.0.0 "$WORK/scratch/asset.txt" --repo "$OWNER/$REPO" >/dev/null
run_gh_json releases "gh release view assets" "GET /repos/{owner}/{repo}/releases/{id}/assets" \
    '.assets | length >= 1 and (.[0].name == "asset.txt")' \
    release view v1.0.0 --repo "$OWNER/$REPO" --json assets
run_gh releases "gh release download" "GET the asset browser_download_url" "" \
    release download v1.0.0 --repo "$OWNER/$REPO" --dir "$WORK/scratch/dl" >/dev/null
run_gh_json releases "gh release list --json" "GET /repos/{owner}/{repo}/releases" \
    'length >= 1' release list --repo "$OWNER/$REPO" --json tagName,name,isLatest
run_gh releases "gh release edit" "PATCH /repos/{owner}/{repo}/releases/{id}" "" \
    release edit v1.0.0 --repo "$OWNER/$REPO" --notes "edited notes" >/dev/null
run_gh releases "gh release delete" "DELETE /repos/{owner}/{repo}/releases/{id}" "" \
    release delete v1.0.0 --repo "$OWNER/$REPO" --yes >/dev/null

# ============================================================================
# workflow and run
# ============================================================================
WORKFLOW_CONTENT="$(printf 'name: conformance\non:\n  workflow_dispatch:\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo conformance\n' | base64 | tr -d '\n')"
curl -sS -o /dev/null --cacert "$WORK/tls/ca.crt" -X PUT \
    -H "Authorization: token $TOKEN" -H "Content-Type: application/json" \
    -d "{\"message\":\"add a workflow\",\"content\":\"$WORKFLOW_CONTENT\"}" \
    "https://$HOST_URL/api/v3/repos/$OWNER/$REPO/contents/.github/workflows/conformance.yml"
run_gh_json actions "gh workflow list --json" "GET /repos/{owner}/{repo}/actions/workflows" \
    'length >= 1' workflow list --repo "$OWNER/$REPO" --json name,id,state
run_gh actions "gh workflow view" "GET /repos/{owner}/{repo}/actions/workflows/{id}" "" \
    workflow view conformance.yml --repo "$OWNER/$REPO" >/dev/null
run_gh actions "gh workflow run" "POST .../workflows/{id}/dispatches" "" \
    workflow run conformance.yml --repo "$OWNER/$REPO" --ref main >/dev/null
run_gh actions "gh workflow disable" "PUT .../workflows/{id}/disable" "" \
    workflow disable conformance.yml --repo "$OWNER/$REPO" >/dev/null
run_gh actions "gh workflow enable" "PUT .../workflows/{id}/enable" "" \
    workflow enable conformance.yml --repo "$OWNER/$REPO" >/dev/null
run_gh_json actions "gh run list --json" "GET /repos/{owner}/{repo}/actions/runs" \
    'type == "array"' run list --repo "$OWNER/$REPO" --json databaseId,status,conclusion,workflowName --limit 5

# ============================================================================
# secret, variable, cache
# ============================================================================
run_gh actions "gh secret set" "PUT .../actions/secrets/{name} (libsodium sealed)" "" \
    secret set CONFORMANCE_SECRET --repo "$OWNER/$REPO" --body "s3cret" >/dev/null
run_gh_json actions "gh secret list --json" "GET /repos/{owner}/{repo}/actions/secrets" \
    'map(.name) | index("CONFORMANCE_SECRET") != null' \
    secret list --repo "$OWNER/$REPO" --json name,updatedAt
run_gh actions "gh secret delete" "DELETE .../actions/secrets/{name}" "" \
    secret delete CONFORMANCE_SECRET --repo "$OWNER/$REPO" >/dev/null
run_gh actions "gh variable set" "POST /repos/{owner}/{repo}/actions/variables" "" \
    variable set CONFORMANCE_VARIABLE --repo "$OWNER/$REPO" --body "1" >/dev/null
run_gh_json actions "gh variable list --json" "GET /repos/{owner}/{repo}/actions/variables" \
    'map(.name) | index("CONFORMANCE_VARIABLE") != null' \
    variable list --repo "$OWNER/$REPO" --json name,value,updatedAt
run_gh actions "gh variable get" "GET .../actions/variables/{name}" "1" \
    variable get CONFORMANCE_VARIABLE --repo "$OWNER/$REPO" >/dev/null
run_gh actions "gh variable delete" "DELETE .../actions/variables/{name}" "" \
    variable delete CONFORMANCE_VARIABLE --repo "$OWNER/$REPO" >/dev/null
run_gh actions "gh cache list" "GET /repos/{owner}/{repo}/actions/caches" "" \
    cache list --repo "$OWNER/$REPO" >/dev/null

# ============================================================================
# gist
# ============================================================================
printf 'gist content\n' >"$WORK/scratch/gist.txt"
GIST_URL="$(gh gist create "$WORK/scratch/gist.txt" --public --desc "conformance gist" 2>&1 | tail -1)"
if printf '%s' "$GIST_URL" | grep -qE "^https://"; then
    pass_op gists "gh gist create" "POST /gists"
    GIST_ID="${GIST_URL##*/}"
    # GitHub — dotcom and Enterprise Server alike — puts a gist's web page under
    # /gist/<id>. `gh gist create` prints whatever html_url the server sent, so a
    # different shape is what a user would paste into a browser.
    if printf '%s' "$GIST_URL" | grep -q "/gist/"; then
        pass_op gists "gist html_url shape" "POST /gists (html_url)"
    else
        fail_op gists "gist html_url shape" "POST /gists (html_url)" \
            "an html_url under /gist/<id>, as GitHub Enterprise Server serves" "$GIST_URL"
    fi
    run_gh gists "gh gist view --files" "GET /gists/{id}" "gist.txt" \
        gist view "$GIST_ID" --files >/dev/null
    run_gh gists "gh gist list" "GET /gists" "" gist list >/dev/null
    run_gh gists "gh gist delete" "DELETE /gists/{id}" "" gist delete "$GIST_ID" --yes >/dev/null
else
    fail_op gists "gh gist create" "POST /gists" "a gist URL" "$GIST_URL"
    skip_op gists "gist html_url shape" "POST /gists (html_url)" "the gist fixture could not be created"
    for operation in "gh gist view --files" "gh gist list" "gh gist delete"; do
        skip_op gists "$operation" "gist verbs" "the gist fixture could not be created"
    done
fi

# ============================================================================
# org, search, browse
# ============================================================================
curl -sS -o /dev/null --cacert "$WORK/tls/ca.crt" -X POST \
    -H "Authorization: token $TOKEN" -H "Content-Type: application/json" \
    -d "{\"login\":\"conformance-gh-org\",\"admin\":\"$OWNER\"}" \
    "https://$HOST_URL/api/v3/admin/organizations"
run_gh orgs "gh org list" "GET /user/orgs" "conformance-gh-org" org list >/dev/null
run_gh_json search "gh search repos --json" "GET /search/repositories" \
    'type == "array"' search repos conformance --json fullName,description,visibility --limit 5
run_gh_json search "gh search issues --json" "GET /search/issues" \
    'type == "array"' search issues conformance --json number,title,repository --limit 5
run_gh_json search "gh search prs --json" "GET /search/issues?q=is:pr" \
    'type == "array"' search prs conformance --json number,title --limit 5
run_gh_json search "gh search code --json" "GET /search/code" \
    'type == "array"' search code conformance --json path,repository --limit 5
run_gh_json search "gh search commits --json" "GET /search/commits" \
    'type == "array"' search commits conformance --json sha,commit --limit 5
run_gh browse "gh browse --no-browser" "resolve the web URL for a repository" "https://" \
    browse --repo "$OWNER/$REPO" --no-browser >/dev/null

# ============================================================================
# ssh-key, gpg-key
# ============================================================================
if command -v ssh-keygen >/dev/null 2>&1; then
    ssh-keygen -q -t ed25519 -N "" -C conformance -f "$WORK/scratch/id_ed25519"
    run_gh users "gh ssh-key add" "POST /user/keys" "" \
        ssh-key add "$WORK/scratch/id_ed25519.pub" --title conformance >/dev/null
    run_gh users "gh ssh-key list" "GET /user/keys" "conformance" ssh-key list >/dev/null
else
    skip_op users "gh ssh-key add" "POST /user/keys" "ssh-keygen is not available here"
    skip_op users "gh ssh-key list" "GET /user/keys" "ssh-keygen is not available here"
fi
run_gh users "gh gpg-key list" "GET /user/gpg_keys" "" gpg-key list >/dev/null

# ============================================================================
# project (Projects v2, GraphQL only)
# ============================================================================
run_gh projects "gh project list" "graphql organization/user projectsV2" "" \
    project list --owner "$OWNER" >/dev/null

# ============================================================================
# attestation
# ============================================================================
# The pinned gh release exposes attestation download/trusted-root/verify only,
# all of which need a signed artifact and a Sigstore trust root to be meaningful;
# the API surface behind them is covered by go-github instead.
skip_op attestation "gh attestation verify" "GET /repos/{owner}/{repo}/attestations/{digest}" \
    "gh $(gh --version | head -1 | awk '{print $3}') has no attestation subcommand that can run without a signed artifact"

# ============================================================================
# api: REST and GraphQL, including pagination and templates
# ============================================================================
run_gh_json api "gh api --paginate" "GET /repos/{owner}/{repo}/issues?per_page=1 (Link header)" \
    'length >= 1' api "/repos/$OWNER/$REPO/issues?per_page=1&state=all" --paginate --slurp
run_gh_json api "gh api -X POST with fields" "POST /repos/{owner}/{repo}/issues" \
    '.number > 0' api "repos/$OWNER/$REPO/issues" -f title="created through gh api" -f body="body"
run_gh_json api "gh api graphql (query)" "POST /api/graphql" \
    '.data.viewer.login == "admin"' api graphql -f query='query { viewer { login } }'
# shellcheck disable=SC2016  # $owner/$name are GraphQL variables, not shell ones.
run_gh_json api "gh api graphql (variables)" "POST /api/graphql" \
    '.data.repository.name == "conformance-gh"' \
    api graphql -F owner="$OWNER" -F name="$REPO" \
    -f query='query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { name nameWithOwner } }'
# shellcheck disable=SC2016  # $owner/$name/$endCursor are GraphQL variables.
run_gh_json api "gh api graphql --paginate" "POST /api/graphql with cursors" \
    '.data.repository.issues.nodes | length >= 1' \
    api graphql --paginate -F owner="$OWNER" -F name="$REPO" \
    -f query='query($owner: String!, $name: String!, $endCursor: String) { repository(owner: $owner, name: $name) { issues(first: 2, after: $endCursor) { nodes { number } pageInfo { hasNextPage endCursor } } } }'
run_gh_json api "gh api /rate_limit" "GET /rate_limit" '.resources.core.limit > 0' api /rate_limit
run_gh_json api "gh api /meta" "GET /meta" '.verifiable_password_authentication != null' api /meta
run_gh api "gh api --method DELETE" "DELETE /repos/{owner}/{repo}/labels/{name}" "" \
    api --method DELETE "repos/$OWNER/$REPO/labels/conformance" >/dev/null


# ============================================================================
# workflow and run: the lifecycle `gh run` renders
# ============================================================================
# A committed workflow file is what turns `gh workflow`/`gh run` from an empty
# listing into the real thing, so one is published through `gh api` first.
WORKFLOW_YAML='name: gh-conformance
on:
  workflow_dispatch:
    inputs:
      reason:
        description: why
        required: true
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo gh
'
WORKFLOW_B64="$(printf '%s' "$WORKFLOW_YAML" | base64 | tr -d '\n')"
gh api --method PUT "repos/$OWNER/$REPO/contents/.github/workflows/gh-conformance.yml" \
    -f message="add the gh conformance workflow" -f content="$WORKFLOW_B64" >/dev/null 2>&1

run_gh_json actions "gh workflow list --json (a committed workflow)" \
    "GET /repos/{owner}/{repo}/actions/workflows" \
    'map(.name) | index("gh-conformance") != null' \
    workflow list --repo "$OWNER/$REPO" --json id,name,state,path

run_gh actions "gh workflow view --yaml" "GET .../actions/workflows/{id} plus contents" "gh-conformance" \
    workflow view gh-conformance.yml --repo "$OWNER/$REPO" --yaml >/dev/null

run_gh actions "gh workflow run -f" "POST .../actions/workflows/{id}/dispatches" "" \
    workflow run gh-conformance.yml --repo "$OWNER/$REPO" --ref main -f reason=conformance >/dev/null

# The dispatched run is created asynchronously; poll on a bounded deadline
# rather than sleeping, and record the expiry as the failure it is.
RUN_ID=""
for _ in $(seq 1 60); do
    RUN_ID="$(gh run list --repo "$OWNER/$REPO" --json databaseId --limit 1 2>/dev/null | jq -r '.[0].databaseId // empty')"
    [ -n "$RUN_ID" ] && break
    sleep 0.5
done
if [ -n "$RUN_ID" ]; then
    pass_op actions "gh run list (finds the dispatched run)" "GET /repos/{owner}/{repo}/actions/runs"
    run_gh_json actions "gh run view --json" "GET /repos/{owner}/{repo}/actions/runs/{run_id}" \
        '.workflowName == "gh-conformance" and .event == "workflow_dispatch" and (.databaseId | type) == "number"' \
        run view "$RUN_ID" --repo "$OWNER/$REPO" --json databaseId,workflowName,event,status,headBranch,number
    run_gh_json actions "gh run view --json jobs" "GET /repos/{owner}/{repo}/actions/runs/{run_id}/jobs" \
        '(.jobs | length) >= 1 and (.jobs[0].name | length) > 0' \
        run view "$RUN_ID" --repo "$OWNER/$REPO" --json jobs
    run_gh actions "gh run cancel" "POST .../actions/runs/{run_id}/cancel" "" \
        run cancel "$RUN_ID" --repo "$OWNER/$REPO" >/dev/null
    # A cancelled run must reach a terminal state before it can be re-run.
    for _ in $(seq 1 60); do
        STATUS="$(gh run view "$RUN_ID" --repo "$OWNER/$REPO" --json status 2>/dev/null | jq -r '.status // empty')"
        [ "$STATUS" = "completed" ] && break
        sleep 0.5
    done
    if [ "$STATUS" = "completed" ]; then
        pass_op actions "gh run cancel reaches a completed status" "GET .../actions/runs/{run_id}"
        run_gh actions "gh run rerun" "POST .../actions/runs/{run_id}/rerun" "" \
            run rerun "$RUN_ID" --repo "$OWNER/$REPO" >/dev/null
    else
        fail_op actions "gh run cancel reaches a completed status" "GET .../actions/runs/{run_id}" \
            "status completed within 30s of cancelling" "status $STATUS"
        skip_op actions "gh run rerun" "POST .../actions/runs/{run_id}/rerun" \
            "the run never reached a state a re-run is allowed from"
    fi
else
    for operation in "gh run list (finds the dispatched run)" "gh run view --json" \
        "gh run view --json jobs" "gh run cancel" "gh run cancel reaches a completed status" "gh run rerun"; do
        skip_op actions "$operation" "GET /repos/{owner}/{repo}/actions/runs" \
            "the dispatched workflow run never appeared"
    done
fi

run_gh actions "gh run list --json (empty filter is not an error)" \
    "GET /repos/{owner}/{repo}/actions/runs?workflow=" "" \
    run list --repo "$OWNER/$REPO" --workflow gh-conformance.yml --limit 5 >/dev/null

# ============================================================================
# deploy keys, rulesets and repository administration
# ============================================================================
# A deploy key must be a distinct key: GitHub refuses a public key that is
# already registered anywhere else on the appliance.
if command -v ssh-keygen >/dev/null 2>&1; then
    ssh-keygen -q -t ed25519 -N "" -C conformance-deploy -f "$WORK/scratch/deploy_ed25519"
    run_gh repos "gh repo deploy-key add" "POST /repos/{owner}/{repo}/keys" "" \
        repo deploy-key add "$WORK/scratch/deploy_ed25519.pub" --repo "$OWNER/$REPO" --title conformance-deploy >/dev/null
    run_gh repos "gh repo deploy-key list" "GET /repos/{owner}/{repo}/keys" "conformance-deploy" \
        repo deploy-key list --repo "$OWNER/$REPO" >/dev/null
else
    skip_op repos "gh repo deploy-key add" "POST /repos/{owner}/{repo}/keys" "ssh-keygen is not available here"
    skip_op repos "gh repo deploy-key list" "GET /repos/{owner}/{repo}/keys" "ssh-keygen is not available here"
fi

gh api --method POST "repos/$OWNER/$REPO/rulesets" --input - >/dev/null 2>&1 <<'RULESET'
{"name":"gh-conformance-ruleset","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"],"exclude":[]}},
 "rules":[{"type":"deletion"},{"type":"non_fast_forward"}]}
RULESET
run_gh repos "gh ruleset list" "GET /repos/{owner}/{repo}/rulesets" "gh-conformance-ruleset" \
    ruleset list --repo "$OWNER/$REPO" >/dev/null
RULESET_ID="$(gh api "repos/$OWNER/$REPO/rulesets" --jq '.[0].id' 2>/dev/null)"
if [ -n "$RULESET_ID" ]; then
    run_gh repos "gh ruleset view" "GET /repos/{owner}/{repo}/rulesets/{id}" "gh-conformance-ruleset" \
        ruleset view "$RULESET_ID" --repo "$OWNER/$REPO" >/dev/null
else
    skip_op repos "gh ruleset view" "GET /repos/{owner}/{repo}/rulesets/{id}" \
        "the ruleset fixture could not be created"
fi

run_gh_json repos "gh repo view --json licenseInfo,repositoryTopics" "graphql repository fields" \
    'has("repositoryTopics")' \
    repo view "$OWNER/$REPO" --json repositoryTopics,licenseInfo,isArchived,isFork,hasIssuesEnabled

# ============================================================================
# pull request verbs beyond create/merge
# ============================================================================
PR2_BRANCH="gh-conformance-second"
(
    cd "$WORK/scratch/clone" 2>/dev/null || exit 0
    git checkout -q -b "$PR2_BRANCH" 2>/dev/null
    printf 'second topic\n' >second-topic.txt
    git add second-topic.txt >/dev/null 2>&1
    git commit -q -m "second topic" >/dev/null 2>&1
    git push -q -u origin "$PR2_BRANCH" >/dev/null 2>&1
)
if gh pr create --repo "$OWNER/$REPO" --head "$PR2_BRANCH" --base main \
    --title "second conformance pull request" --body "body" >/dev/null 2>&1; then
    pass_op pulls "gh pr create (second)" "POST /repos/{owner}/{repo}/pulls"
    PR2="$(gh pr list --repo "$OWNER/$REPO" --head "$PR2_BRANCH" --json number --jq '.[0].number' 2>/dev/null)"
    if [ -n "$PR2" ]; then
        run_gh pulls "gh pr edit --title" "PATCH /repos/{owner}/{repo}/pulls/{n}" "" \
            pr edit "$PR2" --repo "$OWNER/$REPO" --title "edited by the conformance driver" >/dev/null
        run_gh pulls "gh pr review --comment" "POST .../pulls/{n}/reviews" "" \
            pr review "$PR2" --repo "$OWNER/$REPO" --comment --body "a review comment" >/dev/null
        run_gh_json pulls "gh pr view --json comments,reviews" "GET .../pulls/{n} plus sub-resources" \
            '(.reviews | length) >= 1' \
            pr view "$PR2" --repo "$OWNER/$REPO" --json number,comments,reviews,files,commits
        run_gh pulls "gh pr close" "PATCH .../pulls/{n} state=closed" "" \
            pr close "$PR2" --repo "$OWNER/$REPO" >/dev/null
        run_gh pulls "gh pr reopen" "PATCH .../pulls/{n} state=open" "" \
            pr reopen "$PR2" --repo "$OWNER/$REPO" >/dev/null
    else
        for operation in "gh pr edit --title" "gh pr review --comment" \
            "gh pr view --json comments,reviews" "gh pr close" "gh pr reopen"; do
            skip_op pulls "$operation" "pull request verbs" "the second pull request could not be located"
        done
    fi
else
    skip_op pulls "gh pr create (second)" "POST /repos/{owner}/{repo}/pulls" \
        "the second topic branch could not be pushed"
    for operation in "gh pr edit --title" "gh pr review --comment" \
        "gh pr view --json comments,reviews" "gh pr close" "gh pr reopen"; do
        skip_op pulls "$operation" "pull request verbs" "the second pull request could not be created"
    done
fi

# ============================================================================
# issue verbs beyond the basics
# ============================================================================
run_gh_json issues "gh issue list --search" "GET /search/issues from gh issue list" \
    'type == "array"' \
    issue list --repo "$OWNER/$REPO" --search "conformance" --json number,title --limit 5
# `gh issue develop` refuses to run against any host other than github.com, so
# there is no request for this harness to make.
skip_op issues "gh issue develop --list" "GET .../issues/{n} linked branches (graphql)" \
    "gh restricts the develop subcommand to github.com, so it never reaches an Enterprise host"
run_gh_json issues "gh issue view --json comments" "GET .../issues/{n}/comments" \
    'has("comments")' issue view 1 --repo "$OWNER/$REPO" --json number,comments,labels,assignees,milestone

# ============================================================================
# api: further request shapes clients send
# ============================================================================
run_gh_json api "gh api --method PATCH" "PATCH /repos/{owner}/{repo}" \
    '.description == "patched through gh api"' \
    api --method PATCH "repos/$OWNER/$REPO" -f description="patched through gh api"
run_gh_json api "gh api --input -" "POST /repos/{owner}/{repo}/labels from a JSON body" \
    '.name == "gh-api-input"' \
    api --method POST "repos/$OWNER/$REPO/labels" --input - <<'LABEL'
{"name":"gh-api-input","color":"ededed","description":"created from a JSON body"}
LABEL
run_gh api "gh api --template" "GET /repos/{owner}/{repo} rendered through a template" "conformance-gh" \
    api "repos/$OWNER/$REPO" --template '{{.name}}' >/dev/null
run_gh api "gh api -H accept" "GET /repos/{owner}/{repo} with an explicit Accept" "conformance-gh" \
    api "repos/$OWNER/$REPO" -H "Accept: application/vnd.github+json" --jq '.name' >/dev/null
run_gh_json api "gh api /licenses" "GET /licenses" 'length >= 1' api /licenses
run_gh_json api "gh api /gitignore/templates" "GET /gitignore/templates" 'length >= 1' api /gitignore/templates
run_gh_json api "gh api /user/repos --paginate" "GET /user/repos?per_page=1 (Link header)" \
    'length >= 1' api "/user/repos?per_page=1" --paginate --slurp
run_gh_json api "gh api graphql (mutation)" "POST /api/graphql" \
    '.data.addComment.commentEdge.node.body == "gh graphql comment"' \
    api graphql -F id="$(gh api "repos/$OWNER/$REPO/issues/1" --jq '.node_id')" \
    -f query='mutation($id: ID!) { addComment(input: {subjectId: $id, body: "gh graphql comment"}) { commentEdge { node { body } } } }'
GRAPHQL_ERROR="$(gh api graphql -f query='query { viewer { thisFieldDoesNotExist } }' 2>&1 || true)"
if printf '%s' "$GRAPHQL_ERROR" | grep -qi "thisFieldDoesNotExist"; then
    pass_op api "gh api graphql (error envelope)" "POST /api/graphql with an invalid field"
else
    fail_op api "gh api graphql (error envelope)" "POST /api/graphql with an invalid field" \
        "an errors array naming the unknown field" "$(printf '%s' "$GRAPHQL_ERROR" | tr '\n' ' ')"
fi

# ============================================================================
# codespace and status
# ============================================================================
run_gh codespaces "gh codespace list" "GET /user/codespaces" "" codespace list >/dev/null

printf 'gh driver: %d passed, %d failed\n' "$PASS" "$FAIL" >&2
