#!/usr/bin/env bash
# Conformance driver for git itself: clone, fetch and push over both transports,
# plus the capability negotiations real clients rely on (protocol v2, shallow,
# partial clone, large file storage).
#
# Every git subprocess runs with the same hermetic environment the Go suite's
# hermeticGitTestEnv (internal/server/git_testenv_test.go) sets, for the same
# reason: without GIT_CONFIG_NOSYSTEM plus a private GIT_CONFIG_GLOBAL, a clone
# on macOS inherits credential.helper=osxkeychain from the system and user git
# configuration and reaches into the developer's login keychain. This harness
# must never do that, so the environment is built explicitly here and every
# invocation goes through run_git.
set -uo pipefail

: "${BPH_BASE:?BPH_BASE is required}"
: "${BPH_TOKEN:?BPH_TOKEN is required}"
: "${BPH_WORK:?BPH_WORK is required}"
RESULTS="${BPH_RESULTS:-/dev/stdout}"
: >"$RESULTS"

GIT_HOME="$BPH_WORK/git-home"
SCRATCH="$BPH_WORK/git-scratch"
rm -rf "$GIT_HOME" "$SCRATCH"
mkdir -p "$GIT_HOME" "$SCRATCH"

# The hermetic environment, mirroring hermeticGitTestEnv(home).
GIT_ENV=(
    "GIT_CONFIG_NOSYSTEM=1"
    "GIT_CONFIG_GLOBAL=$GIT_HOME/.gitconfig"
    "HOME=$GIT_HOME"
    "XDG_CONFIG_HOME=$GIT_HOME"
    "GIT_TERMINAL_PROMPT=0"
    "GIT_ASKPASS="
    "GIT_AUTHOR_NAME=Conformance"
    "GIT_AUTHOR_EMAIL=conformance@bleephub.local"
    "GIT_COMMITTER_NAME=Conformance"
    "GIT_COMMITTER_EMAIL=conformance@bleephub.local"
)

record() { # domain operation status request [expected] [actual] [message]
    python3 - "$@" <<'PY' >>"$RESULTS"
import json, sys
fields = (sys.argv[1:] + ["", "", "", ""])[:7]
domain, operation, status, request, expected, actual, message = fields
print(json.dumps({
    "client": "git", "domain": domain, "operation": operation, "status": status,
    "request": request, "expected": expected, "actual": actual[:400], "message": message[:400],
}))
PY
}

PASS=0
FAIL=0

run_git() { # runs git hermetically; output in GIT_OUTPUT, status in GIT_STATUS
    local dir="$1"
    shift
    GIT_OUTPUT="$(env -i PATH="$PATH" "${GIT_ENV[@]}" git -C "$dir" "$@" 2>&1)"
    GIT_STATUS=$?
    return $GIT_STATUS
}

# expect_git runs one operation and records it. The assertion is a shell
# snippet evaluated after the command; it sees $GIT_OUTPUT and the working tree.
expect_git() { # domain operation request dir -- git args...
    local domain="$1" operation="$2" request="$3" dir="$4"
    shift 4
    if run_git "$dir" "$@"; then
        record "$domain" "$operation" pass "$request" "" "" ""
        PASS=$((PASS + 1))
        return 0
    fi
    record "$domain" "$operation" fail "$request" "git exits 0" "$GIT_OUTPUT" "git $*"
    FAIL=$((FAIL + 1))
    return 1
}

fail_op() { # domain operation request expected actual
    record "$1" "$2" fail "$3" "$4" "$5" "$5"
    FAIL=$((FAIL + 1))
}

pass_op() {
    record "$1" "$2" pass "$3" "" "" ""
    PASS=$((PASS + 1))
}

skip_op() {
    record "$1" "$2" skip "$3" "" "" "$4"
}

HOST_PORT="${BPH_BASE#http://}"
OWNER="admin"
REPO="conformance-git"
HTTP_URL="http://x-access-token:${BPH_TOKEN}@${HOST_PORT}/${OWNER}/${REPO}.git"
SSH_URL="ssh://git@${BPH_SSH_ADDR}/${OWNER}/${REPO}.git"

api() { # method path body
    local method="$1" path="$2" body="${3:-}"
    if [ -n "$body" ]; then
        curl -sS -o /dev/null -w '%{http_code}' -X "$method" \
            -H "Authorization: token $BPH_TOKEN" -H "Content-Type: application/json" \
            -d "$body" "$BPH_BASE$path"
    else
        curl -sS -o /dev/null -w '%{http_code}' -X "$method" \
            -H "Authorization: token $BPH_TOKEN" "$BPH_BASE$path"
    fi
}

# --- Fixture: an empty repository the driver pushes the first commit into ----
CREATE_STATUS="$(api POST /api/v3/user/repos "{\"name\":\"$REPO\",\"auto_init\":false}")"
if [ "$CREATE_STATUS" != "201" ]; then
    fail_op fixtures "create the repository to push into" "POST /user/repos" "201" "HTTP $CREATE_STATUS"
fi

# Register the harness public key so the Secure Shell transport can authenticate.
SSH_KEY_BODY="$(python3 - "$BPH_SSH_KEY.pub" <<'PY'
import json, sys
print(json.dumps({"title": "conformance", "key": open(sys.argv[1]).read().strip()}))
PY
)"
KEY_STATUS="$(api POST /api/v3/user/keys "$SSH_KEY_BODY")"
if [ "$KEY_STATUS" = "201" ]; then
    pass_op ssh "register a user Secure Shell key" "POST /user/keys"
else
    fail_op ssh "register a user Secure Shell key" "POST /user/keys" "201" "HTTP $KEY_STATUS"
fi

SSH_COMMAND="ssh -i $BPH_SSH_KEY -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes"
GIT_ENV+=("GIT_SSH_COMMAND=$SSH_COMMAND")

# --- Push the initial history over HTTP -------------------------------------
SEED="$SCRATCH/seed"
mkdir -p "$SEED"
run_git "$SEED" init -q -b main
printf 'conformance\n' >"$SEED/README.md"
run_git "$SEED" add README.md
run_git "$SEED" commit -q -m "initial commit"
run_git "$SEED" remote add origin "$HTTP_URL"

expect_git http "push (initial, http)" "POST /{owner}/{repo}.git/git-receive-pack" "$SEED" push -q origin main

# A second commit so fetch/pull have something to move to.
printf 'second\n' >"$SEED/second.txt"
run_git "$SEED" add second.txt
run_git "$SEED" commit -q -m "second commit"
expect_git http "push (fast-forward, http)" "POST /{owner}/{repo}.git/git-receive-pack" "$SEED" push -q origin main

expect_git http "push tag" "POST git-receive-pack (refs/tags)" "$SEED" tag conformance-tag
expect_git http "push --tags" "POST git-receive-pack (refs/tags)" "$SEED" push -q origin --tags

# --- Discovery ---------------------------------------------------------------
if run_git "$SCRATCH" ls-remote "$HTTP_URL"; then
    if printf '%s' "$GIT_OUTPUT" | grep -q "refs/heads/main"; then
        pass_op http "ls-remote" "GET /{owner}/{repo}.git/info/refs?service=git-upload-pack"
    else
        fail_op http "ls-remote" "GET /{owner}/{repo}.git/info/refs" "refs/heads/main advertised" "$GIT_OUTPUT"
    fi
else
    fail_op http "ls-remote" "GET /{owner}/{repo}.git/info/refs" "git exits 0" "$GIT_OUTPUT"
fi

# --- Clone matrix ------------------------------------------------------------
clone_case() { # operation request dir extra args...
    local operation="$1" request="$2" target="$3"
    shift 3
    rm -rf "$target"
    if run_git "$SCRATCH" clone -q "$@" >/dev/null; then
        if [ -f "$target/README.md" ]; then
            pass_op "${CLONE_DOMAIN}" "$operation" "$request"
            return 0
        fi
        fail_op "${CLONE_DOMAIN}" "$operation" "$request" "a working tree containing README.md" "clone produced no README.md"
        return 1
    fi
    fail_op "${CLONE_DOMAIN}" "$operation" "$request" "git exits 0" "$GIT_OUTPUT"
    return 1
}

CLONE_DOMAIN=http
clone_case "clone (http)" "GET info/refs + POST git-upload-pack" "$SCRATCH/clone-http" "$HTTP_URL" clone-http
clone_case "clone --depth 1 (shallow)" "git-upload-pack with deepen 1" "$SCRATCH/clone-shallow" --depth 1 "$HTTP_URL" clone-shallow
clone_case "clone --filter=blob:none (partial)" "git-upload-pack with filter blob:none" "$SCRATCH/clone-partial" --filter=blob:none "$HTTP_URL" clone-partial
clone_case "clone --single-branch" "git-upload-pack with want-ref" "$SCRATCH/clone-single" --single-branch --branch main "$HTTP_URL" clone-single
# A bare clone has no working tree, so it is asserted on its refs instead.
rm -rf "$SCRATCH/clone-bare.git"
if run_git "$SCRATCH" clone -q --bare "$HTTP_URL" clone-bare.git &&
    run_git "$SCRATCH/clone-bare.git" rev-parse refs/heads/main; then
    pass_op http "clone --bare" "git-upload-pack"
else
    fail_op http "clone --bare" "git-upload-pack" "refs/heads/main present in the bare clone" "$GIT_OUTPUT"
fi

# Shallow-specific follow-ups: a shallow clone must be able to deepen.
if [ -d "$SCRATCH/clone-shallow" ]; then
    if run_git "$SCRATCH/clone-shallow" rev-list --count HEAD && [ "$GIT_OUTPUT" = "1" ]; then
        pass_op http "shallow clone really is depth 1"  "git-upload-pack deepen"
    else
        fail_op http "shallow clone really is depth 1" "git-upload-pack deepen" "1 commit in the shallow clone" "$GIT_OUTPUT commits"
    fi
    expect_git http "fetch --unshallow" "git-upload-pack deepen-since/unshallow" "$SCRATCH/clone-shallow" fetch -q --unshallow
fi

# Partial clone must lazily fetch the blob it did not download.
if [ -d "$SCRATCH/clone-partial" ]; then
    if run_git "$SCRATCH/clone-partial" cat-file -p HEAD:README.md && printf '%s' "$GIT_OUTPUT" | grep -q conformance; then
        pass_op http "partial clone lazy blob fetch" "git-upload-pack filter + follow-up fetch"
    else
        fail_op http "partial clone lazy blob fetch" "git-upload-pack filter + follow-up fetch" "the blob is fetched on demand" "$GIT_OUTPUT"
    fi
fi

# --- Protocol version 2 ------------------------------------------------------
GIT_OUTPUT="$(env -i PATH="$PATH" "${GIT_ENV[@]}" GIT_TRACE_PACKET=1 git -c protocol.version=2 ls-remote "$HTTP_URL" 2>&1)"
if printf '%s' "$GIT_OUTPUT" | grep -q "version 2"; then
    pass_op http "protocol v2 negotiated" "GET info/refs with Git-Protocol: version=2"
else
    fail_op http "protocol v2 negotiated" "GET info/refs with Git-Protocol: version=2" \
        "the server answers with version 2" "$(printf '%s' "$GIT_OUTPUT" | tail -3 | tr '\n' ' ')"
fi

# --- Fetch and pull ----------------------------------------------------------
if [ -d "$SCRATCH/clone-http" ]; then
    printf 'third\n' >"$SEED/third.txt"
    run_git "$SEED" add third.txt
    run_git "$SEED" commit -q -m "third commit"
    run_git "$SEED" push -q origin main
    expect_git http "fetch" "POST git-upload-pack" "$SCRATCH/clone-http" fetch -q origin
    expect_git http "pull --ff-only" "POST git-upload-pack" "$SCRATCH/clone-http" pull -q --ff-only origin main
    expect_git http "fetch --tags" "POST git-upload-pack" "$SCRATCH/clone-http" fetch -q --tags origin

    # Push from the clone, exercising the credentials the clone recorded.
    printf 'from the clone\n' >"$SCRATCH/clone-http/clone.txt"
    run_git "$SCRATCH/clone-http" add clone.txt
    run_git "$SCRATCH/clone-http" commit -q -m "push from the clone"
    expect_git http "push from a clone" "POST git-receive-pack" "$SCRATCH/clone-http" push -q origin main

    # Branch create and delete over the wire.
    run_git "$SCRATCH/clone-http" checkout -q -b conformance-branch
    expect_git http "push a new branch" "POST git-receive-pack (create)" "$SCRATCH/clone-http" push -q origin conformance-branch
    expect_git http "push --delete a branch" "POST git-receive-pack (delete)" "$SCRATCH/clone-http" push -q origin --delete conformance-branch
fi

# --- Secure Shell transport --------------------------------------------------
CLONE_DOMAIN=ssh
clone_case "clone (ssh)" "ssh git-upload-pack" "$SCRATCH/clone-ssh" "$SSH_URL" clone-ssh
if [ -d "$SCRATCH/clone-ssh" ]; then
    printf 'over ssh\n' >"$SCRATCH/clone-ssh/ssh.txt"
    run_git "$SCRATCH/clone-ssh" add ssh.txt
    run_git "$SCRATCH/clone-ssh" commit -q -m "push over ssh"
    expect_git ssh "push (ssh)" "ssh git-receive-pack" "$SCRATCH/clone-ssh" push -q origin main
    expect_git ssh "fetch (ssh)" "ssh git-upload-pack" "$SCRATCH/clone-ssh" fetch -q origin
fi
if run_git "$SCRATCH" ls-remote "$SSH_URL"; then
    pass_op ssh "ls-remote (ssh)" "ssh git-upload-pack advertisement"
else
    fail_op ssh "ls-remote (ssh)" "ssh git-upload-pack advertisement" "git exits 0" "$GIT_OUTPUT"
fi

# --- Large file storage ------------------------------------------------------
if command -v git-lfs >/dev/null 2>&1; then
    LFS="$SCRATCH/lfs"
    rm -rf "$LFS"
    # `git lfs install` is the one-time setup every git-lfs user runs: it writes
    # the clean/smudge filters into the git configuration. This harness's
    # configuration is the hermetic GIT_CONFIG_GLOBAL, so without this the
    # filters exist nowhere and a fresh clone checks out pointer files no matter
    # what the server serves — which would measure the harness, not Bleephub.
    env -i PATH="$PATH" "${GIT_ENV[@]}" git lfs install >/dev/null 2>&1
    if run_git "$SCRATCH" clone -q "$HTTP_URL" lfs; then
        env -i PATH="$PATH" "${GIT_ENV[@]}" git -C "$LFS" lfs install --local >/dev/null 2>&1
        run_git "$LFS" lfs track "*.bin"
        head -c 2048 /dev/urandom >"$LFS/blob.bin"
        run_git "$LFS" add .gitattributes blob.bin
        run_git "$LFS" commit -q -m "add a large file storage object"
        if run_git "$LFS" push -q origin main; then
            pass_op lfs "push a large file storage object" "POST /{owner}/{repo}.git/info/lfs/objects/batch"
        else
            fail_op lfs "push a large file storage object" "POST /{owner}/{repo}.git/info/lfs/objects/batch" "git exits 0" "$GIT_OUTPUT"
        fi
        rm -rf "$SCRATCH/lfs-clone"
        if run_git "$SCRATCH" clone -q "$HTTP_URL" lfs-clone && [ -s "$SCRATCH/lfs-clone/blob.bin" ]; then
            SIZE="$(wc -c <"$SCRATCH/lfs-clone/blob.bin" | tr -d ' ')"
            if [ "$SIZE" = "2048" ]; then
                pass_op lfs "clone smudges the large file storage object" "POST info/lfs/objects/batch (download)"
            else
                fail_op lfs "clone smudges the large file storage object" "POST info/lfs/objects/batch (download)" \
                    "a 2048-byte file" "$SIZE bytes (the pointer file was not replaced)"
            fi
        else
            fail_op lfs "clone smudges the large file storage object" "POST info/lfs/objects/batch (download)" "git exits 0" "$GIT_OUTPUT"
        fi
    else
        fail_op lfs "clone for the large file storage fixture" "git-upload-pack" "git exits 0" "$GIT_OUTPUT"
    fi
else
    skip_op lfs "push a large file storage object" "POST info/lfs/objects/batch" "git-lfs is not installed on this machine"
    skip_op lfs "clone smudges the large file storage object" "POST info/lfs/objects/batch" "git-lfs is not installed on this machine"
fi


# --- The wiki is a second git repository -------------------------------------
# GitHub gives a client no API for wiki content: the wiki of <repo> is the git
# repository <repo>.wiki, cloned and pushed over the same transports. A client
# that cannot push to it cannot edit a wiki at all.
api PATCH "/api/v3/repos/$OWNER/$REPO" '{"has_wiki":true}' >/dev/null
WIKI_URL="http://x-access-token:${BPH_TOKEN}@${HOST_PORT}/${OWNER}/${REPO}.wiki.git"
rm -rf "$SCRATCH/wiki"
mkdir -p "$SCRATCH/wiki"
run_git "$SCRATCH/wiki" init -q -b main
printf '# Conformance\n\nA wiki page pushed by the conformance harness.\n' >"$SCRATCH/wiki/Home.md"
run_git "$SCRATCH/wiki" add Home.md
run_git "$SCRATCH/wiki" commit -q -m "add the wiki home page"
if run_git "$SCRATCH/wiki" push -q "$WIKI_URL" main; then
    pass_op wiki "push a wiki page" "git-receive-pack /{owner}/{repo}.wiki.git"
    rm -rf "$SCRATCH/wiki-clone"
    if run_git "$SCRATCH" clone -q "$WIKI_URL" wiki-clone && [ -f "$SCRATCH/wiki-clone/Home.md" ]; then
        pass_op wiki "clone the wiki repository" "git-upload-pack /{owner}/{repo}.wiki.git"
    else
        fail_op wiki "clone the wiki repository" "git-upload-pack /{owner}/{repo}.wiki.git" \
            "a clone containing Home.md" "$GIT_OUTPUT"
    fi
else
    fail_op wiki "push a wiki page" "git-receive-pack /{owner}/{repo}.wiki.git" \
        "git exits 0" "$GIT_OUTPUT"
    skip_op wiki "clone the wiki repository" "git-upload-pack /{owner}/{repo}.wiki.git" \
        "nothing was pushed to the wiki to clone back"
fi

# --- A renamed repository keeps serving its old git remote --------------------
# GitHub redirects the git endpoints of a renamed repository so that every clone
# already on a developer's machine keeps working. A client with a stale remote
# gets a fatal error instead.
RENAME_REPO="conformance-git-rename"
RENAME_STATUS="$(api POST /api/v3/user/repos "{\"name\":\"$RENAME_REPO\",\"auto_init\":true}")"
if [ "$RENAME_STATUS" = "201" ]; then
    OLD_URL="http://x-access-token:${BPH_TOKEN}@${HOST_PORT}/${OWNER}/${RENAME_REPO}.git"
    rm -rf "$SCRATCH/rename-clone"
    run_git "$SCRATCH" clone -q "$OLD_URL" rename-clone
    api PATCH "/api/v3/repos/$OWNER/$RENAME_REPO" "{\"name\":\"$RENAME_REPO-new\"}" >/dev/null
    if run_git "$SCRATCH/rename-clone" fetch -q origin; then
        pass_op http "fetch through a renamed repository's old remote" \
            "git-upload-pack /{owner}/{old_name}.git after a rename"
    else
        fail_op http "fetch through a renamed repository's old remote" \
            "git-upload-pack /{owner}/{old_name}.git after a rename" \
            "the old remote to keep working through a redirect" "$GIT_OUTPUT"
    fi
else
    skip_op http "fetch through a renamed repository's old remote" \
        "git-upload-pack /{owner}/{old_name}.git after a rename" \
        "the rename fixture repository could not be created"
fi

# --- Reference advertisement details clients parse ---------------------------
if run_git "$SCRATCH" ls-remote --symref "$HTTP_URL" HEAD; then
    if printf '%s' "$GIT_OUTPUT" | grep -q "^ref: refs/heads/"; then
        pass_op http "ls-remote --symref advertises HEAD" "git-upload-pack (symref capability)"
    else
        fail_op http "ls-remote --symref advertises HEAD" "git-upload-pack (symref capability)" \
            "a 'ref: refs/heads/<branch> HEAD' line" "$GIT_OUTPUT"
    fi
else
    fail_op http "ls-remote --symref advertises HEAD" "git-upload-pack (symref capability)" \
        "git exits 0" "$GIT_OUTPUT"
fi

# A push that is not a fast forward must be refused, or a client's safety net —
# the whole point of --force being opt-in — is gone.
rm -rf "$SCRATCH/nonff"
if run_git "$SCRATCH" clone -q "$HTTP_URL" nonff; then
    run_git "$SCRATCH/nonff" reset -q --hard HEAD~1
    printf 'divergent\n' >"$SCRATCH/nonff/divergent.txt"
    run_git "$SCRATCH/nonff" add divergent.txt
    run_git "$SCRATCH/nonff" commit -q -m "a divergent commit"
    if run_git "$SCRATCH/nonff" push -q origin main; then
        fail_op http "a non-fast-forward push is refused" "git-receive-pack" \
            "a rejected push" "the push was accepted, so a client can silently discard commits"
    else
        pass_op http "a non-fast-forward push is refused" "git-receive-pack"
        if run_git "$SCRATCH/nonff" push -q --force origin main; then
            pass_op http "push --force overrides the refusal" "git-receive-pack (forced update)"
        else
            fail_op http "push --force overrides the refusal" "git-receive-pack (forced update)" \
                "git exits 0" "$GIT_OUTPUT"
        fi
    fi
else
    skip_op http "a non-fast-forward push is refused" "git-receive-pack" "the fixture clone failed"
    skip_op http "push --force overrides the refusal" "git-receive-pack (forced update)" "the fixture clone failed"
fi

printf 'git driver: %d passed, %d failed\n' "$PASS" "$FAIL" >&2
