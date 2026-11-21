#!/bin/sh
# Development entrypoint for AI48Proxy: obtain a one-shot enrollment token from
# Home on first start, keep it in the worker state volume, then run cargo watch.
set -eu

state=/var/lib/ai48proxy
jwt="$state/home.jwt"
mkdir -p "$state"

# Home may still be downloading modules and compiling on first start.
echo "[dev] waiting for Home at ${AI48_DEV_HOME_URL}"
waited=0
until curl -fsS -o /dev/null -H "X-MANAGEMENT-KEY: ${AI48_DEV_MANAGEMENT_KEY}" \
  "${AI48_DEV_HOME_URL}/v8/management/nodes" 2>/dev/null; do
  sleep 3
  waited=$((waited + 3))
  if [ $((waited % 30)) -eq 0 ]; then
    echo "[dev] still waiting for Home (${waited}s)"
  fi
done
echo "[dev] Home is ready"

if [ ! -s "$jwt" ]; then
  # An identity without its token cannot be re-enrolled; start clean.
  rm -rf "$state/identity"
  echo "[dev] requesting enrollment token from Home"
  response="$(curl -fsS -X POST \
    -H "X-MANAGEMENT-KEY: ${AI48_DEV_MANAGEMENT_KEY}" \
    -H 'Content-Type: application/json' \
    --data '{"node_name":"ai48proxy-dev"}' \
    "${AI48_DEV_HOME_URL}/v8/management/certificates/clients")"
  token="$(printf '%s' "$response" | sed -n 's/.*"home_jwt":"\([^"]*\)".*/\1/p')"
  if [ -z "$token" ]; then
    echo "[dev] Home did not return an enrollment token" >&2
    exit 1
  fi
  umask 077
  printf '%s' "$token" >"$jwt.tmp"
  mv "$jwt.tmp" "$jwt"
  echo "[dev] enrollment token stored in the worker state volume"
fi

# A crash during enrollment can leave this lock behind; nothing else runs here.
rm -f "$state/identity/enrollment.lock"

export HOME_JWT_FILE="$jwt"
exec cargo watch --why -x "run -- --identity-dir $state/identity"
