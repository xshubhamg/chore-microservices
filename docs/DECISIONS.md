# Design decisions

Each decision records the context, the choice, the consequences, and what was
rejected. The goal is to make the trade-offs reviewable, not to claim there is
only one correct answer.

---

## ADR-001: Three services, one repository

**Context.** The domain has three clear responsibilities: service catalog,
booking lifecycle and customer notification. Each has a different consistency
profile (read-heavy CRUD, transactional writes, async side effects).

**Decision.** Keep them as separate deployable binaries in one Go module, with
one database per service on a single Postgres server for local/CI use.

**Consequences.**

- Each service can scale, fail and deploy independently.
- `internal/<service>` is a hard ownership boundary; imports across service
  packages are not allowed.
- A single module keeps refactors atomic and avoids `go.work`/multi-repo
  overhead for a sample of this size. A larger organisation would split repos
  by team ownership, not by technology.
- One Postgres server is a development convenience. Production would use
  separate instances or at least separate credentials per service; the code
  already assumes only its own DSN.

**Rejected.** A single monolith (not the point of the exercise), and three
repositories (needless coordination cost at this scale).

---

## ADR-002: Transactional outbox instead of dual writes

**Context.** After committing a booking, the service must publish
`booking.created`. Doing `commit; publish` can lose the event if the process
dies in between. Doing `publish; commit` can publish an event for a booking
that was rolled back.

**Decision.** Write the event to `outbox_messages` in the same transaction as
the booking. A separate relay polls the table and publishes with publisher
confirms.

**Consequences.**

- No lost or phantom events without a distributed transaction.
- Publishing is asynchronous: consumers see the event shortly after commit,
  not during the request.
- The outbox table needs retention (published rows are purged after 7 days) and
  monitoring (backlog age, not just count).
- The relay is another moving part, but it is stateless and horizontally
  scalable thanks to `FOR UPDATE SKIP LOCKED`.

**Rejected.** Two-phase commit (operationally heavy, poor broker support),
Change Data Capture (great at scale, but adds Debezium/Kafka Connect for a
problem this size), and “just retry the publish in a goroutine” (still loses
events on crash).

---

## ADR-003: At-least-once delivery with idempotent consumers

**Context.** Exactly-once delivery across a database and a broker is not
achievable without cooperation from both sides and a transaction protocol.

**Decision.** Guarantee at-least-once, and make every consumer idempotent. The
notification consumer stores `processed_events.event_id` in the same
transaction as the notification row.

**Consequences.**

- Duplicate deliveries are expected and cheap to absorb.
- Consumers must own a deduplication record. This is the real contract of the
  event bus; it is documented rather than hidden.
- Message handlers must be safe to run twice, including side effects such as
  sending email. For real providers, the provider should accept an idempotency
  key, or the send should be recorded before dispatch with an outbox of its
  own.

**Rejected.** “Exactly once” claims based on broker features alone, and
in-memory deduplication (loses state on restart).

---

## ADR-004: gRPC for the booking → catalog call

**Context.** Booking needs the service name, price and duration before it can
persist a booking, and it must know whether the service exists and is active.

**Decision.** Expose a small gRPC contract (`catalog.v1.CatalogService`) with
`GetService`. Use a typed client, a 2s deadline, and the shared retry service
config.

**Consequences.**

- Strong typing and a compact contract; generated code is checked into
  `internal/gen`.
- The call is synchronous, so catalog availability is part of the booking
  write path. That is an explicit product decision: no booking without a
  valid service.
- The client has a deadline and maps `Unavailable`/`DeadlineExceeded` to a
  `503` for the caller. It does not retry non-idempotent operations blindly.
- The booking stores a snapshot; a later catalog change cannot rewrite history.

**Rejected.** REST for the internal call (works, but loses schema guarantees
and adds JSON overhead), and sharing the catalog database (breaks ownership).

---

## ADR-005: REST + one error envelope for the public API

**Context.** External clients need a stable, debuggable contract.

**Decision.** REST/JSON with Fiber v3. Every error is
`{"error": {"code", "message", "details", "request_id", "trace_id"}}`.

**Consequences.**

- Clients branch on `code`, not on prose or HTTP status alone.
- `request_id` is echoed in the response header and logs, making support
  conversations tractable.
- Internal errors are always generic; causes stay in logs.
- Fiber gives fast routing and a small middleware surface. Handlers still
  depend on `fiber.Ctx` only in `transport`, never in `app` or `domain`.

**Rejected.** gRPC-only public API (poor browser/tooling ergonomics), and
per-endpoint ad-hoc error shapes (impossible to document or handle
consistently).

---

## ADR-006: Keyset pagination, not offsets

**Context.** List endpoints are paginated. Offset pagination drifts when rows
are inserted or deleted and degrades on large tables.

**Decision.** `?after=<last_id>&limit=<n>` with `next_cursor` in the response.

**Consequences.**

- Stable pages and index-friendly queries (`WHERE id > $1 ORDER BY id LIMIT $2`).
- Clients cannot jump to an arbitrary page. That is an acceptable trade-off
  for an append-mostly resource.
- Cursors are opaque to clients even though they are currently numeric IDs.

**Rejected.** `OFFSET`/`LIMIT` (slow and inconsistent under concurrent writes),
and cursor tokens signed with HMAC (overkill until the API is public and
cursors encode filters).

---

## ADR-007: Optimistic concurrency with a version column

**Context.** Two users can update the same service or transition the same
booking concurrently.

**Decision.** `UPDATE ... WHERE id = $1 AND version = $expected` with
`version = version + 1`. Zero rows affected means either `404` or `409`,
distinguished by an existence check.

**Consequences.**

- Lost updates are impossible; the loser gets a clear conflict and can reload.
- No row locks held across user think-time, unlike pessimistic locking.
- Clients must send the current version implicitly by acting on a freshly read
  resource. For booking transitions, the aggregate is loaded inside the use
  case, so the version is always current.

**Rejected.** `SELECT ... FOR UPDATE` held across a request (pool exhaustion,
poor UX), and last-write-wins (silently loses data).

---

## ADR-008: Idempotency-Key on booking creation

**Context.** Mobile clients retry on timeout. Without protection, a retry
creates a second booking and a second charge/notification.

**Decision.** Optional `Idempotency-Key` header. Persist key, request hash,
state and the exact response. Replay completed responses; reject reuse with a
different body; return `409` while the first request is in flight; do not
cache `5xx`.

**Consequences.**

- Safe retries with the same key; no duplicate rows.
- The stored response must be byte-stable; the middleware snapshots it.
- Keys expire after 24h and a janitor removes them. A retry after expiry can
  create a duplicate, which is the standard contract.
- Hashing includes method, path and raw body, so the same key on a different
  endpoint is a conflict.

**Rejected.** Natural-key deduplication only (does not cover retries with
different data), and client-generated request IDs without server-side storage
(no way to replay the response).

---

## ADR-009: Embedded migrations applied at startup

**Context.** A separate migration tool is operationally awkward for a sample
and easy to forget in local development.

**Decision.** Use `golang-migrate` with `embed.FS`, run `Up` at startup when
`DB_AUTO_MIGRATE=true`.

**Consequences.**

- The binary is the deployment unit; no init container or volume mount.
- Migrations run under an advisory lock, so concurrent replicas are safe.
- In a large production system, auto-migrate is often disabled and migrations
  run as a separate, audited job. The flag exists for that.
- Down migrations are kept for development and are not run automatically.

**Rejected.** A `schema.sql` applied manually (drifts immediately) and ORM
auto-migration (no review, no rollback story).

---

## ADR-010: Fiber v3, pgx v5, amqp091-go

**Context.** The prototype already used these libraries, and they are all
credible production choices.

**Decision.** Keep them. Use `pgxpool` directly rather than an ORM, and
`amqp091-go` rather than a framework.

**Consequences.**

- Explicit SQL with predictable performance and no N+1 surprises.
- `pgx` gives real Postgres features: `SKIP LOCKED`, `JSONB`, `TIMESTAMPTZ`,
  `RETURNING`, partial indexes.
- `amqp091-go` is low-level; the reconnecting client, publisher confirms and
  quorum/DLX topology are implemented in `platform/rabbitmq` and tested by the
  e2e flow.
- More adapter code than an ORM/broker framework. That code is small,
  readable and owned.

**Rejected.** GORM (hides query plans and concurrency), and a high-level
messaging framework (hides the delivery semantics this project is meant to
demonstrate).

---

## ADR-011: Separate admin port

**Context.** Probes and metrics should not be reachable through the public API
listener, and the public listener should not be exposed just to scrape
metrics.

**Decision.** Serve `/healthz`, `/readyz`, `/metrics` and `/version` on
`ADMIN_ADDR` (`:9090` in Compose), separate from `HTTP_ADDR`.

**Consequences.**

- In Kubernetes, the probe port and service port can have different network
  policies.
- Prometheus scrapes the admin port only.
- Slightly more wiring in `cmd`, offset by a clean security boundary.

**Rejected.** Serving everything on one port (simple, but couples public
exposure to operational endpoints).

---

## ADR-012: Trace links for asynchronous work

**Context.** An event is published seconds after the HTTP request that caused
it. Making the publish span a child of the request span is a lie about timing;
not linking them at all loses causality.

**Decision.** Persist the `traceparent` in the outbox row at write time. The
relay starts a new span and adds a *link* to the original trace context. The
AMQP publish and consumer spans continue the relay trace, and the consumer
span links back to the producer.

**Consequences.**

- Jaeger shows both traces, with a visible causal link and honest timings.
- The outbox row carries one extra nullable column.
- Trace context survives process restarts and long retry delays.

**Rejected.** Parent-child across an arbitrary delay (misleading latency), and
no propagation (impossible to follow an event end to end).

---

## ADR-013: Soft-delete catalog services

**Context.** Bookings reference services. Hard deletion would orphan history or
require cascading deletes across service boundaries.

**Decision.** `active = false` instead of `DELETE`. The unique name index is
partial (`WHERE active`), so a deactivated name can be reused.

**Consequences.**

- Historical bookings keep valid snapshots.
- The catalog can be audited.
- Listing defaults to all services; clients filter with `?active=true`.
- A “deleted” service can be reactivated, which is a feature, not an accident.

**Rejected.** Hard delete with `ON DELETE SET NULL` (loses the link), and
cascading deletes into booking (violates service ownership).

---

## ADR-014: Snapshot catalog data on the booking

**Context.** Prices and durations change. A booking must reflect what the
customer agreed to, not today’s catalog.

**Decision.** Copy `service_name`, `service_price_cents` and
`duration_minutes` into `bookings` at creation.

**Consequences.**

- Bookings are self-contained and historically correct.
- Denormalisation is intentional and bounded; there is no join to catalog.
- If a price is corrected immediately after booking, the booking keeps the
  original value. Refunds/adjustments are a separate domain concern.

**Rejected.** Joining catalog at read time (breaks ownership and rewrites
history), and storing only `service_id` (same problem plus a cross-service
call on every read).

---

## ADR-015: Authentication is out of scope

**Context.** The project focuses on distributed-systems correctness, not
identity.

**Decision.** Do not implement auth. Document the boundary and the intended
integration point.

**Consequences.**

- The API is open in the sample. Do not deploy it to the public internet as-is.
- In production, terminate TLS and auth at a gateway, or add JWT middleware
  before the handlers. The `apperr` model already has `unauthenticated` and
  `permission_denied`, so adding it does not change handler contracts.

**Rejected.** A half-implemented auth scheme that would give a false sense of
security.
