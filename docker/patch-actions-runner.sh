#!/usr/bin/env bash
# Apply reviewed servicing fixes to the immutable upstream runner source.
set -euo pipefail
: "${RUNNER_SOURCE_DIR:=/runner-src}"
project="$RUNNER_SOURCE_DIR/src/Sdk/Sdk.csproj"
# GHSA-846f-f764-76mx and GHSA-2fmv-rv2c-59x7 affect PKCS through 10.0.12.
# 10.0.15 was published on 2026-11-10; its ASN.1 dependency must match.
for package in System.Security.Cryptography.Pkcs System.Formats.Asn1; do
    grep -Fq "Include=\"$package\" Version=\"10.0.11\"" "$project"
    sed -i "s/Include=\"$package\" Version=\"10.0.11\"/Include=\"$package\" Version=\"10.0.15\"/" "$project"
done

# DevTunnels brings a vulnerable WebUtilities transitively (GHSA-x33c-w48q-qrgc).
# Pin the patched .NET 8 servicing package without changing the runner protocol.
worker="$RUNNER_SOURCE_DIR/src/Runner.Worker/Runner.Worker.csproj"
grep -Fq 'Include="Microsoft.DevTunnels.Connections" Version="1.3.51"' "$worker"
sed -i '/Include="Microsoft.DevTunnels.Connections" Version="1.3.51"/a\
    <PackageReference Include="Microsoft.AspNetCore.WebUtilities" Version="8.0.34" />' "$worker"
