#!/usr/bin/env bash
set -efux

go build -o proxy/proxy ./cmd/proxy
CGO_ENABLED=0 go install ./cmd/yeoman
yeoman -http service deploy proxy
