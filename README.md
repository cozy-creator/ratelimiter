# Rate Limiter

A flexible rate limiting library that supports multiple rate limiting strategies and usage-based billing.

## Features

- Multiple rate limiting strategies:
  - Sliding Window (request or token-based)
  - Fixed Window (request or token-based)
  - Token Bucket (request or token-based)
  - Concurrent Requests Limiting
- Usage-based billing with credits system
- Plan-based rate limiting policies
- High performance with Redis-based rate limiting
- Persistent storage with PostgreSQL
- In-memory caching for plan definitions
- Post-request token and credit consumption

## Requirements

- Go 1.23 or higher
- Docker and Docker Compose
- PostgreSQL 12 or higher (provided via Docker)
- Redis 6 or higher (provided via Docker)

## Local Development Setup

1. Create a `docker-compose.yml` file in your project root:

```yaml
version: '3.8'
services:
  postgres:
    image: postgres:15
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: postgres
      POSTGRES_DB: ratelimiter
    ports:
      - "5432:5432"
    volumes:
      - postgres_data:/var/lib/postgresql/data

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    volumes:
      - redis_data:/data
    command: redis-server --save 60 1 --loglevel warning

volumes:
  postgres_data:
  redis_data:
```

2. Start the services:

```bash
docker-compose up -d
```

3. Install the library:

```bash
go get github.com/cozy-creator/rate-limiter
```

4. Run database migrations:

```bash
go run cmd/migrate/main.go
```

## Usage Examples

### Basic Setup

```go
package main

import (
    "context"
    "log"
    ratelimiter "github.com/cozy-creator/rate-limiter"
)

func main() {
    // Create rate limiter config
    cfg := ratelimiter.Config{
        PostgresDSN:   "postgres://postgres:postgres@localhost:5432/ratelimiter?sslmode=disable",
        RedisAddr:     "localhost:6379",
        RedisPassword: "",
        RedisDB:      0,
    }

    // Create rate limiter client
    client, err := ratelimiter.NewClient(cfg)
    if err != nil {
        log.Fatalf("creating client: %v", err)
    }
    defer client.Close()

    // Use the client...
}
```

### Basic Rate Limiting

```go
// Attempt a request (consumes 1 request unit by default)
allowed, err := client.AttemptRequest(
    ctx,
    "request-123",  // Request ID
    "user-456",     // User ID
    "api/v1/chat",  // Endpoint ID
)
if err != nil {
    log.Fatalf("Error: %v", err)
}
if !allowed {
    log.Println("Rate limit exceeded")
    return
}

// ... handle the request ...

// End the request (just cleanup)
result, err := client.EndRequest(ctx, "request-123")
if err != nil {
    log.Printf("Error ending request: %v", err)
}
```

### Rate Limiting with Token and Credit Consumption

```go
// Start request with initial consumption
allowed, err := client.AttemptRequest(
    ctx,
    "request-123",
    "user-456",
    "api/v1/chat",
    ratelimiter.WithTokens(10),    // Initial token estimate
    ratelimiter.WithCredits(5),    // Initial credit estimate
)
if err != nil {
    log.Fatalf("Error: %v", err)
}
if !allowed {
    log.Println("Rate limit exceeded")
    return
}

// ... handle request ...

// End request with final consumption
result, err := client.EndRequest(ctx, "request-123",
    ratelimiter.WithTokens(100),  // Final token consumption
    ratelimiter.WithCredits(15),  // Final credit consumption
)
if err != nil {
    log.Printf("Error ending request: %v", err)
    return
}
if result != nil && result.RemainingDue > 0 {
    log.Printf("Warning: Could only consume %d of %d requested credits", 
        result.Consumed, result.Requested)
}
```

### Example Policy Configuration

```sql
-- Insert a plan with rate limits
INSERT INTO ratelimit.plan (name, policies, is_default) VALUES (
    'basic_plan',
    -- MessagePack encoded policies for endpoints
    '\x82a9api/v1/chat82ac sliding_windows91a7window1a601a5limit1a64a8unit_kind a7request ab token_buckets91a8bucket_size1a632a8refill_rate1a65a8unit_kind a5token af max_concurrent1a05',
    true
);

-- Assign plan to account
INSERT INTO ratelimit.account (plan_id) VALUES ('plan-uuid-here');
```

This creates a plan with:
- Sliding window: 100 requests per minute
- Token bucket: 50 tokens capacity, refilling at 5 tokens/second
- Maximum 5 concurrent requests

### Policy Types

Each endpoint can have multiple limiters of different types:
- `RequestUnit`: Counts each request as 1 unit (automatically applied to every request)
- `TokenUnit`: Counts based on token consumption (specified via WithTokens)
- Concurrent requests are always counted per-request

## Plan Configuration

Plans can be defined either programmatically using Go structs or via YAML configuration files.

### Endpoint Access Control

Plans explicitly define which endpoints an account can access. If an endpoint is not defined in a plan's policies:
- The account will be denied access to that endpoint
- `AttemptRequest` will return an error: "no access to endpoint: {endpoint_name}"
- This provides a simple but effective access control mechanism

For example, in this plan only the chat API is accessible:
```yaml
plans:
  basic_plan:
    name: "Basic Plan"
    endpoints:
      "api/v1/chat":     # This endpoint is accessible
        sliding_windows:
          - window: 60s
            limit: 100
            unit_kind: request
      # api/v1/images is not defined, so it's not accessible
```

To grant access to a new endpoint, you must explicitly define its policy in the plan:
```yaml
plans:
  premium_plan:
    name: "Premium Plan"
    endpoints:
      "api/v1/chat":     # Chat API is accessible
        sliding_windows:
          - window: 60s
            limit: 1000
      "api/v1/images":   # Image API is now accessible too
        fixed_windows:
          - window: 3600s
            limit: 1000
```

### Real-World Example: OpenAI-Style Rate Limits

Here's an example implementing rate limits similar to OpenAI's API tiers:

```yaml
plans:
  free_tier:
    name: "Free Tier"
    endpoints:
      "v1/chat/completions":
        sliding_windows:
          - window: 60s      # RPM limit
            limit: 3
            unit_kind: request
          - window: 3600s    # Hourly TPM limit
            limit: 40000
            unit_kind: token
        token_buckets:
          - bucket_size: 40000   # Token bucket for bursts
            refill_rate: 11.11   # ~40k tokens per hour
            unit_kind: token
        max_concurrent: 1    # Free tier: 1 request at a time

      "v1/images/generations":
        fixed_windows:
          - window: 60s
            limit: 2         # 2 images per minute
            unit_kind: request
        max_concurrent: 1

  pro_tier:
    name: "Pro Tier"
    endpoints:
      "v1/chat/completions":
        sliding_windows:
          - window: 60s
            limit: 60        # 60 RPM
            unit_kind: request
          - window: 3600s
            limit: 200000    # 200k TPH
            unit_kind: token
        token_buckets:
          - bucket_size: 200000
            refill_rate: 55.56    # ~200k tokens per hour
            unit_kind: token
        max_concurrent: 5

      "v1/images/generations":
        fixed_windows:
          - window: 60s
            limit: 10        # 10 images per minute
            unit_kind: request
        max_concurrent: 3

  enterprise_tier:
    name: "Enterprise Tier"
    endpoints:
      "v1/chat/completions":
        sliding_windows:
          - window: 60s
            limit: 10000     # 10k RPM
            unit_kind: request
          - window: 3600s
            limit: 2000000   # 2M TPH
            unit_kind: token
        token_buckets:
          - bucket_size: 2000000
            refill_rate: 555.56   # ~2M tokens per hour
            unit_kind: token
        max_concurrent: 50

      "v1/images/generations":
        fixed_windows:
          - window: 60s
            limit: 100       # 100 images per minute
            unit_kind: request
        max_concurrent: 10

      "v1/fine-tunes":      # Only available to enterprise tier
        sliding_windows:
          - window: 60s
            limit: 10
            unit_kind: request
        max_concurrent: 2
```

This configuration implements:

1. **Free Tier Limits**:
   - Chat completions: 3 requests/minute, 40K tokens/hour
   - Image generation: 2 images/minute
   - Single concurrent request per endpoint
   - No access to fine-tuning

2. **Pro Tier Limits**:
   - Chat completions: 60 requests/minute, 200K tokens/hour
   - Image generation: 10 images/minute
   - Multiple concurrent requests
   - No access to fine-tuning

3. **Enterprise Tier Limits**:
   - Chat completions: 10K requests/minute, 2M tokens/hour
   - Image generation: 100 images/minute
   - High concurrency limits
   - Access to fine-tuning endpoint

Features demonstrated:
- Multiple rate limit types per endpoint (RPM and TPH)
- Token bucket for handling bursts
- Concurrent request limits
- Access control through endpoint availability
- Mix of time windows (per-minute and per-hour limits)
- Both request-based and token-based limits

## Plan Management

The rate limiter supports flexible plan management through the admin package. Plans can be defined and managed in two ways:

### 1. Using YAML Configuration (Recommended)

Create a `plans.yaml` file to define your rate limiting plans:

```yaml
plans:
  free_plan:
    name: "Free Plan"
    endpoints:
      "api/v1/chat":
        sliding_windows:
          - window: 60s
            limit: 100
            unit_kind: request  # Count requests
        token_buckets:
          - bucket_size: 1000
            refill_rate: 10
            unit_kind: token    # Count tokens
        max_concurrent: 5

  premium_plan:
    name: "Premium Plan"
    endpoints:
      "api/v1/chat":
        sliding_windows:
          - window: 60s
            limit: 1000
            unit_kind: request
        token_buckets:
          - bucket_size: 10000
            refill_rate: 100
            unit_kind: token
        max_concurrent: 20

  enterprise_plan:
    name: "Enterprise Plan"
    endpoints:
      "api/v1/chat":
        sliding_windows:
          - window: 60s
            limit: 10000
            unit_kind: request
        token_buckets:
          - bucket_size: 100000
            refill_rate: 1000
            unit_kind: token
        max_concurrent: 100
```

Load and apply the plans:

```go
import (
    "context"
    "log"
    "github.com/cozy-creator/rate-limiter/admin"
    "github.com/uptrace/bun"
)

func managePlans(ctx context.Context, db *bun.DB) error {
    // Load plans from YAML
    plans, err := admin.LoadPlansFromFile("plans.yaml")
    if err != nil {
        return fmt.Errorf("loading plans: %w", err)
    }

    // Apply plans to database
    if err := admin.ApplyPlans(ctx, db, plans); err != nil {
        return fmt.Errorf("applying plans: %w", err)
    }

    // Set the free plan as default
    if err := admin.SetDefaultPlan(ctx, db, "free_plan"); err != nil {
        return fmt.Errorf("setting default plan: %w", err)
    }

    return nil
}
```

### 2. Using Go Structs

Plans can also be defined programmatically:

```go
import (
    "time"
    "github.com/cozy-creator/rate-limiter/admin"
    "github.com/cozy-creator/rate-limiter/limiters"
)

func definePlans(ctx context.Context, db *bun.DB) error {
    plans := admin.Plans{
        "free_plan": {
            Name: "Free Plan",
            Endpoints: map[string]limiters.Policy{
                "api/v1/chat": {
                    SlidingWindows: []limiters.WindowLimit{
                        {
                            Window:   time.Minute,
                            Limit:    100,
                            UnitKind: limiters.RequestUnit,
                        },
                    },
                    TokenBuckets: []limiters.TokenBucketLimit{
                        {
                            BucketSize: 1000,
                            RefillRate: 10,
                            UnitKind:   limiters.TokenUnit,
                        },
                    },
                    MaxConcurrent: 5,
                },
            },
        },
    }

    // Apply plans
    if err := admin.ApplyPlans(ctx, db, plans); err != nil {
        return fmt.Errorf("applying plans: %w", err)
    }

    // Set as default plan
    return admin.SetDefaultPlan(ctx, db, "free_plan")
}
```

### Managing Plans and Accounts

The admin package provides several functions for managing plans and accounts:

```go
// Get the current default plan
defaultPlan, err := admin.GetDefaultPlan(ctx, db)
if err != nil {
    log.Printf("Error getting default plan: %v", err)
}

// Change the default plan
err = admin.SetDefaultPlan(ctx, db, "premium_plan")
if err != nil {
    log.Printf("Error setting default plan: %v", err)
}

// Assign a plan to an account
err = admin.AssignPlan(ctx, db, "account-123", "premium_plan")
if err != nil {
    log.Printf("Error assigning plan: %v", err)
}

// Get current plan for an account
plan, err := admin.GetAccountPlan(ctx, db, "account-123")
if err != nil {
    log.Printf("Error getting plan: %v", err)
}

// List all available plans
plans, err := admin.ListPlans(ctx, db)
if err != nil {
    log.Printf("Error listing plans: %v", err)
}

// Delete a plan (will fail if it's the default plan or if accounts are using it)
err = admin.DeletePlan(ctx, db, "old_plan")
if err != nil {
    log.Printf("Error deleting plan: %v", err)
}
```

### Default Plan Management

The system maintains exactly one default plan at all times:
- Use `SetDefaultPlan` to change the default plan
- The default plan cannot be deleted while it's set as default
- `GetDefaultPlan` always returns the current default plan
- New accounts without a specified plan get the default plan automatically

```go
    ctx := context.Background()

    type Plans struct {
        Plan1: {
            Endpoint1: [
                RateLimit: 1
                ConcurrencyLimit: 1
            ]
        },
        Plan2: {},
        DefaultPlan: {}, // fallback if not specified by user
    }

    // Fetch plan information
    plans, _ := cli.Get(ctx, "plans")

	planString, err = cli.Get(ctx, "plan:" + "userID")
	if err != nil {
		panic(err)
	}

    policy = Plans[planString]

    limiterState, _ = cli.Get(ctx, "limiterstate:" + "userID:" + "endpointID")

    // check to see if the limiter state doesn't exist, and use the above if not to create
    // a new limiter state here

    allow := limiterState.CanUpdate(usage)
    if !allow {
        return false, nil
    }

    limiterState.update(usage)
    err = cli.Set(ctx, "limiterstate:" + "userID:" + "endpointID", limiterState)

    return true, nil
```
