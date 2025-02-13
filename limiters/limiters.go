package limiters

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/redis/go-redis/v9"
)

// LimiterType represents the type of rate limiter
type LimiterType string

const (
	SlidingWindow LimiterType = "sliding_window"
	FixedWindow  LimiterType = "fixed_window"
	TokenBucket  LimiterType = "token_bucket"
	Concurrency  LimiterType = "concurrency"
)

// UnitKind represents what we're counting (requests or tokens)
type UnitKind string

const (
	RequestUnit UnitKind = "request"
	TokenUnit  UnitKind = "token"
)

// WindowLimit represents a time-based limit
type WindowLimit struct {
	Window   time.Duration `json:"window"`      // Time window
	Limit    int64        `json:"limit"`       // Maximum requests/tokens in window
	UnitKind UnitKind     `json:"unit_kind"`   // Whether to count requests or tokens
}

// TokenBucketLimit represents a token bucket configuration
type TokenBucketLimit struct {
	BucketSize int64   `json:"bucket_size"` // Maximum tokens
	RefillRate float64 `json:"refill_rate"` // Tokens per second
	UnitKind   UnitKind `json:"unit_kind"`  // Whether to count requests or tokens
}

// Policy represents a rate limiting policy for an endpoint
type Policy struct {
	SlidingWindows []WindowLimit      `json:"sliding_windows,omitempty"`
	FixedWindows   []WindowLimit      `json:"fixed_windows,omitempty"`
	TokenBuckets   []TokenBucketLimit `json:"token_buckets,omitempty"`
	MaxConcurrent  int64              `json:"max_concurrent,omitempty"`
}

// RateLimiter handles rate limiting logic
type RateLimiter struct {
	rdb *redis.Client
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(rdb *redis.Client) *RateLimiter {
	return &RateLimiter{rdb: rdb}
}

// tryLock attempts to acquire a lock with retries
func (r *RateLimiter) tryLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	const maxRetries = 5
	const retryDelay = 50 * time.Millisecond

	for i := 0; i < maxRetries; i++ {
		// Try to acquire lock
		acquired, err := r.rdb.SetNX(ctx, key+":lock", "1", ttl).Result()
		if err != nil {
			return false, fmt.Errorf("acquiring lock: %w", err)
		}
		if acquired {
			return true, nil
		}

		// Wait before retrying
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(retryDelay + time.Duration(rand.Int63n(int64(retryDelay)))): // Add jitter
		}
	}
	return false, nil
}

// releaseLock releases a previously acquired lock
func (r *RateLimiter) releaseLock(ctx context.Context, key string) error {
	return r.rdb.Del(ctx, key+":lock").Err()
}

// CheckAndIncrement checks if a request can proceed and increments counters if allowed
func (r *RateLimiter) CheckAndIncrement(ctx context.Context, key string, policy Policy, tokens int64) (bool, error) {
	// Try to acquire lock for the entire operation
	locked, err := r.tryLock(ctx, key, 100*time.Millisecond)
	if err != nil {
		return false, err
	}
	if !locked {
		return false, fmt.Errorf("could not acquire lock")
	}
	defer r.releaseLock(ctx, key)

	now := time.Now()
	nowNano := now.UnixNano()

	// First, check all limits without modifying anything
	
	// 1. Check concurrency
	if policy.MaxConcurrent > 0 {
		count, err := r.rdb.Get(ctx, key+":concurrent").Int64()
		if err != nil {
			if err == redis.Nil {
				count = 0 				// Key doesn't exist, treat as 0 concurrent requests
			} else {
				return false, fmt.Errorf("getting concurrent count: %w", err)
			}
		}
		if count >= policy.MaxConcurrent {
			return false, nil
		}
	}

	// 2. Check sliding windows
	for i, window := range policy.SlidingWindows {
		windowKey := fmt.Sprintf("%s:sliding:%d", key, i)
		windowStart := nowNano - window.Window.Nanoseconds()

		var total int64
		if window.UnitKind == TokenUnit {
			// For token-based limits, sum the scores
			scores, err := r.rdb.ZRangeByScoreWithScores(ctx, windowKey, &redis.ZRangeBy{
				Min: fmt.Sprintf("%d", windowStart),
				Max: fmt.Sprintf("%d", nowNano),
			}).Result()
			if err != nil && err != redis.Nil {
				return false, fmt.Errorf("counting tokens: %w", err)
			}
			for _, z := range scores {
				total += int64(z.Score)
			}
		} else {
			// For request-based limits, count entries
			count, err := r.rdb.ZCount(ctx, windowKey, 
				fmt.Sprintf("%d", windowStart), 
				fmt.Sprintf("%d", nowNano)).Result()
			if err != nil && err != redis.Nil {
				return false, fmt.Errorf("counting requests: %w", err)
			}
			total = count
		}

		increment := int64(1)
		if window.UnitKind == TokenUnit {
			increment = tokens
		}
		if total+increment > window.Limit {
			return false, nil
		}
	}

	// 3. Check fixed windows
	for i, window := range policy.FixedWindows {
		windowKey := fmt.Sprintf("%s:fixed:%d:%d", key, i, now.Unix()/int64(window.Window.Seconds()))
		count, err := r.rdb.Get(ctx, windowKey).Int64()
		if err == redis.Nil {
			count = 0
		} else if err != nil {
			return false, fmt.Errorf("getting fixed window count: %w", err)
		}

		increment := int64(1)
		if window.UnitKind == TokenUnit {
			increment = tokens
		}
		if count+increment > window.Limit {
			return false, nil
		}
	}

	// 4. Check token buckets
	for i, bucket := range policy.TokenBuckets {
		bucketKey := fmt.Sprintf("%s:bucket:%d", key, i)
		lastUpdateKey := bucketKey + ":last_update"

		// Get current tokens and last update time
		currentTokens, err := r.rdb.Get(ctx, bucketKey).Int64()
		if err == redis.Nil {
			currentTokens = bucket.BucketSize
		} else if err != nil {
			return false, fmt.Errorf("getting tokens: %w", err)
		}

		lastUpdate, err := r.rdb.Get(ctx, lastUpdateKey).Int64()
		if err == redis.Nil {
			lastUpdate = now.Unix()
		} else if err != nil {
			return false, fmt.Errorf("getting last update: %w", err)
		}

		// Calculate token refill
		elapsed := now.Unix() - lastUpdate
		currentTokens = min(bucket.BucketSize, currentTokens+int64(float64(elapsed)*bucket.RefillRate))

		increment := int64(1)
		if bucket.UnitKind == TokenUnit {
			increment = tokens
		}
		if currentTokens < increment {
			return false, nil
		}
	}

	// If all checks passed, now perform all increments atomically using a pipeline
	pipe := r.rdb.Pipeline()

	// 1. Increment concurrency
	if policy.MaxConcurrent > 0 {
		pipe.Incr(ctx, key+":concurrent")
		pipe.Expire(ctx, key+":concurrent", 10*time.Minute)
	}

	// 2. Update sliding windows
	for i, window := range policy.SlidingWindows {
		windowKey := fmt.Sprintf("%s:sliding:%d", key, i)
		increment := int64(1)
		if window.UnitKind == TokenUnit {
			increment = tokens
		}
		// Remove old entries and add new one
		pipe.ZRemRangeByScore(ctx, windowKey, "0", fmt.Sprintf("%d", nowNano-window.Window.Nanoseconds()))
		pipe.ZAdd(ctx, windowKey, redis.Z{Score: float64(increment), Member: nowNano})
		pipe.Expire(ctx, windowKey, calculateExpiration(window.Window))
	}

	// 3. Update fixed windows
	for i, window := range policy.FixedWindows {
		windowKey := fmt.Sprintf("%s:fixed:%d:%d", key, i, now.Unix()/int64(window.Window.Seconds()))
		increment := int64(1)
		if window.UnitKind == TokenUnit {
			increment = tokens
		}
		pipe.IncrBy(ctx, windowKey, increment)
		pipe.Expire(ctx, windowKey, calculateExpiration(window.Window))
	}

	// 4. Update token buckets
	for i, bucket := range policy.TokenBuckets {
		bucketKey := fmt.Sprintf("%s:bucket:%d", key, i)
		lastUpdateKey := bucketKey + ":last_update"
		increment := int64(1)
		if bucket.UnitKind == TokenUnit {
			increment = tokens
		}

		currentTokens, _ := r.rdb.Get(ctx, bucketKey).Int64() // We already checked this exists
		lastUpdate, _ := r.rdb.Get(ctx, lastUpdateKey).Int64()
		elapsed := now.Unix() - lastUpdate
		newTokens := min(bucket.BucketSize, currentTokens+int64(float64(elapsed)*bucket.RefillRate)) - increment

		pipe.Set(ctx, bucketKey, newTokens, 24*time.Hour)
		pipe.Set(ctx, lastUpdateKey, now.Unix(), 24*time.Hour)
	}

	// Execute all updates atomically
	_, err = pipe.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("executing updates: %w", err)
	}

	return true, nil
}

// DecrementConcurrency decrements the concurrent requests counter
func (r *RateLimiter) DecrementConcurrency(ctx context.Context, key string) error {
	key = key + ":concurrent"
	pipe := r.rdb.Pipeline()
	pipe.Decr(ctx, key)
	pipe.Expire(ctx, key, 10*time.Minute)
	_, err := pipe.Exec(ctx)
	return err
}

// ForceConsumeTokens consumes tokens from token-based limiters, allowing them to go negative
func (r *RateLimiter) ForceConsumeTokens(ctx context.Context, key string, policy Policy, tokens int64) error {
	// Try to acquire lock
	locked, err := r.tryLock(ctx, key, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("could not acquire lock")
	}
	defer r.releaseLock(ctx, key)

	// Update sliding windows
	for i, window := range policy.SlidingWindows {
		if window.UnitKind == TokenUnit {
			windowKey := fmt.Sprintf("%s:sliding:%d", key, i)
			now := time.Now().UnixNano()
			// Add tokens without checking limits
			err = r.rdb.ZAdd(ctx, windowKey, redis.Z{Score: float64(tokens), Member: now}).Err()
			if err != nil {
				return fmt.Errorf("updating sliding window: %w", err)
			}
		}
	}

	// Update fixed windows
	for i, window := range policy.FixedWindows {
		if window.UnitKind == TokenUnit {
			windowKey := fmt.Sprintf("%s:fixed:%d:%d", key, i, time.Now().Unix()/int64(window.Window.Seconds()))
			// Add tokens without checking limits
			err = r.rdb.IncrBy(ctx, windowKey, tokens).Err()
			if err != nil {
				return fmt.Errorf("updating fixed window: %w", err)
			}
		}
	}

	// Update token buckets
	for i, bucket := range policy.TokenBuckets {
		if bucket.UnitKind == TokenUnit {
			bucketKey := fmt.Sprintf("%s:bucket:%d", key, i)
			// Subtract tokens without checking limits
			err = r.rdb.DecrBy(ctx, bucketKey, tokens).Err()
			if err != nil {
				return fmt.Errorf("updating token bucket: %w", err)
			}
		}
	}

	return nil
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// GetRateLimitInfo returns the current rate limit information for a key and policy
func (r *RateLimiter) GetRateLimitInfo(ctx context.Context, key string, policy Policy) (*RateLimitInfo, error) {
	info := &RateLimitInfo{}

	// Get request-based limits
	for i, window := range policy.SlidingWindows {
		if window.UnitKind == RequestUnit {
			windowKey := fmt.Sprintf("%s:sliding:%d", key, i)
			now := time.Now().UnixNano()
			windowStart := now - window.Window.Nanoseconds()

			// Get current count
			count, err := r.rdb.ZCount(ctx, windowKey, 
				fmt.Sprintf("%d", windowStart), 
				fmt.Sprintf("%d", now)).Result()
			if err != nil && err != redis.Nil {
				return nil, fmt.Errorf("counting requests: %w", err)
			}

			// Update request info
			info.RequestLimit = window.Limit
			info.RequestRemaining = max(0, window.Limit - count)

			// Calculate reset time
			ttl, err := r.rdb.TTL(ctx, windowKey).Result()
			if err != nil && err != redis.Nil {
				return nil, fmt.Errorf("getting ttl: %w", err)
			}
			if ttl > 0 {
				info.RequestReset = ttl
			} else {
				info.RequestReset = window.Window
			}
			break // Use the first request window found
		}
	}

	// Get token-based limits
	for i, window := range policy.SlidingWindows {
		if window.UnitKind == TokenUnit {
			windowKey := fmt.Sprintf("%s:sliding:%d", key, i)
			now := time.Now().UnixNano()
			windowStart := now - window.Window.Nanoseconds()

			// Get current token count
			scores, err := r.rdb.ZRangeByScoreWithScores(ctx, windowKey, &redis.ZRangeBy{
				Min: fmt.Sprintf("%d", windowStart),
				Max: fmt.Sprintf("%d", now),
			}).Result()
			if err != nil && err != redis.Nil {
				return nil, fmt.Errorf("counting tokens: %w", err)
			}

			var total int64
			for _, z := range scores {
				total += int64(z.Score)
			}

			// Update token info
			info.TokenLimit = window.Limit
			info.TokenRemaining = max(0, window.Limit - total)

			// Calculate reset time
			ttl, err := r.rdb.TTL(ctx, windowKey).Result()
			if err != nil && err != redis.Nil {
				return nil, fmt.Errorf("getting ttl: %w", err)
			}
			if ttl > 0 {
				info.TokenReset = ttl
			} else {
				info.TokenReset = window.Window
			}
			break // Use the first token window found
		}
	}

	// If no sliding windows, check token buckets
	if info.TokenLimit == 0 && len(policy.TokenBuckets) > 0 {
		bucket := policy.TokenBuckets[0] // Use the first bucket
		bucketKey := fmt.Sprintf("%s:bucket:0", key)

		// Get current tokens
		tokens, err := r.rdb.Get(ctx, bucketKey).Int64()
		if err == redis.Nil {
			tokens = bucket.BucketSize
		} else if err != nil {
			return nil, fmt.Errorf("getting tokens: %w", err)
		}

		info.TokenLimit = bucket.BucketSize
		info.TokenRemaining = max(0, tokens)

		// For token buckets, reset time is based on refill rate
		if tokens < bucket.BucketSize && bucket.RefillRate > 0 {
			tokensNeeded := bucket.BucketSize - tokens
			info.TokenReset = time.Duration(float64(tokensNeeded) / bucket.RefillRate * float64(time.Second))
		}
	}

	return info, nil
}

// RateLimitInfo represents the current state of rate limits
type RateLimitInfo struct {
	// Request-based limits
	RequestLimit     int64         `json:"request_limit"`      // Maximum requests allowed
	RequestRemaining int64         `json:"request_remaining"`  // Remaining requests
	RequestReset     time.Duration `json:"request_reset"`      // Time until request limit resets

	// Token-based limits
	TokenLimit     int64         `json:"token_limit"`      // Maximum tokens allowed
	TokenRemaining int64         `json:"token_remaining"`  // Remaining tokens
	TokenReset     time.Duration `json:"token_reset"`      // Time until token limit resets
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Calculate safe expiration duration for a window
func calculateExpiration(window time.Duration) time.Duration {
	if window < time.Hour {
		return window + time.Minute // Add 1 minute buffer for short windows
	}
	// Add 5% buffer for longer windows
	return window + time.Duration(float64(window) * 0.05)
}
