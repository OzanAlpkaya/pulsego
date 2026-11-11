#!/usr/bin/env bash
# Seeds demo monitors into a running PulseGo API.
#
#   API_KEY=...  scripts/seed-demo.sh
#
# Environment:
#   API_KEY      required; the key the API was started with
#   API_URL      default http://localhost:8080
#   TARGET_BASE  base URL of the local demo targets (demo/cmd/targets), for
#                example http://127.0.0.1:18082. Loopback targets are only
#                accepted by an API started with ENVIRONMENT=development
#                ALLOW_PRIVATE_TARGETS=true (scripts/demo.sh checks that its
#                caller passed both).
#                Unset: public endpoints are used instead, which suits the
#                Compose stack with the default SSRF policy.
#
# Re-running is safe: a monitor whose name already exists is skipped, not
# duplicated (and not updated — delete it first to change its settings).
set -euo pipefail

API_URL="${API_URL:-http://localhost:8080}"
API_URL="${API_URL%/}"
: "${API_KEY:?API_KEY is required (the key the API was started with)}"

# name|url|interval_seconds|failure_threshold
if [[ -n "${TARGET_BASE:-}" ]]; then
  base="${TARGET_BASE%/}"
  monitors=(
    "Storefront|$base/steady|10|3"
    "Checkout API|$base/slow|10|3"
    "Search|$base/unstable|10|3"
    "Payments webhook|$base/flaky|10|2"
  )
else
  monitors=(
    "Example homepage|https://example.com/|30|3"
    "IANA|https://www.iana.org/|30|3"
    "HTTPBin delay|https://httpbin.org/delay/1|30|3"
    "HTTPBin 503|https://httpbin.org/status/503|10|2"
  )
fi

auth=(-H "Authorization: Bearer $API_KEY")

# One listing is enough: the seed set is far below the 100-per-page limit.
existing="$(curl -fsS "${auth[@]}" "$API_URL/v1/monitors?limit=100")" || {
  echo "seed-demo: cannot list monitors at $API_URL (is the API running, is API_KEY right?)" >&2
  exit 1
}

json_escape() {
  local s="${1//\\/\\\\}"
  printf '%s' "${s//\"/\\\"}"
}

created=0
skipped=0
for entry in "${monitors[@]}"; do
  IFS='|' read -r name url interval threshold <<<"$entry"
  if grep -qF "\"name\":\"$(json_escape "$name")\"" <<<"$existing"; then
    echo "skip    $name (already exists)"
    skipped=$((skipped + 1))
    continue
  fi
  body=$(printf '{"name":"%s","url":"%s","interval_seconds":%d,"timeout_ms":3000,"failure_threshold":%d}' \
    "$(json_escape "$name")" "$(json_escape "$url")" "$interval" "$threshold")
  resp="$(mktemp)"
  code=$(curl -sS -o "$resp" -w '%{http_code}' "${auth[@]}" \
    -H 'Content-Type: application/json' -X POST --data "$body" "$API_URL/v1/monitors")
  if [[ "$code" != 201 ]]; then
    echo "seed-demo: creating \"$name\" failed with HTTP $code:" >&2
    cat "$resp" >&2
    echo >&2
    rm -f "$resp"
    exit 1
  fi
  rm -f "$resp"
  echo "created $name -> $url (every ${interval}s, down after $threshold failures)"
  created=$((created + 1))
done

echo "seed-demo: $created created, $skipped skipped"
