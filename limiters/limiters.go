package limiters

import (
	"context"
	"fmt"
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
	const maxRetries = 3
	const retryDelay = 10 * time.Millisecond

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
		case <-time.After(retryDelay):
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
	// Check concurrency first as it's the simplest
	if policy.MaxConcurrent > 0 {
		allowed, err := r.checkConcurrency(ctx, key, policy.MaxConcurrent)
		if err != nil {
			return false, err
		}
		if !allowed {
			return false, nil
		}
	}

	// Check sliding windows
	for i, window := range policy.SlidingWindows {
		increment := int64(1)
		if window.UnitKind == TokenUnit {
			increment = tokens
		}
		allowed, err := r.checkSlidingWindow(ctx, fmt.Sprintf("%s:sliding:%d", key, i), window, increment)
		if err != nil {
			return false, err
		}
		if !allowed {
			return false, nil
		}
	}

	// Check fixed windows
	for i, window := range policy.FixedWindows {
		increment := int64(1)
		if window.UnitKind == TokenUnit {
			increment = tokens
		}
		allowed, err := r.checkFixedWindow(ctx, fmt.Sprintf("%s:fixed:%d", key, i), window, increment)
		if err != nil {
			return false, err
		}
		if !allowed {
			return false, nil
		}
	}

	// Check token buckets
	for i, bucket := range policy.TokenBuckets {
		increment := int64(1)
		if bucket.UnitKind == TokenUnit {
			increment = tokens
		}
		allowed, err := r.checkTokenBucket(ctx, fmt.Sprintf("%s:bucket:%d", key, i), bucket, increment)
		if err != nil {
			return false, err
		}
		if !allowed {
			return false, nil
		}
	}

	return true, nil
}

// checkSlidingWindow implements a sliding window rate limiter
func (r *RateLimiter) checkSlidingWindow(ctx context.Context, key string, window WindowLimit, increment int64) (bool, error) {
	now := time.Now().UnixNano()
	windowStart := now - window.Window.Nanoseconds()

	// Try to acquire lock
	locked, err := r.tryLock(ctx, key, 100*time.Millisecond)
	if err != nil {
		return false, err
	}
	if !locked {
		return false, fmt.Errorf("could not acquire lock")
	}
	defer r.releaseLock(ctx, key)

	// Remove old entries
	err = r.rdb.ZRemRangeByScore(ctx, key, "0", fmt.Sprintf("%d", windowStart)).Err()
	if err != nil {
		return false, fmt.Errorf("removing old entries: %w", err)
	}

	// Count current window
	var total int64
	if window.UnitKind == TokenUnit {
		// For token-based limits, sum the scores (token counts)
		scores, err := r.rdb.ZRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
			Min: fmt.Sprintf("%d", windowStart),
			Max: fmt.Sprintf("%d", now),
		}).Result()
		if err != nil {
			return false, fmt.Errorf("counting tokens: %w", err)
		}
		for _, z := range scores {
			total += int64(z.Score)
		}
	} else {
		// For request-based limits, count the entries
		count, err := r.rdb.ZCount(ctx, key, fmt.Sprintf("%d", windowStart), fmt.Sprintf("%d", now)).Result()
		if err != nil {
			return false, fmt.Errorf("counting requests: %w", err)
		}
		total = count
	}

	if total+increment > window.Limit {
		return false, nil
	}

	// Add new request/tokens
	err = r.rdb.ZAdd(ctx, key, redis.Z{Score: float64(increment), Member: now}).Err()
	if err != nil {
		return false, fmt.Errorf("adding new entry: %w", err)
	}

	// Set expiry on the key
	err = r.rdb.Expire(ctx, key, window.Window).Err()
	if err != nil {
		return false, fmt.Errorf("setting expiry: %w", err)
	}

	return true, nil
}

// checkFixedWindow implements a fixed window rate limiter
func (r *RateLimiter) checkFixedWindow(ctx context.Context, key string, window WindowLimit, increment int64) (bool, error) {
	// Get current window
	windowKey := fmt.Sprintf("%s:%d", key, time.Now().Unix()/int64(window.Window.Seconds()))

	// Try to acquire lock
	locked, err := r.tryLock(ctx, windowKey, 100*time.Millisecond)
	if err != nil {
		return false, err
	}
	if !locked {
		return false, fmt.Errorf("could not acquire lock")
	}
	defer r.releaseLock(ctx, windowKey)

	// Get current count
	count, err := r.rdb.Get(ctx, windowKey).Int64()
	if err == redis.Nil {
		count = 0
	} else if err != nil {
		return false, fmt.Errorf("getting count: %w", err)
	}

	if count+increment > window.Limit {
		return false, nil
	}

	// Increment by the appropriate amount
	err = r.rdb.IncrBy(ctx, windowKey, increment).Err()
	if err != nil {
		return false, fmt.Errorf("incrementing counter: %w", err)
	}

	if count == 0 {
		// Set expiry for new windows
		err = r.rdb.Expire(ctx, windowKey, window.Window).Err()
		if err != nil {
			return false, fmt.Errorf("setting expiry: %w", err)
		}
	}

	return true, nil
}

// checkTokenBucket implements a token bucket rate limiter
func (r *RateLimiter) checkTokenBucket(ctx context.Context, key string, bucket TokenBucketLimit, increment int64) (bool, error) {
	now := time.Now().Unix()
	lastUpdateKey := key + ":last_update"

	// Try to acquire lock
	locked, err := r.tryLock(ctx, key, 100*time.Millisecond)
	if err != nil {
		return false, err
	}
	if !locked {
		return false, fmt.Errorf("could not acquire lock")
	}
	defer r.releaseLock(ctx, key)

	// Get current tokens and last update time
	tokens, err := r.rdb.Get(ctx, key).Int64()
	if err == redis.Nil {
		tokens = bucket.BucketSize
	} else if err != nil {
		return false, fmt.Errorf("getting tokens: %w", err)
	}

	lastUpdate, err := r.rdb.Get(ctx, lastUpdateKey).Int64()
	if err == redis.Nil {
		lastUpdate = now
	} else if err != nil {
		return false, fmt.Errorf("getting last update: %w", err)
	}

	// Calculate token refill
	elapsed := now - lastUpdate
	tokens = min(bucket.BucketSize, tokens+int64(float64(elapsed)*bucket.RefillRate))

	if tokens < increment {
		return false, nil
	}

	// Update state
	tokens -= increment
	if err := r.rdb.Set(ctx, key, tokens, 24*time.Hour).Err(); err != nil {
		return false, fmt.Errorf("updating tokens: %w", err)
	}
	if err := r.rdb.Set(ctx, lastUpdateKey, now, 24*time.Hour).Err(); err != nil {
		return false, fmt.Errorf("updating last update: %w", err)
	}

	return true, nil
}

// checkConcurrency implements a concurrent requests limiter
func (r *RateLimiter) checkConcurrency(ctx context.Context, key string, limit int64) (bool, error) {
	key = key + ":concurrent"

	// Try to acquire lock
	locked, err := r.tryLock(ctx, key, 100*time.Millisecond)
	if err != nil {
		return false, err
	}
	if !locked {
		return false, fmt.Errorf("could not acquire lock")
	}
	defer r.releaseLock(ctx, key)

	count, err := r.rdb.Get(ctx, key).Int64()
	if err == redis.Nil {
		count = 0
	} else if err != nil {
		return false, fmt.Errorf("getting count: %w", err)
	}

	if count >= limit {
		return false, nil
	}

	if err := r.rdb.Incr(ctx, key).Err(); err != nil {
		return false, fmt.Errorf("incrementing count: %w", err)
	}

	return true, nil
}

// DecrementConcurrency decrements the concurrent requests counter
func (r *RateLimiter) DecrementConcurrency(ctx context.Context, key string) error {
	key = key + ":concurrent"
	return r.rdb.Decr(ctx, key).Err()
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
