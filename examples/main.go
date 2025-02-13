package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/cozy-creator/ratelimiter"
	"github.com/cozy-creator/ratelimiter/admin"
	"github.com/cozy-creator/ratelimiter/limiters"
	"github.com/cozy-creator/ratelimiter/models"
)

func RunStressTest() {
	// Create rate limiter client with default configuration
	client, err := ratelimiter.NewClient(
		ratelimiter.WithPostgresDSN("postgres://postgres:postgres@localhost:5432/ratelimiter?sslmode=disable"),
		ratelimiter.WithRedisAddr("localhost:6379"),
	)
	if err != nil {
		log.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	ctx := context.Background()

	// Define plans
	plans := admin.Plans{
		"free_tier": {
			Name: "Free Tier",
			Endpoints: map[string]limiters.Policy{
				"text2Media": {
					FixedWindows: []limiters.WindowLimit{
						{
							Window:   time.Minute,
							Limit:    10,
							UnitKind: limiters.RequestUnit,
						},
						{
							Window:   3 * time.Hour,
							Limit:    200,
							UnitKind: limiters.RequestUnit,
						},
					},
					TokenBuckets: []limiters.TokenBucketLimit{
						{
							BucketSize: 100,
							RefillRate: 1.0, // 1 token per second
							UnitKind:   limiters.TokenUnit,
						},
					},
					MaxConcurrent: 4,
				},
			},
		},
		"premium_tier": {
			Name: "Premium Tier",
			Endpoints: map[string]limiters.Policy{
				"text2Media": {
					TokenBuckets: []limiters.TokenBucketLimit{
						{
							BucketSize: 1000,
							RefillRate: 10.0, // 10 tokens per second
							UnitKind:   limiters.TokenUnit,
						},
					},
					MaxConcurrent: 8,
				},
			},
		},
	}

	// Apply plans
	if err := admin.ApplyPlans(ctx, client.DB(), plans); err != nil {
		log.Fatalf("applying plans: %v", err)
	}

	// Set free tier as default plan
	if err := admin.SetDefaultPlan(ctx, client.DB(), "free_tier"); err != nil {
		log.Fatalf("unable to set default plan: %v", err)
	}

	// Create test users with their plans and initial quota blocks
	users := []struct {
		id       string
		planID   string
		credits  int64
	}{
		{"user1", "free_tier", 1000},
		{"user2", "free_tier", 1000},
		{"user3", "premium_tier", 5000},
		{"user4", "premium_tier", 5000},
	}

	// Create accounts and assign plans
	for _, user := range users {
		// Create/update account with upsert
		account := &models.Account{
			ID:     user.id,
			PlanID: user.planID,
		}
		_, err := client.DB().NewInsert().
			Model(account).
			On("CONFLICT (id) DO UPDATE").
			Set("plan_id = EXCLUDED.plan_id").
			Set("updated_at = ?", time.Now()).
			Exec(ctx)
		if err != nil {
			log.Fatalf("upserting account for %s: %v", user.id, err)
		}

		// First, expire any existing quota blocks for this account
		_, err = client.DB().NewUpdate().
			Model((*models.QuotaBlock)(nil)).
			Set("expires_at = ?", time.Now()).
			Where("account_id = ? AND (expires_at IS NULL OR expires_at > ?)", user.id, time.Now()).
			Exec(ctx)
		if err != nil {
			log.Fatalf("expiring old quota blocks for %s: %v", user.id, err)
		}

		// Create new quota block
		quotaBlock := &models.QuotaBlock{
			AccountID: user.id,
			Credits:   user.credits,
			ExpiresAt: time.Now().Add(30 * 24 * time.Hour), // Expires in 30 days
		}
		_, err = client.DB().NewInsert().
			Model(quotaBlock).
			Exec(ctx)
		if err != nil {
			log.Fatalf("creating quota block for %s: %v", user.id, err)
		}
	}

	// Run concurrent requests
	var wg sync.WaitGroup
	start := time.Now()

	// Launch 10 goroutines per user
	for _, user := range users {
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(userID string, routineID int) {
				defer wg.Done()

				// Each goroutine makes requests for 1 minute
				timeout := time.After(1 * time.Minute)
				for {
					select {
					case <-timeout:
						return
					default:
						// Generate unique request ID
						requestID := fmt.Sprintf("%s-%d-%d", userID, routineID, time.Now().UnixNano())

						// Attempt request with both token and credit consumption
						allowed, err := client.AttemptRequest(ctx, requestID, userID, "text2Media",
							ratelimiter.DeductTokens(5),
							ratelimiter.DeductCredits(2), // Consume 2 credits per request
						)
						if err != nil {
							log.Printf("Error attempting request for %s: %v", userID, err)
							continue
						}

						if allowed {
							log.Printf("Request allowed for %s (routine %d)", userID, routineID)
							// Simulate some work
							time.Sleep(time.Duration(rand.Int63n(30_000)) * time.Millisecond)

							// End request with additional consumption
							result, err := client.EndRequest(ctx, requestID,
								ratelimiter.DeductTokens(3),  // Additional tokens based on response size
								ratelimiter.DeductCredits(1), // Additional credit for processing
							)
							if err != nil {
								log.Printf("Error ending request for %s: %v", userID, err)
							}
							if result != nil && result.RemainingDue > 0 {
								log.Printf("Warning: Could only consume %d of %d requested credits for %s",
									result.Consumed, result.Requested, userID)
							}
						} else {
							// Get rate limit info to understand why the request was denied
							info, err := client.GetRateLimitInfo(ctx, userID, "text2Media")
							if err != nil {
								log.Printf("Request denied for %s (routine %d) - Error getting rate limit info: %v", 
									userID, routineID, err)
							} else {
								log.Printf("Request denied for %s (routine %d) - Limits: [Requests: %d/%d (resets in %v)] [Tokens: %d/%d (resets in %v)]",
									userID, routineID,
									info.RequestRemaining, info.RequestLimit, info.RequestReset,
									info.TokenRemaining, info.TokenLimit, info.TokenReset)
							}
							// Back off a bit before retrying
							time.Sleep(time.Second)
						}
					}
				}
			}(user.id, i)
		}
	}

	// Wait for all goroutines to finish
	wg.Wait()
	elapsed := time.Since(start)

	log.Printf("Stress test completed in %v", elapsed)
}

func main() {
	log.Println("Starting rate limiter stress test...")
	RunStressTest()
	log.Println("Stress test complete!")
}
