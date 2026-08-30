.PHONY: build run test web-build gh-test shauth-sso-test runner-sockerless-test runner-image conformance conformance-floor

build: web-build
	CGO_ENABLED=0 GOWORK=off go build -o bleephub-server ./cmd/bleephub

run: build
	./bleephub-server

# CORE-013: internal/server/dist is a generated Vite artifact that is NOT
# committed (gitignored except .gitkeep). Regenerate it locally before an
# embedded `go build`; the release image builds it the same way in its
# ui-builder stage. Nothing generated is committed, so it cannot go stale.
web-build:
	cd web && bun install --frozen-lockfile && bun run build
	rm -rf internal/server/dist/*
	cp -R web/dist/. internal/server/dist/

test:
	GOWORK=off go test -tags noui -count=1 -timeout 20m ./...

# Runs the Go scale/concurrency micro-benchmarks (contention, workflow-run
# listing, GraphQL cost, global scans, git storage). Grow the corpus with
# BLEEPHUB_BENCH_REPOS/_ISSUES/_PRS/_RUNS; add -cpu 1,2,4,8 to see lock scaling.
bench:
	GOWORK=off go test -tags noui -run '^$$' -bench . -benchmem -benchtime 5x \
		./internal/server/ ./internal/gitstore/ ./internal/graphqlapi/

# Runs the opt-in scaling ramp: concurrency vs latency percentiles + the knee.
# Tune with BLEEPHUB_SCALE_* (see internal/server/scaling_ramp_test.go).
scale:
	BLEEPHUB_SCALE=1 GOWORK=off go test -tags noui -run TestScalingRamp -v -timeout 30m ./internal/server/

# Bursts every fuzz target (all packages) for FUZZTIME each (default 15s).
fuzz:
	./scripts/fuzz.sh

# Boots a throwaway server, seeds it, and prints the API latency table from
# scripts/perf/bench.mjs. Requires the embedded binary (`make build`) and node.
perf: build
	@set -e; \
	BLEEPHUB_ADMIN_TOKEN=bleephub-admin-token-00000000000000000000 ./bleephub-server -addr :15599 -log-level warn & \
	SERVER_PID=$$!; \
	trap 'kill $$SERVER_PID 2>/dev/null || true' EXIT; \
	for _ in $$(seq 1 50); do curl -s http://localhost:15599/health >/dev/null 2>&1 && break; sleep 0.2; done; \
	bash scripts/perf/seed.sh >/dev/null; \
	node scripts/perf/bench.mjs

gh-test:
	docker buildx build --load -f Dockerfile.gh-test -t bleephub-gh-test:local .
	docker run --rm bleephub-gh-test:local

shauth-sso-test: build
	@test -n "$(SHAUTH_SOURCE_DIR)" || { echo "SHAUTH_SOURCE_DIR must point to a Shauth checkout"; exit 1; }
	@test -f "$(SHAUTH_SOURCE_DIR)/compose.yaml" || { echo "SHAUTH_SOURCE_DIR is not a Shauth checkout"; exit 1; }
	SHAUTH_SOURCE_DIR="$(SHAUTH_SOURCE_DIR)" bash scripts/test-shauth-sso.sh

runner-sockerless-test:
	@test -n "$(SOCKERLESS_ROOT)" || { echo "SOCKERLESS_ROOT must point to a Sockerless checkout"; exit 1; }
	@test -f "$(SOCKERLESS_ROOT)/go.work" || { echo "SOCKERLESS_ROOT is not a Sockerless checkout"; exit 1; }
	@docker run --rm -v /var/run/docker.sock:/var/run/docker.sock alpine:3.20 true >/dev/null 2>&1 || { echo "runner harness requires a bind-mountable Linux Docker API socket at /var/run/docker.sock"; exit 1; }
	docker buildx build --load --build-context sockerless="$(SOCKERLESS_ROOT)" -f test/runner/sockerless/Dockerfile -t bleephub-runner-sockerless:local .
	rm -rf /tmp/bleephub-runner-sockerless-data
	mkdir -p /tmp/bleephub-runner-sockerless-data
	docker run --rm --security-opt label=disable -v /var/run/docker.sock:/var/run/docker.sock -v /tmp/bleephub-runner-sockerless-data:/tmp/bleephub-runner-sockerless-data -e SOCKERLESS_HARNESS_DATA_DIR=/tmp/bleephub-runner-sockerless-data -e BLEEPHUB_BACKEND=ecs -e BLEEPHUB_TEST_FROM -e BLEEPHUB_HOLD -e BLEEPHUB_LOG_LEVEL -p 80:80 -p 3375:3375 -p 5000:4566 bleephub-runner-sockerless:local

runner-image:
	docker buildx build --load -f Dockerfile.runner -t bleephub-runner:local .

# Software development kit / command-line-interface conformance scoreboard.
# Boots a throwaway server, drives the real gh, go-github, octokit.js, PyGithub
# and git clients against it, and writes test/conformance/report/. The `gh`
# driver needs a certificate authority the client trusts: on Linux that is
# SSL_CERT_FILE and it runs natively, on macOS it needs the pinned container, so
# CONFORMANCE_GH=1 opts into Docker locally. Continuous integration runs the
# whole matrix on every push.
conformance:
	CONFORMANCE_GH=$(CONFORMANCE_GH) ./test/conformance/run.sh

# Re-record the ratchet floor after a change that is meant to move the numbers.
conformance-floor:
	CONFORMANCE_GH=$(CONFORMANCE_GH) CONFORMANCE_UPDATE_FLOOR=1 ./test/conformance/run.sh
