package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"ratelimiter/store"

	"github.com/gin-gonic/gin"
)

// TokenBucket represents the state of a rate limiter for a specific key
type TokenBucket struct {
	Tokens     int64     `json:"tokens"`
	LastRefill time.Time `json:"last_refill"`
}

type RateLimiter struct {
	store       *store.Store
	capacity    int64
	refillRate  float64 // tokens per second
}

func NewRateLimiter(bindAddr string, knownNodes []string, capacity int64, refillRate float64) (*RateLimiter, error) {
	s, err := store.NewStore(bindAddr, knownNodes, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create store: %v", err)
	}

	return &RateLimiter{
		store:      s,
		capacity:   capacity,
		refillRate: refillRate,
	}, nil
}

func (rl *RateLimiter) Allow(ctx context.Context, key string, tokens int64) (bool, error) {
	now := time.Now()
	var bucket TokenBucket

	// Get current bucket state
	value, exists := rl.store.Get(key)
	if !exists {
		bucket = TokenBucket{
			Tokens:     rl.capacity,
			LastRefill: now,
		}
	} else {
		if err := json.Unmarshal(value, &bucket); err != nil {
			return false, fmt.Errorf("failed to unmarshal bucket: %v", err)
		}

		// Calculate token refill
		elapsed := now.Sub(bucket.LastRefill).Seconds()
		tokensToAdd := int64(elapsed * rl.refillRate)
		bucket.Tokens = min(rl.capacity, bucket.Tokens+tokensToAdd)
		bucket.LastRefill = now
	}

	// Check if we have enough tokens
	if bucket.Tokens < tokens {
		return false, nil
	}

	// Consume tokens
	bucket.Tokens -= tokens

	// Store updated bucket
	bucketBytes, err := json.Marshal(bucket)
	if err != nil {
		return false, fmt.Errorf("failed to marshal bucket: %v", err)
	}

	if err := rl.store.Set(key, bucketBytes); err != nil {
		return false, fmt.Errorf("failed to update bucket: %v", err)
	}

	return true, nil
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func main() {
	bindAddr := os.Getenv("BIND_ADDR")
	if bindAddr == "" {
		bindAddr = "0.0.0.0:7946" // Default memberlist port
	}

	knownNodes := []string{}
	if peers := os.Getenv("KNOWN_PEERS"); peers != "" {
		knownNodes = []string{peers}
	}

	// Create rate limiter with 100 tokens capacity, refilling at 10 tokens per second
	limiter, err := NewRateLimiter(bindAddr, knownNodes, 100, 10)
	if err != nil {
		log.Fatalf("Failed to create rate limiter: %v", err)
	}

	r := gin.Default()

	r.POST("/consume", func(c *gin.Context) {
		key := c.Query("key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key parameter is required"})
			return
		}

		tokens, err := strconv.ParseInt(c.DefaultQuery("tokens", "1"), 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tokens value"})
			return
		}

		allowed, err := limiter.Allow(c.Request.Context(), key, tokens)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		if !allowed {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if err := r.Run(":" + port); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
} 