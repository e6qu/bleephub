# Amazon ECR Public's copy of Docker's official image, pinned to the digest
# Docker Hub serves: Docker Hub limits anonymous pulls per address, and the
# shared CI runners exhaust it.
FROM public.ecr.aws/docker/library/golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder
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
FROM public.ecr.aws/docker/library/alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates
COPY --from=builder /bleephub /usr/local/bin/bleephub
ENTRYPOINT ["bleephub"]
