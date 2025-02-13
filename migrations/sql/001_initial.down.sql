-- Drop triggers for materialized view refresh
DROP TRIGGER IF EXISTS refresh_account_policies_on_account_change ON ratelimit.account;
DROP TRIGGER IF EXISTS refresh_account_policies_on_plan_change ON ratelimit.plan;
DROP TRIGGER IF EXISTS refresh_credit_balance_on_quota_change ON ratelimit.quota_block;

-- Drop triggers for updated_at
DROP TRIGGER IF EXISTS update_quota_block_updated_at ON ratelimit.quota_block;
DROP TRIGGER IF EXISTS update_account_updated_at ON ratelimit.account;
DROP TRIGGER IF EXISTS update_plan_updated_at ON ratelimit.plan;

-- Drop functions
DROP FUNCTION IF EXISTS ratelimit.refresh_account_policies();
DROP FUNCTION IF EXISTS ratelimit.refresh_account_credit_balance();
DROP FUNCTION IF EXISTS ratelimit.update_updated_at_column();

-- Drop indexes
DROP INDEX IF EXISTS ratelimit.idx_quota_block_account_credits;
DROP INDEX IF EXISTS ratelimit.idx_account_plan_id;

-- Drop materialized views (this will also drop their indexes)
DROP MATERIALIZED VIEW IF EXISTS ratelimit.account_credit_balance;
DROP MATERIALIZED VIEW IF EXISTS ratelimit.account_policies;

-- Drop tables (in correct order due to foreign key constraints)
DROP TABLE IF EXISTS ratelimit.quota_block;
DROP TABLE IF EXISTS ratelimit.default_plan;
DROP TABLE IF EXISTS ratelimit.account;
DROP TABLE IF EXISTS ratelimit.plan;

-- Drop schema
DROP SCHEMA IF EXISTS ratelimit;
