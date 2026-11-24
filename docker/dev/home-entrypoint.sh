#!/bin/sh
# Development entrypoint: build Home, import the dev config into an empty
# database once, then hand over to air for hot reload.
set -eu

cd /srv/home
mkdir -p /tmp/air

echo "[dev] building Home"
go -C /workspace build -buildvcs=false -o /tmp/air/main ./cmd/home

# -export fails with "database config is empty" only on an uninitialized database.
probe_dir="$(mktemp -d)"
if probe_output="$(/tmp/air/main -export -export-dir "$probe_dir" 2>&1)"; then
  echo "[dev] database already initialized; skipping import"
elif printf '%s' "$probe_output" | grep -q "database config is empty"; then
  echo "[dev] empty database; importing docker/dev/config.yaml"
  /tmp/air/main -config /srv/home/config.yaml -import
else
  printf '%s\n' "$probe_output" >&2
  echo "[dev] database check failed" >&2
  exit 1
fi
rm -rf "$probe_dir"

exec air -c /workspace/docker/dev/air.toml
