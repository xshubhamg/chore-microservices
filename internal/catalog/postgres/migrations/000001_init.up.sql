-- Catalog service schema. The catalog owns the services offered by the
-- marketplace; other services never read these tables directly.

CREATE TABLE services (
    id               BIGSERIAL PRIMARY KEY,
    name             TEXT        NOT NULL,
    description      TEXT        NOT NULL DEFAULT '',
    category         TEXT        NOT NULL,
    price_cents      BIGINT      NOT NULL CHECK (price_cents >= 0),
    duration_minutes INTEGER     NOT NULL CHECK (duration_minutes > 0),
    active           BOOLEAN     NOT NULL DEFAULT TRUE,
    version          INTEGER     NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Names must be unique among active services, but a deactivated service frees
-- its name for reuse. The database enforces it, not application code.
CREATE UNIQUE INDEX services_active_name_uq ON services (lower(name)) WHERE active;

CREATE INDEX services_active_id_idx ON services (id) WHERE active;
