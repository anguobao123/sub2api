-- OpenClaw USD Billing Bridge.
--
-- platform_user_hmac is a server-keyed HMAC of an external platform user ID.
-- Raw external identities never enter these tables. The bridge deliberately
-- reuses users.balance / users.frozen_balance as the only monetary source of
-- truth while keeping its mapping, leases, grants, and events isolated from
-- Sub2API gateway billing tables.

CREATE TABLE IF NOT EXISTS openclaw_billing_accounts (
    id                  BIGSERIAL PRIMARY KEY,
    billing_account_id  VARCHAR(64) NOT NULL UNIQUE,
    platform_user_hmac  VARCHAR(64) NOT NULL UNIQUE,
    billing_user_id     BIGINT NOT NULL UNIQUE REFERENCES users(id) ON DELETE RESTRICT,
    initial_grant_usd   DECIMAL(20,8) NOT NULL DEFAULT 0,
    initial_grant_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS openclaw_billing_grants (
    id                  BIGSERIAL PRIMARY KEY,
    billing_account_id  VARCHAR(64) NOT NULL REFERENCES openclaw_billing_accounts(billing_account_id) ON DELETE RESTRICT,
    billing_user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    grant_key           VARCHAR(128) NOT NULL,
    amount_usd          DECIMAL(20,8) NOT NULL CHECK (amount_usd >= 0),
    grant_type          VARCHAR(64) NOT NULL DEFAULT 'initial_mapping',
    applied_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (billing_account_id, grant_key)
);

CREATE TABLE IF NOT EXISTS openclaw_task_budget_leases (
    lease_id                    VARCHAR(64) PRIMARY KEY,
    billing_account_id          VARCHAR(64) NOT NULL REFERENCES openclaw_billing_accounts(billing_account_id) ON DELETE RESTRICT,
    billing_user_id             BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    task_id                     VARCHAR(255) NOT NULL,
    session_id                  VARCHAR(255) NOT NULL,
    node_id                     VARCHAR(255) NOT NULL,
    reserved_usd                DECIMAL(20,8) NOT NULL CHECK (reserved_usd > 0),
    captured_usd                DECIMAL(20,8) NOT NULL DEFAULT 0 CHECK (captured_usd >= 0),
    released_usd                DECIMAL(20,8) NOT NULL DEFAULT 0 CHECK (released_usd >= 0),
    status                      VARCHAR(32) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'captured', 'released', 'expired')),
    reserve_idempotency_key     VARCHAR(255) NOT NULL,
    reserve_request_fingerprint VARCHAR(64) NOT NULL,
    expires_at                  TIMESTAMPTZ NOT NULL,
    finalized_at                TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (captured_usd + released_usd <= reserved_usd),
    UNIQUE (billing_account_id, reserve_idempotency_key)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_openclaw_task_budget_leases_active_task
    ON openclaw_task_budget_leases (billing_account_id, task_id, session_id, node_id)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_openclaw_task_budget_leases_expiry
    ON openclaw_task_budget_leases (expires_at)
    WHERE status = 'active';

CREATE TABLE IF NOT EXISTS openclaw_task_budget_lease_operations (
    id                  BIGSERIAL PRIMARY KEY,
    billing_account_id  VARCHAR(64) NOT NULL REFERENCES openclaw_billing_accounts(billing_account_id) ON DELETE RESTRICT,
    lease_id            VARCHAR(64) NOT NULL REFERENCES openclaw_task_budget_leases(lease_id) ON DELETE RESTRICT,
    operation           VARCHAR(32) NOT NULL CHECK (operation IN ('capture', 'release', 'expiry')),
    idempotency_key     VARCHAR(255) NOT NULL,
    request_fingerprint VARCHAR(64) NOT NULL,
    amount_usd          DECIMAL(20,8) NOT NULL DEFAULT 0 CHECK (amount_usd >= 0),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (billing_account_id, operation, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_openclaw_task_budget_lease_operations_lease
    ON openclaw_task_budget_lease_operations (lease_id, created_at);

CREATE TABLE IF NOT EXISTS openclaw_billing_usage_events (
    event_id            VARCHAR(255) PRIMARY KEY,
    billing_account_id  VARCHAR(64) NOT NULL REFERENCES openclaw_billing_accounts(billing_account_id) ON DELETE RESTRICT,
    billing_user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    task_id             VARCHAR(255) NOT NULL,
    session_id          VARCHAR(255) NOT NULL,
    node_id             VARCHAR(255) NOT NULL,
    codex_thread_id     VARCHAR(255),
    model               VARCHAR(255) NOT NULL,
    input_tokens        BIGINT CHECK (input_tokens >= 0),
    output_tokens       BIGINT CHECK (output_tokens >= 0),
    cache_read_tokens   BIGINT CHECK (cache_read_tokens >= 0),
    cache_write_tokens  BIGINT CHECK (cache_write_tokens >= 0),
    observed_at         TIMESTAMPTZ NOT NULL,
    reported_at         TIMESTAMPTZ NOT NULL,
    pricing_version     VARCHAR(255),
    price_snapshot      JSONB,
    estimated_usd       DECIMAL(20,8) CHECK (estimated_usd >= 0),
    status              VARCHAR(32) NOT NULL CHECK (status IN ('pending_pricing', 'recorded')),
    ingest_fingerprint  VARCHAR(64) NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (price_snapshot IS NULL OR jsonb_typeof(price_snapshot) = 'object'),
    CHECK (
        (status = 'pending_pricing' AND (pricing_version IS NULL OR price_snapshot IS NULL))
        OR
        (status = 'recorded' AND pricing_version IS NOT NULL AND price_snapshot IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_openclaw_billing_usage_events_task
    ON openclaw_billing_usage_events (billing_account_id, task_id, session_id, node_id, created_at);

CREATE INDEX IF NOT EXISTS idx_openclaw_billing_usage_events_pending
    ON openclaw_billing_usage_events (status, reported_at)
    WHERE status = 'pending_pricing';

-- Usage events are accounting evidence. Lease settlement is recorded in the
-- separate lease-operation table; events themselves are immutable so neither
-- generic cleanup nor a later pricing retry can rewrite the original report.
CREATE OR REPLACE FUNCTION prevent_openclaw_billing_usage_event_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'openclaw billing usage events are append-only and cannot be modified, deleted, or truncated';
END;
$$;

DROP TRIGGER IF EXISTS trg_prevent_openclaw_billing_usage_event_mutation ON openclaw_billing_usage_events;
CREATE TRIGGER trg_prevent_openclaw_billing_usage_event_mutation
    BEFORE UPDATE OR DELETE ON openclaw_billing_usage_events
    FOR EACH ROW
    EXECUTE FUNCTION prevent_openclaw_billing_usage_event_mutation();

-- TRUNCATE does not execute row-level UPDATE/DELETE triggers. Guard it
-- explicitly so this accounting evidence cannot be removed through a bulk
-- maintenance path either.
DROP TRIGGER IF EXISTS trg_prevent_openclaw_billing_usage_event_truncate ON openclaw_billing_usage_events;
CREATE TRIGGER trg_prevent_openclaw_billing_usage_event_truncate
    BEFORE TRUNCATE ON openclaw_billing_usage_events
    FOR EACH STATEMENT
    EXECUTE FUNCTION prevent_openclaw_billing_usage_event_mutation();
