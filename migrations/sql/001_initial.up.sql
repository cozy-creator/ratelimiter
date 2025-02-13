-- Create rate limiting schema
CREATE SCHEMA IF NOT EXISTS ratelimit;

-- Create plan table first (since it's referenced by account)
CREATE TABLE IF NOT EXISTS ratelimit.plan (
    id VARCHAR PRIMARY KEY,
    name VARCHAR NOT NULL,
    policies BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Create default plan reference table (with proper single-row constraint)
CREATE TABLE IF NOT EXISTS ratelimit.default_plan (
    plan_id VARCHAR NOT NULL REFERENCES ratelimit.plan(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT ensure_single_default_plan UNIQUE (created_at)
);

-- Create account table
CREATE TABLE IF NOT EXISTS ratelimit.account (
    id VARCHAR PRIMARY KEY,
    plan_id VARCHAR NOT NULL REFERENCES ratelimit.plan(id),
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_account_plan FOREIGN KEY (plan_id) REFERENCES ratelimit.plan(id)
);

-- Create quota block table
CREATE TABLE IF NOT EXISTS ratelimit.quota_block (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id VARCHAR NOT NULL,
    credits BIGINT NOT NULL,
    expires_at TIMESTAMPTZ,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_quota_block_account FOREIGN KEY (account_id) REFERENCES ratelimit.account(id),
    CONSTRAINT credits_non_negative CHECK (credits >= 0)
);

-- Create account_policies as a materialized view
CREATE MATERIALIZED VIEW IF NOT EXISTS ratelimit.account_policies AS
SELECT 
    a.id as account_id,
    p.policies,
    CURRENT_TIMESTAMP as updated_at
FROM ratelimit.account a
JOIN ratelimit.plan p ON a.plan_id = p.id;

-- Create materialized view for pre-computed credit balances
CREATE MATERIALIZED VIEW IF NOT EXISTS ratelimit.account_credit_balance AS
SELECT 
    account_id,
    SUM(credits) as total_credits,
    MIN(CASE WHEN expires_at IS NOT NULL THEN expires_at END) as next_expiration
FROM ratelimit.quota_block
WHERE credits > 0 AND (expires_at > NOW() OR expires_at IS NULL)
GROUP BY account_id;

-- Add indexes for efficient lookups
CREATE INDEX IF NOT EXISTS idx_account_plan_id ON ratelimit.account(plan_id);
CREATE INDEX IF NOT EXISTS idx_quota_block_account_credits 
    ON ratelimit.quota_block(account_id, expires_at) 
    WHERE credits > 0;

-- Add unique indexes on materialized views for fast lookups
CREATE UNIQUE INDEX IF NOT EXISTS idx_account_policies_account_id 
    ON ratelimit.account_policies(account_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_account_credit_balance_account_id 
    ON ratelimit.account_credit_balance(account_id);

-- Add additional indexes for performance
CREATE INDEX IF NOT EXISTS idx_quota_block_expires_at 
    ON ratelimit.quota_block(expires_at) 
    WHERE expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_quota_block_updated_at 
    ON ratelimit.quota_block(updated_at);

-- Create trigger to update updated_at
CREATE OR REPLACE FUNCTION ratelimit.update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ language 'plpgsql';

-- Create function to refresh credit balance materialized view
CREATE OR REPLACE FUNCTION ratelimit.refresh_account_credit_balance()
RETURNS TRIGGER AS $$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY ratelimit.account_credit_balance;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Create function to refresh account policies materialized view
CREATE OR REPLACE FUNCTION ratelimit.refresh_account_policies()
RETURNS TRIGGER AS $$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY ratelimit.account_policies;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Create triggers to update updated_at
CREATE TRIGGER update_plan_updated_at
    BEFORE UPDATE ON ratelimit.plan
    FOR EACH ROW
    EXECUTE FUNCTION ratelimit.update_updated_at_column();

CREATE TRIGGER update_account_updated_at
    BEFORE UPDATE ON ratelimit.account
    FOR EACH ROW
    EXECUTE FUNCTION ratelimit.update_updated_at_column();

CREATE TRIGGER update_quota_block_updated_at
    BEFORE UPDATE ON ratelimit.quota_block
    FOR EACH ROW
    EXECUTE FUNCTION ratelimit.update_updated_at_column();

-- Create trigger to refresh credit balance when quota blocks change
CREATE TRIGGER refresh_credit_balance_on_quota_change
    AFTER INSERT OR UPDATE OR DELETE ON ratelimit.quota_block
    FOR EACH STATEMENT
    EXECUTE FUNCTION ratelimit.refresh_account_credit_balance();

-- Create triggers to refresh account policies when accounts or plans change
CREATE TRIGGER refresh_account_policies_on_account_change
    AFTER INSERT OR UPDATE OR DELETE ON ratelimit.account
    FOR EACH STATEMENT
    EXECUTE FUNCTION ratelimit.refresh_account_policies();

CREATE TRIGGER refresh_account_policies_on_plan_change
    AFTER INSERT OR UPDATE OR DELETE ON ratelimit.plan
    FOR EACH STATEMENT
    EXECUTE FUNCTION ratelimit.refresh_account_policies();

