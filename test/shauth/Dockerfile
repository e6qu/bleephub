# Amazon ECR Public's copy of Docker's official image, pinned to the digest
# Docker Hub serves: Docker Hub limits anonymous pulls per address, and the
# shared CI runners exhaust it.
FROM public.ecr.aws/docker/library/golang:1.26.9-alpine@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade AS builder
# Re-enable toolchain auto-download (the golang image pins GOTOOLCHAIN=local) so
# the image builds with whatever toolchain go.mod requires.
ENV GOTOOLCHAIN=auto
ARG BLEEPHUB_TEST_REVISION
WORKDIR /src
COPY go.mod go.sum ./
# go.mod replaces the gitstore and gcsclient libraries with ./gitstore and
# ./gcsclient, so the module graph cannot be loaded, nor the server built,
# without them.
COPY gitstore ./gitstore
COPY gcsclient ./gcsclient
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags="-X main.version=sso-contract -X main.commit=${BLEEPHUB_TEST_REVISION} -X main.publishedAt=2026-07-21T00:00:00Z" \
      -o /bleephub ./cmd/bleephub

# Amazon ECR Public's copy of Docker's official image, pinned to the digest
# Docker Hub serves: Docker Hub limits anonymous pulls per address, and the
# shared CI runners exhaust it.
FROM public.ecr.aws/docker/library/alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
RUN apk add --no-cache ca-certificates
COPY --from=builder /bleephub /usr/local/bin/bleephub
ENTRYPOINT ["bleephub"]
