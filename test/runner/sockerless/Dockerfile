ARG RUNNER_VERSION=2.336.0
ARG RUNNER_SOURCE_COMMIT=98aabcd429c4e8402406c56ce2d26387fed3b9ce
ARG DOTNET_SDK_VERSION=8.0.423
ARG DOTNET_RUNTIME_VERSION=8.0.29

FROM --platform=$BUILDPLATFORM mcr.microsoft.com/dotnet/sdk:8.0.423-bookworm-slim@sha256:3c0edbfe1549dd93fb789dc96299a40df865ad7bffefcaf38e8c05940686d641 AS actions-runner-builder
ARG TARGETARCH
ARG RUNNER_VERSION
ARG RUNNER_SOURCE_COMMIT
ARG DOTNET_SDK_VERSION
ARG DOTNET_RUNTIME_VERSION
ENV TARGETARCH=$TARGETARCH \
    RUNNER_VERSION=$RUNNER_VERSION \
    RUNNER_SOURCE_COMMIT=$RUNNER_SOURCE_COMMIT \
    DOTNET_SDK_VERSION=$DOTNET_SDK_VERSION \
    DOTNET_RUNTIME_VERSION=$DOTNET_RUNTIME_VERSION
COPY docker/runner-nuget-locks/ /runner-nuget-locks/
COPY docker/build-actions-runner.sh /usr/local/bin/build-actions-runner
RUN bash /usr/local/bin/build-actions-runner

FROM golang:1.26.5@sha256:2005724102f45917a63e9d092fc0e4ea56ea575048ce147caad5f5f61502c365 AS builder

WORKDIR /src

# --- Build the cloud simulators (job containers run as sim cloud tasks) ---
# One image serves multiple backends; run-integration.sh picks via
# BLEEPHUB_BACKEND (ecs → AWS sim, aca → Azure sim, …).
COPY --from=sockerless simulators/realexec/ /realexec/
COPY --from=sockerless simulators/ui-auth/ /ui-auth/
COPY --from=sockerless simulators/aws/ /sim-aws/
RUN cd /sim-aws && GOWORK=off go build -tags noui -o /out/simulator-aws .
COPY --from=sockerless simulators/azure/ /sim-azure/
RUN cd /sim-azure && GOWORK=off go build -tags noui -o /out/simulator-azure .
COPY --from=sockerless simulators/gcp/ /sim-gcp/
RUN cd /sim-gcp && GOWORK=off go build -tags noui -o /out/simulator-gcp .

# --- Build the backends + agent (the runner's DOCKER_HOST target) ---
COPY --from=sockerless api/ api/
COPY --from=sockerless agent/ agent/
COPY --from=sockerless backends/core/ backends/core/
COPY --from=sockerless backends/aws-common/ backends/aws-common/
COPY --from=sockerless backends/ecs/ backends/ecs/
RUN cd backends/ecs && GOWORK=off go mod download && \
    GOWORK=off go build -tags noui -o /out/sockerless-backend-ecs ./cmd/sockerless-backend-ecs
COPY --from=sockerless backends/azure-common/ backends/azure-common/
COPY --from=sockerless backends/aca/ backends/aca/
RUN cd backends/aca && GOWORK=off go mod download && \
    GOWORK=off go build -tags noui -o /out/sockerless-backend-aca ./cmd/sockerless-backend-aca
COPY --from=sockerless backends/azure-functions/ backends/azure-functions/
RUN cd backends/azure-functions && GOWORK=off go mod download && \
    GOWORK=off go build -tags noui -o /out/sockerless-backend-azf ./cmd/sockerless-backend-azf
COPY --from=sockerless backends/gcp-common/ backends/gcp-common/
COPY --from=sockerless backends/cloudrun/ backends/cloudrun/
RUN cd backends/cloudrun && GOWORK=off go mod download && \
    GOWORK=off go build -tags noui -o /out/sockerless-backend-cloudrun ./cmd/sockerless-backend-cloudrun
COPY --from=sockerless backends/cloudrun-functions/ backends/cloudrun-functions/
RUN cd backends/cloudrun-functions && GOWORK=off go mod download && \
    GOWORK=off go build -tags noui -o /out/sockerless-backend-gcf ./cmd/sockerless-backend-gcf
# CGO_ENABLED=0: the agent and the reverse-agent bootstrap are injected
# into arbitrary workload base images (alpine/musl, distroless, scratch),
# so they must be statically linked — a glibc-dynamic binary fails to exec
# on musl with "No such file or directory" (the missing dynamic loader).
RUN cd agent && GOWORK=off go mod download && \
    CGO_ENABLED=0 GOWORK=off go build -o /out/sockerless-agent ./cmd/sockerless-agent && \
    CGO_ENABLED=0 GOWORK=off go build -o /out/sockerless-cloudrun-bootstrap ./cmd/sockerless-cloudrun-bootstrap && \
    CGO_ENABLED=0 GOWORK=off go build -o /out/sockerless-gcf-bootstrap ./cmd/sockerless-gcf-bootstrap && \
    CGO_ENABLED=0 GOWORK=off go build -o /out/sockerless-azf-bootstrap ./cmd/sockerless-azf-bootstrap

# --- Build the dispatcher (runner-as-task control plane) ---
COPY --from=sockerless github-runner-dispatcher-aws/ github-runner-dispatcher-aws/
RUN cd github-runner-dispatcher-aws && GOWORK=off go mod download && \
    GOWORK=off go build -o /out/github-runner-dispatcher-aws ./cmd/github-runner-dispatcher-aws

# --- Build Bleephub (last: the hottest layer while iterating) ---
COPY . /bleephub/
# noui: the runner harness exercises the protocol surface; the UI dist
# is a gitignored build artifact that doesn't exist in CI checkouts.
RUN cd /bleephub && GOWORK=off go build -tags noui -o /out/bleephub ./cmd/bleephub

# --- Runtime stage ---
FROM ubuntu:26.04@sha256:3131b4cc82a783df6c9df078f86e01819a13594b865c2cad47bd1bca2b7063bb

# python3-nacl: the harness seals secrets with the real libsodium
# sealed-box contract before PUTting them.
# docker.io: client CLI for the dispatcher's `docker run` shell-outs
# and the harness's spawn-image build via the mounted host socket.
# openssl: the Google Cloud cells generate service-account RSA keys for the
# simulator service-account JSON (token_uri -> simulator /token) the backend's
# Google clients sign.
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl ca-certificates jq git libicu78 libssl3 openssl python3 python3-nacl docker.io && \
    rm -rf /var/lib/apt/lists/*

# Download the same checksummed runner payload used by Dockerfile.runner.
# The dependency-age gate rejects any drift between these two pin sets.
ARG TARGETARCH
ARG RUNNER_VERSION
ARG RUNNER_SHA256_AMD64=04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d
ARG RUNNER_SHA256_ARM64=58b758e420b87093fbd4bfddd368074960053e2f1388f01848c82624b90f27d1
ARG DOTNET_RUNTIME_VERSION
RUN mkdir -p /runner && \
    ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
        amd64) RUNNER_ARCH=x64; RUNNER_SHA256="$RUNNER_SHA256_AMD64" ;; \
        arm64) RUNNER_ARCH=arm64; RUNNER_SHA256="$RUNNER_SHA256_ARM64" ;; \
        *) echo "Unsupported arch: $ARCH"; exit 1 ;; \
    esac && \
    curl --fail --location --silent --show-error \
      --output /tmp/actions-runner.tar.gz \
      "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" && \
    printf '%s  %s\n' "$RUNNER_SHA256" /tmp/actions-runner.tar.gz | sha256sum --check --strict - && \
    tar --extract --gzip --file /tmp/actions-runner.tar.gz --directory /runner && \
    rm /tmp/actions-runner.tar.gz

COPY --from=actions-runner-builder /runner-src/_layout/bin/ /tmp/actions-runner-bin/

RUN ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
        amd64) RUNNER_ARCH=x64 ;; \
        arm64) RUNNER_ARCH=arm64 ;; \
        *) echo "Unsupported arch: $ARCH"; exit 1 ;; \
    esac && \
    rm -rf /runner/bin && \
    mv /tmp/actions-runner-bin /runner/bin && \
    grep -q "Microsoft.NETCore.App.Runtime.linux-${RUNNER_ARCH}/${DOTNET_RUNTIME_VERSION}" \
      /runner/bin/Runner.Listener.deps.json && \
    grep -q "\"version\": \"${DOTNET_RUNTIME_VERSION}\"" \
      /runner/bin/Runner.Listener.runtimeconfig.json && \
    rm -rf \
      /runner/externals/node20/lib/node_modules \
      /runner/externals/node24/lib/node_modules && \
    rm -f \
      /runner/externals/node20/bin/corepack \
      /runner/externals/node20/bin/npm \
      /runner/externals/node20/bin/npx \
      /runner/externals/node24/bin/corepack \
      /runner/externals/node24/bin/npm \
      /runner/externals/node24/bin/npx && \
    /runner/externals/node20/bin/node --version && \
    /runner/externals/node24/bin/node --version && \
    /runner/bin/Runner.Listener --version | grep -Fx "$RUNNER_VERSION"

COPY --from=builder /out/ /usr/local/bin/
COPY test/runner/sockerless/ /test/
RUN chmod +x /test/*.sh

WORKDIR /test
# bash-form: /test may be a runtime bind mount (local iteration) whose
# exec bit doesn't survive every host filesystem (macOS virtiofs).
ENTRYPOINT ["/bin/bash", "/test/run-integration.sh"]
