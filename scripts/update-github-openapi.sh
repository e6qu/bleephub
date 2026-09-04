#!/usr/bin/env bash
# Refresh the vendored GitHub OpenAPI description that Bleephub's
# API-definition and response-shape fidelity gates measure against.
#
# The description is fetched from a pinned commit of
# github/rest-api-description and checked against a pinned SHA-256 of the
# uncompressed bytes. A gate must not be able to change its verdict
# because upstream moved, so bumping the vendored copy means editing both
# pins here (or passing --commit/--sha256) as a reviewable act.
set -euo pipefail

# github/rest-api-description commit that the vendored copy comes from,
# and the SHA-256 of descriptions/api.github.com/api.github.com.json at
# that commit.
PIN_COMMIT="3cef12e8a02d612ad032473d4fb87266f2befeae"
PIN_SHA256="531b05749a9f86c7be01330e6d89d052fd3102ff31df4e0c765d9550b11bbad8"

usage() {
  cat >&2 <<'USAGE'
usage: update-github-openapi.sh [--commit <sha>] [--sha256 <sha256>] [--print-pin]

  --commit <sha>    fetch this github/rest-api-description commit instead of
                    the pinned one; requires --sha256
  --sha256 <hex>    expected SHA-256 of the uncompressed description
  --print-pin       print the current pins and exit

Bumping the pin: run with --commit <new sha> --sha256 <new digest>, confirm
the gates still pass, then write both values into PIN_COMMIT/PIN_SHA256.
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --commit) PIN_COMMIT="${2:?--commit needs a value}"; OVERRODE_COMMIT=1; shift 2 ;;
    --sha256) PIN_SHA256="${2:?--sha256 needs a value}"; OVERRODE_SHA=1; shift 2 ;;
    --print-pin) echo "commit=$PIN_COMMIT sha256=$PIN_SHA256"; exit 0 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown argument $1" >&2; usage; exit 2 ;;
  esac
done

if [[ -n "${OVERRODE_COMMIT:-}" && -z "${OVERRODE_SHA:-}" ]]; then
  echo "error: --commit requires --sha256; an unpinned fetch is what this script exists to prevent" >&2
  exit 2
fi

if [[ ! "$PIN_COMMIT" =~ ^[0-9a-f]{40}$ ]]; then
  echo "error: commit pin must be a full 40-hex commit SHA, got '$PIN_COMMIT'" >&2
  exit 2
fi
if [[ ! "$PIN_SHA256" =~ ^[0-9a-f]{64}$ ]]; then
  echo "error: sha256 pin must be 64 hex characters, got '$PIN_SHA256'" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="$ROOT/third_party/github-openapi.json.gz"
VERSION_FILE="$ROOT/third_party/github-openapi.VERSION"
ROUTE_INDEX="$ROOT/third_party/github-openapi-routes.txt.gz"
SPEC_PATH="descriptions/api.github.com/api.github.com.json"
URL="https://raw.githubusercontent.com/github/rest-api-description/$PIN_COMMIT/$SPEC_PATH"

# The other official descriptions bleephub cites when a route it serves is
# absent from the dotcom one: the Enterprise Cloud and Enterprise Server
# surfaces, plus older Enterprise Server releases that still describe
# endpoints GitHub has since retired (e.g. ghes-3.7 documents the
# repository-id environment-secret routes go-github still builds). Only their
# route lists are vendored (github-openapi-routes.txt.gz), so
# gh_api_definition_test.go can check a citation instead of trusting a comment.
# Each is pinned by SHA-256 at PIN_COMMIT.
EXTRA_DESCRIPTIONS=(
  "ghec:67c384e6fada6bcdfea4dc32417b50f4079745091a466b088eec0a63e17938b2"
  "ghes-3.21:6d8d1b5e70c167692e14338562cc6b7dae0f44b6ecc2c8950ae8d6d418666803"
  "ghes-3.13:c7a706c67b51c7317bd02b17d83972cdfb269cfde6257cc09062d9c38a950bf8"
  "ghes-3.7:15db3fa0759b86138c21a21dafe4f796bf07665c13eca455e5a7b574fe6eb8a0"
  "ghes-2.22:629f829cc1bbcba9785b2bcca4d0cec9119c963a20690fa550513cf209f777e6"
)

# GitHub still supports the 2022-11-28 API version. The rolling descriptions
# above now describe 2026-03-10 and legitimately omit operations retired only
# in that newer contract, so retain a separately pinned official 2022
# dotcom description for version-specific route citations.
API_2022_COMMIT="4f249d4b6f72c9bdd9a1eed4b797e4260a1b765e"
API_2022_SHA256="7dcf50fb93d26ad2021c9fdb48c88fe78a63ce3b187a78f30f509c7e75869c65"

tmp="$(mktemp)"
extra_tmp="$(mktemp)"
routes_tmp="$(mktemp)"
trap 'rm -f "$tmp" "$extra_tmp" "$routes_tmp"' EXIT

# normalized_routes prints "METHOD /path/with/{}/params", one per line.
normalized_routes() {
  jq -r '
    .paths | to_entries[] | .key as $p
    | (.value | keys[]) as $m
    | select($m | ascii_downcase | test("^(get|post|put|patch|delete|head)$"))
    | (($m | ascii_upcase) + " " + ($p | gsub("\\{[^}]+\\}"; "{}")))
  ' "$1" | LC_ALL=C sort -u
}

echo "Fetching $URL"
curl -sSLf -o "$tmp" "$URL"

got="$(shasum -a 256 "$tmp" | cut -d' ' -f1)"
if [[ "$got" != "$PIN_SHA256" ]]; then
  echo "error: $SPEC_PATH at $PIN_COMMIT hashes to $got, expected $PIN_SHA256" >&2
  echo "       the pinned commit's contents changed; do not vendor this until that is explained" >&2
  exit 1
fi

if ! jq -e '.openapi and .paths' "$tmp" >/dev/null 2>&1; then
  echo "error: downloaded file is not a valid OpenAPI document" >&2
  exit 1
fi

ver="$(jq -r '.info.version' "$tmp")"
paths="$(jq -r '.paths | length' "$tmp")"
# -n keeps the gzip stream free of the mtime and name of the temp file, so
# the vendored artifact is reproducible from the pins above.
gzip -n -9 -c "$tmp" > "$DEST"
gz_sha="$(shasum -a 256 "$DEST" | cut -d' ' -f1)"

: > "$routes_tmp"
extra_meta=""
index_description() {
  local name="$1"
  local commit="$2"
  local path="$3"
  local want="$4"
  local url="https://raw.githubusercontent.com/github/rest-api-description/$commit/$path"
  local got
  local count

  echo "Fetching $url"
  curl -sSLf -o "$extra_tmp" "$url"
  got="$(shasum -a 256 "$extra_tmp" | cut -d' ' -f1)"
  if [[ "$got" != "$want" ]]; then
    echo "error: $path at $commit hashes to $got, expected $want" >&2
    exit 1
  fi
  count=0
  while IFS= read -r route; do
    printf '%s\t%s\n' "$name" "$route" >> "$routes_tmp"
    count=$((count + 1))
  done < <(normalized_routes "$extra_tmp")
  if [[ "$count" -lt 300 ]]; then
    echo "error: $path yielded only $count routes" >&2
    exit 1
  fi
  extra_meta+="route index: $name commit $commit sha256 $want, $count routes"$'\n'
}

for entry in "${EXTRA_DESCRIPTIONS[@]}"; do
  name="${entry%%:*}"
  want="${entry##*:}"
  path="descriptions/$name/$name.json"
  index_description "$name" "$PIN_COMMIT" "$path" "$want"
done
index_description "api-2022" "$API_2022_COMMIT" "$SPEC_PATH" "$API_2022_SHA256"
gzip -n -9 -c "$routes_tmp" > "$ROUTE_INDEX"

cat > "$VERSION_FILE" <<EOF
vendored: github.com/github/rest-api-description ($SPEC_PATH, bundled api.github.com description)
upstream commit: $PIN_COMMIT
source: $URL
content sha256: $PIN_SHA256
gzip sha256: $gz_sha
openapi info.version: $ver
paths: $paths
${extra_meta}route index sha256: $(shasum -a 256 "$ROUTE_INDEX" | cut -d' ' -f1)
refresh: scripts/update-github-openapi.sh
EOF

echo "Vendored $DEST"
echo "  commit $PIN_COMMIT, info.version $ver, $paths paths"
echo "  content sha256 $PIN_SHA256"
echo "  gzip sha256 $gz_sha ($(wc -c < "$DEST" | tr -d ' ') bytes)"
echo "Vendored $ROUTE_INDEX ($(wc -l < "$routes_tmp" | tr -d ' ') routes, $(wc -c < "$ROUTE_INDEX" | tr -d ' ') bytes)"
