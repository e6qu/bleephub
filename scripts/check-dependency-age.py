#!/usr/bin/env python3
"""Reject resolved dependencies published less than 24 hours ago."""

from __future__ import annotations

import argparse
import concurrent.futures
import datetime as dt
import gzip
import io
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
MINIMUM_AGE = dt.timedelta(hours=24)
UTC = dt.timezone.utc
MAXIMUM_METADATA_BYTES = 64 * 1024 * 1024
ALLOWED_METADATA_HOSTS = {
    "api.nuget.org",
    "api.github.com",
    "builds.dotnet.microsoft.com",
    "hub.docker.com",
    "registry.npmjs.org",
    "registry.terraform.io",
    "pypi.org",
}


def validate_metadata_url(url: str) -> None:
    parsed = urllib.parse.urlparse(url)
    if (
        parsed.scheme != "https"
        or parsed.hostname not in ALLOWED_METADATA_HOSTS
        or parsed.username is not None
        or parsed.password is not None
    ):
        raise RuntimeError(f"dependency metadata URL is outside the allowlist: {url}")


class AllowlistedRedirectHandler(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        validate_metadata_url(newurl)
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def request_json(url: str) -> dict:
    validate_metadata_url(url)
    headers = {
        "Accept": "application/json",
        "Accept-Encoding": "gzip",
        "User-Agent": "bleephub-dependency-age-gate",
    }
    if url.startswith("https://api.github.com/") and os.environ.get("GITHUB_TOKEN"):
        headers["Authorization"] = "Bearer " + os.environ["GITHUB_TOKEN"]
        headers["X-GitHub-Api-Version"] = "2022-11-28"
    request = urllib.request.Request(url, headers=headers)
    opener = urllib.request.build_opener(AllowlistedRedirectHandler())
    last_error: Exception | None = None
    for attempt in range(3):
        try:
            # The exact HTTPS host is allowlisted above; file:// and arbitrary
            # network destinations cannot reach this call.
            with opener.open(request, timeout=90) as response:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
                body = response.read(MAXIMUM_METADATA_BYTES + 1)
                if len(body) > MAXIMUM_METADATA_BYTES:
                    raise RuntimeError(f"dependency metadata response is too large: {url}")
                if response.headers.get("Content-Encoding") == "gzip":
                    with gzip.GzipFile(fileobj=io.BytesIO(body)) as compressed:
                        body = compressed.read(MAXIMUM_METADATA_BYTES + 1)
                    if len(body) > MAXIMUM_METADATA_BYTES:
                        raise RuntimeError(
                            f"decompressed dependency metadata is too large: {url}"
                        )
                return json.loads(body)
        except (urllib.error.URLError, json.JSONDecodeError, TimeoutError) as error:
            last_error = error
            if attempt < 2:
                time.sleep(2**attempt)
    raise RuntimeError(f"query {url}: {last_error}") from last_error


def parse_time(value: str) -> dt.datetime:
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).astimezone(UTC)


def concatenated_json(raw: str) -> list[dict]:
    decoder = json.JSONDecoder()
    values = []
    offset = 0
    while offset < len(raw):
        while offset < len(raw) and raw[offset].isspace():
            offset += 1
        if offset >= len(raw):
            break
        value, offset = decoder.raw_decode(raw, offset)
        values.append(value)
    return values


def go_dependencies() -> dict[str, dt.datetime]:
    dependencies: dict[str, dt.datetime] = {}
    modules = (
        (ROOT, []),
        (ROOT / "sdk-tests", []),
        (ROOT / "terraform/wake", []),
        (ROOT / "test/terraform-sockerless", []),
        (ROOT, ["-modfile=.github/security-go.mod"]),
        (ROOT, ["-modfile=.github/workflow-tools-go.mod"]),
    )
    for module, build_flags in modules:
        output = subprocess.check_output(
            ["go", "list", *build_flags, "-m", "-json", "all"],
            cwd=module,
            text=True,
        )
        for item in concatenated_json(output):
            if item.get("Main") or not item.get("Version"):
                continue
            resolved = item.get("Replace") or item
            published = resolved.get("Time") or item.get("Time")
            if not published:
                raise RuntimeError(
                    f"Go module {item['Path']}@{item['Version']} has no publication time"
                )
            dependencies[f"go:{item['Path']}@{item['Version']}"] = parse_time(published)
    return dependencies


def bun_dependency_specs() -> set[tuple[str, str]]:
    output = subprocess.check_output(["bun", "pm", "ls", "--all"], cwd=ROOT / "web", text=True)
    specs: set[tuple[str, str]] = set()
    for line in output.splitlines():
        match = re.match(r"^[├└]── (.+)@([^@]+)$", line)
        if not match or match.group(2).startswith("workspace:"):
            continue
        specs.add((match.group(1), match.group(2)))
    if not specs:
        raise RuntimeError("Bun resolved no registry dependencies")
    return specs


def npm_publication(spec: tuple[str, str]) -> tuple[str, dt.datetime]:
    name, version = spec
    encoded = urllib.parse.quote(name, safe="")
    metadata = request_json(f"https://registry.npmjs.org/{encoded}")
    published = metadata.get("time", {}).get(version)
    if not published:
        raise RuntimeError(f"npm package {name}@{version} has no publication time")
    return f"npm:{name}@{version}", parse_time(published)


def bun_dependencies() -> dict[str, dt.datetime]:
    with concurrent.futures.ThreadPoolExecutor(max_workers=12) as executor:
        return dict(executor.map(npm_publication, sorted(bun_dependency_specs())))


def nuget_dependency_specs() -> set[tuple[str, str]]:
    lock_root = ROOT / "docker/runner-nuget-locks"
    locks = sorted(lock_root.glob("*/packages.lock.json"))
    if not locks:
        raise RuntimeError("Actions Runner source build has no NuGet lock files")

    specs: dict[tuple[str, str], str] = {}
    for path in locks:
        lock = json.loads(path.read_text(encoding="utf-8"))
        if lock.get("version") != 1 or not isinstance(lock.get("dependencies"), dict):
            raise RuntimeError(
                f"{path.relative_to(ROOT)} is not a NuGet lock file version 1"
            )
        for target, packages in lock["dependencies"].items():
            if not isinstance(packages, dict):
                raise RuntimeError(
                    f"{path.relative_to(ROOT)} target {target!r} is malformed"
                )
            for package, item in packages.items():
                if item.get("type") == "Project":
                    continue
                version = item.get("resolved")
                content_hash = item.get("contentHash")
                if not isinstance(version, str) or not isinstance(content_hash, str):
                    raise RuntimeError(
                        f"{path.relative_to(ROOT)} does not fully lock "
                        f"{package!r} for {target}"
                    )
                key = (package.casefold(), version.casefold())
                previous = specs.setdefault(key, content_hash)
                if previous != content_hash:
                    raise RuntimeError(
                        f"NuGet package {package}@{version} has conflicting content hashes"
                    )
    if not specs:
        raise RuntimeError("Actions Runner NuGet locks contain no packages")
    return set(specs)


def nuget_publication(spec: tuple[str, str]) -> tuple[str, dt.datetime]:
    package, version = spec
    encoded_package = urllib.parse.quote(package.casefold(), safe="")
    encoded_version = urllib.parse.quote(version.casefold(), safe="")
    metadata = request_json(
        "https://api.nuget.org/v3/registration5-gz-semver2/"
        f"{encoded_package}/{encoded_version}.json"
    )
    published = metadata.get("published")
    if not published:
        raise RuntimeError(f"NuGet package {package}@{version} has no publication time")
    return f"nuget:{package}@{version}", parse_time(published)


def nuget_dependencies() -> dict[str, dt.datetime]:
    with concurrent.futures.ThreadPoolExecutor(max_workers=12) as executor:
        return dict(executor.map(nuget_publication, sorted(nuget_dependency_specs())))


def terraform_dependencies() -> dict[str, dt.datetime]:
    lock = (ROOT / "terraform/.terraform.lock.hcl").read_text(encoding="utf-8")
    dependencies: dict[str, dt.datetime] = {}
    pattern = re.compile(
        r'provider "registry\.terraform\.io/([^/]+)/([^"]+)" \{\s+version\s+=\s+"([^"]+)"',
        re.MULTILINE,
    )
    for namespace, name, version in pattern.findall(lock):
        metadata = request_json(
            f"https://registry.terraform.io/v1/providers/{namespace}/{name}/{version}"
        )
        published = metadata.get("published_at")
        if not published:
            raise RuntimeError(
                f"Terraform provider {namespace}/{name}@{version} has no publication time"
            )
        dependencies[f"terraform:{namespace}/{name}@{version}"] = parse_time(published)
    if not dependencies:
        raise RuntimeError("Terraform lock file contains no providers")
    return dependencies


def docker_dependencies() -> dict[str, dt.datetime]:
    dependencies: dict[str, dt.datetime] = {}
    local_bases = {"bleephub-runner-sockerless:local"}
    dockerfiles = sorted(
        path
        for path in ROOT.rglob("Dockerfile*")
        if "node_modules" not in path.parts and path.is_file()
    )
    pattern = re.compile(
        r"^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)", re.MULTILINE | re.IGNORECASE
    )
    image_pattern = re.compile(
        r"^(?P<image>[^:@]+(?:/[^:@]+)*):(?P<tag>[^@\s]+)"
        r"@(?P<digest>sha256:[0-9a-f]{64})$"
    )
    for path in dockerfiles:
        for reference in pattern.findall(path.read_text(encoding="utf-8")):
            if reference in local_bases:
                continue
            match = image_pattern.fullmatch(reference)
            if not match:
                raise RuntimeError(
                    f"{path.relative_to(ROOT)} has an unpinned external base image: "
                    f"{reference}"
                )
            image = match.group("image")
            if "." in image.split("/", 1)[0] or ":" in image.split("/", 1)[0]:
                if image == "mcr.microsoft.com/dotnet/sdk":
                    # runner_dependencies validates this sole non-Docker-Hub
                    # image against the immutable SDK/runtime release pins.
                    continue
                raise RuntimeError(
                    f"{path.relative_to(ROOT)} uses an unsupported non-Docker-Hub "
                    f"base image: {reference}"
                )
            repository = image if "/" in image else f"library/{image}"
            tag = urllib.parse.quote(match.group("tag"), safe="")
            metadata = request_json(
                f"https://hub.docker.com/v2/repositories/{repository}/tags/{tag}"
            )
            if metadata.get("digest") != match.group("digest"):
                raise RuntimeError(
                    f"Docker Hub digest changed for {image}:{match.group('tag')}: "
                    f"lock has {match.group('digest')}, registry has "
                    f"{metadata.get('digest')}"
                )
            published = metadata.get("last_updated")
            if not published:
                raise RuntimeError(f"Docker image {reference} has no update time")
            dependencies[f"docker:{reference}"] = parse_time(published)
    if not dependencies:
        raise RuntimeError("Dockerfiles contain no immutable base-image pins")
    return dependencies


def action_dependencies() -> dict[str, dt.datetime]:
    dependencies: dict[str, dt.datetime] = {}
    pattern = re.compile(r"\buses:\s+([^@\s]+)@([0-9a-f]{40})\b")
    workflows = ROOT / ".github/workflows"
    for path in sorted(workflows.glob("*.y*ml")):
        for action, commit in pattern.findall(path.read_text(encoding="utf-8")):
            if action.startswith("./"):
                continue
            repository = "/".join(action.split("/")[:2])
            key = f"action:{repository}@{commit}"
            if key in dependencies:
                continue
            metadata = request_json(
                f"https://api.github.com/repos/{repository}/commits/{commit}"
            )
            published = metadata.get("commit", {}).get("committer", {}).get("date")
            if not published:
                raise RuntimeError(f"GitHub Action {key} has no commit time")
            dependencies[key] = parse_time(published)
    if not dependencies:
        raise RuntimeError("workflows contain no immutable action pins")
    return dependencies


def runner_dependencies() -> dict[str, dt.datetime]:
    dockerfile = (ROOT / "Dockerfile.runner").read_text(encoding="utf-8")

    def argument(source: str, name: str, value_pattern: str) -> str:
        match = re.search(
            rf"^ARG {re.escape(name)}=({value_pattern})$",
            source,
            re.MULTILINE,
        )
        if not match:
            raise RuntimeError(f"Dockerfile.runner has no valid {name} pin")
        return match.group(1)

    version = argument(
        dockerfile, "RUNNER_VERSION", r"[0-9]+\.[0-9]+\.[0-9]+"
    )
    source_commit = argument(
        dockerfile, "RUNNER_SOURCE_COMMIT", r"[0-9a-f]{40}"
    )
    dotnet_sdk_version = argument(
        dockerfile, "DOTNET_SDK_VERSION", r"[0-9]+\.[0-9]+\.[0-9]+"
    )
    dotnet_runtime_version = argument(
        dockerfile, "DOTNET_RUNTIME_VERSION", r"[0-9]+\.[0-9]+\.[0-9]+"
    )
    pins = {
        "x64": argument(dockerfile, "RUNNER_SHA256_AMD64", r"[0-9a-f]{64}"),
        "arm64": argument(dockerfile, "RUNNER_SHA256_ARM64", r"[0-9a-f]{64}"),
    }
    metadata = request_json(
        f"https://api.github.com/repos/actions/runner/releases/tags/v{version}"
    )
    if metadata.get("draft") or metadata.get("prerelease"):
        raise RuntimeError(f"GitHub Actions Runner v{version} is not a stable release")
    published = metadata.get("published_at")
    if not published:
        raise RuntimeError(f"GitHub Actions Runner v{version} has no publication time")

    assets = {item.get("name"): item for item in metadata.get("assets", [])}
    for architecture, expected in pins.items():
        filename = f"actions-runner-linux-{architecture}-{version}.tar.gz"
        asset = assets.get(filename)
        if not asset:
            raise RuntimeError(f"GitHub Actions Runner release has no {filename}")
        actual = asset.get("digest")
        if actual != f"sha256:{expected}":
            raise RuntimeError(
                f"GitHub Actions Runner {filename} digest differs: "
                f"lock has sha256:{expected}, release has {actual}"
            )

    harness = (ROOT / "test/runner/sockerless/Dockerfile").read_text(
        encoding="utf-8"
    )
    expected_harness_pins = {
        "RUNNER_VERSION": version,
        "RUNNER_SOURCE_COMMIT": source_commit,
        "RUNNER_SHA256_AMD64": pins["x64"],
        "RUNNER_SHA256_ARM64": pins["arm64"],
        "DOTNET_SDK_VERSION": dotnet_sdk_version,
        "DOTNET_RUNTIME_VERSION": dotnet_runtime_version,
    }
    for name, expected in expected_harness_pins.items():
        actual = argument(harness, name, re.escape(expected))
        if actual != expected:
            raise RuntimeError(
                f"test runner {name} is {actual}, production runner uses {expected}"
            )
    sdk_image_pattern = re.compile(
        r"^FROM --platform=\$BUILDPLATFORM "
        r"(mcr\.microsoft\.com/dotnet/sdk:"
        + re.escape(dotnet_sdk_version)
        + r"-bookworm-slim@sha256:[0-9a-f]{64}) AS actions-runner-builder$",
        re.MULTILINE,
    )
    production_sdk_image = sdk_image_pattern.findall(dockerfile)
    harness_sdk_image = sdk_image_pattern.findall(harness)
    if len(production_sdk_image) != 1 or harness_sdk_image != production_sdk_image:
        raise RuntimeError(
            "production and test runners must use the same immutable .NET SDK image"
        )

    source_metadata = request_json(
        f"https://api.github.com/repos/actions/runner/commits/{source_commit}"
    )
    source_published = (
        source_metadata.get("commit", {}).get("committer", {}).get("date")
    )
    if not source_published:
        raise RuntimeError(
            f"GitHub Actions Runner source {source_commit} has no commit time"
        )

    feature_band = dotnet_sdk_version.split(".", maxsplit=2)
    metadata = request_json(
        "https://builds.dotnet.microsoft.com/dotnet/release-metadata/"
        f"{feature_band[0]}.{feature_band[1]}/releases.json"
    )
    releases = [
        item
        for item in metadata.get("releases", [])
        if item.get("sdk", {}).get("version") == dotnet_sdk_version
        and item.get("runtime", {}).get("version") == dotnet_runtime_version
    ]
    if len(releases) != 1:
        raise RuntimeError(
            "official .NET metadata has "
            f"{len(releases)} SDK {dotnet_sdk_version} / runtime "
            f"{dotnet_runtime_version} releases"
        )
    dotnet_release = releases[0]
    release_date = dotnet_release.get("release-date")
    if not release_date:
        raise RuntimeError(
            f".NET SDK {dotnet_sdk_version} / runtime "
            f"{dotnet_runtime_version} has no release date"
        )

    dotnet_published = dt.datetime.fromisoformat(release_date).replace(tzinfo=UTC)
    return {
        f"github-release:actions/runner@v{version}": parse_time(published),
        f"github-source:actions/runner@{source_commit}": parse_time(source_published),
        (
            f"microsoft-release:dotnet-sdk@{dotnet_sdk_version}"
            f"+runtime@{dotnet_runtime_version}"
        ): dotnet_published,
    }


def python_dependencies() -> dict[str, dt.datetime]:
    requirements = ROOT / ".github/security-requirements.in"
    required: dict[str, str] = {}
    for raw_line in requirements.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        name, separator, version = line.partition("==")
        if not separator or not name or not version:
            raise RuntimeError(f"security requirement is not exactly pinned: {line}")
        required[re.sub(r"[-_.]+", "-", name).lower()] = version

    python = os.environ.get("BLEEPHUB_PYTHON")
    if python:
        installed_json = subprocess.check_output(
            [python, "-m", "pip", "list", "--format=json"], text=True
        )
        installed = {
            re.sub(r"[-_.]+", "-", item["name"]).lower(): item["version"]
            for item in json.loads(installed_json)
        }
        for name, version in required.items():
            if installed.get(name) != version:
                raise RuntimeError(
                    f"installed Python analyzer {name} is {installed.get(name)!r}, "
                    f"requirements pin {version}"
                )
        specs = installed.items()
    else:
        specs = required.items()

    dependencies: dict[str, dt.datetime] = {}
    for name, version in sorted(specs):
        metadata = request_json(f"https://pypi.org/pypi/{name}/{version}/json")
        uploads = [
            parse_time(item["upload_time_iso_8601"])
            for item in metadata.get("urls", [])
            if item.get("upload_time_iso_8601")
        ]
        if not uploads:
            raise RuntimeError(f"PyPI package {name}=={version} has no upload time")
        # A release can receive a platform wheel after its first source upload.
        # Pip may select that newer artifact, so every artifact for the resolved
        # version must clear quarantine rather than only the oldest one.
        dependencies[f"pypi:{name}=={version}"] = max(uploads)
    if not dependencies:
        raise RuntimeError("security requirements contain no Python packages")
    return dependencies


def check_age(dependencies: dict[str, dt.datetime]) -> None:
    now = dt.datetime.now(UTC)
    too_young = sorted(
        (name, published, now - published)
        for name, published in dependencies.items()
        if now - published < MINIMUM_AGE
    )
    for name, published, age in too_young:
        print(
            f"{name} was published {published.isoformat()} "
            f"({age.total_seconds() / 3600:.1f} hours ago)",
            file=sys.stderr,
        )
    if too_young:
        raise SystemExit(
            f"{len(too_young)} resolved dependencies are younger than 24 hours"
        )
    print(f"verified {len(dependencies)} resolved dependencies are at least 24 hours old")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "ecosystems",
        nargs="*",
        choices=(
            "go",
            "bun",
            "nuget",
            "terraform",
            "docker",
            "actions",
            "runner",
            "python",
        ),
        default=(
            "go",
            "bun",
            "nuget",
            "terraform",
            "docker",
            "actions",
            "runner",
            "python",
        ),
    )
    args = parser.parse_args()
    loaders = {
        "go": go_dependencies,
        "bun": bun_dependencies,
        "nuget": nuget_dependencies,
        "terraform": terraform_dependencies,
        "docker": docker_dependencies,
        "actions": action_dependencies,
        "runner": runner_dependencies,
        "python": python_dependencies,
    }
    dependencies: dict[str, dt.datetime] = {}
    for ecosystem in args.ecosystems:
        dependencies.update(loaders[ecosystem]())
    check_age(dependencies)


if __name__ == "__main__":
    main()
