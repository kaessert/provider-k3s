#!/usr/bin/env bash
# Every test/hooks/post-assert-<resource>.sh is a symlink to this script; the
# invocation name ($0) selects the manifest. ROOT is absolute because `go -C`
# runs the tool with tools/update-tester as its working directory.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
exec go -C "$ROOT/tools/update-tester" tool crossplane-update-tester \
  hook "$(basename "$0")" --root "$ROOT"
