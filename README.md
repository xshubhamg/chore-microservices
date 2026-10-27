<div align="center">

# Chore Booking Microservices

**A production-grade booking platform built with Go — designed around the failure paths, not just the happy path.**

[![CI](https://github.com/xshubhamg/chore-microservices/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/xshubhamg/chore-microservices/actions/workflows/ci.yml)
[![Go 1.27](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![PostgreSQL 17](https://img.shields.io/badge/PostgreSQL-17-4169E1?logo=postgresql&logoColor=white)](https://www.postgresql.org)
[![RabbitMQ 4](https://img.shields.io/badge/RabbitMQ-4-FF6600?logo=rabbitmq&logoColor=white)](https://www.rabbitmq.com)
[![OpenTelemetry](https://img.shields.io/badge/OpenTelemetry-traces-425CC7?logo=opentelemetry&logoColor=white)](https://opentelemetry.io)
[![Docker](https://img.shields.io/badge/Docker-distroless-2496ED?logo=docker&logoColor=white)](deploy/Dockerfile)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

[Quick start](#quick-start) · [Architecture](#architecture) · [API](#api) · [Design decisions](docs/DECISIONS.md) · [Runbook](docs/RUNBOOK.md)

</div>

---

Most microservice demos stop at CRUD and a happy-path request. This one is
built around the questions that decide whether a distributed system survives
production:

> What happens if the process dies after `COMMIT` but before the event is
> published? What if the client retries a booking? What if two writers update
> the same row? What if the broker is down? What if a message is delivered
> twice — or never stops failing?

The answer is a small, complete system with **three services, one owner per
database, a transactional outbox, idempotent writes, optimistic concurrency,
at-least-once messaging with deduplication, and end-to-end tracing** — including
the operational documentation to run it.

## At a glance

| | |
| --- | --- |
| **Services** | `catalog` · `booking` · `notification` |
| **Transports** | REST (Fiber), gRPC, AMQP |
| **Data** | PostgreSQL 17 — database per service |
| **Messaging** | RabbitMQ 4 — quorum queues, publisher confirms, DLX |
| **Observability** | `slog`, Prometheus, OpenTelemetry, Jaeger |
| **Delivery** | At-least-once + idempotent consumers |
| **Deployment** | Distroless, non-root Docker images · Docker Compose |

## Architecture

<p align="center">
  <img src="docs/images/architecture.png" alt="Chore Booking Microservices architecture: client to booking service over REST with an Idempotency-Key; booking calls catalog over gRPC; booking and its outbox event commit atomically to Postgres; the outbox relay publishes with confirms to RabbitMQ; the notification consumer deduplicates events and writes to its own Postgres; Prometheus, Jaeger and OpenTelemetry observe every hop." width="100%">
</p>

<p align="center"><em>Idempotent request → gRPC lookup → atomic write + outbox → publish with confirms → deduplicated consume → notification + audit.</em></p>

<details>
<summary><b>Text-friendly Mermaid diagram</b></summary>

```mermaid
flowchart LR
    client([Client])

    subgraph catalog["catalog"]
        c_api["REST :3002"]
        c_grpc["gRPC :50051"]
        c_db[(chore_catalog)]
        c_api --> c_db
        c_grpc --> c_db
    end

    subgraph booking["booking"]
        b_api["REST :3001"]
        b_relay["outbox relay"]
        b_db[(chore_booking)]
        b_api --> b_db
        b_relay --> b_db
    end

    broker[(RabbitMQ<br/>chore.events)]
    subgraph notification["notification"]
        n_consumer["AMQP consumer"]
        n_db[(chore_notification)]
        n_consumer --> n_db
    end

    client -->|HTTP| c_api
    client -->|HTTP| b_api
    b_api -->|gRPC GetService| c_grpc
    b_relay -->|publish + confirm| broker
    broker -->|booking.*| n_consumer
```

</details>

The design follows four rules:

1. **One owner per table.** No service reads another service's schema.
2. **Synchronous only when the caller needs the answer.** Booking needs the
   catalog price before it writes; notifications are side effects.
3. **Business code never imports a transport.** `domain` and `app` know nothing
   about Fiber, gRPC or AMQP.
4. **The database is the source of truth.** Events are derived from committed
   state — never from a best-effort publish call.

The detailed component map, request sequences and data model live in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Failure modes, handled

| Failure | Response |
| --- | --- |
| Process dies between database commit and broker publish | **Transactional outbox** — the relay republishes |
| Relay dies after publish, before marking the row | **At-least-once delivery** — consumers deduplicate by event ID |
| Client retries `POST /v1/bookings` after a timeout | **`Idempotency-Key`** replays the original response |
| Two writers update the same aggregate | **`version` column** — stale writes get `409` |
| Catalog is down while a booking is created | gRPC deadline → **`503`**, no half-created booking |
| Catalog price changes after a booking | Booking stores a **name/price/duration snapshot** |
| Poison message keeps failing | **Quorum queue delivery limit** → dead-letter queue |
| Outbox row fails repeatedly | **Exponential backoff with jitter** → dead-letter |
| Debugging a slow request across services | **OTel trace** over HTTP, gRPC and AMQP, linked into async events |
| A deploy breaks liveness | Probes and metrics live on a **separate admin port** |

## Quick start

**Requirements:** Docker with Compose. For local Go processes: Go 1.27+.

```bash
make up     # build images; start Postgres, RabbitMQ, 3 services, Jaeger, Prometheus
make e2e    # run the full black-box flow against the running stack
make down   # stop everything
```

The first `make up` builds three distroless images and may take a few minutes.

### Endpoints

| Service | API | Admin (health / metrics) |
| --- | --- | --- |
| **catalog** | http://localhost:3002 | http://localhost:9092 |
| **booking** | http://localhost:3001 | http://localhost:9091 |
| **notification** | — (consumer) | http://localhost:9093 |
| **Jaeger UI** | http://localhost:16686 | |
| **Prometheus** | http://localhost:9090 | |
| **RabbitMQ management** | http://localhost:15672 | `chore` / `chore` |

`GET /healthz`, `/readyz`, `/metrics` and `/version` are served by the admin
listener, never by the public API.

### Local Go processes

```bash
make up-infra          # Postgres on :5435, RabbitMQ on :5672

make run-catalog       # terminal 1
make run-booking       # terminal 2
make run-notification  # terminal 3
```

Migrations are embedded in each binary and applied at startup
(`DB_AUTO_MIGRATE=true` by default), so there is no separate migration step in
development. Run `make help` to see every target.

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

The complete contract — every route, field, status code and error code — is in
[`api/openapi.yaml`](api/openapi.yaml).

## API

| Surface | Path | Protocol |
| --- | --- | --- |
| Catalog CRUD | `/v1/services` | REST / JSON |
| Catalog lookup | `catalog.v1.CatalogService/GetService` | gRPC |
| Booking lifecycle | `/v1/bookings` | REST / JSON |

Every error uses the same envelope, so clients branch on a stable code instead
of parsing prose:

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

`request_id` is also returned in `X-Request-ID` and attached to every log line.
Unexpected errors become `internal_error`; their cause is logged, never returned.

### Booking lifecycle

```text
pending ──▶ confirmed ──▶ completed
   │            │
   └────────────┴──────▶ cancelled
```

Only the transitions shown are legal. Anything else is
`409 invalid_status_transition`.

### Idempotent writes

`POST /v1/bookings` accepts an optional `Idempotency-Key`:

| Situation | Result |
| --- | --- |
| First request | Executes; stores status and body for 24 h |
| Replay, same body | Original status/body + `Idempotent-Replayed: true` |
| Replay, different body | `409 idempotency_key_reuse` |
| Concurrent duplicate | `409 request_in_progress` |
| Original returned `5xx` | Key released; a retry executes again |

The key is scoped to method + path + body hash, stored in Postgres, and purged by
a background janitor.

## Reliability model

**Writes are atomic with their events.** Creating a booking inserts the row and
the `booking.created` outbox row in one transaction. Either both exist or
neither does.

**The relay publishes with confirms.** A row is marked published only after
RabbitMQ acknowledges it. Rows are claimed with `FOR UPDATE SKIP LOCKED` and a
lease, so relays can scale horizontally and a crashed relay does not strand work.

**Delivery is at-least-once.** If the process dies after publish and before
`MarkPublished`, the event is published twice. The notification consumer writes
`processed_events.event_id` in the same transaction as the notification row, so
the duplicate is skipped. Consumers are expected to be idempotent; the broker is
not expected to be exactly-once.

**Failures are bounded.** Outbox publishes retry with exponential backoff and
jitter, then move to `failed_at` after `OUTBOX_MAX_ATTEMPTS`. Consumer failures
re-queue until `x-delivery-limit`, then route to a dead-letter queue. Permanent
errors are dead-lettered immediately.

**Concurrency is explicit.** Catalog and booking updates use a `version`
column. A stale `UPDATE ... WHERE version = $n` affects zero rows and becomes
`409`, rather than silently losing a concurrent change.

## Observability

- **Structured logs** with `service`, `env`, `version`, `request_id`, `trace_id`
  and `span_id`. One access-log line per request.
- **Prometheus** RED metrics for HTTP and gRPC, event publish/consume counters
  by outcome, handler latency, and outbox backlog count and age.
- **OpenTelemetry** traces across HTTP → gRPC → Postgres → outbox → AMQP →
  consumer. The outbox stores `traceparent` at write time, so the relay's
  publish span is *linked* to the original request instead of pretending to be
  its child. Jaeger is included in Compose.
- **Health**: `/healthz` (liveness), `/readyz` (Postgres + RabbitMQ), `/version`
  and `/metrics`, all on the admin port.
- **Graceful shutdown**: SIGINT/SIGTERM drain HTTP requests, the consumer and
  the relay within `SHUTDOWN_TIMEOUT`.

## Testing

| Command | What it proves |
| --- | --- |
| `make test` | Unit tests with the race detector |
| `make test-integration` | Real Postgres: atomic writes, optimistic locking, outbox lifecycle |
| `make cover` | Coverage profile and total |
| `make e2e` | Full black-box flow against a running stack |
| `make lint` | `golangci-lint` |

The e2e script exercises the real paths, not mocks: catalog write, gRPC lookup,
booking + outbox transaction, idempotent replay, confirmation/cancellation,
invalid transition, unknown service, and asynchronous notification delivery.

## Project layout

```text
cmd/{catalog,booking,notification}   composition roots only
internal/<service>/domain            entities, invariants, status machine
internal/<service>/app               use cases and ports (interfaces)
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

Dependencies point inwards: **transport → app → domain**. The domain imports no
framework, database driver or broker library. `platform` packages are shared
infrastructure, not a business-logic dumping ground.

## Configuration

Everything is environment-driven; the same binary runs locally, in CI and in
production. Defaults match the Compose stack.

<details>
<summary><b>Full configuration reference</b></summary>

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

</details>

See [`.env.example`](.env.example) for a ready-to-source local file.

## Documentation

| Document | Contents |
| --- | --- |
| [Architecture](docs/ARCHITECTURE.md) | Components, sequences, data model, transaction boundaries |
| [Design decisions](docs/DECISIONS.md) | 15 ADRs: outbox, at-least-once, keyset pagination, rejected alternatives |
| [Runbook](docs/RUNBOOK.md) | Alerts, outbox diagnosis, DLQ replay, recovery |
| [OpenAPI](api/openapi.yaml) | The complete public HTTP contract |

## Known limitations

This is a complete engineering sample, not a finished SaaS. Intentionally out of
scope:

- **Authentication / authorization** — the API is open; terminate auth at the
  gateway or add JWT middleware before exposing it.
- **Payments, provider assignment, scheduling conflict detection.**
- **Multi-region deployment, sharding and read replicas.**
- **A real email/SMS provider** — `LogSender` writes to the log and the audit
  table.
- **Service mesh, Kubernetes manifests and autoscaling** — the binary is
  12-factor and distroless, so it drops into any orchestrator.

These are boundaries, not accidents. The reasoning is recorded in
[docs/DECISIONS.md](docs/DECISIONS.md).

## License

Released under the [MIT License](LICENSE).

<div align="center">

**[Back to top](#chore-booking-microservices)**

</div>
