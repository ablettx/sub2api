-- 用户 × 上游账号 × 真实周期配额。历史账本不随成员移除或账号软删除级联清理。
CREATE TABLE IF NOT EXISTS account_user_quota_policies (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL UNIQUE REFERENCES accounts(id),
    upstream_account_id VARCHAR(200) NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    enforced_at TIMESTAMPTZ NULL,
    config_version BIGINT NOT NULL DEFAULT 1 CHECK (config_version > 0),
    fallback_group_ids JSONB NOT NULL DEFAULT '[]',
    carryover_seconds INTEGER NOT NULL DEFAULT 1800 CHECK (carryover_seconds BETWEEN 600 AND 86400),
    updated_by BIGINT NULL,
    history_note VARCHAR(1000) NOT NULL DEFAULT '',
    history_verified_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(fallback_group_ids) = 'array')
);
CREATE INDEX IF NOT EXISTS account_user_quota_policies_upstream_identity
    ON account_user_quota_policies(upstream_account_id) WHERE enabled;
CREATE INDEX IF NOT EXISTS ablett_accounts_quota_upstream_identity
    ON accounts((credentials->>'chatgpt_account_id'))
    WHERE platform='openai' AND type='oauth' AND parent_account_id IS NULL AND deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS account_user_quota_members (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES accounts(id),
    user_id BIGINT NOT NULL REFERENCES users(id),
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (account_id, user_id)
);
CREATE INDEX IF NOT EXISTS account_user_quota_members_user_id ON account_user_quota_members(user_id);

CREATE TABLE IF NOT EXISTS account_user_quota_cycles (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES accounts(id),
    starts_at TIMESTAMPTZ NOT NULL,
    resets_at TIMESTAMPTZ NOT NULL,
    ready BOOLEAN NOT NULL DEFAULT FALSE,
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    account_stats_cost NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (account_stats_cost >= 0 AND account_stats_cost < 1000000000000),
    estimate NUMERIC(20,8) NULL CHECK (estimate > 0 AND estimate < 1000000000000),
    estimate_at TIMESTAMPTZ NULL,
    estimate_source_cycle_id BIGINT NULL,
    observed_at TIMESTAMPTZ NULL,
    used_percent DOUBLE PRECISION NULL,
    missing_since TIMESTAMPTZ NULL,
    backfill_through_id BIGINT NOT NULL DEFAULT 0 CHECK (backfill_through_id >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (account_id, resets_at),
    CHECK (resets_at > starts_at),
    CHECK (used_percent IS NULL OR (used_percent >= 0 AND used_percent <= 100))
);
CREATE INDEX IF NOT EXISTS account_user_quota_cycles_account_start ON account_user_quota_cycles(account_id, starts_at);

CREATE TABLE IF NOT EXISTS account_user_quota_usage (
    id BIGSERIAL PRIMARY KEY,
    cycle_id BIGINT NOT NULL REFERENCES account_user_quota_cycles(id),
    user_id BIGINT NOT NULL REFERENCES users(id),
    actual_cost NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (actual_cost >= 0 AND actual_cost < 1000000000000),
    reserved_cost NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (reserved_cost >= 0 AND reserved_cost < 1000000000000),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (cycle_id, user_id)
);
CREATE INDEX IF NOT EXISTS account_user_quota_usage_user_id ON account_user_quota_usage(user_id);

CREATE TABLE IF NOT EXISTS account_user_quota_reservations (
    id BIGSERIAL PRIMARY KEY,
    token VARCHAR(64) NOT NULL UNIQUE,
    account_id BIGINT NOT NULL REFERENCES accounts(id),
    cycle_id BIGINT NOT NULL REFERENCES account_user_quota_cycles(id),
    user_id BIGINT NOT NULL REFERENCES users(id),
    api_key_id BIGINT NOT NULL,
    original_group_id BIGINT NULL,
    effective_group_id BIGINT NULL,
    subscription_id BIGINT NULL,
    request_id VARCHAR(128) NOT NULL DEFAULT '',
    pricing_at TIMESTAMPTZ NOT NULL,
    config_version BIGINT NOT NULL CHECK (config_version > 0),
    reserved_cost NUMERIC(20,8) NOT NULL CHECK (reserved_cost >= 0 AND reserved_cost < 1000000000000),
    actual_cost NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (actual_cost >= 0 AND actual_cost < 1000000000000),
    state VARCHAR(16) NOT NULL DEFAULT 'reserved'
        CHECK (state IN ('reserved', 'sent', 'settled', 'released', 'uncertain')),
    lease_until TIMESTAMPTZ NOT NULL,
    settled_at TIMESTAMPTZ NULL,
    billing_request_id VARCHAR(128) NULL,
    calibration_run_id VARCHAR(64) NULL,
    calibration_ordinal INTEGER NULL CHECK (calibration_ordinal BETWEEN 1 AND 3),
    calibration_max_requests INTEGER NULL CHECK (calibration_max_requests BETWEEN 1 AND 3),
    calibration_budget NUMERIC(20,8) NULL CHECK (calibration_budget > 0 AND calibration_budget < 1000000000000),
    reconciliation_note VARCHAR(1000) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (calibration_run_id, calibration_ordinal),
    CHECK ((calibration_run_id IS NULL AND calibration_ordinal IS NULL AND calibration_budget IS NULL AND calibration_max_requests IS NULL)
        OR (calibration_run_id IS NOT NULL AND calibration_ordinal IS NOT NULL AND calibration_budget IS NOT NULL AND calibration_max_requests IS NOT NULL
            AND calibration_ordinal <= calibration_max_requests))
);
CREATE INDEX IF NOT EXISTS account_user_quota_reservations_cycle_user_state
    ON account_user_quota_reservations(cycle_id, user_id, state);
CREATE INDEX IF NOT EXISTS account_user_quota_reservations_account_lease
    ON account_user_quota_reservations(account_id, lease_until);

-- 可空关联字段兼容历史日志；扣费事实仍由统一事务及配额账本持有。
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS account_user_quota_cycle_id BIGINT NULL,
    ADD COLUMN IF NOT EXISTS original_group_id BIGINT NULL;
CREATE INDEX IF NOT EXISTS usage_logs_account_user_quota_cycle_user
    ON usage_logs(account_user_quota_cycle_id, user_id)
    WHERE account_user_quota_cycle_id IS NOT NULL;

-- Optional receipt metadata preserves the existing request fingerprint contract.
ALTER TABLE usage_billing_dedup
    ADD COLUMN IF NOT EXISTS account_id BIGINT NULL,
    ADD COLUMN IF NOT EXISTS user_id BIGINT NULL,
    ADD COLUMN IF NOT EXISTS execution_started_at TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS account_user_quota_cycle_id BIGINT NULL,
    ADD COLUMN IF NOT EXISTS actual_cost NUMERIC(20,8) NULL,
    ADD COLUMN IF NOT EXISTS account_stats_cost NUMERIC(20,8) NULL;
ALTER TABLE usage_billing_dedup_archive
    ADD COLUMN IF NOT EXISTS account_id BIGINT NULL,
    ADD COLUMN IF NOT EXISTS user_id BIGINT NULL,
    ADD COLUMN IF NOT EXISTS execution_started_at TIMESTAMPTZ NULL,
    ADD COLUMN IF NOT EXISTS account_user_quota_cycle_id BIGINT NULL,
    ADD COLUMN IF NOT EXISTS actual_cost NUMERIC(20,8) NULL,
    ADD COLUMN IF NOT EXISTS account_stats_cost NUMERIC(20,8) NULL;
CREATE INDEX IF NOT EXISTS ablett_usage_billing_unassigned_account
    ON usage_billing_dedup(account_id,execution_started_at) WHERE account_user_quota_cycle_id IS NULL AND actual_cost IS NOT NULL;
CREATE INDEX IF NOT EXISTS ablett_usage_billing_archive_unassigned_account
    ON usage_billing_dedup_archive(account_id,execution_started_at) WHERE account_user_quota_cycle_id IS NULL AND actual_cost IS NOT NULL;

-- 账号周期账本一旦绑定上游身份，换成其他上游账号必须使用新的账号记录。
-- Token 刷新仍可更新 credentials；仅阻止改变配额所有者的身份或类型。
CREATE OR REPLACE FUNCTION ablett_protect_quota_account_identity() RETURNS TRIGGER AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM account_user_quota_policies p WHERE p.account_id=NEW.id AND p.upstream_account_id<>''
          AND (p.upstream_account_id IS DISTINCT FROM NEW.credentials->>'chatgpt_account_id'
            OR NEW.platform<>'openai' OR NEW.type<>'oauth' OR NEW.parent_account_id IS NOT NULL)
    ) THEN
        RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='ablett_quota_account_identity',
            MESSAGE='Account with quota history cannot change upstream identity; create a new account';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS ablett_quota_account_identity ON accounts;
CREATE TRIGGER ablett_quota_account_identity
    BEFORE UPDATE OF credentials,platform,type,parent_account_id ON accounts
    FOR EACH ROW EXECUTE FUNCTION ablett_protect_quota_account_identity();
