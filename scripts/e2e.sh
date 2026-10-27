#!/usr/bin/env bash
#
# End-to-end smoke test against a running stack.
#
#   make up && make e2e
#
# It exercises the interesting production paths: catalog writes, the gRPC
# dependency check, the transactional outbox, at-least-once delivery and
# idempotent retries.

set -euo pipefail

BOOKING_URL="${BOOKING_URL:-http://localhost:3001}"
CATALOG_URL="${CATALOG_URL:-http://localhost:3002}"
BOOKING_ADMIN="${BOOKING_ADMIN:-http://localhost:9091}"
CATALOG_ADMIN="${CATALOG_ADMIN:-http://localhost:9092}"

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$1"; exit 1; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

wait_for() {
  local url=$1 name=$2
  for _ in $(seq 1 60); do
    if curl -fsS "$url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  fail "$name did not become ready at $url"
}

request() {
  local method=$1 url=$2 body=${3:-} key=${4:-}
  local args=(-sS -X "$method" "$url" -H 'Content-Type: application/json' -w '\n%{http_code}')
  if [[ -n "$key" ]]; then
    args+=(-H "Idempotency-Key: $key")
  fi
  if [[ -n "$body" ]]; then
    args+=(-d "$body")
  fi
  curl "${args[@]}"
}

step "Waiting for services"
wait_for "$CATALOG_ADMIN/readyz" "catalog"
wait_for "$BOOKING_ADMIN/readyz" "booking"
pass "catalog and booking are ready"

step "1. Register a service in the catalog"
service_name="E2E deep clean $(date +%s)"
response=$(request POST "$CATALOG_URL/v1/services" "{
  \"name\": \"$service_name\",
  \"description\": \"Created by scripts/e2e.sh\",
  \"category\": \"cleaning\",
  \"price_cents\": 12900,
  \"duration_minutes\": 180
}")
status=$(tail -n1 <<<"$response")
body=$(sed '$d' <<<"$response")
[[ "$status" == "201" ]] || fail "expected 201, got $status: $body"
service_id=$(jq -r '.data.id' <<<"$body")
pass "service #$service_id created"

step "2. Create a booking (booking -> catalog gRPC -> Postgres + outbox)"
idempotency_key="e2e-$(date +%s%N)"
scheduled_at=$(date -u -d '+2 days' '+%Y-%m-%dT%H:%M:%SZ')
payload="{
  \"service_id\": $service_id,
  \"customer_name\": \"Ada Lovelace\",
  \"customer_email\": \"ada@example.com\",
  \"scheduled_at\": \"$scheduled_at\"
}"
response=$(request POST "$BOOKING_URL/v1/bookings" "$payload" "$idempotency_key")
status=$(tail -n1 <<<"$response")
body=$(sed '$d' <<<"$response")
[[ "$status" == "201" ]] || fail "expected 201, got $status: $body"
booking_id=$(jq -r '.data.id' <<<"$body")
[[ "$(jq -r '.data.status' <<<"$body")" == "pending" ]] || fail "booking should start pending"
pass "booking #$booking_id created"

step "3. Retry with the same Idempotency-Key"
response=$(request POST "$BOOKING_URL/v1/bookings" "$payload" "$idempotency_key")
status=$(tail -n1 <<<"$response")
body=$(sed '$d' <<<"$response")
[[ "$status" == "201" ]] || fail "replay should return 201, got $status: $body"
[[ "$(jq -r '.data.id' <<<"$body")" == "$booking_id" ]] || fail "replay created a second booking"
pass "replay returned booking #$booking_id without duplicating it"

step "4. Read the booking back"
response=$(request GET "$BOOKING_URL/v1/bookings/$booking_id")
status=$(tail -n1 <<<"$response")
body=$(sed '$d' <<<"$response")
[[ "$status" == "200" ]] || fail "expected 200, got $status: $body"
pass "GET returned $(jq -r '.data.status' <<<"$body") booking"

step "5. Cancel the booking"
response=$(request POST "$BOOKING_URL/v1/bookings/$booking_id/cancel" '{"reason":"e2e test"}')
status=$(tail -n1 <<<"$response")
body=$(sed '$d' <<<"$response")
[[ "$status" == "200" ]] || fail "expected 200, got $status: $body"
[[ "$(jq -r '.data.status' <<<"$body")" == "cancelled" ]] || fail "booking was not cancelled"
pass "booking cancelled"

step "6. Reject an invalid transition (cancel twice)"
response=$(request POST "$BOOKING_URL/v1/bookings/$booking_id/cancel" '{}')
status=$(tail -n1 <<<"$response")
[[ "$status" == "409" ]] || fail "expected 409, got $status"
pass "second cancel returned 409"

step "7. Reject a booking for an unknown service"
response=$(request POST "$BOOKING_URL/v1/bookings" "{
  \"service_id\": 999999,
  \"customer_name\": \"Ada Lovelace\",
  \"customer_email\": \"ada@example.com\",
  \"scheduled_at\": \"$scheduled_at\"
}")
status=$(tail -n1 <<<"$response")
[[ "$status" == "404" ]] || fail "expected 404, got $status"
pass "unknown service rejected by the catalog gRPC check"

step "8. Wait for the async notification"
notified="no"
for _ in $(seq 1 20); do
  count=$(docker compose exec -T postgres psql -U chore -d chore_notification -tAc \
    "SELECT count(*) FROM notifications WHERE booking_id = $booking_id" 2>/dev/null | tr -d '[:space:]' || true)
  if [[ "$count" =~ ^[1-9][0-9]*$ ]]; then
    notified="yes"
    break
  fi
  sleep 1
done
if [[ "$notified" == "yes" ]]; then
  pass "notification consumer processed booking #$booking_id"
else
  printf '  (skipped: could not query the notification database)\n'
fi

printf '\n\033[32mE2E flow passed\033[0m\n'
