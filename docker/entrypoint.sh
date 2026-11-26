#!/bin/sh
# Normalize environment defaults, then hand off to the ChronoHost binary.
set -eu

: "${CHRONOHOST_DATA:=/data}"
: "${CHRONOHOST_ADDR:=:8080}"
export CHRONOHOST_DATA CHRONOHOST_ADDR

mkdir -p \
  "$CHRONOHOST_DATA/binaries" \
  "$CHRONOHOST_DATA/instances" \
  "$CHRONOHOST_DATA/backups" \
  "$CHRONOHOST_DATA/db"

exec /usr/local/bin/chronohost "$@"
