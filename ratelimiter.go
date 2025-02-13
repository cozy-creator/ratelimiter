package ratelimiter

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/cozy-creator/ratelimiter/limiters"
	"github.com/cozy-creator/ratelimiter/service"
	"github.com/redis/go-redis/v9"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// RequestOptions holds the options for a rate limit request
type RequestOptions struct {
	tokens  int64
	credits int64
}

// RequestOption is a function that configures RequestOptions
type RequestOption func(*RequestOptions)

// DeductTokens specifies the number of tokens to consume
func DeductTokens(tokens int64) RequestOption {
	return func(o *RequestOptions) {
		o.tokens = tokens
	}
}

// DeductCredits specifies the number of credits to consume
func DeductCredits(credits int64) RequestOption {
	return func(o *RequestOptions) {
		o.credits = credits
	}
}

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
// By default, this consumes 1 request unit. Additional token or credit consumption
// can be specified using DeductTokens() and DeductCredits() options.
func (c *Client) AttemptRequest(ctx context.Context, requestID, accountID, endpointID string, opts ...RequestOption) (bool, error) {
	options := &RequestOptions{}
	for _, opt := range opts {
		opt(options)
	}
	return c.svc.AttemptRequest(ctx, requestID, accountID, endpointID, options.tokens, options.credits)
}

// EndRequest handles cleanup after a request ends and optionally consumes additional tokens/credits.
// By default, this only cleans up the request tracking. Additional token or credit consumption
// can be specified using DeductTokens() and DeductCredits() options.
func (c *Client) EndRequest(ctx context.Context, requestID string, opts ...RequestOption) (*service.ConsumeResult, error) {
	options := &RequestOptions{}
	for _, opt := range opts {
		opt(options)
	}
	return c.svc.EndRequest(ctx, requestID, options.tokens, options.credits)
}

// GetRateLimitInfo returns the current rate limit information for an account and endpoint
func (c *Client) GetRateLimitInfo(ctx context.Context, accountID, endpointID string) (*limiters.RateLimitInfo, error) {
	return c.svc.GetRateLimitInfo(ctx, accountID, endpointID)
}

// DB returns the underlying bun.DB instance for admin operations
func (c *Client) DB() *bun.DB {
	return c.svc.DB()
}

