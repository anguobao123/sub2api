-- Follows upstream 0.2.4 migration 237_add_minimax_platform.sql.
ALTER TABLE openclaw_billing_accounts ADD COLUMN IF NOT EXISTS gateway_key_id BIGINT REFERENCES api_keys(id);
ALTER TABLE openclaw_billing_accounts ADD COLUMN IF NOT EXISTS gateway_group_id BIGINT REFERENCES groups(id);
ALTER TABLE openclaw_task_budget_leases ADD COLUMN IF NOT EXISTS gateway_api_key_id BIGINT REFERENCES api_keys(id);
ALTER TABLE openclaw_task_budget_leases ADD COLUMN IF NOT EXISTS allowed_models JSONB;
ALTER TABLE openclaw_task_budget_leases DROP CONSTRAINT openclaw_task_budget_leases_reserved_usd_check;
ALTER TABLE openclaw_task_budget_leases ADD CHECK (reserved_usd >= 0);
ALTER TABLE openclaw_task_budget_leases DROP CONSTRAINT openclaw_task_budget_leases_status_check;
ALTER TABLE openclaw_task_budget_leases ADD CHECK (status IN ('active','captured','released','expired','closing','closed','unsettled'));
CREATE TABLE openclaw_gateway_requests (
    request_id VARCHAR(255) NOT NULL, api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    lease_id VARCHAR(64) NOT NULL REFERENCES openclaw_task_budget_leases(lease_id),
    billing_user_id BIGINT NOT NULL REFERENCES users(id), model VARCHAR(255) NOT NULL,
    input_tokens BIGINT, output_tokens BIGINT, cache_read_tokens BIGINT, cache_creation_tokens BIGINT,
    actual_cost_usd NUMERIC(20,8), funding_source VARCHAR(32) CHECK (funding_source IN ('wallet','subscription','none')),
    status VARCHAR(32) NOT NULL CHECK (status IN ('inflight','settled','unsettled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), settled_at TIMESTAMPTZ,
    PRIMARY KEY (request_id, api_key_id)
);
CREATE INDEX openclaw_gateway_requests_lease ON openclaw_gateway_requests(lease_id,status);
CREATE INDEX openclaw_gateway_requests_unsettled ON openclaw_gateway_requests(billing_user_id) WHERE status='unsettled';
CREATE TABLE openclaw_task_session_closures (
    platform_user_hmac VARCHAR(64) NOT NULL, task_id VARCHAR(255) NOT NULL,
    session_id VARCHAR(255) NOT NULL, node_id VARCHAR(255) NOT NULL,
    idempotency_key VARCHAR(255) NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(platform_user_hmac,task_id)
);
CREATE TABLE openclaw_billing_adjustments (
    billing_account_id VARCHAR(64) NOT NULL REFERENCES openclaw_billing_accounts(billing_account_id),
    idempotency_key VARCHAR(255) NOT NULL, operation VARCHAR(16) NOT NULL CHECK(operation IN ('add','set')),
    amount_usd NUMERIC(20,8) NOT NULL CHECK(amount_usd>=0), reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(billing_account_id,idempotency_key)
);
CREATE TABLE openclaw_billing_subscription_assignments (
    billing_account_id VARCHAR(64) NOT NULL REFERENCES openclaw_billing_accounts(billing_account_id),
    idempotency_key VARCHAR(255) NOT NULL, group_id BIGINT NOT NULL REFERENCES groups(id),
    days INTEGER NOT NULL, reason TEXT NOT NULL, subscription_id BIGINT NOT NULL REFERENCES user_subscriptions(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(billing_account_id,idempotency_key)
);
