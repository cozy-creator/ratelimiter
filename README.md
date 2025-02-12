### General Idea

- Fetch all plan information
- Fetch the policy for a given user

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