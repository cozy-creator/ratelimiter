-- Policy assigned to each customer_id + endpoint_id combination
CREATE TABLE user_endpoint_policy (
    customer_id     TEXT NOT NULL,
    endpoint_id TEXT NOT NULL,
    policy_id   UUID NOT NULL,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (customer_id, endpoint_id),
    FOREIGN KEY (policy_id) REFERENCES policy_definition (id)
);

-- The definition for creating a limiter
CREATE TABLE policy_definition (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT UNIQUE NOT NULL,

    -- Entire policy definition stored in JSONB:
    rules       JSONB NOT NULL DEFAULT '{}',

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The rate-limiter state, for a given userid + endpointid
CREATE TABLE limiter_state (
    customer_id     TEXT NOT NULL,
    endpoint_id TEXT NOT NULL,
    
    -- Store the current usage/state (fixed/sliding/token bucket arrays, semaphore locks, etc.)
    state       JSONB NOT NULL DEFAULT '{}',

    version     INT NOT NULL DEFAULT 0,  -- optimistic lock
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (customer_id, endpoint_id)
);

-- A credit system, e.g., if users have to buy API credits
CREATE TABLE credit_block (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id     TEXT NOT NULL,

    original_tokens INT NOT NULL,
    remaining_tokens INT NOT NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ,

    -- For frequent lookups
    INDEX credit_blocks_user_idx (customer_id)
);

-- A usage tracking system, e.g., if users are billed monthly for API usage
-- Bucketed by the hour
CREATE TABLE usage_history (
    customer_id        TEXT NOT NULL,
    endpoint_id    TEXT NOT NULL,
    window_start   TIMESTAMPTZ NOT NULL, -- truncated to the hour

    requests_count INT NOT NULL DEFAULT 0,
    tokens_consumed INT NOT NULL DEFAULT 0,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (customer_id, endpoint_id, window_start)
);
