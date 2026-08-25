#!/usr/bin/env bash
# Regenerate internal/graphqlschema from the vendored GitHub public GraphQL
# schema (third_party/github-graphql-schema.graphql.gz).
#
# The generated package defines every one of GitHub's 1,807 GraphQL types —
# object, interface, union, enum, input object and custom scalar — with
# GitHub's exact field names, argument names and types, default values,
# nullability, list nesting, deprecations, interface implementations and
# descriptions. Hand-writing that surface is what this replaces.
#
# The generator is a Go build-time command in-tree (internal/graphqlschemagen),
# following internal/emojigen: no network, no pinned external toolchain, and
# the same parser (github.com/graphql-go/graphql/language/parser) the schema
# ratchet in internal/server uses to read the same vendored file, so the
# generator and its oracle cannot disagree about what the SDL says.
#
# Completeness is proved by
# `go test ./internal/server -run TestGeneratedGraphQLSchema`, which
# introspects the generated schema and diffs it against the vendored SDL.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GOWORK=off go run ./internal/graphqlschemagen "$@"
gofmt -l internal/graphqlschema
