#!/usr/bin/env sh
set -eu

: "${FX_QUOTES_SMOKE_URL:=http://127.0.0.1:8080}"
: "${FX_QUOTES_SMOKE_PAIR:=USD/EUR}"
: "${FX_QUOTES_SMOKE_EXPECTED_RATE:?set the exact expected rate from a deterministic provider fixture}"
: "${FX_QUOTES_SMOKE_TIMEOUT_SECONDS:=30}"

command -v curl >/dev/null
command -v jq >/dev/null

case "$FX_QUOTES_SMOKE_TIMEOUT_SECONDS" in
  ''|0*|*[!0-9]*|?????*) echo "smoke timeout must be an integer from 1 to 3600" >&2; exit 1 ;;
esac
[ "$FX_QUOTES_SMOKE_TIMEOUT_SECONDS" -le 3600 ] || { echo "smoke timeout must be at most 3600 seconds" >&2; exit 1; }
deadline=$(jq -n --argjson timeout "$FX_QUOTES_SMOKE_TIMEOUT_SECONDS" 'now + $timeout')
remaining_time() {
  jq -en --argjson deadline "$deadline" '
    $deadline - now | if . > 0 then . else error("smoke deadline exceeded") end
  '
}

request() {
  remaining=$(remaining_time) || return 1
  curl --connect-timeout "$remaining" --max-time "$remaining" "$@"
}

post_body=$(mktemp)
post_headers=$(mktemp)
replay_body=$(mktemp)
trap 'rm -f "$post_body" "$post_headers" "$replay_body"' EXIT
key="smoke-$(date +%s)-$$-${post_body##*/}"

created_status=$(request -sS -D "$post_headers" -o "$post_body" -w '%{http_code}' \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $key" \
  -d "{\"pair\":\"$FX_QUOTES_SMOKE_PAIR\"}" \
  "$FX_QUOTES_SMOKE_URL/v1/quote-updates")
[ "$created_status" = "202" ]
id=$(jq -er '.id' < "$post_body")
location=$(awk '/^[Ll]ocation:/{sub(/\r$/, ""); sub(/^[^:]*: /, ""); print; exit}' "$post_headers")
[ "$location" = "/v1/quote-updates/$id" ]

while :; do
  update=$(request -fsS "$FX_QUOTES_SMOKE_URL$location")
  status=$(printf '%s' "$update" | jq -er '.status')
  if [ "$status" = "completed" ]; then
    [ "$(printf '%s' "$update" | jq -er '.quote.rate')" = "$FX_QUOTES_SMOKE_EXPECTED_RATE" ]
    break
  fi
  [ "$status" != "failed" ]
  remaining=$(remaining_time)
  sleep "$(jq -n --argjson remaining "$remaining" '[$remaining, 1] | min')"
done

latest=$(request -fsS --get --data-urlencode "pair=$FX_QUOTES_SMOKE_PAIR" "$FX_QUOTES_SMOKE_URL/v1/quotes/latest")
[ "$(printf '%s' "$latest" | jq -er '.rate')" = "$FX_QUOTES_SMOKE_EXPECTED_RATE" ]

replay_status=$(request -sS -o "$replay_body" -w '%{http_code}' \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $key" \
  -d "{\"pair\":\"$FX_QUOTES_SMOKE_PAIR\"}" \
  "$FX_QUOTES_SMOKE_URL/v1/quote-updates")
[ "$replay_status" = "200" ]
[ "$(jq -er '.id' < "$replay_body")" = "$id" ]
remaining_time >/dev/null
echo "smoke passed for $id"
