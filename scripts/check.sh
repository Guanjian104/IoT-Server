#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=local
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "gofmt needed:"
  printf '%s\n' "$unformatted"
  exit 1
fi
go test ./...
go vet ./...
go test -race ./...
