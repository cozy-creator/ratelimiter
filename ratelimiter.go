package ratelimiter

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/cozy-creator/ratelimiter/admin"
	"github.com/cozy-creator/ratelimiter/limiters"
	"github.com/cozy-creator/ratelimiter/models"
	"github.com/cozy-creator/ratelimiter/service"
	"github.com/redis/go-redis/v9"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// Client represents a rate limiter client instance
type Client struct {
	svc *service.Service
	db  *bun.DB       // Only non-nil if we own the connection
	rdb *redis.Client // Only non-nil if we own the connection
}

// ClientOption is a function that configures the client
type ClientOption func(*clientOptions)

type clientOptions struct {
	postgresDSN   string
	redisAddr     string
	redisPassword string
	redisDB       int
	db            *bun.DB
	rdb           *redis.Client
}

// WithPostgresDSN sets the PostgreSQL connection string
func WithPostgresDSN(dsn string) ClientOption {
	return func(o *clientOptions) {
		o.postgresDSN = dsn
	}
}

// WithRedisAddr sets the Redis address
func WithRedisAddr(addr string) ClientOption {
	return func(o *clientOptions) {
		o.redisAddr = addr
	}
}

// WithRedisPassword sets the Redis password
func WithRedisPassword(password string) ClientOption {
	return func(o *clientOptions) {
		o.redisPassword = password
	}
}

// WithRedisDB sets the Redis database number
func WithRedisDB(db int) ClientOption {
	return func(o *clientOptions) {
		o.redisDB = db
	}
}

// WithDBClient sets an external bun.DB client
func WithDBClient(db *bun.DB) ClientOption {
	return func(o *clientOptions) {
		o.db = db
	}
}

// WithRedisClient sets an external Redis client
func WithRedisClient(rdb *redis.Client) ClientOption {
	return func(o *clientOptions) {
		o.rdb = rdb
	}
}

// NewClient creates a new rate limiter client with the given options
func NewClient(opts ...ClientOption) (*Client, error) {
	options := &clientOptions{
		postgresDSN:   "postgres://postgres:postgres@localhost:5432/ratelimiter?sslmode=disable",
		redisAddr:     "localhost:6379",
		redisPassword: "",
		redisDB:       0,
	}

	for _, opt := range opts {
		opt(options)
	}

	ctx := context.Background()
	var db *bun.DB
	var rdb *redis.Client

	// Setup PostgreSQL connection
	if options.db != nil {
		db = options.db
		if err := db.PingContext(ctx); err != nil {
			return nil, fmt.Errorf("postgres connection check failed: %w", err)
		}
	} else {
		sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(options.postgresDSN)))
		db = bun.NewDB(sqldb, pgdialect.New())
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return nil, fmt.Errorf("connecting to postgres: %w", err)
		}
	}

	// Setup Redis connection
	if options.rdb != nil {
		rdb = options.rdb
		if err := rdb.Ping(ctx).Err(); err != nil {
			if options.db == nil {
				db.Close()
			}
			return nil, fmt.Errorf("redis connection check failed: %w", err)
		}
	} else {
		rdb = redis.NewClient(&redis.Options{
			Addr:     options.redisAddr,
			Password: options.redisPassword,
			DB:       options.redisDB,
		})
		if err := rdb.Ping(ctx).Err(); err != nil {
			if options.db == nil {
				db.Close()
			}
			rdb.Close()
			return nil, fmt.Errorf("connecting to redis: %w", err)
		}
	}

	// Create service
	svc := service.NewService(db, rdb)

	// Determine which connections to store based on ownership
	var ownedDB *bun.DB
	if options.db == nil {
		ownedDB = db
	}
	var ownedRDB *redis.Client
	if options.rdb == nil {
		ownedRDB = rdb
	}

	return &Client{
		svc: svc,
		db:  ownedDB,    // Only store if we own it
		rdb: ownedRDB,   // Only store if we own it
	}, nil
}

// Close closes the client's connections if it owns them
func (c *Client) Close() error {
	if err := c.svc.Close(); err != nil {
		return fmt.Errorf("closing service: %w", err)
	}
	
	// Only close connections we own
	if c.db != nil {
		if err := c.db.Close(); err != nil {
			return fmt.Errorf("closing postgres: %w", err)
		}
	}
	if c.rdb != nil {
		if err := c.rdb.Close(); err != nil {
			return fmt.Errorf("closing redis: %w", err)
		}
	}
	return nil
}

// AttemptRequest attempts to make a request with the given parameters.
// By default, this consumes 1 request unit. Additional unit consumption
// can be specified using WithUnits() and WithMinCreditBalance() options.
//
// Returns:
// - allowed: whether the request is allowed to proceed
// - info: rate limit information for each unit type, showing the most constraining window
// - error: any error that occurred
//
// The rate limit info includes:
// - For each unit type (e.g., "request", "token", "gpu-second"):
//   - Limit: maximum units allowed in the window
//   - Remaining: units remaining in the window
//   - Reset: time until the window resets
// - If concurrency limits are enabled, includes a "concurrent" entry with:
//   - Limit: maximum concurrent requests
//   - Remaining: remaining concurrent request slots
//   - Reset: time until request is no longer counted as concurrent
func (c *Client) AttemptRequest(ctx context.Context, requestID, accountID, endpointID string, opts ...service.RequestOption) (bool, map[string]*limiters.WindowInfo, error) {
	return c.svc.AttemptRequest(ctx, requestID, accountID, endpointID, opts...)
}

// EndRequest handles cleanup after a request ends and optionally deducts final units
func (c *Client) EndRequest(ctx context.Context, requestID string, opts ...service.RequestOption) error {
	return c.svc.EndRequest(ctx, requestID, opts...)
}

// GetRateLimitInfo returns the current rate limit information for an account and endpoint
func (c *Client) GetRateLimitInfo(ctx context.Context, accountID, endpointID string) (map[string]*limiters.WindowInfo, error) {
	return c.svc.GetRateLimitInfo(ctx, accountID, endpointID)
}

// DB returns the underlying bun.DB instance for admin operations
func (c *Client) DB() *bun.DB {
	return c.svc.DB()
}

// DeductUnits returns an option that specifies units to deduct for a request.
// This is a convenience wrapper that creates a map with a single unit type.
func DeductUnits(unitType string, amount int64) service.RequestOption {
	units := map[string]int64{unitType: amount}
	return service.DeductUnits(units)
}

// HasMinBalance checks if an account has at least the specified credit balance available
func (c *Client) HasMinBalance(ctx context.Context, accountID string, minAmount int64) (bool, error) {
	return c.svc.HasMinBalance(ctx, accountID, minAmount)
}

// GetBalance returns the total available credit balance for an account
func (c *Client) GetBalance(ctx context.Context, accountID string) (int64, error) {
	return c.svc.GetBalance(ctx, accountID)
}

// WithRequireFullAmount configures whether to require the full amount to be available when deducting credits
func WithRequireFullAmount(require bool) service.DeductCreditsOption {
	return service.WithRequireFullAmount(require)
}

// DeductCredits attempts to deduct credits from an account's quota blocks.
// By default, it will deduct as many credits as possible, even if the account doesn't have enough.
// Use WithRequireFullAmount(true) to require the full amount to be available.
func (c *Client) DeductCredits(ctx context.Context, accountID string, credits int64, opts ...service.DeductCreditsOption) (*service.DeductCreditsResult, error) {
	return c.svc.DeductCredits(ctx, accountID, credits, opts...)
}

// CreateAccount creates a new account with the given plan
func (c *Client) CreateAccount(ctx context.Context, accountID string, planID string) error {
	account := &models.Account{
		ID:     accountID,
		PlanID: planID,
	}
	_, err := c.DB().NewInsert().
		Model(account).
		On("CONFLICT (id) DO UPDATE").
		Set("plan_id = EXCLUDED.plan_id").
		Set("updated_at = ?", time.Now()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("creating account: %w", err)
	}
	return nil
}

// AssignPlan assigns a plan to an account
func (c *Client) AssignPlan(ctx context.Context, accountID string, planID string) error {
	return admin.AssignPlan(ctx, c.DB(), accountID, planID)
}

// SetPlan creates or updates a rate limiting plan
func (c *Client) SetPlan(ctx context.Context, planID string, name string, policy map[string]limiters.Policy) error {
	plan := admin.Plan{
		Name:      name,
		Endpoints: policy,
	}
	return admin.ApplyPlans(ctx, c.DB(), admin.Plans{planID: plan})
}

// AddPlan is deprecated, use SetPlan instead
func (c *Client) AddPlan(ctx context.Context, planID string, name string, endpoints map[string]limiters.Policy) error {
	return c.SetPlan(ctx, planID, name, endpoints)
}

// CreateQuotaBlock creates a new quota block for an account
func (c *Client) CreateQuotaBlock(ctx context.Context, accountID string, credits int64, expiresAt time.Time, metadata []byte) error {
	quotaBlock := &models.QuotaBlock{
		AccountID: accountID,
		Credits:   credits,
		ExpiresAt: expiresAt,
		Metadata:  metadata,
	}
	_, err := c.DB().NewInsert().
		Model(quotaBlock).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("creating quota block: %w", err)
	}
	return nil
}

// DeleteQuotaBlock deletes a quota block by ID
func (c *Client) DeleteQuotaBlock(ctx context.Context, blockID string) error {
	_, err := c.DB().NewDelete().
		Model((*models.QuotaBlock)(nil)).
		Where("id = ?", blockID).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting quota block: %w", err)
	}
	return nil
}

// ExpireQuotaBlocks expires all active quota blocks for an account
func (c *Client) ExpireQuotaBlocks(ctx context.Context, accountID string) error {
	_, err := c.DB().NewUpdate().
		Model((*models.QuotaBlock)(nil)).
		Set("expires_at = ?", time.Now()).
		Where("account_id = ? AND (expires_at IS NULL OR expires_at > ?)", accountID, time.Now()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("expiring quota blocks: %w", err)
	}
	return nil
}

// SetDefaultPlan sets a plan as the default
func (c *Client) SetDefaultPlan(ctx context.Context, planID string) error {
	return admin.SetDefaultPlan(ctx, c.DB(), planID)
}

