-- Baseline schema for the Postgres reservation path.
--
-- Two constraints carry the correctness argument independently of application
-- code: CHECK (stock >= 0) refuses to oversell, and UNIQUE (sku, request_id)
-- refuses to apply a replay twice.
--
-- Idempotency is scoped per SKU rather than globally because the Redis path keeps
-- its keys under one {sku} hash tag to stay legal on a Redis Cluster; v1 matches
-- v2 so the comparison stays honest.

CREATE TABLE IF NOT EXISTS products (
    sku            TEXT PRIMARY KEY,
    stock          BIGINT      NOT NULL CHECK (stock >= 0),
    per_user_limit BIGINT      NOT NULL DEFAULT 1 CHECK (per_user_limit >= 1),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orders (
    id         BIGSERIAL   PRIMARY KEY,
    request_id TEXT        NOT NULL,
    sku        TEXT        NOT NULL REFERENCES products (sku) ON DELETE CASCADE,
    user_id    TEXT        NOT NULL,
    qty        BIGINT      NOT NULL CHECK (qty > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (sku, request_id)
);

-- Supports the per-user limit lookup on the reservation path.
CREATE INDEX IF NOT EXISTS orders_sku_user_idx ON orders (sku, user_id);
