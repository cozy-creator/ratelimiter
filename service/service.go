package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/cozy-creator/ratelimiter/limiters"
	"github.com/cozy-creator/ratelimiter/models"
	"github.com/puzpuzpuz/xsync/v3"
	"github.com/redis/go-redis/v9"
	"github.com/uptrace/bun"
	"github.com/vmihailenco/msgpack/v5"
)

// RequestInfo stores information about an active request
type RequestInfo struct {
	AccountID  string
	EndpointID string
	StartTime  time.Time
	Allowed    bool    // Whether the request was allowed
}

var ErrDuplicateRequest = fmt.Errorf("duplicate request ID")

// CachedPolicies represents cached endpoint policies with expiration
type CachedPolicies struct {
	Policies   EndpointPolicies
	ExpiresAt  time.Time
}

// Service handles the rate limiting logic
type Service struct {
	db             *bun.DB
	rdb            *redis.Client
	limiter        *limiters.RateLimiter
	planCache      *xsync.MapOf[string, *CachedPolicies] // Cache for plan policies
	activeRequests *xsync.MapOf[string, *RequestInfo]    // Track active requests
	cacheDuration  time.Duration                         // How long to cache policies
}

// NewService creates a new rate limiter service
func NewService(db *bun.DB, rdb *redis.Client) *Service {
	return &Service{
		db:             db,
		rdb:            rdb,
		limiter:        limiters.NewRateLimiter(rdb),
		planCache:      xsync.NewMapOf[string, *CachedPolicies](),
		activeRequests: xsync.NewMapOf[string, *RequestInfo](),
		cacheDuration:  5 * time.Minute, // Cache policies for 5 minutes
	}
}

// EndpointPolicies represents rate limiting policies for each endpoint
type EndpointPolicies map[string]limiters.Policy

// AttemptRequest attempts to make a request with the given parameters
func (s *Service) AttemptRequest(ctx context.Context, requestID, accountID, endpointID string, tokens, credits int64) (bool, error) {
	// Check for duplicate request
	if info, exists := s.activeRequests.Load(requestID); exists {
		// If this is truly the same request (same account and endpoint), return the previous result
		if info.AccountID == accountID && info.EndpointID == endpointID {
			return info.Allowed, nil
		}
		// If different account or endpoint, this is a conflict
		return info.Allowed, fmt.Errorf("%w: requestID %s already in use", ErrDuplicateRequest, requestID)
	}

	if tokens < 0 {
		return false, fmt.Errorf("tokens cannot be negative: %d", tokens)
	}
	if credits < 0 {
		return false, fmt.Errorf("credits cannot be negative: %d", credits)
	}

	// 1. Check credits if needed
	if credits > 0 {
		hasCredits, err := s.hasEnoughCredits(ctx, accountID, credits)
		if err != nil {
			return false, fmt.Errorf("checking credits: %w", err)
		}
		if !hasCredits {
			return false, nil
		}
	}

	// 2. Get policies directly from materialized view
	policies, err := s.getAccountPolicies(ctx, accountID)
	if err != nil {
		return false, fmt.Errorf("getting account policies: %w", err)
	}

	policy, ok := policies[endpointID]
	if !ok {
		return false, fmt.Errorf("no access to endpoint: %s", endpointID)
	}

	// 3. Check rate limits
	allowed, err := s.limiter.CheckAndIncrement(ctx, 
		fmt.Sprintf("ratelimit:%s:%s", accountID, endpointID),
		policy,
		tokens)
	if err != nil {
		return false, fmt.Errorf("checking rate limits: %w", err)
	}
	if !allowed {
		// Store the failed attempt for idempotency
		s.activeRequests.Store(requestID, &RequestInfo{
			AccountID:  accountID,
			EndpointID: endpointID,
			StartTime:  time.Now(),
			Allowed:    false,
		})
		return false, nil
	}

	// 3. If rate limits pass and credits are required, try to consume them
	if credits > 0 {
		// Try to consume credits - we do our best effort here
		consumed, err := s.consumeCredits(ctx, accountID, credits)
		if err != nil {
			// Log the error but don't fail the request since rate limits passed
			log.Printf("Warning: Error consuming credits for account %s: %v", accountID, err)
		} else if consumed < credits {
			// Log if we couldn't consume all credits but still allow the request
			log.Printf("Warning: Could only consume %d of %d requested credits for account %s", 
				consumed, credits, accountID)
		}
	}

	// Store request info for later use
	s.activeRequests.Store(requestID, &RequestInfo{
		AccountID:  accountID,
		EndpointID: endpointID,
		StartTime:  time.Now(),
		Allowed:    true,
	})

	return true, nil
}

// ConsumeResult represents the result of consuming credits
type ConsumeResult struct {
	Requested     int64 // How many credits were requested
	Consumed      int64 // How many credits were actually consumed
	RemainingDue  int64 // How many credits couldn't be consumed
}

// EndRequest handles cleanup after a request ends and optionally consumes additional tokens/credits
func (s *Service) EndRequest(ctx context.Context, requestID string, finalTokens, finalCredits int64) (*ConsumeResult, error) {
	if finalTokens < 0 {
		return nil, fmt.Errorf("final tokens cannot be negative: %d", finalTokens)
	}
	if finalCredits < 0 {
		return nil, fmt.Errorf("final credits cannot be negative: %d", finalCredits)
	}

	// Get request info
	info, ok := s.activeRequests.Load(requestID)
	if !ok {
		return nil, fmt.Errorf("request not found: %s", requestID)
	}
	defer s.activeRequests.Delete(requestID)

	// Get the policies
	policies, err := s.getAccountPolicies(ctx, info.AccountID)
	if err != nil {
		return nil, fmt.Errorf("getting account policies: %w", err)
	}

	policy, ok := policies[info.EndpointID]
	if !ok {
		return nil, fmt.Errorf("no access to endpoint: %s", info.EndpointID)
	}

	// Handle final token consumption if specified
	if finalTokens > 0 {
		// Always attempt to consume tokens, even if it exceeds limits
		err := s.limiter.ForceConsumeTokens(ctx,
			fmt.Sprintf("ratelimit:%s:%s", info.AccountID, info.EndpointID),
			policy,
			finalTokens)
		if err != nil {
			return nil, fmt.Errorf("consuming final tokens: %w", err)
		}
	}

	var result *ConsumeResult
	// Handle final credit consumption if specified
	if finalCredits > 0 {
		consumed, err := s.consumeCredits(ctx, info.AccountID, finalCredits)
		if err != nil {
			return nil, fmt.Errorf("consuming final credits: %w", err)
		}
		result = &ConsumeResult{
			Requested:    finalCredits,
			Consumed:     consumed,
			RemainingDue: finalCredits - consumed,
		}
	}

	// Clean up concurrency tracking
	if policy.MaxConcurrent > 0 {
		err := s.limiter.DecrementConcurrency(ctx, 
			fmt.Sprintf("ratelimit:%s:%s", info.AccountID, info.EndpointID))
		if err != nil {
			return nil, fmt.Errorf("decrementing concurrency: %w", err)
		}
	}

	return result, nil
}

// hasEnoughCredits checks if the account has enough credits available
func (s *Service) hasEnoughCredits(ctx context.Context, accountID string, credits int64) (bool, error) {
	var total int64
	err := s.db.NewSelect().
		Table("ratelimit.account_credit_balance").
		Column("total_credits").
		Where("account_id = ?", accountID).
		Scan(ctx, &total)
	if err != nil {
		return false, fmt.Errorf("getting credit balance: %w", err)
	}

	return total >= credits, nil
}

// consumeCredits consumes credits from quota blocks, starting with the ones expiring soonest
// Returns the number of credits actually consumed
func (s *Service) consumeCredits(ctx context.Context, accountID string, credits int64) (int64, error) {
	// Get quota blocks ordered by expiration (NULLs last)
	var blocks []models.QuotaBlock
	err := s.db.NewSelect().
		Model(&blocks).
		Where("account_id = ?", accountID).
		Where("credits > 0").
		Where("expires_at > ? OR expires_at IS NULL", time.Now()).
		OrderExpr("COALESCE(expires_at, 'infinity'::timestamptz)").
		Scan(ctx)
	if err != nil {
		return 0, fmt.Errorf("getting quota blocks: %w", err)
	}

	var consumed int64
	remaining := credits
	for i := range blocks {
		if remaining <= 0 {
			break
		}

		toConsume := min(blocks[i].Credits, remaining)
		blocks[i].Credits -= toConsume
		remaining -= toConsume
		consumed += toConsume

		_, err = s.db.NewUpdate().
			Model(&blocks[i]).
			Column("credits").
			Where("id = ?", blocks[i].ID).
			Exec(ctx)
		if err != nil {
			return consumed, fmt.Errorf("updating quota block: %w", err)
		}
	}

	return consumed, nil
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// getAccountPolicies gets an account's policies from the materialized view
func (s *Service) getAccountPolicies(ctx context.Context, accountID string) (EndpointPolicies, error) {
	// Check cache first
	if cached, ok := s.planCache.Load(accountID); ok {
		if time.Now().Before(cached.ExpiresAt) {
			return cached.Policies, nil
		}
		// Cache expired, remove it
		s.planCache.Delete(accountID)
	}

	// Query the materialized view
	var policies []byte
	err := s.db.NewSelect().
		Table("ratelimit.account_policies").
		Column("policies").
		Where("account_id = ?", accountID).
		Scan(ctx, &policies)
	if err != nil {
		return nil, fmt.Errorf("finding policies: %w", err)
	}

	var endpointPolicies EndpointPolicies
	err = msgpack.Unmarshal(policies, &endpointPolicies)
	if err != nil {
		return nil, fmt.Errorf("unmarshaling policies: %w", err)
	}

	// Cache the policies with expiration
	s.planCache.Store(accountID, &CachedPolicies{
		Policies:  endpointPolicies,
		ExpiresAt: time.Now().Add(s.cacheDuration),
	})

	return endpointPolicies, nil
}

// Close cleans up service connections
func (s *Service) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing database: %w", err)
	}
	if err := s.rdb.Close(); err != nil {
		return fmt.Errorf("closing redis: %w", err)
	}
	return nil
}

// GetRateLimitInfo returns the current rate limit information for an account and endpoint
func (s *Service) GetRateLimitInfo(ctx context.Context, accountID string, endpoint string) (*limiters.RateLimitInfo, error) {
	// Get the account's plan and policies
	policies, err := s.getAccountPolicies(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("getting account policies: %w", err)
	}

	// Check if endpoint exists in policies
	policy, ok := policies[endpoint]
	if !ok {
		return nil, fmt.Errorf("endpoint %s not found in plan policies", endpoint)
	}

	// Get rate limit info using the limiter
	key := fmt.Sprintf("%s:%s", accountID, endpoint)
	info, err := s.limiter.GetRateLimitInfo(ctx, key, policy)
	if err != nil {
		return nil, fmt.Errorf("getting rate limit info: %w", err)
	}

	return info, nil
}

// RateLimitInfo represents the current state of rate limits for an endpoint
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

// DB returns the underlying bun.DB instance for admin operations
func (s *Service) DB() *bun.DB {
	return s.db
}
