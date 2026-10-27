# Runbook

Operational guide for the chore-booking stack. Commands assume the Compose
project from the repository root (`docker compose ...`), and `psql` access via
the `postgres` container.

## 1. At a glance

| What | Where |
| --- | --- |
| Public APIs | catalog `:3002`, booking `:3001` |
| Admin/probes/metrics | catalog `:9092`, booking `:9091`, notification `:9093` |
| Health | `GET /healthz`, `GET /readyz`, `GET /version`, `GET /metrics` |
| Traces | Jaeger UI http://localhost:16686 |
| Metrics | Prometheus http://localhost:9090 |
| Broker UI | RabbitMQ management http://localhost:15672 (`chore`/`chore`) |
| Logs | `docker compose logs -f <service>` |
| Queue | `notification.booking-events` |
| DLQ | `notification.booking-events.dlq` via exchange `chore.events.dlx` |
| Exchange | `chore.events` (topic) |

Service logs are structured JSON. Useful keys: `service`, `env`, `version`,
`request_id`, `trace_id`, `span_id`, `event_id`, `outbox_id`, `booking_id`,
`status`, `duration`, `error`.

## 2. Alert signals

There are no alert rules wired into Prometheus in this sample, but these are
the queries worth alerting on.

### 2.1 Outbox backlog is stuck

```promql
chore_outbox_oldest_pending_seconds{service="booking"} > 120
```

Alert on **age**, not count. A steady backlog of 20 messages that drains in
milliseconds is fine; one 3-minute-old message means publishing is broken.

Diagnose:

```bash
# Are rows pending, retrying, or dead?
docker compose exec -T postgres psql -U chore -d chore_booking -c "
SELECT
  count(*) FILTER (WHERE published_at IS NULL AND failed_at IS NULL) AS pending,
  count(*) FILTER (WHERE failed_at IS NOT NULL)                    AS dead,
  min(available_at) FILTER (WHERE published_at IS NULL AND failed_at IS NULL) AS oldest_available,
  max(attempts)                                                    AS max_attempts
FROM outbox_messages;"

# Which errors are repeating?
docker compose exec -T postgres psql -U chore -d chore_booking -c "
SELECT attempts, event_type, left(last_error, 160) AS error, count(*)
FROM outbox_messages
WHERE published_at IS NULL AND failed_at IS NULL
GROUP BY 1,2,3 ORDER BY 1 DESC;"
```

Common causes:

| Symptom | Cause | Action |
| --- | --- | --- |
| `last_error` mentions connection/EOF | RabbitMQ restart or network blip | The relay retries automatically; confirm broker health |
| `timed out waiting for publisher confirm` | Broker overloaded or slow disk | Check RabbitMQ memory/disk alarms; scale broker |
| `broker rejected message (nack)` | Exchange missing or policy mismatch | Verify exchange/topology; restart booking to re-declare |
| Rows stay `claimed_at` set, attempts not increasing | Relay process is stuck or dead | Restart booking; the 2-minute lease makes rows claimable again |

Check the service is alive and publishing:

```bash
curl -fsS http://localhost:9091/readyz | jq
docker compose logs --since=10m booking | grep -i outbox
```

### 2.2 DLQ is growing

```bash
# Queue depth
docker compose exec -T rabbitmq rabbitmqctl list_queues name messages

# Inspect without consuming (management UI is easier for payloads)
docker compose exec -T rabbitmq rabbitmqadmin list queues name messages
```

Typical causes: invalid event payload (a producer bug), unsupported event type,
or repeated database failures during handling.

Replay strategy:

1. Fix the consumer or the producer. Never replay into a broken consumer.
2. Inspect one message in the management UI (Queues → DLQ → Get messages,
   `requeue=false` to peek/pop carefully).
3. Move messages back:

   ```bash
   # RabbitMQ has no supported "move" CLI; use shovel or the management UI.
   # Shovel is the safe operational path because it preserves payload/headers.
   docker compose exec -T rabbitmq rabbitmq-plugins enable rabbitmq_shovel rabbitmq_shovel_management
   ```

   Create a dynamic shovel from `notification.booking-events.dlq` to
   `notification.booking-events` (AMQP URI `amqp://chore:chore@rabbitmq:5672/`),
   run it, then delete the shovel. The consumer deduplicates by `event_id`, so
   replaying a message that was already processed is harmless.

4. If the message is genuinely poison, discard it and record why:

   ```bash
   # Pop one message without requeue (management UI is preferable for audit)
   docker compose exec -T rabbitmq rabbitmqadmin get queue=notification.booking-events.dlq count=1 requeue=false
   ```

### 2.3 Consumer stopped consuming

```bash
docker compose ps notification
curl -fsS http://localhost:9093/readyz | jq
docker compose logs --since=15m notification | tail -100
```

- `readyz` fails on `rabbitmq`: broker unreachable. The consumer reconnects
  with exponential backoff (1s → 30s); check broker health.
- `readyz` fails on `postgres`: notification cannot record processed events;
  messages will be nacked and retried until the DLQ limit. Restore the DB
  first, then replay from the DLQ.
- Process alive but no `consumer ready` line: topology declaration failed
  (permissions, conflicting queue arguments). The error is in the logs.

### 2.4 High error rate / latency

```promql
sum by (route, status) (rate(chore_http_requests_total{status=~"5.."}[5m]))

histogram_quantile(0.99,
  sum by (le, route) (rate(chore_http_request_duration_seconds_bucket[5m])))

sum by (method, code) (rate(chore_grpc_requests_total{code!="OK"}[5m]))
```

Then find a trace:

1. Take a `trace_id` from the error envelope or the log line.
2. Open `http://localhost:16686/trace/<trace_id>`.
3. Check the span that failed: HTTP handler → gRPC client → catalog server →
   Postgres, or the outbox publish span linked to the original trace.

Common patterns:

| Pattern | Meaning |
| --- | --- |
| `booking` 503, gRPC `Unavailable` | Catalog down/unreachable; check catalog `/readyz` |
| `booking` 500 with `internal_error` | Unexpected bug; use `request_id` to find the log line and stack of wrapped causes |
| Latency high only on `POST /v1/bookings` | Catalog gRPC slow (2s deadline) or DB contention |
| Latency high on all routes | Postgres pool saturation; check `DB_MAX_CONNS`, slow queries |
| 429 responses | Per-IP rate limit; adjust `HTTP_RATE_LIMIT_PER_MINUTE` if intended |

### 2.5 Postgres or RabbitMQ down

| Dependency | Symptom | Behaviour |
| --- | --- | --- |
| Postgres | `/readyz` 503; writes 500 | No data loss for committed rows. Outbox rows remain. Restore DB, restart service. |
| RabbitMQ | `/readyz` 503; outbox backlog grows | Bookings continue to be accepted. Events publish when the broker returns; nothing is lost. |

When RabbitMQ is restored, watch `chore_outbox_oldest_pending_seconds` fall. If
it does not, check publisher confirms and the exchange declaration.

### 2.6 Idempotency conflicts

`409 idempotency_key_reuse` means the client reused a key with a different
method/path/body. This is a client bug; do not “fix” it server-side.

`409 request_in_progress` means a duplicate arrived while the first request is
still running. It is safe for the client to retry after a short delay. If it
persists, the first request may have crashed before releasing the key:

```bash
docker compose exec -T postgres psql -U chore -d chore_booking -c "
SELECT key, state, created_at, expires_at
FROM idempotency_keys
WHERE state = 'in_progress' AND created_at < now() - interval '5 minutes';"
```

The janitor deletes expired keys hourly. For an immediate unblock, delete the
specific stuck key after confirming no request is running:

```sql
DELETE FROM idempotency_keys WHERE key = '<key>';
```

Do not truncate the table during an incident; that removes replay protection for
in-flight clients.

## 3. Routine operations

### Deploy

```bash
make build          # local binaries
make test           # race tests
make up             # build images + start/update the stack
```

Images are distroless, non-root, and contain one binary. Migrations run at
startup (`DB_AUTO_MIGRATE=true`); to gate them, set it to `false` and run the
migration step separately.

### Roll back

```bash
git revert <sha>
make up             # rebuilds and recreates changed services
```

Database migrations are additive in this project. If a migration must be rolled
back in development:

```bash
# Example: inspect current version first
docker compose exec -T postgres psql -U chore -d chore_booking -c "SELECT * FROM schema_migrations;"
```

There is no automatic down path in production. Prefer forward-fixing migrations
and take a backup before destructive changes.

### Scale

- **catalog**: stateless; scale HTTP replicas. gRPC clients need a load
  balancer/service discovery in front of replicas (`catalog:50051` in Compose is
  a single DNS name).
- **booking**: stateless for HTTP; multiple relays are safe
  (`SKIP LOCKED` + lease). Keep `OUTBOX_BATCH_SIZE` and poll interval sensible
  per replica.
- **notification**: competing consumers on a quorum queue are safe; the dedupe
  table makes redelivery harmless.

### Backup and restore

```bash
# Backup one service database
docker compose exec -T postgres pg_dump -U chore -Fc chore_booking > booking.dump

# Restore into a fresh database
docker compose exec -T postgres createdb -U chore chore_booking_restore
docker compose exec -T postgres pg_restore -U chore -d chore_booking_restore < booking.dump
```

Restore the outbox table with the booking database; publishing resumes
automatically. Restore `processed_events` with the notification database to
preserve deduplication history.

### Local reset

```bash
make down
docker volume rm chore_postgres_data chore_rabbitmq_data   # destructive
make up
```

## 4. Health checks

```bash
for port in 9092 9091 9093; do
  echo "== $port =="
  curl -fsS "http://localhost:$port/healthz" | jq
  curl -fsS "http://localhost:$port/readyz" | jq
done
```

`/healthz` is process liveness only. `/readyz` returns `503` with a per-check
map when a dependency is unavailable; use it for readiness probes, not for
restart decisions (a restart will not fix Postgres).

## 5. Incident checklist

1. **Scope it.** One service or all? `make ps`, then each `/readyz`.
2. **Check dependencies.** Postgres and RabbitMQ first; most symptoms are
   downstream.
3. **Check the outbox.** Age and `last_error` tell you whether writes are
   accumulating events that cannot be published.
4. **Check the DLQ.** A growing DLQ is a consumer bug or a poison message.
5. **Get a trace.** Use `trace_id` from the error envelope; do not guess from
   timestamps.
6. **Correlate logs.** Search by `request_id` or `event_id`, not by message
   text.
7. **Prefer replay over repair.** Fix the consumer, then replay the DLQ; the
   dedupe table makes it safe.
8. **Write it down.** Add the failure mode and the query that found it to this
   runbook.
