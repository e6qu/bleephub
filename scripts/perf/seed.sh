#!/usr/bin/env bash
# Seeds a local Bleephub instance with a realistic org/repo/issues/PRs data set
# for UX review and the perf benchmark (scripts/perf/bench.mjs). Idempotence is
# not attempted: run against a fresh server.
set -e
B="${BLEEPHUB_BASE:-http://localhost:15599}"
T="Authorization: token ${BLEEPHUB_TOKEN:-bleephub-admin-token-00000000000000000000}"
api() { # method path json
  curl -s -o /dev/null -w "%{http_code} $2\n" -X "$1" -H "$T" -H "Content-Type: application/json" "$B$2" ${3:+-d "$3"}
}
apiq() { curl -s -X "$1" -H "$T" -H "Content-Type: application/json" "$B$2" ${3:+-d "$3"}; }

b64() { printf '%s' "$1" | base64; }

# second user (GHES site-admin create), org
api POST /api/v3/admin/users '{"login":"octocat","email":"octocat@bleephub.local"}'
api POST /api/v3/admin/organizations '{"login":"acme","admin":"admin","profile_name":"Acme Corp"}'

# main repo
api POST /api/v3/user/repos '{"name":"hello-app","description":"A sample web application for UX review","has_wiki":true,"has_issues":true,"has_discussions":true}'

put_file() { # path message content branch
  local d="{\"message\":$(printf '%s' "$2" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))'),\"content\":\"$(b64 "$3")\"${4:+,\"branch\":\"$4\"}}"
  curl -s -o /dev/null -w "%{http_code} put $1\n" -X PUT -H "$T" -H "Content-Type: application/json" "$B/api/v3/repos/admin/hello-app/contents/$1" -d "$d"
}

put_file README.md "Initial commit" "# hello-app

A sample application used to review Bleephub's UX.

## Features
- REST API server
- Web dashboard
- CLI tooling

\`\`\`bash
make build && ./hello-app serve
\`\`\`

See [docs](docs/) for details."
put_file .gitignore "Add gitignore" "node_modules/
dist/
*.log"
put_file src/main.go "Add entrypoint" 'package main

import "fmt"

func main() {
	fmt.Println("hello-app starting")
	serve()
}'
put_file src/server.go "Add HTTP server" 'package main

import "net/http"

func serve() {
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	http.ListenAndServe(":8080", nil)
}'
put_file src/handlers/users.go "Add users handler" 'package handlers

// ListUsers returns all users.
func ListUsers() []string { return []string{"a", "b"} }'
put_file docs/architecture.md "Document architecture" "# Architecture

The app has three layers: API, service, storage."
put_file docs/deploy.md "Document deployment" "# Deploying

Use the release image."
put_file Makefile "Add Makefile" 'build:
	go build -o hello-app ./src'
# some churn commits on README
for i in 1 2 3; do
  sha=$(apiq GET /api/v3/repos/admin/hello-app/contents/README.md | python3 -c 'import json,sys;print(json.load(sys.stdin)["sha"])')
  curl -s -o /dev/null -w "%{http_code} readme-rev $i\n" -X PUT -H "$T" -H "Content-Type: application/json" \
    "$B/api/v3/repos/admin/hello-app/contents/README.md" \
    -d "{\"message\":\"docs: refresh README (rev $i)\",\"content\":\"$(b64 "# hello-app (rev $i)

A sample application used to review Bleephub's UX. Revision $i.

## Features
- REST API server
- Web dashboard
- CLI tooling")\",\"sha\":\"$sha\"}"
done

# labels + milestones
api POST /api/v3/repos/admin/hello-app/labels '{"name":"bug","color":"d73a4a","description":"Something is broken"}'
api POST /api/v3/repos/admin/hello-app/labels '{"name":"enhancement","color":"a2eeef","description":"New feature"}'
api POST /api/v3/repos/admin/hello-app/labels '{"name":"docs","color":"0075ca","description":"Documentation"}'
api POST /api/v3/repos/admin/hello-app/milestones '{"title":"v1.0","description":"First stable release","due_on":"2026-10-01T00:00:00Z"}'

# issues at scale
for i in $(seq 1 120); do
  lab='"bug"'; [ $((i % 3)) -eq 0 ] && lab='"enhancement"'; [ $((i % 5)) -eq 0 ] && lab='"docs"'
  apiq POST /api/v3/repos/admin/hello-app/issues "{\"title\":\"Issue $i: intermittent failure in module $((i % 7))\",\"body\":\"Steps to reproduce issue **$i**:\\n\\n1. run the server\\n2. hit /health\\n3. observe\\n\\nExpected: ok. Actual: error $i.\",\"labels\":[$lab],\"milestone\":1}" > /dev/null
done
echo "120 issues created"
# comments + close a third
for i in 5 10 15 20 25; do
  apiq POST /api/v3/repos/admin/hello-app/issues/$i/comments "{\"body\":\"Confirmed on main — bisected to the *handlers* package. cc @admin\"}" > /dev/null
done
for i in $(seq 3 3 60); do
  apiq PATCH /api/v3/repos/admin/hello-app/issues/$i '{"state":"closed"}' > /dev/null
done
echo "comments + closures done"

# PR: branch + change + open
mainsha=$(apiq GET /api/v3/repos/admin/hello-app/git/ref/heads/main | python3 -c 'import json,sys;print(json.load(sys.stdin)["object"]["sha"])')
api POST /api/v3/repos/admin/hello-app/git/refs "{\"ref\":\"refs/heads/feature/metrics\",\"sha\":\"$mainsha\"}"
put_file src/metrics.go "Add metrics endpoint" 'package main

// Metrics exposes counters.
func Metrics() map[string]int { return map[string]int{"requests": 0} }' feature/metrics
put_file docs/metrics.md "Document metrics" "# Metrics

Exposes request counters at /metrics." feature/metrics
api POST /api/v3/repos/admin/hello-app/pulls '{"title":"Add metrics endpoint","head":"feature/metrics","base":"main","body":"Adds a `/metrics` endpoint with request counters.\n\n- new `Metrics()` helper\n- docs page"}'

api POST /api/v3/repos/admin/hello-app/git/refs "{\"ref\":\"refs/heads/fix/health-race\",\"sha\":\"$mainsha\"}"
put_file src/server.go2 "Fix health race" 'package main
// placeholder fix file' fix/health-race
api POST /api/v3/repos/admin/hello-app/pulls '{"title":"Fix race in health handler","head":"fix/health-race","base":"main","body":"Fixes a data race when /health is hit during shutdown.","draft":true}'

# release + tag
api POST /api/v3/repos/admin/hello-app/releases "{\"tag_name\":\"v0.9.0\",\"name\":\"v0.9.0 — beta\",\"body\":\"## Highlights\\n- first beta\\n- metrics groundwork\",\"target_commitish\":\"main\"}"

# org repo + team
api POST /api/v3/orgs/acme/repos '{"name":"platform","description":"Acme platform services"}'
api POST /api/v3/orgs/acme/teams '{"name":"core","description":"Core team"}'

# gist
api POST /api/v3/gists '{"description":"handy snippet","public":true,"files":{"snippet.sh":{"content":"#!/bin/sh\necho hi"}}}'

# star + watch for social surfaces
api PUT /api/v3/user/starred/admin/hello-app ''
echo SEED DONE
