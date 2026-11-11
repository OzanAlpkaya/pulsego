#!/usr/bin/env bash
# End-to-end demo: runs PulseGo against local demo targets, walks a monitor
# through an incident (down -> open -> up -> resolved), and regenerates
#   docs/screenshots/dashboard.png
#   docs/screenshots/incident-open.png
#   docs/screenshots/incident-resolved.png
#   docs/demo.gif
#
#   ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true make demo
#   ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true DATABASE_URL=postgres://... scripts/demo.sh
#
# LOCAL DEVELOPMENT ONLY. The demo targets listen on 127.0.0.1, which
# PulseGo's SSRF protection refuses unless the api and worker run with
# ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true. That switch turns SSRF
# protection off, so no committed file sets it — not this script, the
# Makefile, .env.example, Compose or CI (CLAUDE.md Rule 8). The caller passes
# both variables on the command line, which makes the decision explicit
# every time; the script refuses to start anything unless the inherited
# environment has exactly ENVIRONMENT=development and
# ALLOW_PRIVATE_TARGETS=true, and the api and worker simply inherit them.
#
# With make: the Makefile includes .env (if present) and exports it, but a
# value already in the environment (`VAR=... make demo`) or given as a make
# argument (`make demo VAR=...`) wins over .env. So a .env copied from
# .env.example (ALLOW_PRIVATE_TARGETS=false) does not undo the command-line
# opt-in, and with no opt-in on the command line that .env value makes the
# script refuse, as intended. Remaining .env settings (DATABASE_URL, ...)
# still apply unless overridden the same way.
#
# Requirements: Go, curl, ffmpeg, a Chromium/Chrome on PATH, and an empty
# PostgreSQL database in DATABASE_URL (the script migrates it; re-runs reuse
# the seeded monitors, so earlier incidents stay visible — use a fresh
# database for clean screenshots). In the agent-office sandbox:
#   pg-test-up
#   psql "$PULSEGO_TEST_DATABASE_URL" -c 'CREATE DATABASE pulsego_demo'
#   ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true \
#     DATABASE_URL='postgres://pulsego@127.0.0.1:54329/pulsego_demo?sslmode=disable' make demo
#
# Optional environment:
#   PULSEGO_DEMO_API_KEY  API key for the demo api (default: random, never printed)
#   DEMO_API_ADDR         public api address        (default 127.0.0.1:18080)
#   DEMO_TARGETS_ADDR     demo targets address      (default 127.0.0.1:18082)
#   DEMO_API_INTERNAL_ADDR / DEMO_WORKER_INTERNAL_ADDR (19090 / 19091)
#   DEMO_WARMUP           seconds of check history before capturing (default 60)
#   DEMO_GIF_FPS          GIF frames per second  (default 3)
#   DEMO_GIF_WIDTH        GIF width in pixels    (default 960)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# The SSRF opt-in comes from the caller, never from this script. Check it
# before building or starting anything.
if [[ "${ENVIRONMENT:-}" != "development" || "${ALLOW_PRIVATE_TARGETS:-}" != "true" ]]; then
  cat >&2 <<EOF
demo: refusing to start: the demo needs ENVIRONMENT=development and
ALLOW_PRIVATE_TARGETS=true in the environment, got:
  ENVIRONMENT=${ENVIRONMENT:-<unset>}
  ALLOW_PRIVATE_TARGETS=${ALLOW_PRIVATE_TARGETS:-<unset>}
Together they turn SSRF protection off so the api and worker can check the
demo targets on 127.0.0.1.

Run it as (local demo only, never in production):

  ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true make demo

EOF
  exit 2
fi

: "${DATABASE_URL:?DATABASE_URL is required: point it at a throwaway PostgreSQL database}"
api_addr="${DEMO_API_ADDR:-127.0.0.1:18080}"
targets_addr="${DEMO_TARGETS_ADDR:-127.0.0.1:18082}"
api_internal="${DEMO_API_INTERNAL_ADDR:-127.0.0.1:19090}"
worker_internal="${DEMO_WORKER_INTERNAL_ADDR:-127.0.0.1:19091}"
warmup="${DEMO_WARMUP:-60}"
gif_fps="${DEMO_GIF_FPS:-3}"
gif_width="${DEMO_GIF_WIDTH:-960}"
shots="$root/docs/screenshots"
gif="$root/docs/demo.gif"

for tool in go curl ffmpeg; do
  command -v "$tool" >/dev/null || { echo "demo: $tool is required" >&2; exit 1; }
done

# A throwaway key that is obviously a demo key; it lives only in this process
# tree and is never printed or written to disk.
api_key="${PULSEGO_DEMO_API_KEY:-demo-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')}"

work="$(mktemp -d "${TMPDIR:-/tmp}/pulsego-demo.XXXXXX")"
pids=()
ok=0
cleanup() {
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in "${pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done
  if [[ "$ok" == 1 ]]; then
    rm -rf "$work"
  else
    echo "demo: failed; logs kept in $work" >&2
  fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

log() { printf '\n==> %s\n' "$*"; }

wait_http() { # url, label
  for _ in $(seq 1 60); do
    if curl -fsS -o /dev/null "$1" 2>/dev/null; then return 0; fi
    sleep 0.5
  done
  echo "demo: $2 did not come up at $1 (see $work/*.log)" >&2
  return 1
}

cat >&2 <<'EOF'

  WARNING: local demo only. The api and worker below inherit
  ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true from your command
  line, which turns SSRF protection off so they can check targets on
  127.0.0.1. Never use this setting outside local development.

EOF

# Build everything before the browser starts: Chromium and a parallel build
# compete for the same process budget.
log "building binaries"
go build -o "$work/api" ./cmd/api
go build -o "$work/worker" ./cmd/worker
go build -o "$work/migrate" ./cmd/migrate
(cd demo && go build -o "$work/targets" ./cmd/targets && go build -o "$work/capture" ./cmd/capture)

# Shared settings. ENVIRONMENT and ALLOW_PRIVATE_TARGETS come from the caller
# (checked above) and are inherited unchanged.
export DATABASE_URL ENVIRONMENT ALLOW_PRIVATE_TARGETS API_KEY="$api_key" LOG_LEVEL=warn OTEL_ENABLED=false

log "migrating"
"$work/migrate" up

log "starting demo targets on $targets_addr"
"$work/targets" -addr "$targets_addr" >"$work/targets.log" 2>&1 &
pids+=($!)
wait_http "http://$targets_addr/steady" "demo targets"

log "starting api on $api_addr and worker"
HTTP_ADDR="$api_addr" INTERNAL_HTTP_ADDR="$api_internal" \
  "$work/api" >"$work/api.log" 2>&1 &
pids+=($!)
INTERNAL_HTTP_ADDR="$worker_internal" WORKER_POLL_INTERVAL=1s \
  "$work/worker" >"$work/worker.log" 2>&1 &
pids+=($!)
wait_http "http://$api_addr/healthz" "api"
wait_http "http://$worker_internal/healthz" "worker"

log "seeding monitors"
API_URL="http://$api_addr" TARGET_BASE="http://$targets_addr" scripts/seed-demo.sh

log "collecting ${warmup}s of check history"
sleep "$warmup"

log "capturing (flaky goes down, incident opens, flaky recovers, incident resolves)"
mkdir -p "$shots"
PULSEGO_DEMO_API_KEY="$api_key" "$work/capture" \
  -url "http://$api_addr/" \
  -flaky-control "http://$targets_addr/control/flaky" \
  -out "$shots" -frames "$work/frames"
for f in dashboard.png incident-open.png incident-resolved.png; do
  test -s "$shots/$f" || { echo "demo: $shots/$f is missing or empty" >&2; exit 1; }
done

log "building $gif"
ffmpeg -hide_banner -loglevel error -y -framerate "$gif_fps" -i "$work/frames/%03d.png" \
  -vf "scale=${gif_width}:-1:flags=lanczos,split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" \
  "$gif"
test -s "$gif"

ok=1
log "done"
ls -l "$shots"/*.png "$gif"
