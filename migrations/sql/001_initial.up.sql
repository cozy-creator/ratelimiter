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

-- Create default plan reference table
CREATE TABLE IF NOT EXISTS ratelimit.default_plan (
    plan_id VARCHAR NOT NULL REFERENCES ratelimit.plan(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT single_default_plan CHECK (true) NO INHERIT
);

-- Create account table
CREATE TABLE IF NOT EXISTS ratelimit.account (
    id VARCHAR PRIMARY KEY,
    plan_id VARCHAR NOT NULL REFERENCES ratelimit.plan(id),
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Create quota block table
CREATE TABLE IF NOT EXISTS ratelimit.quota_block (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id VARCHAR NOT NULL REFERENCES ratelimit.account(id),
    credits BIGINT NOT NULL,
    expires_at TIMESTAMPTZ,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Create account_policies table for caching
CREATE TABLE IF NOT EXISTS ratelimit.account_policies (
    account_id VARCHAR PRIMARY KEY REFERENCES ratelimit.account(id),
    policies BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Add indexes for efficient lookups
CREATE INDEX IF NOT EXISTS idx_plan_is_default ON ratelimit.plan(is_default) WHERE is_default = true;
CREATE INDEX IF NOT EXISTS idx_account_plan_id ON ratelimit.account(plan_id);
CREATE INDEX IF NOT EXISTS idx_quota_block_account_id ON ratelimit.quota_block(account_id);
CREATE INDEX IF NOT EXISTS idx_quota_block_expires_at ON ratelimit.quota_block(expires_at);

-- Create trigger to update updated_at
CREATE OR REPLACE FUNCTION ratelimit.update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = CURRENT_TIMESTAMP;
    RETURN NEW;
END;
$$ language 'plpgsql';

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

CREATE TRIGGER update_account_policies_updated_at
    BEFORE UPDATE ON ratelimit.account_policies
    FOR EACH ROW
    EXECUTE FUNCTION ratelimit.update_updated_at_column();

-- Create function to refresh the materialized view
CREATE OR REPLACE FUNCTION ratelimit.refresh_account_policies()
RETURNS TRIGGER AS $$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY ratelimit.account_policies;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Create triggers to refresh the materialized view
CREATE TRIGGER refresh_account_policies_on_account_change
    AFTER INSERT OR UPDATE OR DELETE ON ratelimit.account
    FOR EACH STATEMENT
    EXECUTE FUNCTION ratelimit.refresh_account_policies();

CREATE TRIGGER refresh_account_policies_on_plan_change
    AFTER INSERT OR UPDATE OR DELETE ON ratelimit.plan
    FOR EACH STATEMENT
    EXECUTE FUNCTION ratelimit.refresh_account_policies();

-- Add foreign key constraints
ALTER TABLE ratelimit.account 
    ADD CONSTRAINT fk_account_plan_id 
    FOREIGN KEY (plan_id) 
    REFERENCES ratelimit.plan(id);

ALTER TABLE ratelimit.quota_block 
    ADD CONSTRAINT fk_quota_block_account_id 
    FOREIGN KEY (account_id) 
    REFERENCES ratelimit.account(id);
    