#!/bin/sh
set -eu

# Use a disposable installation: the user-flow suite creates test users and keys.
: "${BASE_URL:?Set BASE_URL to a disposable Sub2API installation with registration enabled}"
cd "$(dirname "$0")/.."
exec go test -tags=e2e -count=1 -timeout=300s "$@" ./internal/integration/...
