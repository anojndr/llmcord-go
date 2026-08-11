#!/usr/bin/env bash
#
# restart.sh - restart the llmcord-go Discord bot.
#
# Stops any currently running instance, then starts a fresh one. Old
# instances get SIGTERM first so the bot shuts down gracefully and saves its
# Discord resume state; stragglers get SIGKILL after a short grace period.
# Works whether or not a bot is already running.
#
# It kills both halves of a `go run ./cmd/llmcord-go` invocation: the `go run`
# wrapper process and the compiled `llmcord-go` binary (go-build cache copies
# or the .run/llmcord-go build).
#
# Default: the new instance runs in the background, output appended to
# ./llmcord-go.log (override with LLMCORD_LOG_FILE). Pass --foreground to run
# it in this terminal instead (Ctrl+C stops it, like a manual `go run`).
#
# The script inherits your environment, so config overrides work as usual:
#   LLMCORD_CONFIG_PATH=/path/to/config.yaml ./restart.sh

set -euo pipefail

cd "$(dirname "$0")"

LOG_FILE="${LLMCORD_LOG_FILE:-llmcord-go.log}"
ONLINE_TIMEOUT="${LLMCORD_ONLINE_TIMEOUT:-30}"

log() { printf '[restart] %s\n' "$*"; }

# PIDs of any running bot instance: the compiled binary (process name
# "llmcord-go") and the `go run ./cmd/llmcord-go` wrapper process.
llmcord_pids() {
  { pgrep -x llmcord-go; pgrep -f 'go run ./cmd/llmcord-go'; } 2>/dev/null || true
}

stop_bot() {
  local pids
  pids="$(llmcord_pids)"
  if [[ -z "$pids" ]]; then
    log "no running bot found"
    return 0
  fi
  log "stopping pids: $(echo "$pids" | tr '\n' ' ')"
  kill -TERM $pids 2>/dev/null || true

  local waited=0
  while [[ -n "$(llmcord_pids)" ]] && (( waited < 10 )); do
    sleep 1
    waited=$(( waited + 1 ))
  done

  pids="$(llmcord_pids)"
  if [[ -n "$pids" ]]; then
    log "still running after ${waited}s, forcing kill"
    kill -KILL $pids 2>/dev/null || true
    sleep 1
  fi
  log "stopped"
}

wait_online() {
  local pid="$1"
  local start waited=0
  start="$(wc -l < "$LOG_FILE")"
  while (( waited < ONLINE_TIMEOUT )); do
    if ! kill -0 "$pid" 2>/dev/null && [[ -z "$(llmcord_pids)" ]]; then
      log "bot exited before coming online; last log lines:"
      tail -n 20 "$LOG_FILE" || true
      return 1
    fi
    if awk -v start="$((start + 1))" \
        'NR >= start && /bot is online/ { found = 1; exit } END { exit !found }' \
        "$LOG_FILE"; then
      log "bot is online"
      return 0
    fi
    sleep 1
    waited=$(( waited + 1 ))
  done
  log "not online after ${ONLINE_TIMEOUT}s; check: tail -f $LOG_FILE"
  return 1
}

start_bot() {
  if (( foreground )); then
    log "starting in foreground (Ctrl+C stops it)"
    exec go run ./cmd/llmcord-go
  fi
  : >> "$LOG_FILE"
  nohup go run ./cmd/llmcord-go >> "$LOG_FILE" 2>&1 &
  local pid="$!"
  log "started pid $pid; output -> $LOG_FILE (tail -f $LOG_FILE)"
  wait_online "$pid"
}

foreground=0
case "${1:-}" in
  '') ;;
  --foreground) foreground=1 ;;
  *) echo "usage: $0 [--foreground]" >&2; exit 2 ;;
esac

stop_bot
start_bot
