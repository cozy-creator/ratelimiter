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

	// Reserved unit types that cannot be used in policies
	ReservedCreditUnit = "credit"
	DefaultRequestUnit = "request" // Default unit type if none specified
)

// UnitKind represents what we're counting (requests or tokens)
type UnitKind string

const (
	RequestUnit UnitKind = "request"
	TokenUnit  UnitKind = "token"
)

// WindowLimit represents a time-based limit for a specific unit type
type WindowLimit struct {
	Window    time.Duration `json:"window"`     // Time window
	Limit     int64        `json:"limit"`      // Maximum units in window
	UnitType  string       `json:"unit_type"`  // Type of unit being limited (e.g., "request", "token", "image"). Defaults to "request" if empty
}

// Validate checks if the window limit configuration is valid
func (w *WindowLimit) Validate() error {
	if w.Window <= 0 {
		return fmt.Errorf("window duration must be positive")
	}
	if w.Limit <= 0 {
		return fmt.Errorf("limit must be positive")
	}
	if w.UnitType == ReservedCreditUnit {
		return fmt.Errorf("'%s' is a reserved unit type that cannot be used in policies", ReservedCreditUnit)
	}
	return nil
}

// GetUnitType returns the unit type, defaulting to "request" if not specified
func (w *WindowLimit) GetUnitType() string {
	if w.UnitType == "" {
		return DefaultRequestUnit
	}
	return w.UnitType
}

// TokenBucketLimit represents a token bucket configuration
type TokenBucketLimit struct {
	BucketSize int64   `json:"bucket_size"` // Maximum tokens
	RefillRate float64 `json:"refill_rate"` // Tokens per second
	UnitKind   UnitKind `json:"unit_kind"`  // Whether to count requests or tokens
}

// Policy represents a rate limiting policy for an endpoint
type Policy struct {
	Windows            []WindowLimit  `json:"windows,omitempty"`
	MaxConcurrent      int64         `json:"max_concurrent,omitempty"`      // Maximum concurrent requests
	ConcurrencyExpiry  time.Duration `json:"concurrency_expiry,omitempty"` // How long to track a request for concurrency
}

// Validate checks if the policy configuration is valid
func (p *Policy) Validate() error {
	for _, window := range p.Windows {
		if err := window.Validate(); err != nil {
			return fmt.Errorf("invalid window configuration: %w", err)
		}
	}
	if p.MaxConcurrent < 0 {
		return fmt.Errorf("max concurrent must be non-negative")
	}
	return nil
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

// CheckAndIncrement checks if a request can proceed and increments counters if allowed.
// Returns whether the request is allowed, rate limit info, and any error.
func (r *RateLimiter) CheckAndIncrement(ctx context.Context, key string, policy Policy, unitAmounts map[string]int64, requestID string) (bool, map[string]*WindowInfo, error) {
	// Validate policy first
	if err := policy.Validate(); err != nil {
		return false, nil, fmt.Errorf("invalid policy: %w", err)
	}

	// Try to acquire lock for the entire operation
	locked, err := r.tryLock(ctx, key, 100*time.Millisecond)
	if err != nil {
		return false, nil, err
	}
	if !locked {
		return false, nil, fmt.Errorf("could not acquire lock")
	}
	defer r.releaseLock(ctx, key)

	now := time.Now()
	info := make(map[string]*WindowInfo)

	// Group windows by unit type to track most constraining for each
	windowsByUnit := make(map[string]*WindowInfo)

	// Check all windows and gather rate limit info in one pass
	for i, window := range policy.Windows {
		windowKey := fmt.Sprintf("%s:window:%d:%d", key, i, now.Unix()/int64(window.Window.Seconds()))
		
		// Get current usage
		count, err := r.rdb.Get(ctx, windowKey).Int64()
		if err == redis.Nil {
			count = 0
		} else if err != nil {
			return false, nil, fmt.Errorf("getting window count: %w", err)
		}

		// Get TTL
		ttl, err := r.rdb.TTL(ctx, windowKey).Result()
		if err != nil && err != redis.Nil {
			return false, nil, fmt.Errorf("getting ttl: %w", err)
		}
		if ttl <= 0 {
			// Calculate time until next window boundary
			windowSeconds := int64(window.Window.Seconds())
			currentWindowStart := (now.Unix() / windowSeconds) * windowSeconds
			nextWindowStart := currentWindowStart + windowSeconds
			ttl = time.Duration(nextWindowStart - now.Unix()) * time.Second
		}

		// Calculate remaining units
		unitType := window.GetUnitType()
		amount := unitAmounts[unitType]
		if amount == 0 && unitType == DefaultRequestUnit {
			amount = 1
		}

		// Check if this would exceed the limit
		if count+amount > window.Limit {
			// Create window info for the window that caused denial
			windowInfo := &WindowInfo{
				Limit:     window.Limit,
				Remaining: max(0, window.Limit - count),
				Reset:     ttl,
			}
			info[window.GetUnitType()] = windowInfo
			return false, info, nil
		}

		// Update most constraining window info for this unit type
		windowInfo := &WindowInfo{
			Limit:     window.Limit,
			Remaining: max(0, window.Limit - count),
			Reset:     ttl,
		}

		existing, ok := windowsByUnit[unitType]
		if !ok || (float64(windowInfo.Remaining)/float64(windowInfo.Limit)) < 
			(float64(existing.Remaining)/float64(existing.Limit)) {
			windowsByUnit[unitType] = windowInfo
		}
	}

	// Check concurrency if enabled
	if policy.MaxConcurrent > 0 {
		expiry := policy.ConcurrencyExpiry
		if expiry == 0 {
			expiry = time.Minute // Default expiry
		}

		count, err := r.rdb.ZCount(ctx,
			key+":concurrent",
			fmt.Sprintf("%d", now.Add(-expiry).Unix()),
			fmt.Sprintf("%d", now.Unix()),
		).Result()
		if err != nil && err != redis.Nil {
			return false, nil, fmt.Errorf("counting concurrent requests: %w", err)
		}

		info["concurrent"] = &WindowInfo{
			Limit:     policy.MaxConcurrent,
			Remaining: max(0, policy.MaxConcurrent - count),
			Reset:     expiry,
		}

		if count >= policy.MaxConcurrent {
			return false, info, nil
		}
	}

	// If we got here, all checks passed. Perform all increments atomically
	pipe := r.rdb.Pipeline()

	// Record concurrent request if enabled
	if policy.MaxConcurrent > 0 {
		expiry := policy.ConcurrencyExpiry
		if expiry == 0 {
			expiry = time.Minute
		}
		pipe.ZAdd(ctx, key+":concurrent", redis.Z{
			Score:  float64(now.Unix()),
			Member: requestID,
		})
		pipe.Expire(ctx, key+":concurrent", expiry)
	}

	// Update fixed windows
	for i, window := range policy.Windows {
		windowKey := fmt.Sprintf("%s:window:%d:%d", key, i, now.Unix()/int64(window.Window.Seconds()))
		amount := unitAmounts[window.GetUnitType()]
		if amount == 0 && window.GetUnitType() == DefaultRequestUnit {
			amount = 1
		}
		if amount > 0 {
			pipe.IncrBy(ctx, windowKey, amount)
			pipe.Expire(ctx, windowKey, calculateExpiration(window.Window))
		}
	}

	// Execute all updates atomically
	_, err = pipe.Exec(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("executing updates: %w", err)
	}

	// Return the most constraining window info for each unit type
	for unitType, windowInfo := range windowsByUnit {
		info[unitType] = windowInfo
	}

	// Update concurrency info if enabled
	if policy.MaxConcurrent > 0 {
		info["concurrent"].Remaining = max(0, info["concurrent"].Remaining - 1) // Subtract 1 for the request we just added
	}

	return true, info, nil
}

// gatherRemainingWindowInfo ensures we have window info for all unit types
// even if we're returning early due to a limit being exceeded
func (r *RateLimiter) gatherRemainingWindowInfo(existingInfo map[string]*WindowInfo) map[string]*WindowInfo {
	info := make(map[string]*WindowInfo)
	for unitType, windowInfo := range existingInfo {
		info[unitType] = windowInfo
	}
	return info
}

// ForceDeductUnits forcefully deducts units from the rate limiter,
// even if it would exceed limits. This is used for post-request accounting
// when resources have already been consumed.
func (r *RateLimiter) ForceDeductUnits(ctx context.Context, key string, policy Policy, unitAmounts map[string]int64) error {
	// Try to acquire lock for the operation
	locked, err := r.tryLock(ctx, key, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("could not acquire lock")
	}
	defer r.releaseLock(ctx, key)

	now := time.Now()
	pipe := r.rdb.Pipeline()

	// Update fixed windows
	for i, window := range policy.Windows {
		windowKey := fmt.Sprintf("%s:window:%d:%d", key, i, now.Unix()/int64(window.Window.Seconds()))
		amount := unitAmounts[window.GetUnitType()]
		if amount > 0 {
			pipe.IncrBy(ctx, windowKey, amount)
			pipe.Expire(ctx, windowKey, calculateExpiration(window.Window))
		}
	}

	// Execute all updates atomically
	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("executing updates: %w", err)
	}

	return nil
}

// EndRequest handles cleanup after a request ends
func (r *RateLimiter) EndRequest(ctx context.Context, key string, policy Policy, requestID string, finalUnits map[string]int64) error {
	// First, try to deduct any final units
	if len(finalUnits) > 0 {
		if err := r.ForceDeductUnits(ctx, key, policy, finalUnits); err != nil {
			return fmt.Errorf("deducting final units: %w", err)
		}
	}

	// Then clean up the concurrent request tracking
	if policy.MaxConcurrent > 0 {
		err := r.rdb.ZRem(ctx, key+":concurrent", requestID).Err()
		if err != nil {
			return fmt.Errorf("removing concurrent request: %w", err)
		}
	}

	return nil
}

// GetRateLimitInfo returns the current rate limit information for a key and policy
func (r *RateLimiter) GetRateLimitInfo(ctx context.Context, key string, policy Policy) (map[string]*WindowInfo, error) {
	info := make(map[string]*WindowInfo)
	now := time.Now()

	// Get window limits
	for i, window := range policy.Windows {
		windowKey := fmt.Sprintf("%s:window:%d:%d", key, i, now.Unix()/int64(window.Window.Seconds()))
		
		count, err := r.rdb.Get(ctx, windowKey).Int64()
		if err == redis.Nil {
			count = 0
		} else if err != nil {
			return nil, fmt.Errorf("getting window count: %w", err)
		}

		ttl, err := r.rdb.TTL(ctx, windowKey).Result()
		if err != nil && err != redis.Nil {
			return nil, fmt.Errorf("getting ttl: %w", err)
		}
		if ttl <= 0 {
			// Calculate time until next window boundary
			windowSeconds := int64(window.Window.Seconds())
			currentWindowStart := (now.Unix() / windowSeconds) * windowSeconds
			nextWindowStart := currentWindowStart + windowSeconds
			ttl = time.Duration(nextWindowStart - now.Unix()) * time.Second
		}

		info[window.GetUnitType()] = &WindowInfo{
			Limit:     window.Limit,
			Remaining: max(0, window.Limit - count),
			Reset:     ttl,
		}
	}

	// Get concurrency info if enabled
	if policy.MaxConcurrent > 0 {
		expiry := policy.ConcurrencyExpiry
		if expiry == 0 {
			expiry = time.Minute
		}

		count, err := r.rdb.ZCount(ctx,
			key+":concurrent",
			fmt.Sprintf("%d", now.Add(-expiry).Unix()),
			fmt.Sprintf("%d", now.Unix()),
		).Result()
		if err != nil && err != redis.Nil {
			return nil, fmt.Errorf("counting concurrent requests: %w", err)
		}

		info["concurrent"] = &WindowInfo{
			Limit:     policy.MaxConcurrent,
			Remaining: max(0, policy.MaxConcurrent - count),
			Reset:     expiry,
		}
	}

	return info, nil
}

// WindowInfo represents the current state of a rate limit window
type WindowInfo struct {
	Limit     int64         `json:"limit"`      // Maximum units allowed
	Remaining int64         `json:"remaining"`   // Remaining units
	Reset     time.Duration `json:"reset"`      // Time until limit resets
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
