# Disposable SSO-contract test harness, never a published runtime image.
# trivy:ignore:AVD-DS-0002 # throwaway test container
# trivy:ignore:AVD-DS-0026 # a finite test job, not a health-checked service

FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
# Re-enable toolchain auto-download (the golang image pins GOTOOLCHAIN=local) so
# this 1.26.5 image builds with the go1.26.6 toolchain go.mod requires.
ENV GOTOOLCHAIN=auto
ARG BLEEPHUB_TEST_REVISION
WORKDIR /src
COPY go.mod go.sum ./
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
