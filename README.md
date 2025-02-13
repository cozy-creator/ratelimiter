### Rate Limiter

Create tiered rate-limiting plans for your users, just like OpenAI does. Metering usage is especially important for AI services, whose backends are quite expensive.

For each one of your endpoints you specify a policy, such as such as 10 requests per minute + 200 requests per day + 2_000 tokens per minute for the `gpt-4o` endpoint. All of the policies together are called a `plan` as in like, "premium-plan" or "free-plan".

Then you assign plans to your users. You can assign default plans too, if you don't want to assign plans per user.

For simiplicity, we only use fixed-window rate limits. You can specify whatever unit types you like in your policy: `gpu-second`, `token`, `image`, etc., and a limit on how many of those a user can consume in a specified period of time (Example: 500 images per minute). The default unit-type (if unspecified) is just `request`.

We also support limiting concurrent requests; i.e., a user can only have 5 jobs processing at a time.

We also support `credits`, which are meant to be pre-purchased balances (either your own funny-money or actual dollars) that your users consume every time they use your API. We call credits granted to users `quota blocks` in postgres, and they can have expiration dates.

We currently do not support logging of requests for usage-based billing, but that may come in the future.

### Requirements

- Postgres instance
- Redis-compatible key-value store

We use Postgres to store usage-plans, and Redis to store rate-limit data. Redis kinda sucks though, but you can use any key-value store that has a Redis-compatible API; we recommend [microsoft/Garnet](https://github.com/microsoft/garnet) instead. Note that we use postgres-specific features in our tables, so Postgres can't be swapped out with another SQL database easily.

### Running Locally

1. Add our golang library to your project:

```bash
go get github.com/cozy-creator/ratelimiter
```

2. In this repo's root, start the Postgres + Garnet by using docker compose:

```bash
docker-compose up -d
```

3. Run database migrations; this creates the initial tables in postgres that we'll use:

```bash
go run cmd/migrate/main.go
``` 

## Usage Examples

### Construct a ratelimiter client

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

### Limit By Request Count

```go
// Attempt a request (consumes 1 request unit by default)
allowed, info, err := client.AttemptRequest(
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
    for unitType, window := range info {
        log.Printf("%s: %d/%d (resets in %v)",
            unitType, window.Remaining, window.Limit, window.Reset)
    }
    return
}

// ... handle the request ...

// End the request (just cleanup)
err = client.EndRequest(ctx, "request-123")
if err != nil {
    log.Printf("Error ending request: %v", err)
}
```

### Limit By Tokens + Consume Credits

```go
// Start request with initial consumption
allowed, info, err := client.AttemptRequest(
    ctx,
    "request-123",
    "user-456",
    "api/v1/chat",
    ratelimiter.DeductUnits("token", 10),    // Deduct tokens
)
if err != nil {
    log.Fatalf("Error: %v", err)
}
if !allowed {
    log.Println("Rate limit exceeded")
    for unitType, window := range info {
        log.Printf("%s: %d/%d (resets in %v)",
            unitType, window.Remaining, window.Limit, window.Reset)
    }
    return
}

// ... handle request ...

// End request with final consumption
err = client.EndRequest(ctx, "request-123",
    ratelimiter.DeductUnits("token", 100),  // Final token consumption
)
if err != nil {
    log.Printf("Error ending request: %v", err)
    return
}

// Check and deduct credits
hasBalance, err := client.HasMinBalance(ctx, "user-456", 15)
if err != nil {
    log.Printf("Error checking balance: %v", err)
    return
}
if !hasBalance {
    log.Printf("Insufficient credits")
    return
}

result, err := client.DeductCredits(ctx, "user-456", 15, ratelimiter.WithRequireFullAmount(true))
if err != nil {
    log.Printf("Error deducting credits: %v", err)
    return
}
if result.Remaining > 0 {
    log.Printf("Warning: Could only deduct %d of %d requested credits", 
        result.Deducted, result.Requested)
}
```

## Plan and Account Management

Plans and accounts can be managed using the admin package. This includes creating and managing plans, assigning plans to accounts, and managing quota blocks.

### 1. Using YAML Configuration (Recommended)

Create a `plans.yaml` file to define your rate limiting plans:

```yaml
plans:
  free_tier:
    name: "Free Tier"
    endpoints:
      "gpt-4-turbo":
        fixed_windows:
          - window: 60s      # Requests per minute
            limit: 3
            unit_type: request
          - window: 86_400s  # Requests per day
            limit: 200
            unit_type: request
          - window: 60s     # Tokens per minute
            limit: 40_000
            unit_type: token
        max_concurrent: 4
        concurrency_expiry: 60s

      "dall-e-3":
        fixed_windows:
          - window: 60s   # 1 image per minute
            limit: 1
            unit_type: image
```

Load and apply the plans:

```go
import (
    "context"
    "log"
    "github.com/cozy-creator/ratelimiter/admin"
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

### Managing Plans and Accounts

The admin package provides functions for managing plans, accounts, and quota blocks:

```go
// Plan Management
err = admin.ApplyPlans(ctx, db, plans)                    // Apply plans from config
err = admin.SetDefaultPlan(ctx, db, "premium_plan")       // Change default plan
err = admin.AssignPlan(ctx, db, "account-123", "premium") // Assign plan to account
plan, err := admin.GetAccountPlan(ctx, db, "account-123") // Get account's plan
plans, err := admin.ListPlans(ctx, db)                    // List all plans
err = admin.DeletePlan(ctx, db, "old_plan")              // Delete unused plan

// Quota Block Management
err = admin.CreateQuotaBlock(ctx, db, "account-123", 1000, expiresAt, metadata)  // Create quota block
err = admin.DeleteQuotaBlock(ctx, db, "block-456")                               // Delete quota block
err = admin.ExpireQuotaBlocks(ctx, db, "account-123")                           // Expire all blocks
```

### Default Plan Management

The system maintains exactly one default plan at all times:
- Use `admin.SetDefaultPlan` to change the default plan
- The default plan cannot be deleted while it's set as default
- `admin.GetDefaultPlan` always returns the current default plan
- Accounts without a specified plan get the default plan automatically

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

## Rate Limit Information

Applications can retrieve information about their current rate limits using the `GetRateLimitInfo` method:

```go
info, err := client.GetRateLimitInfo(ctx, accountID, "api/v1/chat")
if err != nil {
    log.Printf("Error getting rate limit info: %v", err)
    return
}

for unitType, window := range info {
    fmt.Printf("%s: %d/%d (resets in %v)\n", 
        unitType, window.Remaining, window.Limit, window.Reset)
}
```

The rate limit information includes:
- For each unit type (e.g., "request", "token", "gpu-second"):
  - Limit: maximum units allowed in the window
  - Remaining: units remaining in the window
  - Reset: time until the window resets
- If concurrency limits are enabled, includes a "concurrent" entry with:
  - Limit: maximum concurrent requests
  - Remaining: remaining concurrent request slots
  - Reset: time until request is no longer counted as concurrent

This information is also could be included in the HTTP response headers like OpenAI does, if you want to be helpful to your API-consumers.
