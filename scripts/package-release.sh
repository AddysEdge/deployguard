#!/usr/bin/env bash
# Build self-contained demo bundles. Run from any directory with Go and tar installed.
set -euo pipefail

version="${1:?Usage: scripts/package-release.sh vMAJOR.MINOR.PATCH}"
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "Version must be vMAJOR.MINOR.PATCH" >&2
  exit 2
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
mkdir -p dist
build_root="$(mktemp -d "$root/dist/build.XXXXXX")"

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os="${target%/*}"
  arch="${target#*/}"
  stage="$build_root/${os}_${arch}"
  mkdir -p "$stage"
  suffix=""
  [[ "$os" != windows ]] || suffix=".exe"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$stage/deployguard$suffix" ./cmd/deployguard
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w" -o "$stage/dg-fixture$suffix" ./cmd/dg-fixture
  cp examples/deployguard.yaml "$stage/deployguard.yaml"
  if [[ "$os/$arch" == "$(go env GOOS)/$(go env GOARCH)" ]]; then
    "$stage/deployguard$suffix" version
    "$stage/deployguard$suffix" validate --config "$stage/deployguard.yaml"
  fi
  cat > "$stage/README.txt" <<'NOTES'
DeployGuard release bundle

Includes deployguard, the local dg-fixture demo server and deployguard.yaml.
No Go installation, database or API key is required for these executables.

Run "deployguard version" and "deployguard help" (use .exe on Windows).
For a complete local comparison and cleanup commands:
https://github.com/AddysEdge/deployguard/blob/main/docs/downloads.md

Compare only services you own or have permission to test.
NOTES
  tar -czf "$root/dist/deployguard_${version}_${os}_${arch}.tar.gz" -C "$stage" .
done

(cd dist && sha256sum deployguard_"${version}"_*.tar.gz > checksums.txt)
echo "Built six release bundles and dist/checksums.txt"
