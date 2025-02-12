package main

import (
	"context"
	"fmt"
	"os"
	"time"

	limiter "github.com/cozy-creator/rate-limiter"
	"github.com/cozy-creator/rate-limiter/policy"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

func constructPolicy() policy.Policy {
	policy := policy.Policy{
		FixedWindowRules: []policy.FixedWindowRule{
			{
				UnitKind: policy.Request,
				Limit:    100,
				Window:   1 * time.Hour,
			},
		},
		TokenBucketRules: []policy.TokenBucketRule{
			{
				UnitKind:    policy.Token,
				Capacity:    10,
				RefillRate:  2,
				RefillEvery: 30 * time.Second,
			},
		},
		TrackUsage:    false,
	}

	return policy
}

func policyFromYAML() (*policy.Policy, error) {
	data, err := os.ReadFile("policy.yaml")
	if err != nil {
		return nil, err
	}

	var p policy.Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, err
	}

	return &p, nil
}

// Limitd to 10 requests per minute, or 100 per 3 hours, with a max concurrency of 5
func doujinsPolicy() *policy.Policy {
	policy := policy.Policy{
		SlidingWindowRules: []policy.SlidingWindowRule{
			{
				UnitKind: policy.Request,
				Limit:    10,
				Window:   1 * time.Minute,
			},
			{
				UnitKind: policy.Request,
				Limit:    100,
				Window:   3 * time.Hour,
			},
		},
		MaxConcurrency: 5,
		LockExpiry: 2 * time.Minute,
	}

	return &policy
}

// Limits users to 5 requests per day, with an initial start of 10. Max concurrency 4
func freeUserVoidTech() *policy.Policy {
	policy := policy.Policy{
		TokenBucketRules: []policy.TokenBucketRule{
			{
				UnitKind: policy.Request,
				Capacity: 10,
				RefillEvery: 24 * time.Hour,
				RefillRate: 5,
			},
		},
		MaxConcurrency: 4,
		LockExpiry: 2 * time.Minute,
	}

	return &policy
}

func doujinsConsumption() {
	customerID := "abc123"
	endpointID := "text2Media"
	requestID := uuid.New().String()

	client, err := limiter.NewClient(
		limiter.WithDefaultPolicies(map[string]interface{}{
			"/": "free_tier",
		}),
	)
	if err != nil {
		panic(fmt.Sprintf("failed to create client: %v", err))
	}

	ctx := context.Background()

	// Creates or overwrites the premium_tier_1 policy
	client.SavePolicy(ctx, "premium_tier_1", doujinsPolicy())

	// Change the user's policy to tier-1
	err = client.AssignCustomerPolicy(ctx, customerID, endpointID, "premium_tier_1")

	// $0.04 for a call to flux-pro. We measure in thousands of a penny
	fluxProAPICost := int64(4000)

	// Reserves flux pro api costs from credits, and registers 1 request
	// This will be stored in memory
	// This registers a unique request, regardless of whether or not it fails or succeeds
	allow, err := client.Acquire(ctx, customerID, endpointID, requestID, limiter.ReserveCredits(fluxProAPICost))
	if !allow {
		// examine the error
	}
	defer client.Cancel(ctx, requestID) // in case something goes wrong

	// proceed with generation...

	// Releases the concurrency lock and takes the credits
	client.Release(ctx, requestID, limiter.DeductCredits(fluxProAPICost))
}

// After the fact billing
func openAIConsumption() {
	customerID := "abc123"
	endpointID := "text2Media"
	requestID := uuid.New().String()

	client, err := limiter.NewClient(
		limiter.WithDefaultPolicies(map[string]interface{}{
			"/": "free_tier",
		}),
	)
	if err != nil {
		panic(fmt.Sprintf("failed to create client: %v", err))
	}


	ctx := context.Background()

	// This puts some credits on reserve, to ensure you'll be able to pay for the request.
	// We may not know the exact cost upfront.
	// It will also deduct + log one request from the rate limiter (if that matters)
	allow, err := client.Acquire(ctx, customerID, endpointID, requestID, limiter.ReserveCredits(500))
	if !allow {
	}
	// Releases concurrency locks and reservations
	defer client.Cancel(ctx, requestID)

	// do generation
	// The amount of usage is not known until afterwards in this case
	tokensUsed := int64(50_000)
	creditsUsed := int64(tokensUsed / 200)

	// Perhaps credit balances should be universal per user, and not per endpoint?
	// Ideally we need a way to link customerID + endpoint -> credit account
	// we do kind of also want request ids for idempotency perhaps
	// Releases the concurrency lock and reservations, and records the actual values
	err = client.Release(ctx, requestID, limiter.DeductTokens(tokensUsed), limiter.DeductCredits(creditsUsed))
}

func main() {
	// TO DO
}
