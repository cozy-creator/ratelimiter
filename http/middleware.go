package http

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/cozy-creator/ratelimiter/service"
)

// AccountIDExtractor extracts the account ID from the request
type AccountIDExtractor func(r *http.Request) (string, error)

// EndpointIDExtractor extracts the endpoint ID from the request
type EndpointIDExtractor func(r *http.Request) string

// TokenExtractor extracts the number of tokens to consume from the request
type TokenExtractor func(r *http.Request) int64

// CreditExtractor extracts the number of credits to consume from the request
type CreditExtractor func(r *http.Request) int64

// RequestIDGenerator generates a unique ID for each request
type RequestIDGenerator func(r *http.Request) string

// MiddlewareConfig configures how the rate limiting middleware behaves
type MiddlewareConfig struct {
	// Required configurations
	GetAccountID AccountIDExtractor
	GetEndpoint  EndpointIDExtractor

	// Optional configurations with defaults
	GetTokens   TokenExtractor   // Defaults to 1 token per request
	GetCredits  CreditExtractor  // Defaults to 0 credits
	GenerateID  RequestIDGenerator

	// Whether to return 429 Too Many Requests or let the request through
	// when rate limit is exceeded
	EnforceRateLimit bool
}

// DefaultMiddlewareConfig returns a configuration with sensible defaults
func DefaultMiddlewareConfig() MiddlewareConfig {
	return MiddlewareConfig{
		GetAccountID: func(r *http.Request) (string, error) {
			id := r.Header.Get("X-Account-ID")
			if id == "" {
				return "", errors.New("no account ID provided")
			}
			return id, nil
		},
		GetEndpoint: func(r *http.Request) string {
			return r.URL.Path
		},
		GetTokens: func(r *http.Request) int64 {
			return 1 // Default to 1 token per request
		},
		GetCredits: func(r *http.Request) int64 {
			return 0 // Default to no credits
		},
		GenerateID: func(r *http.Request) string {
			return fmt.Sprintf("%s-%s-%d", r.URL.Path, r.RemoteAddr, time.Now().UnixNano())
		},
		EnforceRateLimit: true,
	}
}

var ErrRateLimitExceeded = errors.New("rate limit exceeded")

// RateLimitMiddleware creates middleware that enforces rate limits
func RateLimitMiddleware(svc *service.Service, cfg MiddlewareConfig) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			// Get required information
			accountID, err := cfg.GetAccountID(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}

			endpoint := cfg.GetEndpoint(r)
			requestID := cfg.GenerateID(r)
			tokens := cfg.GetTokens(r)
			credits := cfg.GetCredits(r)

			// Get rate limit info
			info, err := svc.GetRateLimitInfo(ctx, accountID, endpoint)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			// Add rate limit headers
			w.Header().Set("X-RateLimit-Limit-Requests", fmt.Sprintf("%d", info.RequestLimit))
			w.Header().Set("X-RateLimit-Remaining-Requests", fmt.Sprintf("%d", info.RequestRemaining))
			w.Header().Set("X-RateLimit-Reset-Requests", fmt.Sprintf("%d", int(info.RequestReset.Seconds())))
			
			if info.TokenLimit > 0 {
				w.Header().Set("X-RateLimit-Limit-Tokens", fmt.Sprintf("%d", info.TokenLimit))
				w.Header().Set("X-RateLimit-Remaining-Tokens", fmt.Sprintf("%d", info.TokenRemaining))
				w.Header().Set("X-RateLimit-Reset-Tokens", fmt.Sprintf("%d", int(info.TokenReset.Seconds())))
			}

			// Attempt the request
			allowed, err := svc.AttemptRequest(ctx, requestID, accountID, endpoint, tokens, credits)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !allowed && cfg.EnforceRateLimit {
				http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
				return
			}

			// Call the next handler
			next.ServeHTTP(w, r)

			// End the request and handle any final consumption
			result, err := svc.EndRequest(ctx, requestID, tokens, credits)
			if err != nil {
				log.Printf("Error ending request: %v", err)
			}
			if result != nil && result.RemainingDue > 0 {
				log.Printf("Warning: Could only consume %d of %d requested credits", 
					result.Consumed, result.Requested)
			}
		})
	}
}

// Example usage:
/*
func main() {
    // Create service...
    svc := service.NewService(db, rdb)

    // Configure middleware
    cfg := DefaultMiddlewareConfig()
    
    // Customize account ID extraction (e.g., from JWT)
    cfg.GetAccountID = func(r *http.Request) (string, error) {
        claims, err := extractJWTClaims(r)
        if err != nil {
            return "", err
        }
        return claims.AccountID, nil
    }

    // Customize token consumption based on request body size
    cfg.GetTokens = func(r *http.Request) int64 {
        return int64(r.ContentLength / 1000) // 1 token per KB
    }

    // Create router and add middleware
    router := http.NewServeMux()
    router.Handle("/api/", RateLimitMiddleware(svc, cfg)(apiHandler))
}
*/ 