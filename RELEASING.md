# Releasing

Bleephub versions releases with [semantic versioning](https://semver.org). Cut a
release by pushing an annotated tag — nothing else by hand, nothing in the
GitHub UI.

```
git tag -a v1.2.3 -m 'v1.2.3'
git push origin v1.2.3
```

## What the tag triggers

The tag enters `publish-container.yml`, the same workflow every push to `main`
enters. A release ships bytes built by the continuously-running pipeline, not a
path exercised once a quarter.

A tagged run differs from a `main` run in four ways:

- `metadata` derives a version from the tag and fails the run if it is not
  semver. `main` runs leave the version empty.
- `build` stamps `BLEEPHUB_VERSION` and the `org.opencontainers.image.version`
  label with the version instead of the commit tag.
- `manifest` publishes `:1.2.3` and `:latest` alongside the usual `:<12-char
  sha>`, all pointing at the same digest.
- `release` creates the GitHub Release, attaching the startup and wake bundles,
  with the commit log since the previous tag as the changelog.

A tag containing a hyphen — `v1.3.0-rc.1` — publishes as a prerelease and does
not move `:latest`.

## Retention

`prune` keeps the twenty most recent commit-tagged package versions. Semantic
version tags and `latest` are exempt and kept indefinitely: a published `1.2.3`
must keep resolving after the twenty-first following commit.

## Changelog

The changelog is the generated release notes on each GitHub Release, derived
from the commits between tags. No separate file — a hand-updated one goes stale
and lies. Commit messages are the source, so they carry the reasoning.

## Before tagging

Continuous integration on `main` must be green. It already builds both release
Dockerfiles and both bundles on every pull request, so a tag is never the first
time a packaging change runs.
