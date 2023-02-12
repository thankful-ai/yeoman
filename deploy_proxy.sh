#!/usr/bin/env bash
set -efux

CGO_ENABLED=0 go build -o proxy/proxy ./cmd/proxy
go install ./cmd/yeoman
cp ~/.config/gcloud/application_default_credentials.json proxy/
yeoman -http service deploy proxy
rm proxy/proxy proxy/application_default_credentials.json
