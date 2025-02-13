### Rate Limiter

Create tiered rate-limiting plans for your users, just like OpenAI does. Metering usage is especially important for AI services, whose backends are quite expensive.

First, you define usage plans; these are limits applied to each endpoint, such as 10 requests per minute + 200 requests per day + 2_000 tokens per minute. Then you assign plans to users.

### Requirements

- Go 1.23 or higher
- Postgres instance
- Redis-compatible key-value store

We use Postgres to store usage-plans, and Redis to store rate-limit data. Redis kinda sucks though, but you can use any key-value store that has a Redis-compatible API; we recommend (microsoft/Garnet)[https://github.com/microsoft/garnet] instead. Note that we use postgres-specific features in our tables, so Postgres can't be swapped out with another SQL database easily.

### Deploy locally

1. Add our golang library to your project:

```bash
go get github.com/cozy-creator/ratelimiter
```

2. In this repo's root, start the postgres + docker by running:

```bash
docker-compose up -d
```

3. Run database migrations; this creates the initial tables in postgres that we'll use:

```bash
go run cmd/migrate/main.go
```

## Usage Examples

### Construct the client

```go
package main

import (
    "context"
    "log"
    ratelimiter "github.com/cozy-creator/ratelimiter"
)

func main() {
    // Create client with default configuration (localhost postgres and redis)
    client, err := ratelimiter.NewClient()
    if err != nil {
        log.Fatalf("creating client: %v", err)
    }
    defer client.Close()

    // Or create with custom configuration using functional options
    client, err = ratelimiter.NewClient(
        ratelimiter.WithPostgresDSN("postgres://user:pass@host:5432/db"),
        ratelimiter.WithRedisAddr("redis:6379"),
        ratelimiter.WithRedisPassword("optional-password"),
    )
    if err != nil {
        log.Fatalf("creating client: %v", err)
    }
    defer client.Close()

    // Or reuse existing database clients
    client, err = ratelimiter.NewClient(
        ratelimiter.WithDBClient(existingDB),
        ratelimiter.WithRedisClient(existingRedis),
    )
    if err != nil {
        log.Fatalf("creating client: %v", err)
    }
    defer client.Close()
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
    ratelimiter.DeductTokens(10),    // Deduct tokens
    ratelimiter.DeductCredits(5),    // Deduct credits
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
    ratelimiter.DeductTokens(100),  // Final token consumption
    ratelimiter.DeductCredits(15),  // Final credit consumption
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

### Limiter Policies

Each endpoint can have multiple limiters of different types:
- `RequestUnit`: Counts each request as 1 unit (automatically applied to every request)
- `TokenUnit`: Counts based on token consumption (specified via WithTokens)
- Concurrent requests are always counted per-request, if a concurrency limit is specified

## Plan Management

Plans can be defined either programmatically using Go structs or via YAML configuration files.

The rate limiter supports flexible plan management through the admin package. Plans can be defined and managed in two ways:

### 1. Using YAML Configuration (Recommended)

Create a `plans.yaml` file to define your rate limiting plans:

```yaml
plans:
  free_tier:
    name: "Free Tier"
    endpoints:
      "gtp-4o-mini":
        fixed_windows:
          - window: 60s      # Requests per minute
            limit: 3
            unit_kind: request
          - window: 86_400s  # Requests per day
            limit: 200
            unit_kind: request
          - window: 60s     # Tokens per minute
            limit: 40_000
            unit_kind: token

      "dall-e-3":
        fixed_windows:
          - window: 60s   # 1 image per minute
            limit: 1
            unit_kind: token

  tier_1:
    name: "Tier 1"
    endpoints:
      "gtp-4o-mini":
        fixed_windows:
          - window: 60s
            limit: 500
            unit_kind: request
          - window: 86_400s  # Requests per day
            limit: 10_000
            unit_kind: request
          - window: 60s
            limit: 200_000
            unit_kind: token
      
      "o1-preview":
        fixed_windows:
          - window: 60s
            limit: 500
            unit_kind: request
          - window: 60s
            limit: 30_000
            unit_kind: token

      "dall-e-3":
        fixed_windows:
          - window: 60s   # 500 images per minute
            limit: 500
            unit_kind: token

  tier_2:
    name: "Tier 2"
    endpoints:
      "gtp-4o-mini":
        fixed_windows:
          - window: 60s
            limit: 5_000
            unit_kind: request
          - window: 60s
            limit: 2_000_000
            unit_kind: token
      
      "o1-preview":
        fixed_windows:
          - window: 60s
            limit: 5_000
            unit_kind: request
          - window: 60s
            limit: 450_000
            unit_kind: token

      "dall-e-3":
        fixed_windows:
          - window: 60s   # 2,500 images per minute
            limit: 2_500
            unit_kind: token
```

Load and apply the plans:

```go
import (
    "context"
    "log"
    "github.com/cozy-creator/ratelimiter/admin"
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
    "github.com/cozy-creator/ratelimiter/admin"
    "github.com/cozy-creator/ratelimiter/limiters"
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
- Accounts without a specified plan get the default plan automatically (for example, if you just want one plan to apply to all non-logged in users, who are identified with just IP-addresses)


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
            limit: 200
            unit_kind: request
      "api/v1/images":   # Image API is now accessible too
        fixed_windows:
          - window: 3600s
            limit: 100
            unit_kind: request
```

## Rate Limit Information

Applications can retrieve information about their current rate limits using the `GetRateLimitInfo` method. This is useful for displaying rate limit status to users or implementing client-side rate limiting.

```go
info, err := service.GetRateLimitInfo(ctx, accountID, "api/v1/chat")
if err != nil {
    log.Printf("Error getting rate limit info: %v", err)
    return
}

fmt.Printf("Request Limits: %d/%d (resets in %v)\n", 
    info.RequestRemaining, info.RequestLimit, info.RequestReset)
fmt.Printf("Token Limits: %d/%d (resets in %v)\n",
    info.TokenRemaining, info.TokenLimit, info.TokenReset)
```

The rate limit information includes:
- `RequestLimit`: Maximum number of requests allowed in the current window
- `RequestRemaining`: Number of requests remaining in the current window
- `RequestReset`: Duration until the request counter resets
- `TokenLimit`: Maximum number of tokens allowed in the current window
- `TokenRemaining`: Number of tokens remaining in the current window
- `TokenReset`: Duration until the token counter resets

This information is also included in HTTP response headers when using the HTTP middleware:
```
X-RateLimit-Limit-Requests: 100
X-RateLimit-Remaining-Requests: 95
X-RateLimit-Reset-Requests: 55
X-RateLimit-Limit-Tokens: 40000
X-RateLimit-Remaining-Tokens: 38500
X-RateLimit-Reset-Tokens: 3540
```

The reset times are provided in seconds. A value of 0 indicates that the limit has already reset.
