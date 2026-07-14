.PHONY: build run test web-build gh-test runner-sockerless-test

build: web-build
	CGO_ENABLED=0 go build -o bleephub-server ./cmd/bleephub

run: build
	./bleephub-server

web-build:
	cd web && bun install --frozen-lockfile && bun run build
	cp -R web/dist/. internal/server/dist/

test:
	go test -tags noui -count=1 -timeout 8m ./...

gh-test:
	docker buildx build --load -f Dockerfile.gh-test -t bleephub-gh-test:local .
	docker run --rm bleephub-gh-test:local

runner-sockerless-test:
	@test -n "$(SOCKERLESS_ROOT)" || { echo "SOCKERLESS_ROOT must point to a Sockerless checkout"; exit 1; }
	@test -f "$(SOCKERLESS_ROOT)/go.work" || { echo "SOCKERLESS_ROOT is not a Sockerless checkout"; exit 1; }
	@docker run --rm -v /var/run/docker.sock:/var/run/docker.sock alpine:3.20 true >/dev/null 2>&1 || { echo "runner harness requires a bind-mountable Linux Docker API socket at /var/run/docker.sock"; exit 1; }
	docker buildx build --load --build-context sockerless="$(SOCKERLESS_ROOT)" -f test/runner/sockerless/Dockerfile -t bleephub-runner-sockerless:local .
	rm -rf /tmp/bleephub-runner-sockerless-data
	mkdir -p /tmp/bleephub-runner-sockerless-data
	docker run --rm --security-opt label=disable -v /var/run/docker.sock:/var/run/docker.sock -v /tmp/bleephub-runner-sockerless-data:/tmp/bleephub-runner-sockerless-data -e SOCKERLESS_HARNESS_DATA_DIR=/tmp/bleephub-runner-sockerless-data -e BLEEPHUB_BACKEND=ecs -p 80:80 -p 3375:3375 -p 5000:4566 bleephub-runner-sockerless:local
