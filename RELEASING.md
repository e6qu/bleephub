# Releasing

Bleephub versions releases with [semantic versioning](https://semver.org). A
release is cut by pushing an annotated tag; there is nothing else to do by hand,
and nothing to do in the GitHub user interface.

```
git tag -a v1.2.3 -m 'v1.2.3'
git push origin v1.2.3
```

## What the tag triggers

The tag enters `publish-container.yml` — the same workflow every push to `main`
enters, not a second one beside it, so a release ships bytes built by the
pipeline that has been running continuously rather than by a path exercised
once a quarter.

A tagged run differs from a `main` run in four ways:

- `metadata` derives a version from the tag and fails the run if it is not
  semver. `main` runs leave the version empty.
- `build` stamps `BLEEPHUB_VERSION` and the `org.opencontainers.image.version`
  label with the version instead of the commit tag.
- `manifest` publishes `:1.2.3` and `:latest` alongside the usual `:<12-char
  sha>`, all pointing at the same digest.
- `release` creates the GitHub Release, attaching the startup and wake bundles,
  with the commit log since the previous tag as the changelog.

A tag containing a hyphen — `v1.3.0-rc.1` — is published as a prerelease and
does not move `:latest`.

## Retention

`prune` keeps the twenty most recent commit-tagged package versions. Semantic
version tags and `latest` are exempt and kept indefinitely: a published `1.2.3`
has to keep resolving after the twenty-first commit that follows it.

## Changelog

The changelog is the generated release notes on each GitHub Release, derived
from the commits between tags. There is no separately maintained file, because
one that is updated by hand goes stale and then lies. Commit messages are the
source, so they carry the reasoning.

## Before tagging

Continuous integration on `main` must be green. It already builds both release
Dockerfiles and both bundles on every pull request, so a tag cannot be the first
time a packaging change is exercised.
