#!/usr/bin/env python3
"""Fail when a pinned container image is behind what Docker Hub serves for it.

Every image bleephub pulls from Docker Hub is pulled through a copy instead,
pinned by tag and digest: public.ecr.aws/docker/library/<name> for official
images and mirror.gcr.io/<namespace>/<name> for the rest. check-dependency-age.py
only gates the adoption of a pin; a pin that never moves rots, so this check
compares every pin in the tracked tree with Docker Hub:

  ok      the pin is the digest Docker Hub serves for its tag, and the mirror
          serves that digest too
  HELD    Docker Hub has something newer (a new digest for the tag, or a newer
          release with the same variant on the same release line, which is the
          major version unless RELEASE_LINES says otherwise), but it is less
          than 24 hours old
  DRIFT   Docker Hub has had something newer for at least 24 hours, or the
          mirror serves a different digest than the pin: fails

Dependabot cannot do this: its anonymous lookups on public.ecr.aws exceed ECR
Public's limit of one unauthenticated request per second and fail with 429, and
it applies no cooldown to either mirror. This check makes three or four requests
per image and spaces its ECR Public requests 1.1 seconds apart.
"""

from __future__ import annotations

import datetime as dt
import json
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
PIN_PATTERN = re.compile(
    r"(?P<registry>public\.ecr\.aws/docker/library|mirror\.gcr\.io)/"
    r"(?P<path>[a-z0-9._/-]+):(?P<tag>[A-Za-z0-9_.-]+)@(?P<digest>sha256:[0-9a-f]{64})"
)
VERSION_PATTERN = re.compile(r"(?P<prefix>v?)(?P<numbers>[0-9]+(?:\.[0-9]+)*)(?P<suffix>-[A-Za-z0-9_.-]+)?")
MANIFEST_TYPES = ", ".join(
    (
        "application/vnd.oci.image.index.v1+json",
        "application/vnd.docker.distribution.manifest.list.v2+json",
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    )
)
ECR_PUBLIC_SPACING = 1.1
ALLOWED_HOSTS = {"hub.docker.com", "public.ecr.aws", "mirror.gcr.io"}

# How many leading version numbers name a release line, where that is not the
# major version alone. A newer release only counts when it is on the same line.
RELEASE_LINES: dict[str, int] = {
    # Go supports its two newest minor releases, and each go.mod's go line
    # chooses one; a newer patch on that minor is what the pin must follow.
    "library/golang": 2,
    # The images build on an LTS release; the interim releases between LTS
    # releases are supported for nine months only.
    "library/ubuntu": 2,
}


class Pin:
    def __init__(self, registry: str, path: str, tag: str, digest: str) -> None:
        self.registry = registry
        self.path = path
        self.tag = tag
        self.digest = digest
        if registry == "mirror.gcr.io":
            self.hub = path if "/" in path else f"library/{path}"
        else:
            self.hub = f"library/{path}"

    def key(self) -> tuple[str, str, str, str]:
        return (self.registry, self.path, self.tag, self.digest)

    def reference(self) -> str:
        return f"{self.registry}/{self.path}:{self.tag}"


def parse_time(value: str) -> dt.datetime:
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).astimezone(UTC)


def find_pins(files: list[tuple[str, str]]) -> dict[tuple[str, str, str, str], tuple[Pin, set[str]]]:
    pins: dict[tuple[str, str, str, str], tuple[Pin, set[str]]] = {}
    for path, text in files:
        for match in PIN_PATTERN.finditer(text):
            pin = Pin(match["registry"], match["path"], match["tag"], match["digest"])
            pins.setdefault(pin.key(), (pin, set()))[1].add(path)
    return pins


def newer_releases(
    tag: str, candidates: list[tuple[str, dt.datetime]], line: int = 1
) -> list[tuple[str, dt.datetime]]:
    """Newer releases on the same release line and variant as tag, newest first.

    The release line is the first `line` version numbers (the major version by
    default). A tag that is not a version (latest, alpine) has no newer releases."""
    match = VERSION_PATTERN.fullmatch(tag)
    if match is None:
        return []
    numbers = tuple(int(part) for part in match["numbers"].split("."))
    suffix = match["suffix"] or ""
    newer: list[tuple[tuple[int, ...], str, dt.datetime]] = []
    for name, pushed in candidates:
        other = VERSION_PATTERN.fullmatch(name)
        if other is None or other["prefix"] != match["prefix"] or (other["suffix"] or "") != suffix:
            continue
        version = tuple(int(part) for part in other["numbers"].split("."))
        if len(version) == len(numbers) and version[:line] == numbers[:line] and version > numbers:
            newer.append((version, name, pushed))
    return [(name, pushed) for _, name, pushed in sorted(newer, reverse=True)]


def verdict(
    pinned: str,
    hub_digest: str,
    hub_pushed: dt.datetime,
    mirror_digest: str,
    newer: list[tuple[str, dt.datetime]],
    now: dt.datetime,
) -> tuple[str, list[str]]:
    problems: list[str] = []
    held: list[str] = []
    if hub_digest != pinned:
        if now - hub_pushed >= MINIMUM_AGE:
            problems.append(f"Docker Hub has served {hub_digest} for the tag since {hub_pushed.isoformat()}")
        else:
            held.append(f"Docker Hub moved the tag to {hub_digest} at {hub_pushed.isoformat()}")
    elif mirror_digest != pinned:
        problems.append(f"the mirror serves {mirror_digest} for the tag, not the pinned digest")
    eligible = [(name, pushed) for name, pushed in newer if now - pushed >= MINIMUM_AGE]
    if eligible:
        problems.append(f"newer release {eligible[0][0]} has been on Docker Hub since {eligible[0][1].isoformat()}")
    elif newer:
        held.append(f"newer release {newer[0][0]} is inside the 24-hour quarantine")
    if problems:
        return "DRIFT", problems
    if held:
        return "HELD", held
    return "ok", []


_last_ecr_public = 0.0


def fetch(url: str, headers: dict[str, str], method: str = "GET") -> tuple[dict[str, str], bytes]:
    global _last_ecr_public
    parsed = urllib.parse.urlparse(url)
    if parsed.scheme != "https" or parsed.hostname not in ALLOWED_HOSTS:
        raise RuntimeError(f"image metadata URL is outside the allowlist: {url}")
    if parsed.hostname == "public.ecr.aws":
        # ECR Public allows one unauthenticated request per second.
        wait = _last_ecr_public + ECR_PUBLIC_SPACING - time.monotonic()
        if wait > 0:
            time.sleep(wait)
    request = urllib.request.Request(
        url, method=method, headers={"User-Agent": "bleephub-image-pin-check", **headers}
    )
    try:
        # The exact HTTPS host is allowlisted above.
        with urllib.request.urlopen(request, timeout=30) as response:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
            return dict(response.headers.items()), response.read()
    except urllib.error.URLError as error:
        raise RuntimeError(f"query {url}: {error}") from error
    finally:
        if parsed.hostname == "public.ecr.aws":
            _last_ecr_public = time.monotonic()


def hub_tag(pin: Pin) -> tuple[str, dt.datetime]:
    tag = urllib.parse.quote(pin.tag, safe="")
    _, body = fetch(f"https://hub.docker.com/v2/repositories/{pin.hub}/tags/{tag}", {"Accept": "application/json"})
    data = json.loads(body)
    if not data.get("digest") or not data.get("tag_last_pushed"):
        raise RuntimeError(f"Docker Hub returned no digest or push time for {pin.hub}:{pin.tag}")
    return data["digest"], parse_time(data["tag_last_pushed"])


def hub_candidates(pin: Pin) -> list[tuple[str, dt.datetime]]:
    match = VERSION_PATTERN.fullmatch(pin.tag)
    if match is None:
        return []
    line = ".".join(match["numbers"].split(".")[: RELEASE_LINES.get(pin.hub, 1)])
    name = urllib.parse.quote(match["prefix"] + line + ".", safe="")
    _, body = fetch(
        f"https://hub.docker.com/v2/repositories/{pin.hub}/tags?page_size=100&ordering=last_updated&name={name}",
        {"Accept": "application/json"},
    )
    return [
        (row["name"], parse_time(row["tag_last_pushed"]))
        for row in json.loads(body)["results"]
        if row.get("tag_last_pushed")
    ]


def mirror_digest(pin: Pin) -> str:
    headers = {"Accept": MANIFEST_TYPES}
    if pin.registry == "mirror.gcr.io":
        reply, _ = fetch(f"https://mirror.gcr.io/v2/{pin.path}/manifests/{pin.tag}", headers, "HEAD")
    else:
        scope = urllib.parse.quote(f"repository:docker/library/{pin.path}:pull", safe="")
        _, body = fetch(f"https://public.ecr.aws/token/?scope={scope}&service=public.ecr.aws", {})
        headers["Authorization"] = "Bearer " + json.loads(body)["token"]
        reply, _ = fetch(
            f"https://public.ecr.aws/v2/docker/library/{pin.path}/manifests/{pin.tag}", headers, "HEAD"
        )
    digest = {name.lower(): value for name, value in reply.items()}.get("docker-content-digest")
    if not digest:
        raise RuntimeError(f"{pin.registry} returned no digest for {pin.path}:{pin.tag}")
    return digest


def tracked_files() -> list[tuple[str, str]]:
    result = subprocess.run(
        ["git", "grep", "-lE", r"(public\.ecr\.aws/docker/library|mirror\.gcr\.io)/[a-z0-9._/-]+:[A-Za-z0-9_.-]+@sha256:", "--", ":!*.md"],
        cwd=ROOT,
        capture_output=True,
        text=True,
    )
    if result.returncode not in (0, 1):
        raise RuntimeError(f"git grep failed: {result.stderr.strip()}")
    return [(name, (ROOT / name).read_text(encoding="utf-8")) for name in result.stdout.splitlines() if name]


def main() -> None:
    now = dt.datetime.now(UTC)
    pins = find_pins(tracked_files())
    if not pins:
        raise SystemExit("found no pinned images; the check is not looking at anything")
    failed = 0
    for _, (pin, where) in sorted(pins.items()):
        label = f"{pin.reference()} ({', '.join(sorted(where))})"
        try:
            hub_digest, hub_pushed = hub_tag(pin)
            state, notes = verdict(
                pin.digest,
                hub_digest,
                hub_pushed,
                mirror_digest(pin),
                newer_releases(pin.tag, hub_candidates(pin), RELEASE_LINES.get(pin.hub, 1)),
                now,
            )
        except (RuntimeError, KeyError, ValueError) as error:
            print(f"ERROR  {label}: could not be checked: {error}", file=sys.stderr)
            failed += 1
            continue
        print(f"{state:<6} {label}" + "".join(f"\n         {note}" for note in notes))
        if state == "DRIFT":
            failed += 1
    if failed:
        raise SystemExit(
            f"{failed} image pin(s) are behind Docker Hub or could not be checked; bump each "
            "to the digest Docker Hub serves once the mirror serves the same digest"
        )
    print(f"verified {len(pins)} pinned images are current or held")


if __name__ == "__main__":
    main()
