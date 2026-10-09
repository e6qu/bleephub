FROM golang:1.26.9-alpine@sha256:cdfd4fe2da6b225d8b40c6b7a105736e548e83ff56d5d8f9394446eeb5eb84e0 AS builder
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

FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
RUN apk add --no-cache ca-certificates
COPY --from=builder /bleephub /usr/local/bin/bleephub
ENTRYPOINT ["bleephub"]
