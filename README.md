# Chore Booking Microservices

A small but complete booking platform built the way a production Go service
should be: explicit boundaries, a transactional outbox instead of
dual-write bugs, idempotent writes, optimistic concurrency, at-least-once
messaging with deduplication, and first-class observability.

It is deliberately scoped to three services. The point is not the number of
services — it is that every cross-service interaction has a failure mode that
has been thought through.

```
            ┌──────────────┐        gRPC (GetService)        ┌──────────────┐
  client ──▶│   booking    │────────────────────────────────▶│   catalog    │
            │  REST :3001  │                                  │  REST :3002  │
            └──────┬───────┘                                  │  gRPC :50051 │
                   │ booking + outbox (1 tx)                   └──────────────┘
                   ▼
            ┌──────────────┐   publish w/ confirms   ┌──────────────┐
            │  Postgres    │◀──── outbox relay ──────▶│  RabbitMQ    │
            │  (booking)   │                          │ quorum + DLQ │
            └──────────────┘                          └──────┬───────┘
                                                             │ booking.*
                                                             ▼
                                                      ┌──────────────┐
                                                      │ notification │
                                                      │  dedupe +    │
                                                      │  audit table │
                                                      └──────────────┘
```

## Table of contents

- [Why this project exists](#why-this-project-exists)
- [Quickstart](#quickstart)
- [API](#api)
- [Service boundaries](#service-boundaries)
- [Reliability model](#reliability-model)
- [Observability](#observability)
- [Configuration](#configuration)
- [Testing](#testing)
- [Project layout](#project-layout)
- [Documentation](#documentation)
- [Known limitations](#known-limitations)

## Why this project exists

Most "microservices demo" repositories show happy-path CRUD and call it a day.
The interesting parts of distributed systems live in the failure paths:

| Failure | This project's answer |
| --- | --- |
| Process dies between database commit and broker publish | Transactional outbox; the relay republishes |
| Relay dies after publish, before marking the row | At-least-once delivery; consumers deduplicate by event ID |
| Client retries `POST /v1/bookings` after a timeout | `Idempotency-Key` replays the original response |
| Two writers update the same aggregate | `version` column; stale writes get `409` |
| Catalog is down while booking is created | gRPC returns `503`, no half-created booking |
| Catalog price changes after a booking | Booking stores a name/price/duration snapshot |
| Poison message keeps failing | Quorum queue delivery limit + dead-letter queue |
| Outbox row fails repeatedly | Exponential backoff with jitter, then dead-letter |
| Debugging a slow request across services | OTel trace propagated over HTTP, gRPC and AMQP, linked into async events |
| A deploy breaks liveness | Probes and metrics live on a separate admin port |

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the detailed design and
[docs/DECISIONS.md](docs/DECISIONS.md) for the trade-offs and rejected
alternatives.

## Quickstart

Requirements: Docker (with Compose), or Go 1.27+ and Docker for infrastructure.

### Full stack

```bash
make up          # build images; starts Postgres, RabbitMQ, 3 services, Jaeger, Prometheus
make e2e         # run the end-to-end script against the running stack
make down
```

Endpoints:

| Service | API | Admin (health/metrics) |
| --- | --- | --- |
| catalog | http://localhost:3002 | http://localhost:9092 |
| booking | http://localhost:3001 | http://localhost:9091 |
| notification | — (consumer) | http://localhost:9093 |
| Jaeger UI | http://localhost:16686 | |
| Prometheus | http://localhost:9090 | |
| RabbitMQ management | http://localhost:15672 (`chore` / `chore`) | |

### Local Go processes

```bash
make up-infra    # Postgres on :5435, RabbitMQ on :5672

make run-catalog      # terminal 1
make run-booking      # terminal 2
make run-notification # terminal 3
```

Migrations are embedded in each binary and applied at startup
(`DB_AUTO_MIGRATE=true` by default), so there is no separate migration step in
development.

### First requests

```bash
# 1. Create a catalog service
curl -sS -X POST http://localhost:3002/v1/services \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Deep clean",
    "description": "Top-to-bottom apartment clean",
    "category": "cleaning",
    "price_cents": 12900,
    "duration_minutes": 180
  }'

# 2. Book it (retry-safe)
curl -sS -X POST http://localhost:3001/v1/bookings \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: 6f0d7f5e-4f0e-4f0e-9f0e-1f0e2f0e3f0e' \
  -d '{
    "service_id": 1,
    "customer_name": "Ada Lovelace",
    "customer_email": "ada@example.com",
    "scheduled_at": "2026-11-02T10:00:00Z"
  }'

# 3. Confirm, complete or cancel
curl -sS -X POST http://localhost:3001/v1/bookings/1/confirm
curl -sS -X POST http://localhost:3001/v1/bookings/1/cancel \
  -H 'Content-Type: application/json' -d '{"reason":"plans changed"}'
```

The complete contract, including every error code, is in
[`api/openapi.yaml`](api/openapi.yaml).

## API

Base paths:

- Catalog: `/v1/services` (REST), `catalog.v1.CatalogService` (gRPC)
- Booking: `/v1/bookings`

Every error uses one envelope:

```json
{
  "error": {
    "code": "invalid_status_transition",
    "message": "booking cannot move from cancelled to cancelled",
    "details": { "from": "cancelled", "to": "cancelled" },
    "request_id": "5d4c1f7e-...",
    "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736"
  }
}
```

`code` is stable and machine-readable. `details` is field-level context.
Unexpected errors become `internal_error`; their cause is logged, never
returned.

Booking status machine:

```
pending ──▶ confirmed ──▶ completed
   │            │
   └────────────┴──────▶ cancelled
```

Only the transitions shown are legal. Anything else is `409
invalid_status_transition`.

`POST /v1/bookings` with an `Idempotency-Key`:

| Situation | Result |
| --- | --- |
| First request | Executes; stores status and body for 24h |
| Replay, same body | Original status/body + `Idempotent-Replayed: true` |
| Replay, different body | `409 idempotency_key_reuse` |
| Concurrent duplicate | `409 request_in_progress` |
| Original returned `5xx` | Key released; a retry executes again |

## Service boundaries

| Service | Owns | Does not |
| --- | --- | --- |
| **catalog** | `services` table, service CRUD, gRPC read API | Know about bookings |
| **booking** | `bookings`, `outbox_messages`, `idempotency_keys` | Read catalog tables directly; send emails |
| **notification** | `processed_events`, `notifications` audit trail | Mutate bookings; call other services |

One Postgres server hosts three databases (`chore_catalog`, `chore_booking`,
`chore_notification`). That keeps local setup cheap while still enforcing
ownership: no service can join across another service's tables.

The booking service snapshots `service_name`, `service_price_cents` and
`duration_minutes` when a booking is created. A later catalog price change does
not rewrite history.

## Reliability model

**Writes are atomic with their events.** `POST /v1/bookings` inserts the booking
and the `booking.created` outbox row in one transaction. Either both exist or
neither does.

**The relay publishes with confirms.** A message is only marked published after
RabbitMQ acknowledges it. Rows are claimed with `FOR UPDATE SKIP LOCKED` and a
lease, so multiple relays can run safely and a crashed relay does not strand
work.

**Delivery is at-least-once.** If the process dies after publish and before
`MarkPublished`, the event is published twice. The notification consumer
records `processed_events.event_id` in the same transaction as the notification
row, so the duplicate is skipped. Consumers are expected to be idempotent; the
broker is not expected to be exactly-once.

**Failures are bounded.** Outbox publishes retry with exponential backoff and
jitter, then move to `failed_at` after `OUTBOX_MAX_ATTEMPTS`. Consumer failures
nack with requeue until `x-delivery-limit`, then route to a dead-letter queue.
Permanent errors (invalid payload, unsupported event) are dead-lettered
immediately.

**Concurrency is explicit.** Catalog and booking updates use a `version`
column. A stale `UPDATE ... WHERE version = $n` affects zero rows and becomes
`409`, rather than silently losing a concurrent change.

## Observability

- **Structured logs** (`slog`) with `service`, `env`, `version`, `request_id`,
  `trace_id` and `span_id`. One access-log line per request.
- **Prometheus** on the admin port: `chore_http_requests_total`,
  `chore_http_request_duration_seconds`, `chore_grpc_requests_total`,
  `chore_events_published_total`, `chore_events_consumed_total`,
  `chore_event_handle_duration_seconds`,
  `chore_outbox_publish_duration_seconds`, plus `chore_outbox_messages_pending`
  and `chore_outbox_oldest_pending_seconds` gauges.
- **OpenTelemetry** traces across HTTP → gRPC → Postgres → outbox → AMQP →
  consumer. The outbox stores the `traceparent` at write time, so the relay's
  publish span is *linked* to the original request instead of pretending to be
  its child. Jaeger is included in Compose.
- **Health**: `/healthz` (liveness), `/readyz` (Postgres + RabbitMQ), `/version`
  and `/metrics`, all on the admin port so they are not exposed with the public
  API.
- **Graceful shutdown**: SIGINT/SIGTERM stop the HTTP server, consumer and relay
  within `SHUTDOWN_TIMEOUT`; in-flight requests are drained.

## Configuration

Everything is environment-driven; the same binary runs locally, in CI and in
production. Defaults match the Compose stack.

| Variable | Default | Notes |
| --- | --- | --- |
| `ENV` | `dev` | `dev`, `test`, `production` |
| `VERSION` | `dev` | Exposed on `/version` and `chore_build_info` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `HTTP_ADDR` | `:8080` | Public API |
| `ADMIN_ADDR` | `:9090` | Probes and metrics |
| `GRPC_ADDR` | `:50051` | Catalog only |
| `DATABASE_URL` | `postgres://chore:chore@localhost:5435/chore_<service>` | Redacted in logs |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `10` / `1` | pgx pool |
| `DB_AUTO_MIGRATE` | `true` | Embedded golang-migrate |
| `RABBITMQ_URL` | `amqp://chore:chore@localhost:5672/` | Reconnecting client |
| `RABBITMQ_EXCHANGE` | `chore.events` | Topic exchange |
| `RABBITMQ_PREFETCH` | `20` | Consumer QoS |
| `CATALOG_GRPC_ADDR` | `localhost:50051` | Booking only |
| `CATALOG_GRPC_TIMEOUT` | `2s` | Per-call deadline |
| `OUTBOX_POLL_INTERVAL` | `500ms` | Relay poll |
| `OUTBOX_BATCH_SIZE` | `50` | Rows per claim |
| `OUTBOX_MAX_ATTEMPTS` | `10` | Then dead-letter |
| `OUTBOX_BASE_BACKOFF` / `OUTBOX_MAX_BACKOFF` | `2s` / `5m` | Jittered |
| `HTTP_RATE_LIMIT_PER_MINUTE` | `0` (off) | Per-IP on writes |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty (off) | e.g. `jaeger:4317` |
| `OTEL_SAMPLE_RATIO` | `1` | 0–1 |

See [`.env.example`](.env.example) for a ready-to-source local file.

## Testing

```bash
make test              # unit tests with -race
make test-integration  # Postgres-backed repository tests (needs `make up-infra`)
make cover             # coverage profile + total
make e2e               # black-box HTTP flow against a running stack
make lint              # golangci-lint
```

The e2e script exercises the real paths, not mocks: catalog write, gRPC lookup,
booking + outbox transaction, idempotent replay, confirmation/cancellation,
invalid transition, unknown service, and async notification delivery.

Unit tests cover the domain invariants, idempotency middleware contract,
outbox retry/backoff behavior and the shared error translator. Integration
tests assert that booking and outbox rows are written atomically and that
optimistic locking rejects stale writers.

## Project layout

```
cmd/{catalog,booking,notification}   composition roots only
internal/<service>/domain            entities, invariants, status machine
internal/<service>/app               use cases, ports (interfaces)
internal/<service>/postgres          adapters + embedded migrations
internal/<service>/transport         HTTP / gRPC / AMQP adapters
internal/<service>/infra             outbound adapters (gRPC client, publisher)
internal/platform/...                shared infrastructure
internal/gen/...                     generated protobuf (do not edit)
api/openapi.yaml                     public HTTP contract
deploy/                              Dockerfile, Compose, Prometheus, DB init
docs/                                architecture, decisions, runbook
proto/                               gRPC contracts
scripts/e2e.sh                       black-box smoke test
```

Dependencies point inwards: transport → app → domain. The domain imports no
framework, database driver or broker library. `platform` packages are
infrastructure shared by services, not a business-logic dumping ground.

## Documentation

- [Architecture](docs/ARCHITECTURE.md) — components, sequences, data model
- [Decisions](docs/DECISIONS.md) — why outbox, why at-least-once, why keyset
  pagination, and what was deliberately left out
- [Runbook](docs/RUNBOOK.md) — alerts, diagnosis, DLQ replay, recovery

## Known limitations

This is a complete engineering sample, not a finished SaaS. Intentionally out
of scope:

- Authentication/authorization. The API is open; in production, terminate auth
  at the gateway or add JWT middleware.
- Payments, provider assignment and scheduling conflict detection.
- Multi-region deployment, sharding and read replicas.
- A real email/SMS provider — `LogSender` writes notifications to the log and
  the audit table.
- Service mesh, Kubernetes manifests and autoscaling. The binary is 12-factor
  and distroless, so it drops into any orchestrator.

These are boundaries, not accidents. See
[docs/DECISIONS.md](docs/DECISIONS.md) for the reasoning.
