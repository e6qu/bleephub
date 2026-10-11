ARG RUNNER_VERSION=2.337.0
ARG RUNNER_SOURCE_COMMIT=397b032cbf865e9c3ddfab89d533ec19325e1273
ARG DOTNET_SDK_VERSION=8.0.425
ARG DOTNET_RUNTIME_VERSION=8.0.31

FROM --platform=$BUILDPLATFORM mcr.microsoft.com/dotnet/sdk:8.0.425-bookworm-slim@sha256:ec9c0a0dc5f60adc2065762050638dd5ed5facfc53716d4580a42d86027c8e80 AS actions-runner-builder
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

# Amazon ECR Public's copy of Docker's official image, pinned to the digest
# Docker Hub serves: Docker Hub limits anonymous pulls per address, and the
# shared CI runners exhaust it.
FROM public.ecr.aws/docker/library/golang:1.26.9@sha256:d7722066f0b60ceccb6c0643cbed1f5f9e15506ac237146c95333504d7805d89 AS builder
# Re-enable toolchain auto-download (the golang image pins GOTOOLCHAIN=local) so
# the image builds with whatever toolchain go.mod requires.
ENV GOTOOLCHAIN=auto

WORKDIR /src

# --- Install the cloud simulators from sockerless-cloud release artifacts ---
# One image serves multiple backends; run-integration.sh picks via
# BLEEPHUB_BACKEND (ecs → AWS sim, aca → Azure sim, …). Download the prebuilt
# simulator binaries from the github.com/e6qu/sockerless-cloud release rather
# than compiling them here — building the simulators would drag the AWS, Azure
# and GCP SDKs into this image. Pin every tarball by SHA-256 per architecture.
ARG TARGETARCH
ARG SOCKERLESS_CLOUD_VERSION=v0.30.10
RUN set -eux; \
    base="https://github.com/e6qu/sockerless-cloud/releases/download/${SOCKERLESS_CLOUD_VERSION}"; \
    case "${TARGETARCH}" in \
      amd64) aws=d77e60471cb2f44935bddf4f838ceeffab5e03912c7643e608a81fd9dcdffe1b; \
             azure=92cdc3f79d3bba9d394cb874e6c189cee0d4ca57b8686dbb4007604c86d40033; \
             gcp=ab569a330298e703ac56467032fd9e41999332266b7f4c53749f541b30c04b27 ;; \
      arm64) aws=5c408e4ff808b6967df272bcb1dc6f3bf7f167c6a786fa38a807d562cac7c45d; \
             azure=f7a421ab85adebd67c73fec85930b321a57a0c5e0709b0f7e40270b850d665a2; \
             gcp=f70897146cee24f947cd6b1072911c4adeec985a46e49e83fa44df8706520fdf ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    mkdir -p /out; \
    for entry in "aws:${aws}" "azure:${azure}" "gcp:${gcp}"; do \
      name="${entry%%:*}"; want="${entry##*:}"; \
      tarball="/tmp/simulator-${name}.tar.gz"; \
      curl -fsSL -o "${tarball}" "${base}/simulator-${name}_${SOCKERLESS_CLOUD_VERSION}_linux_${TARGETARCH}.tar.gz"; \
      echo "${want}  ${tarball}" | sha256sum -c -; \
      tar -xzf "${tarball}" -C /out "simulator-${name}"; \
      chmod +x "/out/simulator-${name}"; \
      rm -f "${tarball}"; \
    done

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
# Amazon ECR Public's copy of Docker's official image, pinned to the digest
# Docker Hub serves: Docker Hub limits anonymous pulls per address, and the
# shared CI runners exhaust it.
FROM public.ecr.aws/docker/library/ubuntu:26.04@sha256:f144425ff09be612d6d9ad965196e9cdc23dae1f42110a8a11a3e9a8198759f7

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
ARG RUNNER_SHA256_AMD64=70920811a4f8ad4328818682bca5c6469c1c942fab52448868071d0063816613
ARG RUNNER_SHA256_ARM64=9b1dc70626422526e3c94767cf024896beb15da5342a3f4819bf2feac13e0393
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
