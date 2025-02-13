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
	"github.com/google/uuid"
	"github.com/vmihailenco/msgpack/v5"
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
					Windows: []limiters.WindowLimit{
						{
							Window:   time.Minute,
							Limit:    10,
							UnitType: "request",
						},
						{
							Window:   time.Hour,
							Limit:    1000,
							UnitType: "token",
						},
						{
							Window:   time.Hour,
							Limit:    100,
							UnitType: "gpu-second",
						},
					},
					MaxConcurrent:     4,
					ConcurrencyExpiry: time.Minute,
				},
			},
		},
		"premium_tier": {
			Name: "Premium Tier",
			Endpoints: map[string]limiters.Policy{
				"text2Media": {
					Windows: []limiters.WindowLimit{
						{
							Window:   time.Hour,
							Limit:    5000,
							UnitType: "token",
						},
						{
							Window:   time.Hour,
							Limit:    500,
							UnitType: "gpu-second",
						},
					},
					MaxConcurrent:     8,
					ConcurrencyExpiry: time.Minute,
				},
			},
		},
	}

	// Apply plans
	if err := client.SetPlan(ctx, "free_tier", "Free Tier", plans["free_tier"].Endpoints); err != nil {
		log.Fatalf("setting free tier plan: %v", err)
	}
	if err := client.SetPlan(ctx, "premium_tier", "Premium Tier", plans["premium_tier"].Endpoints); err != nil {
		log.Fatalf("setting premium tier plan: %v", err)
	}

	// Set free tier as default plan
	if err := client.SetDefaultPlan(ctx, "free_tier"); err != nil {
		log.Fatalf("unable to set default plan: %v", err)
	}

	// Create test users with their plans and initial quota blocks
	users := []struct {
		id       string
		planID   string
		credits  int64
	}{
		{uuid.New().String(), "free_tier", 1000},
		{uuid.New().String(), "free_tier", 1000},
		{uuid.New().String(), "premium_tier", 5000},
		{uuid.New().String(), "premium_tier", 5000},
	}

	// Print user IDs for reference
	for _, user := range users {
		log.Printf("User %s: %s plan", user.id, user.planID)
	}

	// Create accounts and assign plans
	for _, user := range users {
		// Create account with plan
		if err := client.CreateAccount(ctx, user.id, user.planID); err != nil {
			log.Fatalf("creating account for %s: %v", user.id, err)
		}

		// Expire any existing quota blocks
		if err := client.ExpireQuotaBlocks(ctx, user.id); err != nil {
			log.Fatalf("expiring old quota blocks for %s: %v", user.id, err)
		}

		// Create multiple random quota blocks
		totalCredits := user.credits
		numBlocks := rand.Intn(3) + 2 // 2-4 blocks
		for i := 0; i < numBlocks; i++ {
			var credits int64
			if i == numBlocks-1 {
				// Last block gets remaining credits
				credits = totalCredits
			} else {
				// Random portion of remaining credits
				maxCredits := totalCredits * 2 / 3 // At most 2/3 of remaining
				if maxCredits == 0 {
					continue
				}
				credits = rand.Int63n(maxCredits) + 1
				totalCredits -= credits
			}

			// Random expiration between 1 day and 60 days
			expiresAt := time.Now().Add(time.Duration(rand.Intn(59)+1) * 24 * time.Hour)
			
			// 20% chance of no expiration
			if rand.Float32() < 0.2 {
				expiresAt = time.Time{} // Zero time means no expiration
			}

			metadata, err := msgpack.Marshal(map[string]interface{}{
				"block_number": i + 1,
				"total_blocks": numBlocks,
			})
			if err != nil {
				log.Fatalf("marshaling metadata for %s: %v", user.id, err)
			}

			if err := client.CreateQuotaBlock(ctx, user.id, credits, expiresAt, metadata); err != nil {
				log.Fatalf("creating quota block for %s: %v", user.id, err)
			}
		}
	}

	// Run concurrent requests
	var wg sync.WaitGroup
	start := time.Now()

	// Launch 3 goroutines per user (reduced from 10)
	for _, user := range users {
		for i := 0; i < 3; i++ {
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

						// Simulate different request sizes
						tokens := int64(rand.Intn(100) + 1)     // 1-100 tokens
						gpuSeconds := int64(rand.Intn(10) + 1)  // 1-10 GPU seconds
						credits := int64(rand.Intn(5) + 1)      // 1-5 credits

						// Check if user has enough credits before attempting request
						hasBalance, err := client.HasMinBalance(ctx, userID, credits)
						if err != nil {
							log.Printf("User %s: error checking balance - %v", userID, err)
							continue
						}
						if !hasBalance {
							log.Printf("User %s: insufficient balance (need %d)", userID, credits)
							time.Sleep(time.Second) // Back off before retrying
							continue
						}

						// Attempt request with token consumption
						allowed, info, err := client.AttemptRequest(ctx, requestID, userID, "text2Media",
							ratelimiter.DeductUnits("token", tokens),
							ratelimiter.DeductUnits("gpu-second", gpuSeconds),
						)
						if err != nil {
							log.Printf("User %s: error - %v", userID, err)
							continue
						}

						if allowed {
							log.Printf("User %s: request allowed", userID)
							for unitType, window := range info {
								log.Printf("  %s: %d/%d (reset in %v)", 
									unitType, window.Remaining, window.Limit, window.Reset)
							}

							// Simulate some work
							time.Sleep(time.Duration(rand.Int63n(100)) * time.Millisecond)

							// End request with final consumption
							finalTokens := int64(rand.Intn(100) + 1)  // 1-100 additional tokens
							finalGPU := int64(rand.Intn(5) + 1)       // 1-5 additional GPU seconds
							
							if err := client.EndRequest(ctx, requestID,
								ratelimiter.DeductUnits("token", finalTokens),
								ratelimiter.DeductUnits("gpu-second", finalGPU),
							); err != nil {
								log.Printf("User %s: error ending request - %v", userID, err)
							}

							// Deduct final credits
							finalCredits := int64(rand.Intn(3) + 1)   // 1-3 additional credits
							result, err := client.DeductCredits(ctx, userID, finalCredits, ratelimiter.WithRequireFullAmount(true))
							if err != nil {
								log.Printf("User %s: error deducting credits - %v", userID, err)
							} else if result.Remaining > 0 {
								log.Printf("User %s: partial credit deduction %d/%d", 
									userID, result.Deducted, result.Requested)
							}
						} else {
							log.Printf("User %s: request denied", userID)
							for unitType, window := range info {
								log.Printf("  %s: %d/%d (reset in %v)",
									unitType, window.Remaining, window.Limit, window.Reset)
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

	log.Printf("Test completed in %v", elapsed)
}

func main() {
	log.Println("Starting rate limiter stress test...")

	RunStressTest()

	log.Println("Stress test complete!")
}
