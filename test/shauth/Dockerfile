FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /bleephub ./cmd/bleephub

FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=builder /bleephub /usr/local/bin/bleephub
ENTRYPOINT ["bleephub"]
