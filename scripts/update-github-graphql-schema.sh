#!/usr/bin/env bash
# Refresh GitHub's official public GraphQL schema behind an explicit digest pin.
set -euo pipefail

PIN_SHA256="ee878de9ade05f0b88be0c29ea461755c23c3fd541d86d875a10aade686bbbc3"
# GitHub's rolling docs endpoint serves several reviewed feature-flag variants
# from different CDN edges during the ProjectV2 multi-select/view rollout, so
# which one a runner sees depends on its region. Keep the richer contract
# vendored, while letting the drift check recognize the other official variants,
# all observed from docs.github.com. The current pin (ee878de9) adds the
# `AddCloseIssueReferences`/`RemoveCloseIssueReferences` mutations (their Input
# and Payload types) and the `PullRequestReviewThreadResolutionReason` enum,
# plus the CIDR `0.0.0.0/0`/`::/0` allow-list-entry restriction note, over the
# prior pin (e67fbf3c, IssueFieldUpdateInput/IssueFieldUpdateOperation/
# UpdateIssueInput.issueFieldUpdates), which some edges still serve from cache
# and stays accepted through the rollout; 30347e4f
# (Enterprise.innersourceVulnerabilities), b716c684 (IssueFieldValueFilter),
# 86e8e001, c504a0ed, 0c5ad89a and fc99569d are earlier variants of the same
# rollout. 4d75bf4a (observed on CI runners) and f95edf4a (observed from a
# cache-busted edge) are the newest wave: reviewed against the vendored pin, they
# only ADD the `ENTERPRISE_ORGANIZATION_CREATION` OrganizationInvitationReason,
# the `ADDED_TO_STACK_EVENT`/`REMOVED_FROM_STACK_EVENT` pull-request timeline
# events, and an optional `enterpriseRoleDatabaseId` bypass field, plus
# description rewording — additive and non-breaking, so the richer contract stays
# vendored and both digests are accepted through the rollout. Any further digest
# remains blocking.
ROLLOUT_SHA256="e67fbf3cf655481ac815f53b0495efabed0f792a12a405577f74d6532e70b792 30347e4f3bb975195f982351e55636ca03c76969c279980ca515a96ea7b40c3d b716c6844750283b8c33200f660989337254e7158f40ed869f06418bafb3bc2e 86e8e001eb3db2469348cefd25aacf22e623d3fbeed6affddef47ad98f12a9fc c504a0ed454276c878d5a873b782fa9824f2dec3205de3370845d40977e41322 0c5ad89a426609cf1b79679155a17609cd04d7a09914eee9c56894eea18bb031 fc99569d6628bfe0176eded638b4797ee64ab50e0bf2b671a660ec717d085dae 4d75bf4a20c06623f6bce580ffc5bdad52a10bbafd09ff481b2b5a98a8adffbf f95edf4ae7cf146241b4553da3859dc4002d580f60034c6ea19ee19e157f8065"
SOURCE_URL="https://docs.github.com/public/fpt/schema.docs.graphql"

usage() {
  echo "usage: $0 [--sha256 <hex>] [--print-pin] [--print-accepted-sha256]" >&2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --sha256)
      PIN_SHA256="${2:?--sha256 needs a value}"
      shift 2
      ;;
    --print-pin)
      echo "sha256=$PIN_SHA256 source=$SOURCE_URL"
      exit 0
      ;;
    --print-accepted-sha256)
      echo "$PIN_SHA256 $ROLLOUT_SHA256"
      exit 0
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "error: unknown argument $1" >&2
      usage
      exit 2
      ;;
  esac
done

if [[ ! "$PIN_SHA256" =~ ^[0-9a-f]{64}$ ]]; then
  echo "error: sha256 pin must be 64 lowercase hex characters" >&2
  exit 2
fi
read -r -a rollout_sha256 <<< "$ROLLOUT_SHA256"
for digest in "${rollout_sha256[@]}"; do
  if [[ ! "$digest" =~ ^[0-9a-f]{64}$ ]]; then
    echo "error: rollout sha256 must be 64 lowercase hex characters" >&2
    exit 2
  fi
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="$ROOT/third_party/github-graphql-schema.graphql.gz"
VERSION="$ROOT/third_party/github-graphql-schema.VERSION"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

curl --fail --location --silent --show-error "$SOURCE_URL" --output "$tmp"
got="$(shasum -a 256 "$tmp" | awk '{print $1}')"
if [[ "$got" != "$PIN_SHA256" ]]; then
  echo "error: current GitHub GraphQL schema hashes to $got, expected $PIN_SHA256" >&2
  echo "review the schema diff, then rerun with --sha256 $got and update PIN_SHA256" >&2
  exit 1
fi

if ! grep -q '^type Query' "$tmp" || ! grep -q '^type Mutation' "$tmp"; then
  echo "error: downloaded document is not the GitHub GraphQL SDL" >&2
  exit 1
fi

gzip -n -9 -c "$tmp" > "$DEST"
gzip_sha="$(shasum -a 256 "$DEST" | awk '{print $1}')"
lines="$(wc -l < "$tmp" | tr -d ' ')"
{
  echo "vendored: GitHub GraphQL public schema"
  echo "source: $SOURCE_URL"
  echo "content sha256: $PIN_SHA256"
  echo "gzip sha256: $gzip_sha"
  echo "lines: $lines"
  echo "refreshed: $(date -u +%F)"
  echo "refresh: scripts/update-github-graphql-schema.sh"
} > "$VERSION"
echo "vendored GitHub GraphQL schema: $lines lines, sha256 $PIN_SHA256"
