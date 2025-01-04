# Distributed Rate Limiter

A minimalist distributed rate limiter service that uses a token bucket algorithm and gossip protocol for synchronization across multiple nodes. Perfect for rate limiting API requests across a distributed system.

## Features

- API key based authentication
- Configurable rate limits and bucket sizes
- REST API interface

## Quick Start

1. Set up the environment variables in `docker-compose.yml`:
   - `API_KEYS`: Comma-separated list of valid API keys
   - `PORT`: HTTP port for the service
   - `BIND_ADDR`: Address for node communication
   - `KNOWN_PEERS`: Addresses of other nodes to connect to

2. Run the cluster:
```bash
docker-compose up
```

## API Documentation

### Authentication

All API endpoints require authentication using a Bearer token. Include your API key in the Authorization header:

```
Authorization: Bearer your-api-key
```

### Endpoints

#### Check Rate Limit

```http
POST /consume?key=<identifier>&tokens=<count>
```

Parameters:
- `key` (required): Unique identifier for the rate limit bucket (e.g., user ID, IP address)
- `tokens` (optional): Number of tokens to consume (default: 1)

Headers:
- `Authorization: Bearer <api-key>` (required)

Response Codes:
- 200: Request allowed
- 429: Rate limit exceeded
- 401: Invalid or missing API key
- 400: Invalid parameters

Example Request:
```bash
curl -X POST "http://localhost:8080/consume?key=user123&tokens=1" \
     -H "Authorization: Bearer your-api-key"
```

Example Success Response:
```json
{
    "success": true
}
```

Example Rate Limit Exceeded Response:
```json
{
    "error": "rate limit exceeded"
}
```

## Integration Guide

### Using as a Rate Limiting Service

1. Deploy the rate limiter cluster using Docker Compose or your preferred orchestration tool.

2. Configure your application with one of the valid API keys.

3. Before processing any request in your application, check the rate limit:

```python
# Python example
import requests

def check_rate_limit(user_id, tokens=1):
    response = requests.post(
        f"http://localhost:8080/consume",
        params={"key": user_id, "tokens": tokens},
        headers={"Authorization": "Bearer your-api-key"}
    )
    
    if response.status_code == 200:
        return True  # Request allowed
    elif response.status_code == 429:
        return False  # Rate limit exceeded
    else:
        raise Exception(f"Rate limiter error: {response.text}")

# Usage in your API
def handle_api_request(user_id):
    if not check_rate_limit(user_id):
        return "Rate limit exceeded", 429
    
    # Process the request
    return "Success", 200
```

```go
// Go example
package main

import (
    "fmt"
    "net/http"
)

func checkRateLimit(client *http.Client, userID string, tokens int) (bool, error) {
    req, err := http.NewRequest("POST", 
        fmt.Sprintf("http://localhost:8080/consume?key=%s&tokens=%d", userID, tokens),
        nil)
    if err != nil {
        return false, err
    }
    
    req.Header.Add("Authorization", "Bearer your-api-key")
    
    resp, err := client.Do(req)
    if err != nil {
        return false, err
    }
    defer resp.Body.Close()
    
    return resp.StatusCode == http.StatusOK, nil
}

// Usage in your API handler
func handleRequest(w http.ResponseWriter, r *http.Request) {
    userID := getUserID(r) // Get user ID from request
    
    allowed, err := checkRateLimit(http.DefaultClient, userID, 1)
    if err != nil {
        http.Error(w, "Rate limiter error", http.StatusInternalServerError)
        return
    }
    
    if !allowed {
        http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
        return
    }
    
    // Process the request
    w.Write([]byte("Success"))
}
```

### Configuration

The rate limiter can be configured using environment variables:

- `API_KEYS`: Comma-separated list of valid API keys
- `PORT`: HTTP port for the service (default: 8080)
- `BIND_ADDR`: Address for node communication (default: 0.0.0.0:7946)
- `KNOWN_PEERS`: Comma-separated list of other nodes to connect to

### Default Rate Limits

- Bucket capacity: 100 tokens
- Refill rate: 10 tokens per second

To modify these defaults, update the values in `main.go` or make them configurable via environment variables.

## Architecture

The rate limiter uses a distributed architecture with the following components:

1. Token Bucket Algorithm: Each rate limit key has its own token bucket that refills at a constant rate.
2. Gossip Protocol: Uses Hashicorp's memberlist for node discovery and state synchronization.
3. Eventual Consistency: Updates are propagated to all nodes asynchronously.

## Best Practices

1. Choose appropriate rate limit keys:
   - User ID for per-user limits
   - IP address for per-client limits
   - API key for per-application limits
   - Combination of above for more granular control

2. Set reasonable token counts:
   - Use higher token counts for batch operations
   - Use lower token counts for simple API calls

3. Handle rate limit errors gracefully:
   - Implement exponential backoff
   - Show user-friendly error messages
   - Consider providing retry-after headers

4. Monitor rate limit usage:
   - Track rate limit errors
   - Alert on unusual patterns
   - Adjust limits based on usage patterns

