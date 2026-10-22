#!/bin/sh
# Builds the plugin for the current CPU architecture.
# Usage: sh scripts/build.sh [version]
set -eu
VERSION=${1:-0.0.0-dev}
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
mkdir -p "$ROOT/dist"
EXT=dylib
[ "$(uname)" != "Darwin" ] && EXT=so
go build -buildvcs=false -trimpath -buildmode=c-shared \
  -ldflags "-s -w -X main.version=$VERSION" \
  -o "dist/codex-quota-guard-v$VERSION.$EXT" .
rm -f "dist/codex-quota-guard-v$VERSION.h"
echo "$ROOT/dist/codex-quota-guard-v$VERSION.$EXT"
