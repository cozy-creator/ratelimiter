package ratelimiter

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/cozy-creator/rate-limiter/service"
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

// WithTokens specifies the number of tokens to consume
func WithTokens(tokens int64) RequestOption {
	return func(o *RequestOptions) {
		o.tokens = tokens
	}
}

// WithCredits specifies the number of credits to consume
func WithCredits(credits int64) RequestOption {
	return func(o *RequestOptions) {
		o.credits = credits
	}
}

// Client represents a rate limiter client instance
type Client struct {
	service *service.Service
}

// Config holds the configuration for the rate limiter client
type Config struct {
	// PostgreSQL connection string
	PostgresDSN string
	// Redis connection information
	RedisAddr     string
	RedisPassword string
	RedisDB       int
}

// DefaultConfig returns a configuration with default values
func DefaultConfig() Config {
	return Config{
		PostgresDSN:   "postgres://postgres:postgres@localhost:5432/ratelimiter?sslmode=disable",
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       0,
	}
}

// NewClient creates a new rate limiter client with the given configuration
func NewClient(cfg Config) (*Client, error) {
	// Setup database
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(cfg.PostgresDSN)))
	db := bun.NewDB(sqldb, pgdialect.New())

	// Test PostgreSQL connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	// Setup Redis
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})

	// Test Redis connection
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("connecting to redis: %w", err)
	}

	// Create rate limiter service
	svc := service.NewService(db, rdb)

	return &Client{service: svc}, nil
}

// Close closes the client's connections
func (c *Client) Close() error {
	// Cleanup logic for database and redis connections
	if err := c.service.Close(); err != nil {
		return fmt.Errorf("closing service: %w", err)
	}
	return nil
}

// AttemptRequest attempts to make a request with the given parameters.
// By default, this consumes 1 request unit. Additional token or credit consumption
// can be specified using WithTokens() and WithCredits() options.
func (c *Client) AttemptRequest(ctx context.Context, requestID, accountID, endpointID string, opts ...RequestOption) (bool, error) {
	options := &RequestOptions{}
	for _, opt := range opts {
		opt(options)
	}
	return c.service.AttemptRequest(ctx, requestID, accountID, endpointID, options.tokens, options.credits)
}

// EndRequest handles cleanup after a request ends and optionally consumes additional tokens/credits.
// By default, this only cleans up the request tracking. Additional token or credit consumption
// can be specified using WithTokens() and WithCredits() options.
func (c *Client) EndRequest(ctx context.Context, requestID string, opts ...RequestOption) (*service.ConsumeResult, error) {
	options := &RequestOptions{}
	for _, opt := range opts {
		opt(options)
	}
	return c.service.EndRequest(ctx, requestID, options.tokens, options.credits)
}

