#!/usr/bin/env bash
# Build Go binaries and copy them into npm platform packages for publishing.
# Usage: scripts/npm-pack.sh [version]
set -euo pipefail

VERSION="${1:-0.0.0}"
DIST="dist"

echo "==> Building Go binaries (version: $VERSION)"
VERSION="$VERSION" make dist

echo "==> Copying binaries into npm packages"

copy_binary() {
  local goos=$1 goarch=$2 npm_dir=$3
  local src="$DIST/troupe-${goos}-${goarch}"
  local dest="npm/${npm_dir}/bin/troupe"
  mkdir -p "npm/${npm_dir}/bin"
  cp "$src" "$dest"
  chmod +x "$dest"
  echo "  $src -> $dest"
}

copy_binary darwin arm64 darwin-arm64
copy_binary darwin amd64 darwin-x64
copy_binary linux  amd64 linux-x64
copy_binary linux  arm64 linux-arm64

echo "==> Updating package versions to $VERSION"
for pkg in npm/troupe npm/darwin-arm64 npm/darwin-x64 npm/linux-x64 npm/linux-arm64; do
  # Use node to update version in package.json
  node -e "
    const fs = require('fs');
    const p = JSON.parse(fs.readFileSync('$pkg/package.json', 'utf8'));
    p.version = '$VERSION';
    if (p.optionalDependencies) {
      for (const k of Object.keys(p.optionalDependencies)) {
        p.optionalDependencies[k] = '$VERSION';
      }
    }
    fs.writeFileSync('$pkg/package.json', JSON.stringify(p, null, 2) + '\n');
  "
done

echo "==> Done. Packages ready in npm/"
echo "To publish: cd npm/<pkg> && npm publish --access public"
