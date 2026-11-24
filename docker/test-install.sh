#!/usr/bin/env bash
# Offline production installer checks; never contacts Docker or Home.
set -Eeuo pipefail
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
sed '/^main "\$@"$/d' "$ROOT/install.sh" > "$TMP/functions.sh"
source "$TMP/functions.sh"
STATE="$TMP/state"
CALLS="$TMP/calls"
MODE=normal
curl() {
  printf '%s\n' "$*" >> "$CALLS"
  case "${!#}" in
    */v8/management/plugins)
      [[ "$MODE" != read-failure ]] || return 22
      if [[ "$MODE" == invalid ]]; then
        printf '{"plugins_enabled":"unknown"}\n'
      else
        printf '{"plugins_enabled":%s}\n' "$(cat "$STATE")"
      fi
      ;;
    */v8/management/config)
      [[ "$*" == *'-X PATCH'* ]] || return 1
      [[ "$*" == *'{"plugins":{"enabled":false}}'* ]] || return 1
      [[ "$MODE" != patch-failure ]] || return 22
      if [[ "$MODE" != unchanged ]]; then printf 'false\n' > "$STATE"; fi
      printf '{"status":"ok"}\n'
      ;;
    *) return 1 ;;
  esac
}
reset() {
  MODE="$1"
  printf '%s\n' "$2" > "$STATE"
  : > "$CALLS"
}
reset normal false
ensure_plugins_disabled synthetic-test-key
! grep -q -- '-X PATCH' "$CALLS"
reset normal true
ensure_plugins_disabled synthetic-test-key
[[ "$(cat "$STATE")" == false ]]
[[ "$(grep -c '/v8/management/plugins' "$CALLS")" == 2 ]]
grep -q -- '-X PATCH' "$CALLS"
for mode in read-failure invalid patch-failure unchanged; do
  reset "$mode" true
  export MODE STATE CALLS
  export -f curl fail log ensure_plugins_disabled
  if bash -e -o pipefail -c 'ensure_plugins_disabled synthetic-test-key' > "$TMP/output" 2>&1; then
    printf 'FAIL: installer accepted %s\n' "$mode" >&2
    exit 1
  fi
done
BASE_DIR="$TMP"
COMPOSE_FILE="$TMP/docker-compose.yml"
ENV_FILE="$TMP/.env"
: > "$ENV_FILE"
write_files test-password test-management test-gateway
grep -A 1 '^plugins:$' "$TMP/config.yaml" | grep -q 'enabled: false'
# Exercise all worker-starting entry points with mocked infrastructure.
require_existing_installation() { :; }
read_env_value() { printf 'synthetic-value\n'; }
validate_unique_jwts() { :; }
wait_for_postgres() { :; }
wait_for_home() { :; }
home_is_ready() { return 0; }
ensure_home_initialized() { :; }
ensure_worker_jwts() { :; }
set_env_value() { :; }
generate_home_jwt() { printf 'synthetic-jwt\n'; }
ensure_plugins_disabled() { printf 'guard\n' >> "$CALLS"; }
compose() { printf 'compose %s\n' "$*" >> "$CALLS"; }
for action in start_cluster restart_cluster upgrade_home repair_worker deploy_cluster; do
  : > "$CALLS"
  "$action" 1 > "$TMP/output" 2>&1
  awk '/^guard$/ {guard=1} /^compose up .*ai48proxy-/ {if (!guard) exit 1; started=1} END {if (!started) exit 1}' "$CALLS"
done
printf 'Home installer plugin and startup-order checks passed.\n'
