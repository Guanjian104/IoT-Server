#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
export GOTOOLCHAIN=local
version=${VERSION:-dev}
revision=${REVISION:-none}
mkdir -p bin
go build -trimpath \
  -ldflags "-X iot-server/internal/buildinfo.Version=${version} -X iot-server/internal/buildinfo.Revision=${revision}" \
  -o bin/server ./cmd/server
