package service

import (
	"context"
	"fmt"
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
	activeRequests *xsync.MapOf[string, RequestInfo]    // Track active requests
	cacheDuration  time.Duration                         // How long to cache policies
}

// NewService creates a new rate limiter service
func NewService(db *bun.DB, rdb *redis.Client) *Service {
	return &Service{
		db:             db,
		rdb:            rdb,
		limiter:        limiters.NewRateLimiter(rdb),
		planCache:      xsync.NewMapOf[string, *CachedPolicies](),
		activeRequests: xsync.NewMapOf[string, RequestInfo](),
		cacheDuration:  5 * time.Minute, // Cache policies for 5 minutes
	}
}

// EndpointPolicies represents rate limiting policies for each endpoint
type EndpointPolicies map[string]limiters.Policy

// RequestOption is a function that configures a request
type RequestOption func(*RequestOptions)

// RequestOptions holds the configuration for a request
type RequestOptions struct {
	Units map[string]int64
}

// DeductUnits specifies the units to deduct for the request
func DeductUnits(units map[string]int64) RequestOption {
	return func(o *RequestOptions) {
		if o.Units == nil {
			o.Units = make(map[string]int64)
		}
		for k, v := range units {
			o.Units[k] = v
		}
	}
}

// AttemptRequest attempts to make a request with the given parameters
func (s *Service) AttemptRequest(ctx context.Context, requestID, accountID, endpointID string, opts ...RequestOption) (bool, map[string]*limiters.WindowInfo, error) {
	// Parse options
	reqOpts := &RequestOptions{
		Units: make(map[string]int64),
	}
	for _, opt := range opts {
		opt(reqOpts)
	}

	// Check for duplicate request ID
	if info, exists := s.activeRequests.Load(requestID); exists {
		// If this is a duplicate request, return the same result as before
		if info.AccountID == accountID && info.EndpointID == endpointID {
			return info.Allowed, nil, ErrDuplicateRequest
		}
		// If different account/endpoint, treat as a conflict
		return false, nil, fmt.Errorf("request ID conflict: %s already in use by different account/endpoint", requestID)
	}

	// Store request info before proceeding
	reqInfo := RequestInfo{
		AccountID:  accountID,
		EndpointID: endpointID,
		StartTime:  time.Now(),
		Allowed:    false, // Will be updated if request is allowed
	}
	s.activeRequests.Store(requestID, reqInfo)

	// Validate units
	for unitType, amount := range reqOpts.Units {
		if amount < 0 {
			return false, nil, fmt.Errorf("invalid negative amount for unit type %s: %d", unitType, amount)
		}
	}

	// Get policies from materialized view
	policies, err := s.getAccountPolicies(ctx, accountID)
	if err != nil {
		return false, nil, fmt.Errorf("getting policies: %w", err)
	}

	policy, ok := policies[endpointID]
	if !ok {
		return false, nil, fmt.Errorf("no policy found for endpoint %s", endpointID)
	}

	// Check rate limits
	key := fmt.Sprintf("ratelimit:%s:%s", accountID, endpointID)
	allowed, info, err := s.limiter.CheckAndIncrement(ctx, key, policy, reqOpts.Units, requestID)
	if err != nil {
		return false, nil, fmt.Errorf("checking rate limit: %w", err)
	}

	// Update the stored request info with the result
	reqInfo.Allowed = allowed
	s.activeRequests.Store(requestID, reqInfo)

	return allowed, info, nil
}

// ConsumeResult represents the result of consuming credits
type ConsumeResult struct {
	Requested     int64 // How many credits were requested
	Consumed      int64 // How many credits were actually consumed
	RemainingDue  int64 // How many credits couldn't be consumed
}

// EndRequest handles cleanup after a request ends and optionally deducts final units
func (s *Service) EndRequest(ctx context.Context, requestID string, opts ...RequestOption) error {
	// Parse options
	reqOpts := &RequestOptions{
		Units: make(map[string]int64),
	}
	for _, opt := range opts {
		opt(reqOpts)
	}

	// Get request info
	reqInfo, ok := s.activeRequests.Load(requestID)
	if !ok {
		return fmt.Errorf("request not found: %s", requestID)
	}
	defer s.activeRequests.Delete(requestID)

	// Get policies from materialized view
	policies, err := s.getAccountPolicies(ctx, reqInfo.AccountID)
	if err != nil {
		return fmt.Errorf("getting policies: %w", err)
	}

	policy, ok := policies[reqInfo.EndpointID]
	if !ok {
		return fmt.Errorf("no policy found for endpoint %s", reqInfo.EndpointID)
	}

	// Clean up request tracking and deduct final units
	key := fmt.Sprintf("ratelimit:%s:%s", reqInfo.AccountID, reqInfo.EndpointID)
	if err := s.limiter.EndRequest(ctx, key, policy, requestID, reqOpts.Units); err != nil {
		return fmt.Errorf("ending request: %w", err)
	}

	return nil
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

// DB returns the underlying bun.DB instance for admin operations
func (s *Service) DB() *bun.DB {
	return s.db
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

// GetRateLimitInfo returns the current rate limit information for an account and endpoint
func (s *Service) GetRateLimitInfo(ctx context.Context, accountID, endpointID string) (map[string]*limiters.WindowInfo, error) {
	// Get policies from materialized view
	policies, err := s.getAccountPolicies(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("getting policies: %w", err)
	}

	policy, ok := policies[endpointID]
	if !ok {
		return nil, fmt.Errorf("no policy found for endpoint %s", endpointID)
	}

	// Get rate limit info
	key := fmt.Sprintf("ratelimit:%s:%s", accountID, endpointID)
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

// DeductCreditsResult represents the result of a credit deduction attempt
type DeductCreditsResult struct {
	Requested int64 // How many credits were requested to be deducted
	Deducted  int64 // How many credits were actually deducted
	Remaining int64 // How many credits couldn't be deducted
}

// HasMinBalance checks if an account has at least the specified credit balance available
func (s *Service) HasMinBalance(ctx context.Context, accountID string, minAmount int64) (bool, error) {
	if minAmount < 0 {
		return false, fmt.Errorf("minimum amount must be non-negative, got %d", minAmount)
	}
	return s.hasEnoughCredits(ctx, accountID, minAmount)
}

// GetBalance returns the total available credit balance for an account
func (s *Service) GetBalance(ctx context.Context, accountID string) (int64, error) {
	var total int64
	err := s.db.NewSelect().
		Table("ratelimit.account_credit_balance").
		Column("total_credits").
		Where("account_id = ?", accountID).
		Scan(ctx, &total)
	if err != nil {
		return 0, fmt.Errorf("getting credit balance: %w", err)
	}
	return total, nil
}

// DeductCreditsOptions represents options for deducting credits
type DeductCreditsOptions struct {
	RequireFullAmount bool // If true, fail if the account doesn't have enough credits
}

// DeductCreditsOption is a function that configures credit deduction options
type DeductCreditsOption func(*DeductCreditsOptions)

// WithRequireFullAmount configures whether to require the full amount to be available
func WithRequireFullAmount(require bool) DeductCreditsOption {
	return func(o *DeductCreditsOptions) {
		o.RequireFullAmount = require
	}
}

// DeductCredits attempts to deduct credits from an account's quota blocks.
// By default, it will deduct as many credits as possible, even if the account doesn't have enough.
// Use WithRequireFullAmount(true) to require the full amount to be available.
func (s *Service) DeductCredits(ctx context.Context, accountID string, credits int64, opts ...DeductCreditsOption) (*DeductCreditsResult, error) {
	if credits <= 0 {
		return nil, fmt.Errorf("credits must be positive, got %d", credits)
	}

	// Parse options
	options := &DeductCreditsOptions{
		RequireFullAmount: false,
	}
	for _, opt := range opts {
		opt(options)
	}

	// Start transaction
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// If requiring full amount, check balance within transaction
	if options.RequireFullAmount {
		var total int64
		err := tx.NewSelect().
			Table("ratelimit.account_credit_balance").
			Column("total_credits").
			Where("account_id = ?", accountID).
			Scan(ctx, &total)
		if err != nil {
			return nil, fmt.Errorf("checking credit balance: %w", err)
		}
		if total < credits {
			return nil, fmt.Errorf("insufficient credit balance: need %d credits, have %d", credits, total)
		}
	}

	// Get quota blocks ordered by expiration (NULLs last)
	var blocks []models.QuotaBlock
	err = tx.NewSelect().
		Model(&blocks).
		Where("account_id = ?", accountID).
		Where("credits > 0").
		Where("expires_at > ? OR expires_at IS NULL", time.Now()).
		OrderExpr("COALESCE(expires_at, 'infinity'::timestamptz)").
		For("UPDATE").  // Lock rows for update
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting quota blocks: %w", err)
	}

	var deducted int64
	remaining := credits
	for i := range blocks {
		if remaining <= 0 {
			break
		}

		toConsume := min(blocks[i].Credits, remaining)
		blocks[i].Credits -= toConsume
		remaining -= toConsume
		deducted += toConsume

		_, err = tx.NewUpdate().
			Model(&blocks[i]).
			Column("credits").
			Where("id = ?", blocks[i].ID).
			Exec(ctx)
		if err != nil {
			return nil, fmt.Errorf("updating quota block: %w", err)
		}
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return &DeductCreditsResult{
		Requested: credits,
		Deducted:  deducted,
		Remaining: credits - deducted,
	}, nil
}
