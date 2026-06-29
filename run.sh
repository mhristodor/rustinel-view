#!/usr/bin/env bash
# Build and launch rustinel-view against a Rustinel EDR snapshot.
#
# Usage:
#   RV_ALERTS=/path/to/alerts.json RV_EVENTS=/path/to/rustinel.log ./run.sh
#
# Optional overrides (env vars):
#   RV_ADDR   bind address   (default 127.0.0.1:18080, loopback-only)
#   RV_LOG    log level       (default info)
#
# For convenience, put the exports in a local, gitignored .run.env next to
# this script; it is sourced automatically if present:
#   # .run.env
#   RV_ALERTS=/path/to/alerts.json
#   RV_EVENTS=/path/to/rustinel.log
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Optional per-machine config (snapshot paths, bind address) kept out of
# version control so the committed defaults stay generic.
# shellcheck source=/dev/null
[[ -f "$REPO/.run.env" ]] && source "$REPO/.run.env"

ALERTS="${RV_ALERTS:-./sample/alerts.json}"
EVENTS="${RV_EVENTS:-./sample/rustinel.log}"
# Loopback-only by default: the viewer has no authentication and no TLS, so
# binding wider exposes the full EDR snapshot to anyone who can reach the
# port. Override with RV_ADDR=0.0.0.0:18080 to expose it on the LAN.
ADDR="${RV_ADDR:-127.0.0.1:18080}"
LOGLVL="${RV_LOG:-info}"

if [[ ! -f "$ALERTS" || ! -f "$EVENTS" ]]; then
  echo "error: alerts/events snapshot not found." >&2
  echo "  alerts: $ALERTS" >&2
  echo "  events: $EVENTS" >&2
  echo >&2
  echo "set RV_ALERTS and RV_EVENTS (via env or a local .run.env), e.g.:" >&2
  echo "  RV_ALERTS=/path/to/alerts.json RV_EVENTS=/path/to/rustinel.log ./run.sh" >&2
  exit 1
fi

cd "$REPO"

echo "==> building"
go build -o ./rustinel-view ./cmd/rustinel-view

echo "==> launching"
echo "    bind:    ${ADDR}"
echo "    alerts:  ${ALERTS}"
echo "    events:  ${EVENTS}  ($(du -h "${EVENTS}" | cut -f1))"
if [[ "${ADDR%:*}" == "0.0.0.0" || "${ADDR%:*}" == "" ]]; then
  # Surface the LAN access URL so it's obvious the viewer is reachable from
  # other devices on the network when bound to all interfaces.
  LAN_IP="$(ip -4 -brief addr 2>/dev/null | awk '$2=="UP" {split($3,a,"/"); print a[1]; exit}')"
  echo
  echo "    !! NO AUTH, NO TLS — bound on all interfaces."
  echo "    !! Anyone on this network can read the full EDR snapshot."
  if [[ -n "${LAN_IP:-}" ]]; then
    echo "    !! LAN URL: http://${LAN_IP}:${ADDR##*:}/"
  fi
fi
echo
exec ./rustinel-view -addr "$ADDR" -log-level "$LOGLVL" "$ALERTS" "$EVENTS"
